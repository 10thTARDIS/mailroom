// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// TestSetMailboxFeedsDesk pins "folders feed the desk" (#442, #446): flipping
// the flag appends exactly one Mailbox change row, and re-asserting the same
// value appends none — the same change-log discipline SetMailboxSyncScope
// already has (#62).

import (
	"context"
	"testing"

	"github.com/oklog/ulid/v2"
)

func TestSetMailboxFeedsDesk(t *testing.T) {
	h := newHarness(t)
	s := h.appStore()
	ctx := context.Background()
	u := seedUser(t, h.super, "mfd-"+NewULID().String())
	userID := mustULID(t, u.ID)
	mailboxID := mustULID(t, u.MailboxID)

	var before, afterFlip, afterNoop int64
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		var e error
		if before, e = tx.ChangeState(ctx, ObjectMailbox); e != nil {
			return e
		}
		changed, e := tx.SetMailboxFeedsDesk(ctx, mailboxID, true)
		if e != nil {
			return e
		}
		if !changed {
			t.Error("flipping feeds_desk on a fresh mailbox reported no change")
		}
		afterFlip, e = tx.ChangeState(ctx, ObjectMailbox)
		return e
	}); err != nil {
		t.Fatalf("flip on: %v", err)
	}
	if afterFlip-before != 1 {
		t.Errorf("flipping feeds_desk appended %d change rows, want 1", afterFlip-before)
	}

	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		changed, e := tx.SetMailboxFeedsDesk(ctx, mailboxID, true)
		if e != nil {
			return e
		}
		if changed {
			t.Error("re-asserting the same feeds_desk value reported a change")
		}
		afterNoop, e = tx.ChangeState(ctx, ObjectMailbox)
		return e
	}); err != nil {
		t.Fatalf("re-assert: %v", err)
	}
	if afterNoop != afterFlip {
		t.Errorf("re-asserting the same value appended %d change rows, want 0", afterNoop-afterFlip)
	}

	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		views, e := tx.GetMailboxes(ctx, []ulid.ULID{mailboxID})
		if e != nil {
			return e
		}
		if len(views) != 1 || !views[0].FeedsDesk {
			t.Errorf("GetMailboxes FeedsDesk = %+v, want true", views)
		}
		return nil
	}); err != nil {
		t.Fatalf("read back: %v", err)
	}
}
