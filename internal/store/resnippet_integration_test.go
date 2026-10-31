// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// Integration suite for the resnippet backfill store surface (the mailroom-admin
// `resnippet` command): the keyset message-blob-ref reader and the guarded
// snippet update. The load-bearing invariant (CLAUDE.md) is the focus — an
// actual snippet change appends exactly ONE Email `updated` change_log row in the
// same UserTx, while a no-op (identical value, or a message that does not exist
// for the caller) appends none. Runs against the throwaway Postgres provisioned
// by scripts/integration-test.sh; skips when the env is absent (harness_test.go).

import (
	"context"
	"testing"

	"github.com/oklog/ulid/v2"
)

// emailUpdatedCount counts the Email `updated` change_log rows for one message,
// read inside the caller's transaction so it sees uncommitted appends.
func emailUpdatedCount(t *testing.T, tx *UserTx, msgID string) int {
	t.Helper()
	var n int
	if err := tx.tx.QueryRow(context.Background(), `
		SELECT count(*) FROM app.change_log
		 WHERE user_id = $1 AND object_type = 'Email' AND op = 'updated' AND object_id = $2`,
		tx.userID.String(), msgID).Scan(&n); err != nil {
		t.Fatalf("count Email updated change_log: %v", err)
	}
	return n
}

func TestUpdateMessageSnippet(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	msgID := mustULID(t, a.MessageID)

	err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		before := emailUpdatedCount(t, tx, a.MessageID)

		// 1. Real change → snippet written + exactly one new Email updated row.
		changed, err := tx.UpdateMessageSnippet(ctx, msgID, "fresh preview text")
		if err != nil {
			return err
		}
		if !changed {
			t.Error("first UpdateMessageSnippet: changed = false, want true")
		}
		if got := emailUpdatedCount(t, tx, a.MessageID); got != before+1 {
			t.Errorf("change_log Email/updated after change = %d, want %d", got, before+1)
		}
		var snip string
		if err := tx.tx.QueryRow(ctx,
			`SELECT snippet FROM app.messages WHERE user_id = $1 AND id = $2`,
			tx.userID.String(), msgID.String()).Scan(&snip); err != nil {
			return err
		}
		if snip != "fresh preview text" {
			t.Errorf("stored snippet = %q, want %q", snip, "fresh preview text")
		}

		// 2. No-op (identical value) → no write, no new change_log row.
		changed, err = tx.UpdateMessageSnippet(ctx, msgID, "fresh preview text")
		if err != nil {
			return err
		}
		if changed {
			t.Error("no-op UpdateMessageSnippet: changed = true, want false")
		}
		if got := emailUpdatedCount(t, tx, a.MessageID); got != before+1 {
			t.Errorf("change_log Email/updated after no-op = %d, want %d (no new row)", got, before+1)
		}

		// 3. Unknown id → changed = false, no error, and no change_log row.
		other := NewULID()
		changed, err = tx.UpdateMessageSnippet(ctx, other, "irrelevant")
		if err != nil {
			return err
		}
		if changed {
			t.Error("unknown-id UpdateMessageSnippet: changed = true, want false")
		}
		if got := emailUpdatedCount(t, tx, other.String()); got != 0 {
			t.Errorf("change_log Email/updated for unknown id = %d, want 0", got)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// MessageBlobRefsAfter returns the caller's messages with their content_hash and
// current snippet, in ascending id order, and honors the exclusive `after`
// bound. It is RLS-scoped: user A never sees user B's messages.
func TestMessageBlobRefsAfter(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, b := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	aMsg := mustULID(t, a.MessageID)

	err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		refs, err := tx.MessageBlobRefsAfter(ctx, ulid.ULID{}, 100)
		if err != nil {
			return err
		}
		var found bool
		for _, r := range refs {
			if r.ID == aMsg {
				found = true
				if len(r.ContentHash) != 32 {
					t.Errorf("content_hash len = %d, want 32", len(r.ContentHash))
				}
			}
			if r.ID.String() == b.MessageID {
				t.Error("MessageBlobRefsAfter leaked another user's message (RLS)")
			}
		}
		if !found {
			t.Errorf("seeded message %s not returned by MessageBlobRefsAfter", aMsg)
		}

		// The exclusive `after` bound excludes the message itself.
		afterSelf, err := tx.MessageBlobRefsAfter(ctx, aMsg, 100)
		if err != nil {
			return err
		}
		for _, r := range afterSelf {
			if r.ID == aMsg {
				t.Error("MessageBlobRefsAfter(after=self) returned the boundary row")
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// SetMessageHasAttachments repairs the #377 drift, and has_attachments is served
// as the JMAP Email property `hasAttachment` and drawn by the client as the
// paperclip. So the load-bearing invariant applies in full: a real change owes
// exactly one Email `updated` row, and a no-op owes none.
//
// ⚠ THE NO-OP HALF IS THE ONE THAT MATTERS HERE, and it is not symmetry for its
// own sake. The backfill walks every message and calls this for each
// disagreement; if an already-correct value still wrote a row, re-running the
// repair would churn the change_log and force every synced client to re-fetch
// mail that had not changed. #351 B4 was the opposite failure — a real mutation
// with no row at all — and both are ways of lying to a client about what moved.
func TestSetMessageHasAttachments(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	msgID := mustULID(t, a.MessageID)

	read := func(tx *UserTx) bool {
		t.Helper()
		var got bool
		if err := tx.tx.QueryRow(ctx,
			`SELECT has_attachments FROM app.messages WHERE user_id = $1 AND id = $2`,
			tx.userID.String(), msgID.String()).Scan(&got); err != nil {
			t.Fatalf("read has_attachments: %v", err)
		}
		return got
	}

	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		before := emailUpdatedCount(t, tx, a.MessageID)
		start := read(tx)

		// 1. A real repair: flip it away from whatever the fixture holds.
		changed, err := tx.SetMessageHasAttachments(ctx, msgID, !start)
		if err != nil {
			return err
		}
		if !changed {
			t.Error("first SetMessageHasAttachments: changed = false, want true")
		}
		if got := read(tx); got != !start {
			t.Errorf("stored has_attachments = %v, want %v", got, !start)
		}
		if got := emailUpdatedCount(t, tx, a.MessageID); got != before+1 {
			t.Errorf("change_log Email/updated after repair = %d, want %d — a client that "+
				"never learns the marker changed keeps drawing the old paperclip", got, before+1)
		}

		// 2. Same value again: no write, and NO new change row.
		changed, err = tx.SetMessageHasAttachments(ctx, msgID, !start)
		if err != nil {
			return err
		}
		if changed {
			t.Error("no-op SetMessageHasAttachments: changed = true, want false")
		}
		if got := emailUpdatedCount(t, tx, a.MessageID); got != before+1 {
			t.Errorf("change_log Email/updated after no-op = %d, want %d — re-running the "+
				"backfill must be free, not a re-sync for every client", got, before+1)
		}

		// 3. An id that does not resolve for this caller: no error, no row. RLS
		// makes "another user's message" and "no such message" the same answer,
		// which is the correct one to give.
		other := NewULID()
		changed, err = tx.SetMessageHasAttachments(ctx, other, true)
		if err != nil {
			return err
		}
		if changed {
			t.Error("unknown-id SetMessageHasAttachments: changed = true, want false")
		}
		if got := emailUpdatedCount(t, tx, other.String()); got != 0 {
			t.Errorf("change_log Email/updated for unknown id = %d, want 0", got)
		}
		return nil
	}); err != nil {
		t.Fatalf("tx: %v", err)
	}
}
