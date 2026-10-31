// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/attachscan"
	"mailroom/internal/store"
)

// Email.attachmentScan and Email/releaseAttachment — the JMAP surface of the
// attachment quarantine (#396, ADR-027 §8, §9).
//
// # Why it is one Email property keyed by partId, and not a field on a body part
//
// The obvious shape is a `scanStatus` entry in `bodyProperties`, hanging off
// each EmailBodyPart. It is the wrong one, and the reason is a divergence this
// codebase pinned a test around before either half existed
// (internal/mime/partpath_test.go:122):
//
//   - `mime.IsAttachmentLeaf` — what app.attachments projects, and therefore
//     what carries a verdict — counts ANY non-text leaf, INCLUDING an image
//     marked `Content-Disposition: inline`.
//   - RFC 8621's rule — what body.go implements — puts a CID-referenced image in
//     `htmlBody`, so it never appears in the Email's `attachments` array.
//
// A CID inline image is therefore exactly the part that renders on message open
// with zero clicks (ADR-027 §8's first bullet) AND the part a property attached
// to `attachments` would never describe. Keying on partId covers every part in
// the message, whichever list the RFC's algorithm puts it in.
//
// It also costs no blob read. Everything here comes from app.attachments, so it
// is a Postgres property like `keywords`, not a body property like
// `authResults` — which means a LIST view can carry a quarantine badge without
// parsing 50 messages.
//
// # What syncs (ADR-027 §9)
//
// The STATE, and nothing else. Verdict age and signature-DB version are
// deliberately absent: they change on every re-scan pass, so syncing them would
// append change rows several times a day for facts that did not move, and force
// every client to refetch. They are served instead through Account/getHealth's
// computed read, the searchStalenessSeconds pattern.
//
// ⚠ AND THE WORD "clean" IS NOT SENT ALONE. §1a is explicit that for a careful
// user a stale green badge is worse than no badge: it inverts the caution the
// absence of a claim preserves. The property reports the stored state and the
// release; it is the client's job never to render `clean` as a reassurance, and
// the account-health envelope's age is what qualifies it.

// attachmentScanObject renders Email.attachmentScan for one message, or nil when
// the message has no projected parts.
//
// ⚠ nil MEANS "NOTHING IS PROJECTED", NOT "NOTHING IS WRONG". A message ingested
// before #393, or one with no attachment leaves at all, both land here. A client
// must not read null as an all-clear, which is why there is no `state: "clean"`
// summary field to be read that way by accident.
func attachmentScanObject(states []store.PartScanState) any {
	if len(states) == 0 {
		return nil
	}
	parts := make(map[string]any, len(states))
	quarantined := make([]string, 0, 2)
	for _, st := range states {
		parts[st.PartPath] = map[string]any{
			"state": st.Status,
			// released is reported even when the verdict is `clean`, because it
			// is a fact about the user's own decision and a client showing "you
			// released this" should not have it disappear when a re-scan
			// happens to come back clean.
			"released": st.Released,
			// releasedAt is the UTCDate of that same decision, or null. ⚠ IT IS
			// NOT A SCANNER FACT: the package comment above is explicit that
			// verdict age and signature-DB version stay unexposed because they
			// churn on every re-scan pass. This timestamp is the opposite kind
			// of value — it records when the USER released the content, so it
			// only ever changes on a release or a revoke, never on a re-scan —
			// which is what lets the viewer print "released by you · <date>".
			"releasedAt": jmapDateOrNil(st.ReleasedAt),
		}
		if attachscan.Blocked(st.Status, st.Released) {
			quarantined = append(quarantined, st.PartPath)
		}
	}
	return map[string]any{
		"parts": parts,
		// The partIds the server WILL REFUSE TO SERVE. Derived from the same
		// attachscan.Blocked the download gate calls, so a client cannot render
		// a working download link for a part the gate blocks: the two answers
		// come from one function rather than two readings of a state table.
		"quarantined": quarantined,
	}
}

// resolveAttachmentScans batches the app.attachments read for Email/get, and
// only when the client asked for the property.
func (a *API) resolveAttachmentScans(ctx context.Context, userID ulid.ULID, ids []ulid.ULID, props map[string]bool, ba bodyArgs) (map[ulid.ULID][]store.PartScanState, *MethodError) {
	// ⚠ TWO CONSUMERS, NOT ONE (#413). The property is opt-in, but the BODY-VALUE
	// GATE is not: a client that never asks for attachmentScan can still ask for
	// fetchAllBodyValues, and that path must not hand back a quarantined text
	// part. Gating this read on the property alone is what left that door open.
	if (!props["attachmentScan"] && !ba.gatesBodyValues()) || len(ids) == 0 {
		return nil, nil
	}
	var out map[ulid.ULID][]store.PartScanState
	if mErr := a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
		var err error
		out, err = tx.AttachmentScanStates(ctx, ids)
		if err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		return nil
	}); mErr != nil {
		return nil, mErr
	}
	return out, nil
}

// releaseAttachmentArgs is Email/releaseAttachment's request.
type releaseAttachmentArgs struct {
	AccountID string `json:"accountId"`
	ID        string `json:"id"`
	PartID    string `json:"partId"`
	// Revoke undoes a release instead of making one, re-arming the gate.
	Revoke bool `json:"revoke"`
}

// emailReleaseAttachment is the RELEASE DATA MODEL's method surface (ADR-027
// §8, §14).
//
// ⚠ THE INTERACTION IS DEFERRED, THE METHOD IS NOT, and the split is
// deliberate. §14 defers the quarantine-release *interaction* to the client
// milestone (#371–#376) — how obstructive the confirmation is, what it says, how
// a false positive is made recoverable without making a real detection
// dismissible on muscle memory. That is a design problem about a human. But
// shipping enforcement with no release path at all would mean a false positive
// makes an attachment permanently unreachable for anyone without a shell on the
// server, which §8 names as the opposite failure and the worse one.
//
// ⚠ IT RELEASES CONTENT, NOT A ROW. The store keys the release on the part's
// content digest, so every copy of those bytes the user holds — including copies
// that arrive tomorrow — is covered by the one decision. The response says how
// many messages changed, so a caller can see that it was more than one.
func (a *API) emailReleaseAttachment(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	var args releaseAttachmentArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	if mErr := resolveAccount(userID, args.AccountID); mErr != nil {
		return nil, mErr
	}
	msgID, err := ulid.Parse(args.ID)
	if err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: "id is not an Email id"}
	}
	if args.PartID == "" {
		return nil, &MethodError{Type: errInvalidArguments,
			Description: "Email/releaseAttachment requires partId"}
	}

	var (
		affected []ulid.ULID
		state    int64
	)
	if mErr := a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
		st, found, err := tx.AttachmentScanState(ctx, msgID, args.PartID)
		if err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		if !found {
			return &MethodError{Type: errInvalidArguments,
				Description: "no projected attachment at partId " + args.PartID}
		}
		if args.Revoke {
			if len(st.PartHash) == 0 {
				return &MethodError{Type: errInvalidArguments,
					Description: "this attachment has no recorded content, so there is no release to revoke"}
			}
			affected, err = tx.RevokeAttachmentRelease(ctx, st.PartHash)
		} else {
			_, affected, err = tx.ReleaseAttachment(ctx, msgID, args.PartID)
		}
		if err != nil {
			// A part with no digest has never been scanned, so there is nothing
			// to key a release on. Reported as an argument error rather than a
			// server failure: it is a true statement about the request.
			if errors.Is(err, store.ErrAttachmentNoContentHash) {
				return &MethodError{Type: errInvalidArguments,
					Description: "this attachment has not been scanned, so its content cannot be released"}
			}
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		state, err = tx.ChangeState(ctx, store.ObjectEmail)
		if err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		return nil
	}); mErr != nil {
		return nil, mErr
	}

	ids := make([]string, 0, len(affected))
	for _, id := range affected {
		ids = append(ids, id.String())
	}
	return mustMarshal(map[string]any{
		"accountId": userID.String(),
		"newState":  encodeState(state),
		"released":  !args.Revoke,
		// ⚠ EVERY message the decision touched, not just the one asked about.
		// A caller that assumed a release was local to one Email would be
		// wrong, and the honest way to say so is to hand back the list.
		"affectedEmailIds": ids,
	}), nil
}

// blockedPartSet is the set of part paths the gate refuses, keyed for a map
// lookup by partId.
//
// ⚠ IT CALLS attachscan.Blocked RATHER THAN RE-READING THE STATUS, so the
// body-value gate, the download gate and the `quarantined` list in the
// attachmentScan property are three consumers of ONE rule. Re-deriving "is this
// blocked" from st.Status here is how `unknown` would quietly stop blocking on
// one path and keep blocking on the others.
func blockedPartSet(states []store.PartScanState) map[string]bool {
	if len(states) == 0 {
		return nil
	}
	out := make(map[string]bool, len(states))
	for _, st := range states {
		if attachscan.Blocked(st.Status, st.Released) {
			out[st.PartPath] = true
		}
	}
	return out
}
