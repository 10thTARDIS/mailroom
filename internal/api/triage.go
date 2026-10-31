// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/ml"
	"mailroom/internal/store"
)

// Triage control methods for the urn:mailroom:triage extension (spec §8, #34).
// PR1 exposed the read-only priorityScore/isImportant/explanation on the Email
// object; this file adds the USER-FACING controls that pair with that display:
//
//   - Triage/get     — read the caller's priority-model state (active version +
//     lineage), so a client can show "which model is deciding my mail".
//   - Triage/set     — the model-control safety valve: reset (start the model
//     over from a fresh baseline) or rollback (re-activate an earlier version).
//     This is the surface for the #35 Registry backend, which nothing exposed
//     until now.
//   - Triage/correct — one-click importance correction: the explicit training
//     signal that pairs with PR1's explanation ("this reason is wrong → correct
//     it"). It records a feedback_event the priority model learns from AND, since
//     #464, the user's own placement of that message relative to the desk's line,
//     which moves the row immediately.
//
// The model (app.ml_models) and feedback (app.feedback_events) are OPERATIONAL
// data — the SAME category as triage_scores, NOT JMAP-visible objects. Writing
// either appends NO change_log row and neither is a store ObjectType (CLAUDE.md's
// load-bearing invariant is about JMAP-visible objects only); nothing about the
// MODEL is ever synced via /changes, the indexer, or a client cursor.
//
// ⚠ THE ONE EXCEPTION IS Triage/correct's PLACEMENT (#464), and it is not an
// exception to the rule so much as a second write of a different kind: it
// mutates app.messages.line_side, and a message IS a JMAP-visible object, so it
// appends the Email `updated` row in the same transaction like every other
// message mutation (store.UserTx.SetLineSide). Triage/get and Triage/set touch
// the model alone and remain change-row-free.
//
// The model is per-user, so there is no account scoping to do (a JMAP account IS
// the user): every read/write goes through the caller's own UserTx / Registry
// call, and RLS scopes it to that user — no identity, no rows (fail closed).

// Correction values a client sends to Triage/correct, and the feedback action
// tokens they map to. The tokens MUST equal ml/eval.go's actionLabels keys — an
// unmapped token is silently dropped by the eval/training harness, so a drift
// would quietly lose the signal. feedback_action_test.go / triage_test.go
// cross-check them against ml.LabelForAction.
const (
	// correctionImportant / correctionNotImportant are the two values a client
	// may send per message id in a Triage/correct request.
	correctionImportant    = "important"
	correctionNotImportant = "notImportant"

	// feedbackMarkImportant (positive) / feedbackMarkNotImportant (negative) are
	// the feedback_events action tokens the corrections translate to. They are the
	// explicit-correction counterparts to the implicit signals in feedback.go.
	feedbackMarkImportant    = "mark_important"
	feedbackMarkNotImportant = "mark_not_important"
)

// correctionToken maps a client-supplied correction value to its feedback action
// token. ok is false for any unrecognized value (surfaced as invalidArguments
// for that entry).
func correctionToken(value string) (string, bool) {
	switch value {
	case correctionImportant:
		return feedbackMarkImportant, true
	case correctionNotImportant:
		return feedbackMarkNotImportant, true
	}
	return "", false
}

// correctionSide maps the same client-supplied correction value to the SIDE OF
// THE DESK'S LINE it places the message on (#464): "important" is above the
// line, "notImportant" is below it. ok is false for exactly the values
// correctionToken rejects — the two share one vocabulary, and
// TestCorrectionSideAgreesWithTheFeedbackSign makes a drift between them a red
// build rather than a correction that trains the model without moving the mail.
func correctionSide(value string) (store.LineSide, bool) {
	switch value {
	case correctionImportant:
		return store.LineSideSurface, true
	case correctionNotImportant:
		return store.LineSideDrawer, true
	}
	return "", false
}

// errTriageUnavailable is the clean method error a Triage control returns when
// no model Registry is wired (a degraded instance with no blob store). It is a
// serverFail — transient/unavailable per RFC 8620 §3.6.2 — never a panic.
func errTriageUnavailable() *MethodError {
	return &MethodError{Type: errServerFail, Description: "triage model registry is not configured"}
}

// ---- result shapes --------------------------------------------------------

// triageVersion is one row of the caller's model lineage. parentVersion is the
// version NUMBER of the lineage parent (nil at a root — the first snapshot, or a
// Reset that deliberately broke lineage).
type triageVersion struct {
	Version       int       `json:"version"`
	Active        bool      `json:"active"`
	CreatedAt     time.Time `json:"createdAt"`
	ParentVersion *int      `json:"parentVersion"`
}

// triageStateResult is the shared Triage/get and Triage/set response: the caller's
// active version plus its full lineage. activeVersion is 0 when the user has no
// activated model (a fresh user on the cold-start heuristic).
type triageStateResult struct {
	AccountID     string          `json:"accountId"`
	ActiveVersion int             `json:"activeVersion"`
	Versions      []triageVersion `json:"versions"`
}

// buildTriageState renders a lineage (oldest first, as ListVersions returns it)
// into the response shape. activeVersion is read from the active row directly, so
// it never has to load the model artifact out of the blob store — this method is
// a pure lineage read that reports "which version is live", not the model itself.
// A parent_id is resolved to the parent's version number via the row-id → version
// index built from the same list.
func buildTriageState(userID ulid.ULID, versions []store.VersionInfo) triageStateResult {
	versionByID := make(map[ulid.ULID]int, len(versions))
	for _, v := range versions {
		versionByID[v.ID] = v.Version
	}
	out := triageStateResult{
		AccountID: userID.String(),
		Versions:  make([]triageVersion, 0, len(versions)),
	}
	for _, v := range versions {
		tv := triageVersion{Version: v.Version, Active: v.Active, CreatedAt: v.CreatedAt}
		if v.ParentID != nil {
			if pv, ok := versionByID[*v.ParentID]; ok {
				tv.ParentVersion = &pv
			}
		}
		if v.Active {
			out.ActiveVersion = v.Version
		}
		out.Versions = append(out.Versions, tv)
	}
	return out
}

// ---- Triage/get -----------------------------------------------------------

// triageGet returns the caller's PRIORITY model state: the active version and the
// full lineage of snapshots (version, active, createdAt, parentVersion). It is
// read-only and per-user (no account scoping); RLS scopes ListVersions to the
// caller, so it can only ever describe the caller's own model.
func (a *API) triageGet(ctx context.Context, userID ulid.ULID, _ json.RawMessage) (json.RawMessage, *MethodError) {
	if a.registry == nil {
		return nil, errTriageUnavailable()
	}
	versions, err := a.registry.ListVersions(ctx, userID, ml.KindPriority)
	if err != nil {
		return nil, &MethodError{Type: errServerFail, Description: err.Error()}
	}
	return mustMarshal(buildTriageState(userID, versions)), nil
}

// ---- Triage/set -----------------------------------------------------------

// triageSetArgs is the model-control request: EXACTLY ONE of reset or rollbackTo.
type triageSetArgs struct {
	// Reset:true snapshots a fresh baseline (lineage broken) and activates it — the
	// "start my model over" valve.
	Reset *bool `json:"reset"`
	// RollbackTo re-activates an existing earlier version (never deletes a row).
	RollbackTo *int `json:"rollbackTo"`
}

// triageSet is the model-control safety valve. It performs exactly one of:
//   - reset:true       → registry.Reset (fresh baseline, lineage broken, activated);
//   - rollbackTo:<n>   → registry.Rollback (re-activate an existing version n).
//
// Neither or both is invalidArguments; a rollback to a version that does not exist
// is invalidArguments (ErrModelVersionNotFound), never a serverFail. On success it
// returns the resulting model state (like Triage/get). These mutate app.ml_models
// only — operational data, so NO change_log row.
//
// ⚠ IT DOES NOT TOUCH LINE OVERRIDES (#464). Resetting or rolling back the
// MODEL is not the user retracting the placements they made by hand: every
// app.messages.line_side stays exactly as it is, so mail the user pulled above
// the line does not fall back into a drawer because they started their model
// over. The model is the thing being rolled back; their corrections are not
// part of it, and a rescoring never clears them either (see
// store.UserTx.SetLineSide).
func (a *API) triageSet(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	if a.registry == nil {
		return nil, errTriageUnavailable()
	}
	var args triageSetArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	reset := args.Reset != nil && *args.Reset
	rollback := args.RollbackTo != nil
	// Reject neither and both: reset==rollback is true for (false,false) and
	// (true,true) alike. Exactly one control per request.
	if reset == rollback {
		return nil, &MethodError{Type: errInvalidArguments,
			Description: "exactly one of reset:true or rollbackTo:<version> is required"}
	}

	if reset {
		if _, err := a.registry.Reset(ctx, userID, ml.KindPriority); err != nil {
			return nil, &MethodError{Type: errServerFail, Description: err.Error()}
		}
	} else {
		if err := a.registry.Rollback(ctx, userID, ml.KindPriority, *args.RollbackTo); err != nil {
			if errors.Is(err, store.ErrModelVersionNotFound) {
				return nil, &MethodError{Type: errInvalidArguments,
					Description: "no such model version to roll back to"}
			}
			return nil, &MethodError{Type: errServerFail, Description: err.Error()}
		}
	}

	versions, err := a.registry.ListVersions(ctx, userID, ml.KindPriority)
	if err != nil {
		return nil, &MethodError{Type: errServerFail, Description: err.Error()}
	}
	return mustMarshal(buildTriageState(userID, versions)), nil
}

// ---- Triage/correct -------------------------------------------------------

// triageCorrectArgs is the one-click correction request: a map of message id →
// correction value ("important" | "notImportant").
type triageCorrectArgs struct {
	Corrections map[string]string `json:"corrections"`
}

// triageCorrectResult reports, JMAP set-style, which corrections were recorded and
// which were not (with a reason token per rejected id).
type triageCorrectResult struct {
	AccountID    string            `json:"accountId"`
	Corrected    []string          `json:"corrected"`
	NotCorrected map[string]string `json:"notCorrected"`
}

// Reason tokens for a rejected correction.
const (
	triageReasonNotFound         = "notFound"         // no such message for the caller (or another user's id)
	triageReasonInvalidArguments = "invalidArguments" // unrecognized correction value
	triageReasonServerFail       = "serverFail"       // the feedback insert failed (best-effort)
)

// triageCorrect records an explicit importance correction per message: a
// feedback_event the priority model learns from, AND — since #464 — the user's
// own placement of that message relative to the desk's line, which takes effect
// immediately. It is the training signal that pairs with PR1's explanation
// display ("this reason is wrong → correct it"); it still does NOT overwrite any
// score (the signal trains the model over time), but it no longer leaves the row
// sitting where the old score put it until the next retrain.
//
// ⚠ THE PLACEMENT IS THE POINT, NOT A SIDE EFFECT (#375 §5). Shift+↑/↓ moves a
// row across the line; the crossing is what the model learns from. Before this,
// nothing the desk READS changed — Area/getSummary's counts, Email/query's
// `onSurface` and the drawer sweep's candidates all split on the raw score — so
// the row stayed put and the sweep could archive a message the user had just
// pulled above the line.
//
// All corrections run inside ONE UserTx. For each entry it:
//   - verifies the message EXISTS for the caller (an RLS-scoped MessageRef). A
//     missing message — including another user's id, which is indistinguishable
//     under RLS — is recorded notFound and emits NOTHING. This is exactly what
//     makes the cross-user probe clean: A naming B's message reaches no row, so no
//     feedback is written, and no placement either.
//   - maps the value to its feedback token and EmitFeedbackEvent (savepoint-
//     guarded, best-effort — a failed insert is recorded, never fatal to the rest).
//   - writes the placement with SetLineSide, in the SAME transaction, which
//     appends the Email change_log row that tells every client the message moved
//     (also savepoint-guarded, for the same per-id reason).
//
// A real store error (not "not found") aborts the whole call as serverFail, so a
// partial write is never committed on a genuine failure.
// maxTriageCorrections bounds one Triage/correct call (#921). The web client's
// bulk "Mark as important" sends at most one "select all matching" batch (its
// BULK_BATCH, 1000) per call; the bound stops a caller walking an unbounded
// map through one transaction, which is the same reason Triage/open has one.
const maxTriageCorrections = 1000

func (a *API) triageCorrect(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	var args triageCorrectArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	if len(args.Corrections) > maxTriageCorrections {
		return nil, &MethodError{Type: errInvalidArguments,
			Description: fmt.Sprintf("at most %d corrections per call", maxTriageCorrections)}
	}
	res := triageCorrectResult{
		AccountID:    userID.String(),
		Corrected:    []string{},
		NotCorrected: map[string]string{},
	}
	if len(args.Corrections) == 0 {
		return mustMarshal(res), nil
	}

	mErr := a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
		for rawID, value := range args.Corrections {
			token, ok := correctionToken(value)
			if !ok {
				res.NotCorrected[rawID] = triageReasonInvalidArguments
				continue
			}
			msgID, err := ulid.Parse(rawID)
			if err != nil {
				// An unparseable id can never be one of the caller's messages.
				res.NotCorrected[rawID] = triageReasonNotFound
				continue
			}
			// RLS-scoped existence check: another user's message is not found here,
			// so nothing downstream can emit for it.
			if _, err := tx.MessageRef(ctx, msgID); err != nil {
				if errors.Is(err, store.ErrMessageNotFound) {
					res.NotCorrected[rawID] = triageReasonNotFound
					continue
				}
				return &MethodError{Type: errServerFail, Description: err.Error()}
			}
			// Best-effort, savepoint-guarded: a feedback-insert failure leaves the
			// outer tx usable and does not fail the other corrections.
			if err := tx.EmitFeedbackEvent(ctx, msgID, token, nil); err != nil {
				res.NotCorrected[rawID] = triageReasonServerFail
				continue
			}
			// The placement, in the SAME transaction as the feedback event, so
			// the message crosses the line for every reader now (#464).
			// correctionToken above already accepted this value, so the side
			// cannot be missing — the vocabularies are locked together by
			// TestCorrectionSideAgreesWithTheFeedbackSign.
			side, ok := correctionSide(value)
			if !ok {
				res.NotCorrected[rawID] = triageReasonInvalidArguments
				continue
			}
			// ⚠ A FAILURE HERE IS notCorrected EVEN THOUGH THE FEEDBACK EVENT
			// LANDED. The training signal is invisible; the placement is the
			// half the user can SEE, so reporting success while the row stayed
			// put would be the silent lie. SetLineSide is savepoint-guarded, so
			// this costs this id alone.
			if err := tx.SetLineSide(ctx, msgID, side); err != nil {
				res.NotCorrected[rawID] = triageReasonServerFail
				continue
			}
			res.Corrected = append(res.Corrected, rawID)
		}
		return nil
	})
	if mErr != nil {
		return nil, mErr
	}
	return mustMarshal(res), nil
}
