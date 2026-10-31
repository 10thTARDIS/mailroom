// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// Triage/open — the explicit "the user read this" signal (#842).
//
// The priority model had no way to learn that a message mattered short of a
// reply, a star or an explicit correction: a mark-read was the only trace of
// reading, the server cannot tell an open from a bulk "mark all read", and it
// was recorded as a NEGATIVE. The web client now calls this once per message,
// after the message has been shown in the reading pane for its dwell time
// (OPEN_DWELL_MS in web/src/app/ui/useOpenDwell.ts), and the server records an
// `open` feedback event (label 1). A quick flick past a message never calls it.
//
// SESSION-ONLY BY INTENT. An API key or agent has no reading pane; an `open`
// from one would be a claim about a human's attention that nobody made. When
// API-key scopes land (#841, agent_scopes.go) this method belongs in its
// notForKeys set — recorded in ADR-043 so the merge does not miss it.
//
// Like Triage/correct it writes operational data only (app.feedback_events),
// so it appends NO change_log row, and every id is checked under RLS first: an
// id the caller cannot see records nothing and is reported notFound.

// feedbackOpen is the token Triage/open records. It must equal
// store.FeedbackActionOpen and be a positive label (feedback_action_test.go).
const feedbackOpen = store.FeedbackActionOpen

// maxTriageOpenIDs bounds one call. The client sends one id per dwell; the
// bound only stops a hostile caller walking an unbounded list through one
// transaction.
const maxTriageOpenIDs = 50

type triageOpenArgs struct {
	EmailIDs []string `json:"emailIds"`
}

type triageOpenResult struct {
	AccountID string `json:"accountId"`
	// Opened lists the ids now carrying an open signal — newly recorded or
	// already recorded (the call is idempotent per message).
	Opened    []string          `json:"opened"`
	NotOpened map[string]string `json:"notOpened"`
}

func (a *API) triageOpen(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	// Session-only in the scope table, and refused here too if a key ever
	// reached it (#49 review, as pendingSet): an agent's reads must not train
	// the priority model as if a person had read the mail.
	if principalFrom(ctx) != nil {
		return nil, forbidden("Triage/open records that a person read a message; API keys cannot call it")
	}
	var args triageOpenArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	if len(args.EmailIDs) > maxTriageOpenIDs {
		return nil, &MethodError{Type: errInvalidArguments,
			Description: fmt.Sprintf("at most %d emailIds per call", maxTriageOpenIDs)}
	}
	res := triageOpenResult{AccountID: userID.String(), Opened: []string{}, NotOpened: map[string]string{}}
	if len(args.EmailIDs) == 0 {
		return mustMarshal(res), nil
	}
	mErr := a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
		for _, raw := range args.EmailIDs {
			id, err := ulid.Parse(raw)
			if err != nil {
				res.NotOpened[raw] = triageReasonNotFound
				continue
			}
			_, found, err := tx.RecordOpenFeedback(ctx, id)
			if err != nil {
				return &MethodError{Type: errServerFail, Description: err.Error()}
			}
			if !found {
				res.NotOpened[raw] = triageReasonNotFound
				continue
			}
			res.Opened = append(res.Opened, raw)
		}
		return nil
	})
	if mErr != nil {
		return nil, mErr
	}
	return mustMarshal(res), nil
}
