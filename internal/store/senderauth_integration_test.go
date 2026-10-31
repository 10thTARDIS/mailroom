// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// EmailView.LocallyAuthored — the flag the sender-authentication display hangs
// its "this question does not apply" case on (#55 increment 1).
//
// ⚠ THIS FILE EXISTS BECAUSE THE API-LEVEL TESTS CANNOT COVER IT. They call
// authResultsObject with locallyAuthored passed in by hand, so every one of them
// would still pass if GetEmails never set the field and it were false on every
// message forever. The visible result would be Mailroom telling users their own
// drafts and Sent copies are unauthenticated — which is the exact failure the
// property was built to avoid, arriving through the read path being silently
// empty (#155).

import (
	"context"
	"testing"

	"github.com/oklog/ulid/v2"
)

// TestGetEmailsMarksLocallyAuthoredMail asserts BOTH directions, because a field
// that is always true and a field that is always false are equally broken and a
// one-sided test cannot tell either from correct.
func TestGetEmailsMarksLocallyAuthoredMail(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	drafts := seedMailbox(t, ctx, h, a, "Drafts", `\Drafts`)

	err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		// Mailroom wrote this one: it has a local_mailbox_id and no provenance.
		draftID, _ := makeDraft(t, ctx, tx, a, drafts, "a draft we composed")
		// This one came from a server: no local_mailbox_id.
		arrivedID := insertTestMessage(t, tx, "sender-auth-arrived", FunctionSignals{})

		views, err := tx.GetEmails(ctx, []ulid.ULID{draftID, arrivedID})
		if err != nil {
			return err
		}
		if len(views) != 2 {
			t.Fatalf("got %d views, want 2", len(views))
		}
		byID := map[string]EmailView{}
		for _, v := range views {
			byID[v.ID.String()] = v
		}

		d, ok := byID[draftID.String()]
		if !ok {
			t.Fatal("the draft is missing from the result")
		}
		if !d.LocallyAuthored {
			t.Error("LocallyAuthored is false for a message Mailroom composed — " +
				"the display would call the user's own draft unauthenticated")
		}

		m, ok := byID[arrivedID.String()]
		if !ok {
			t.Fatal("the arrived message is missing from the result")
		}
		if m.LocallyAuthored {
			t.Error("LocallyAuthored is true for a message that came from a server — " +
				"the display would then suppress a real authentication result")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("UserTx: %v", err)
	}
}
