// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

import (
	"context"
	"testing"
)

// #740. RecordKeywordProbes is the one writer of accepts_keywords: it stores
// what a read-write SELECT said, true and false alike, and only for the
// caller's own mailboxes. UpdateMailboxCursors — the sync pass's EXAMINE-fed
// write — no longer touches the column, so a later sync cannot downgrade it.
//
// NEGATIVE CONTROL: put `accepts_keywords = false` back into
// UpdateMailboxCursors' SET list and the "a sync pass must not downgrade" step
// goes red.
func TestRecordKeywordProbes(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, b := seedTwoUsers(t, h.super)
	aID := mustULID(t, a.ID)
	acct := mustULID(t, a.AccountID)
	accepts := func() bool {
		var v bool
		if err := h.super.QueryRow(ctx, `SELECT accepts_keywords FROM app.mailboxes WHERE id = $1`, a.MailboxID).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	record := func(user string, probes map[string]bool) int {
		var n int
		if err := h.appStore().UserTx(ctx, mustULID(t, user), func(tx *UserTx) error {
			var e error
			n, e = tx.RecordKeywordProbes(ctx, acct, probes)
			return e
		}); err != nil {
			t.Fatal(err)
		}
		return n
	}

	if accepts() {
		t.Fatal("premise: the seeded INBOX starts at the default, false")
	}
	// RLS negative: user b naming a's account and mailbox changes nothing.
	if n := record(b.ID, map[string]bool{"INBOX": true}); n != 0 || accepts() {
		t.Fatalf("another user's probe changed %d rows / accepts=%v on a's mailbox — RLS leak", n, accepts())
	}
	if n := record(a.ID, map[string]bool{"INBOX": true, "NoSuchFolder": true}); n != 1 || !accepts() {
		t.Fatalf("owner's probe changed %d rows, accepts=%v; want 1 and true (an unknown name is skipped)", n, accepts())
	}
	if n := record(a.ID, map[string]bool{"INBOX": true}); n != 0 {
		t.Errorf("re-recording the same answer changed %d rows, want 0", n)
	}

	// A sync pass writes its cursors from an EXAMINE: it must leave the probe.
	if err := h.appStore().UserTx(ctx, aID, func(tx *UserTx) error {
		return tx.UpdateMailboxCursors(ctx, mustULID(t, a.MailboxID), MailboxCursors{UIDValidity: 7, LastSeenUID: 3})
	}); err != nil {
		t.Fatal(err)
	}
	if !accepts() {
		t.Fatal("UpdateMailboxCursors downgraded accepts_keywords: a sync pass's EXAMINE overwrote a read-write probe (#740)")
	}

	// A read-write SELECT that leaves `\*` out is an honest no, and is recorded.
	if n := record(a.ID, map[string]bool{"INBOX": false}); n != 1 || accepts() {
		t.Fatalf("a read-write 'no' changed %d rows, accepts=%v; want 1 and false", n, accepts())
	}
}
