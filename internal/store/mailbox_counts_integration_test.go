// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// Mailbox state on count changes (ADR-052, #115).
//
// Every assertion counts change_log rows PER OBJECT TYPE, read as the superuser
// so RLS cannot be the reason a row looks missing, and the bulk case asserts the
// BOUND, not just "some row was written".

import (
	"context"
	"errors"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
)

// seedBulkMessages inserts n unseen messages for user u, live in mailbox mb.
func seedBulkMessages(t *testing.T, h *harness, u fixtureUser, mb ulid.ULID, n int) []ulid.ULID {
	t.Helper()
	ctx := context.Background()
	ids := make([]ulid.ULID, n)
	strs := make([]string, n)
	uids := make([]int64, n)
	for i := range ids {
		ids[i] = NewULID()
		strs[i] = ids[i].String()
		nextSeededUID++
		uids[i] = nextSeededUID
	}
	var threadID any
	if err := h.super.QueryRow(ctx, `SELECT thread_id FROM app.messages WHERE id = $1`, u.MessageID).
		Scan(&threadID); err != nil {
		t.Fatalf("read seed message: %v", err)
	}
	// One blob row per message: (user, content_hash) is unique.
	if _, err := h.super.Exec(ctx, `
		INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id)
		SELECT $2, sha256(convert_to(i, 'UTF8')), 100, 132, $3 FROM unnest($1::text[]) AS i`,
		strs, u.ID, NewULID().String()); err != nil {
		t.Fatalf("seed bulk blobs: %v", err)
	}
	if _, err := h.super.Exec(ctx, `
		INSERT INTO app.messages (id, user_id, content_hash, thread_id, subject, from_address, received_at, size_bytes)
		SELECT i, $2, sha256(convert_to(i, 'UTF8')), $3, 'bulk', 'friend@example.com', now(), 100
		  FROM unnest($1::text[]) AS i`,
		strs, u.ID, threadID); err != nil {
		t.Fatalf("seed bulk messages: %v", err)
	}
	if _, err := h.super.Exec(ctx, `
		INSERT INTO app.message_provenance (id, user_id, message_id, account_id, mailbox_id, uid, uidvalidity)
		SELECT 'P' || substr(i, 2), $2, i, $3, $4, u, 1
		  FROM unnest($1::text[], $5::bigint[]) AS t(i, u)`,
		strs, u.ID, u.AccountID, mb.String(), uids); err != nil {
		t.Fatalf("seed bulk provenance: %v", err)
	}
	return ids
}

// nextSeededUID gives bulk provenance distinct UIDs (the table is unique per UID).
var nextSeededUID int64 = 1000

func mailboxRowsSince(t *testing.T, h *harness, userID string, since int64) (rows, countsOnly int) {
	t.Helper()
	if err := h.super.QueryRow(context.Background(), `
		SELECT count(*), count(*) FILTER (WHERE counts_only) FROM app.change_log
		 WHERE user_id = $1 AND object_type = 'Mailbox' AND seq > $2`, userID, since).Scan(&rows, &countsOnly); err != nil {
		t.Fatalf("count mailbox rows: %v", err)
	}
	return
}

func TestMailboxStateMovesWhenAMessageIsMoved(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, b := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID, bID := mustULID(t, a.ID), mustULID(t, b.ID)
	inbox := mustULID(t, a.MailboxID)
	trash := seedMailbox(t, ctx, h, a, "Trash", `\Trash`)
	ids := seedBulkMessages(t, h, a, inbox, 2)

	mbBefore := stateOf(t, ctx, s, aID, ObjectMailbox)
	bBefore := stateOf(t, ctx, s, bID, ObjectMailbox)
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		_, _, err := tx.ApplyEmailUpdate(ctx, ids[0], EmailUpdate{MoveToMailboxID: &trash})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if after := stateOf(t, ctx, s, aID, ObjectMailbox); after <= mbBefore {
		t.Fatalf("Mailbox state did not move on a move: %d -> %d", mbBefore, after)
	}
	var page ChangesPage
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		var err error
		page, err = tx.Changes(ctx, ObjectMailbox, mbBefore, 0)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	got := map[ulid.ULID]bool{}
	for _, id := range page.Updated {
		got[id] = true
	}
	if !got[inbox] || !got[trash] || len(page.Updated) != 2 {
		t.Fatalf("Mailbox/changes updated = %v, want exactly the Inbox and Trash", page.Updated)
	}
	if !page.UpdatedCountsOnly[inbox] || !page.UpdatedCountsOnly[trash] {
		t.Fatalf("count-only marks = %v", page.UpdatedCountsOnly)
	}
	// And the counts it announces really moved.
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		mv, err := tx.GetMailboxes(ctx, []ulid.ULID{trash})
		if err != nil || len(mv) != 1 || mv[0].TotalEmails != 1 || mv[0].UnreadEmails != 1 {
			t.Errorf("trash view = %+v, %v", mv, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// RLS negative case: the other user's state did not move and the other
	// user's changes never list A's mailboxes.
	if st := stateOf(t, ctx, s, bID, ObjectMailbox); st != bBefore {
		t.Errorf("another user's Mailbox state moved: %d -> %d", bBefore, st)
	}
	if err := s.UserTx(ctx, bID, func(tx *UserTx) error {
		p, err := tx.Changes(ctx, ObjectMailbox, 0, 0)
		if err != nil {
			return err
		}
		for _, id := range append(append([]ulid.ULID{}, p.Updated...), p.Created...) {
			if id == inbox || id == trash {
				t.Errorf("user B sees A's mailbox %s in Mailbox/changes", id)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestMailboxStateMovesWhenAMessageIsRead(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	inbox := mustULID(t, a.MailboxID)
	ids := seedBulkMessages(t, h, a, inbox, 2)
	seen, flagged := true, true

	before := stateOf(t, ctx, s, aID, ObjectMailbox)
	// NEGATIVE CONTROL: a flag that cannot move a count appends no Mailbox row,
	// or every star would wake every client.
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		_, _, err := tx.ApplyEmailUpdate(ctx, ids[0], EmailUpdate{Flagged: &flagged})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if rows, _ := mailboxRowsSince(t, h, a.ID, before); rows != 0 {
		t.Fatalf("a flag change appended %d Mailbox rows", rows)
	}

	var unreadBefore int64
	_ = s.UserTx(ctx, aID, func(tx *UserTx) error {
		mv, _ := tx.GetMailboxes(ctx, []ulid.ULID{inbox})
		unreadBefore = int64(mv[0].UnreadEmails)
		return nil
	})
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		_, _, err := tx.ApplyEmailUpdate(ctx, ids[0], EmailUpdate{Seen: &seen})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if rows, only := mailboxRowsSince(t, h, a.ID, before); rows != 1 || only != 1 {
		t.Fatalf("marking seen appended %d Mailbox rows (%d counts-only), want 1 and 1", rows, only)
	}
	_ = s.UserTx(ctx, aID, func(tx *UserTx) error {
		mv, _ := tx.GetMailboxes(ctx, []ulid.ULID{inbox})
		if int64(mv[0].UnreadEmails) != unreadBefore-1 {
			t.Errorf("unreadEmails %d -> %d, want one fewer", unreadBefore, mv[0].UnreadEmails)
		}
		return nil
	})
}

// The cost bound: a bulk move appends one row per mailbox touched, however many
// messages moved. N comes from MAILROOM_BULK_MOVE (default 500) so the same test
// is the 5,000-message timing run.
func TestBulkMoveAppendsOneMailboxRowPerMailboxTouched(t *testing.T) {
	n := 500
	if v, err := strconv.Atoi(os.Getenv("MAILROOM_BULK_MOVE")); err == nil && v > 0 {
		n = v
	}
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	inbox := mustULID(t, a.MailboxID)
	archive := seedMailbox(t, ctx, h, a, "Archive", `\Archive`)
	receipts := seedMailbox(t, ctx, h, a, "Receipts", "")
	trash := seedMailbox(t, ctx, h, a, "Trash", `\Trash`)
	ids := append(seedBulkMessages(t, h, a, inbox, n/2),
		append(seedBulkMessages(t, h, a, archive, n/4), seedBulkMessages(t, h, a, receipts, n-n/2-n/4)...)...)

	mbBefore := stateOf(t, ctx, s, aID, ObjectMailbox)
	emBefore := stateOf(t, ctx, s, aID, ObjectEmail)
	start := time.Now()
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		for _, id := range ids {
			if _, _, err := tx.ApplyEmailUpdate(ctx, id, EmailUpdate{MoveToMailboxID: &trash}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	t.Logf("moved %d messages across 3 mailboxes in one transaction: %v", n, time.Since(start))

	rows, only := mailboxRowsSince(t, h, a.ID, mbBefore)
	if rows != 4 || only != 4 {
		t.Fatalf("a %d-message move across 3 sources into Trash appended %d Mailbox rows (%d counts-only), want 4 (O(mailboxes))", n, rows, only)
	}
	if emRows := changeRowsSince(t, ctx, h, a.ID, ObjectEmail, emBefore); emRows != n {
		t.Errorf("Email rows = %d, want %d (one per message, unchanged)", emRows, n)
	}
}

// A rolled-back transaction leaves no Mailbox row, as it leaves no Email row.
func TestMailboxCountNotesDieWithARolledBackTransaction(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	inbox := mustULID(t, a.MailboxID)
	trash := seedMailbox(t, ctx, h, a, "Trash", `\Trash`)
	ids := seedBulkMessages(t, h, a, inbox, 1)
	before := stateOf(t, ctx, s, aID, ObjectMailbox)
	boom := errors.New("boom")
	err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		if _, _, err := tx.ApplyEmailUpdate(ctx, ids[0], EmailUpdate{MoveToMailboxID: &trash}); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if rows, _ := mailboxRowsSince(t, h, a.ID, before); rows != 0 {
		t.Fatalf("%d Mailbox rows survived a rollback", rows)
	}
}

// The consumers of the stream tolerate the new rows: the Email stream (what the
// search indexer reads) is unchanged by them, and a Mailbox row is the only
// thing a Mailbox cursor sees.
func TestEmailStreamIsUnchangedByMailboxCountRows(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	inbox := mustULID(t, a.MailboxID)
	trash := seedMailbox(t, ctx, h, a, "Trash", `\Trash`)
	ids := seedBulkMessages(t, h, a, inbox, 3)
	emBefore := stateOf(t, ctx, s, aID, ObjectEmail)
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		for _, id := range ids {
			if _, _, err := tx.ApplyEmailUpdate(ctx, id, EmailUpdate{MoveToMailboxID: &trash}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_ = s.UserTx(ctx, aID, func(tx *UserTx) error {
		p, err := tx.Changes(ctx, ObjectEmail, emBefore, 0)
		if err != nil || len(p.Updated) != 3 || len(p.Created)+len(p.Destroyed) != 0 {
			t.Errorf("Email changes = %+v, %v; want exactly the 3 moved messages", p, err)
		}
		for _, id := range p.Updated {
			if id == inbox || id == trash {
				t.Errorf("a mailbox id %s leaked into the Email stream", id)
			}
		}
		return nil
	})
}
