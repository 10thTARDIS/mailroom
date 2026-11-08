// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// Shared Foo/changes handling (RFC 8620 §5.2). Every typed *changes method
// (Email/changes, Mailbox/changes, Thread/changes) is the same logic over a
// different object type: decode sinceState, run the store's §3.2 collapse, and
// map the result — including the cannotCalculateChanges fallback.

// changesArgs is the RFC 8620 §5.2 Foo/changes request.
type changesArgs struct {
	AccountID  string `json:"accountId"`
	SinceState string `json:"sinceState"`
	MaxChanges *int   `json:"maxChanges"`
}

// changesResult is the RFC 8620 §5.2 Foo/changes response.
type changesResult struct {
	AccountID      string   `json:"accountId"`
	OldState       string   `json:"oldState"`
	NewState       string   `json:"newState"`
	HasMoreChanges bool     `json:"hasMoreChanges"`
	Created        []string `json:"created"`
	Updated        []string `json:"updated"`
	Destroyed      []string `json:"destroyed"`
}

// handleChanges runs Foo/changes for object type t.
func (a *API) handleChanges(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage, t store.ObjectType) (json.RawMessage, *MethodError) {
	var args changesArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	if mErr := resolveAccount(userID, args.AccountID); mErr != nil {
		return nil, mErr
	}
	// maxChanges, if given, MUST be a positive integer (RFC 8620 §5.2).
	maxChanges := 0
	if args.MaxChanges != nil {
		if *args.MaxChanges <= 0 {
			return nil, &MethodError{Type: errInvalidArguments, Description: "maxChanges must be a positive integer"}
		}
		maxChanges = *args.MaxChanges
	}
	// A key's /changes is always bounded, by the bound its admission charged
	// for (keyChangesLimit, #852).
	if principalFrom(ctx) != nil {
		maxChanges = keyChangesLimit(args.MaxChanges)
	}
	// A malformed sinceState is not something we can compute from — force the
	// designed refetch fallback rather than trusting a garbled cursor.
	since, err := decodeState(args.SinceState)
	if err != nil {
		return nil, &MethodError{Type: errCannotCalculateChanges}
	}

	var page store.ChangesPage
	mErr := a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
		p, err := tx.Changes(ctx, t, since, maxChanges)
		if err != nil {
			if errors.Is(err, store.ErrCannotCalculateChanges) {
				return &MethodError{Type: errCannotCalculateChanges}
			}
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		page = p
		return nil
	})
	if mErr != nil {
		return nil, mErr
	}

	if t == store.ObjectMailbox {
		return mustMarshal(mailboxChangesResult{changesResult: changesFrom(userID, page), UpdatedProperties: mailboxUpdatedProperties(page)}), nil
	}
	return mustMarshal(changesFrom(userID, page)), nil
}

// mailboxChangesResult is Mailbox/changes (RFC 8621 §2.2): the generic result
// plus updatedProperties.
type mailboxChangesResult struct {
	changesResult
	UpdatedProperties []string `json:"updatedProperties"`
}

// mailboxCountProperties are the Mailbox properties a count row may change.
var mailboxCountProperties = []string{"totalEmails", "unreadEmails", "totalThreads", "unreadThreads"}

// mailboxUpdatedProperties is RFC 8621 §2.2's updatedProperties: the count
// properties when EVERY updated mailbox in the window changed only in its counts
// (store.ChangesPage.UpdatedCountsOnly, ADR-052), and null otherwise: null is the
// RFC's answer for "the server cannot say", which is also the answer for a
// window that holds a rename, a role or scope change, or rows older than the
// counts_only mark.
func mailboxUpdatedProperties(page store.ChangesPage) []string {
	if len(page.Updated) == 0 {
		return nil
	}
	for _, id := range page.Updated {
		if !page.UpdatedCountsOnly[id] {
			return nil
		}
	}
	return mailboxCountProperties
}

func changesFrom(userID ulid.ULID, page store.ChangesPage) changesResult {
	res := changesResult{
		AccountID:      userID.String(),
		OldState:       encodeState(page.OldState),
		NewState:       encodeState(page.NewState),
		HasMoreChanges: page.HasMore,
		Created:        ulidStrings(page.Created),
		Updated:        ulidStrings(page.Updated),
		Destroyed:      ulidStrings(page.Destroyed),
	}
	return res
}

// ulidStrings renders a ULID slice as a JSON-friendly []string, never nil (JMAP
// change arrays are always present, possibly empty).
func ulidStrings(ids []ulid.ULID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.String()
	}
	return out
}

// mustMarshal marshals a value that is known to be serializable (our own result
// structs); a failure is a programming error, surfaced as empty JSON.
func mustMarshal(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}
