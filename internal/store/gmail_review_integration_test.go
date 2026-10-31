// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// Regression tests for the independent review of #907 (#52), against
// PostgreSQL. Each was written to fail against the reviewed code (d322138).

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
)

// H1: an observation with NO label reading (nil) is refused and changes
// nothing — never read as "no labels".
func TestApplyGmailObservationNilIsNoReading(t *testing.T) {
	f := newGmailFixture(t)
	m := f.message(t, "keep my labels", "")
	f.prov(t, m, f.gmail, f.allMail, 30)
	f.observe(t, m, f.allMail, 30, `\Inbox`, "Work")
	err := f.st.UserTx(context.Background(), f.user, func(tx *UserTx) error {
		_, err := tx.ApplyGmailObservation(context.Background(), m, GmailObservation{MailboxID: f.allMail, UIDValidity: 1, UID: 30})
		return err
	})
	if !errors.Is(err, ErrGmailNoReading) {
		t.Errorf("nil labels: err = %v, want ErrGmailNoReading", err)
	}
	if f.labelsOf(t, m)["Work"] != "user" || f.moved(t, m) != f.inbox.String() {
		t.Errorf("a non-reading changed the message: labels=%v moved=%q", f.labelsOf(t, m), f.moved(t, m))
	}
}

// H2 (4): a label name with NUL and control characters is sanitised before
// it reaches PostgreSQL (whose text type rejects NUL outright).
func TestApplyGmailObservationSanitisesLabels(t *testing.T) {
	f := newGmailFixture(t)
	m := f.message(t, "hostile labels", "")
	f.prov(t, m, f.gmail, f.allMail, 31)
	f.observe(t, m, f.allMail, 31, "a\x00b", "tab\there\u0085", "ok", "\x00")
	got := f.labelsOf(t, m)
	if len(got) != 3 || got["ab"] != "user" || got["tabhere"] != "user" || got["ok"] != "user" {
		t.Errorf("labels = %v, want ab, tabhere, ok", got)
	}
}

// H2 (5): a label that cannot be stored rolls back ONLY the label reading
// (savepoint): the caller's transaction — the message's ingest — stays usable.
func TestApplyGmailObservationFailureIsIsolated(t *testing.T) {
	f := newGmailFixture(t)
	ctx := context.Background()
	f.exec(t, `CREATE FUNCTION app.test_refuse_boom() RETURNS trigger LANGUAGE plpgsql AS $$
	           BEGIN
	             IF EXISTS (SELECT 1 FROM app.labels WHERE id = NEW.label_id AND name = 'boom') THEN
	               RAISE EXCEPTION 'injected: label boom cannot be stored';
	             END IF;
	             RETURN NEW;
	           END $$`)
	f.exec(t, `CREATE TRIGGER test_refuse_boom BEFORE INSERT ON app.message_labels
	           FOR EACH ROW EXECUTE FUNCTION app.test_refuse_boom()`)
	t.Cleanup(func() {
		f.exec(t, `DROP TRIGGER test_refuse_boom ON app.message_labels`)
		f.exec(t, `DROP FUNCTION app.test_refuse_boom()`)
	})
	m := f.message(t, "boom", "")
	f.prov(t, m, f.gmail, f.allMail, 34)
	var applyErr error
	err := f.st.UserTx(ctx, f.user, func(tx *UserTx) error {
		_, applyErr = tx.ApplyGmailObservation(ctx, m, GmailObservation{MailboxID: f.allMail, UIDValidity: 1, UID: 34,
			Labels: []string{`\Inbox`, "boom"}})
		// The outer transaction must still work after the failure.
		_, err := tx.tx.Exec(ctx, `UPDATE app.messages SET subject = 'still writable' WHERE id = $1`, m.String())
		return err
	})
	if applyErr == nil {
		t.Fatal("the injected failure did not fire; the isolation assertion proves nothing")
	}
	if err != nil {
		t.Fatalf("the failed label reading poisoned the caller's transaction: %v", err)
	}
	if n := f.count(t, `SELECT count(*) FROM app.messages WHERE id = $1 AND subject = 'still writable'`, m.String()); n != 1 {
		t.Error("the caller's write after the failure did not commit")
	}
	if f.moved(t, m) != "" || len(f.labelsOf(t, m)) != 0 {
		t.Errorf("the failed reading was partly applied: moved=%q labels=%v", f.moved(t, m), f.labelsOf(t, m))
	}
}

// M3: a Gmail folder change does not undo a move the user made in Mailroom.
func TestApplyGmailObservationKeepsMailroomMove(t *testing.T) {
	f := newGmailFixture(t)
	receipts := NewULID()
	f.exec(t, `INSERT INTO app.mailboxes (id, user_id, account_id, imap_name, display_name)
	           VALUES ($1, $2, $3, 'Receipts', 'Receipts')`, receipts.String(), f.user.String(), f.gmail.String())
	m := f.message(t, "filed by me", "")
	f.prov(t, m, f.gmail, f.allMail, 32)
	f.observe(t, m, f.allMail, 32, `\Inbox`)
	f.exec(t, `UPDATE app.messages SET moved_mailbox_id = $2 WHERE id = $1`, m.String(), receipts.String())
	f.observe(t, m, f.allMail, 32) // archived in Gmail: \Inbox gone
	if f.moved(t, m) != receipts.String() {
		t.Errorf("a Gmail folder change undid the Mailroom move: moved=%q", f.moved(t, m))
	}
	// Control: a placement Gmail made IS moved by Gmail.
	m2 := f.message(t, "placed by gmail", "")
	f.prov(t, m2, f.gmail, f.allMail, 33)
	f.observe(t, m2, f.allMail, 33, `\Inbox`)
	f.observe(t, m2, f.allMail, 33)
	if f.moved(t, m2) != "" {
		t.Errorf("Gmail's own placement was not moved: %q", f.moved(t, m2))
	}
}

// H3: NO GHOSTS. A placed Gmail message whose All Mail copy is expunged
// without appearing in Spam or Trash (a sent or discarded draft, a
// delete-forever) loses its placement and vanishes, instead of staying in
// Drafts or INBOX forever.
func TestReapClearsGmailLabelViewPlacement(t *testing.T) {
	f := newGmailFixture(t)
	ctx := context.Background()
	draft := f.message(t, "a draft", "")
	f.prov(t, draft, f.gmail, f.allMail, 40)
	f.observe(t, draft, f.allMail, 40, `\Draft`)
	inbox := f.message(t, "deleted forever", "")
	f.prov(t, inbox, f.gmail, f.allMail, 41)
	f.observe(t, inbox, f.allMail, 41, `\Inbox`)
	keep := f.message(t, "still there", "")
	f.prov(t, keep, f.gmail, f.allMail, 42)
	f.observe(t, keep, f.allMail, 42, `\Inbox`)
	if f.moved(t, draft) != f.drafts.String() || f.moved(t, inbox) != f.inbox.String() {
		t.Fatal("setup: not placed")
	}
	var res ReapResult
	if err := f.st.UserTx(ctx, f.user, func(tx *UserTx) error {
		var err error
		res, err = tx.ReapMailbox(ctx, ReapObservation{MailboxID: f.allMail, UIDValidity: 1, ObservedUIDs: []int64{42}, UIDBound: 43})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for name, m := range map[string]ulid.ULID{"draft": draft, "inbox": inbox} {
		if f.moved(t, m) != "" {
			t.Errorf("%s: the expunged message is still placed in %q — a ghost", name, f.moved(t, m))
		}
		if n := f.count(t, `SELECT count(*) FROM app.messages WHERE id = $1 AND vanished_at IS NOT NULL`, m.String()); n != 1 {
			t.Errorf("%s: not vanished", name)
		}
	}
	if f.moved(t, keep) != f.inbox.String() {
		t.Error("a message still on the server lost its placement")
	}
	if res.MessagesVanished != 2 {
		t.Errorf("vanished = %d, want 2", res.MessagesVanished)
	}

	// Control: a GENERIC account's placement is untouched by the same reap.
	moved := NewULID()
	f.exec(t, `INSERT INTO app.mailboxes (id, user_id, account_id, imap_name, display_name, special_use)
	           VALUES ($1, $2, $3, 'Archive', 'Archive', '\Archive')`, moved.String(), f.user.String(), f.generic.String())
	g := f.message(t, "generic move pending", "")
	f.prov(t, g, f.generic, f.genericInbox, 7)
	f.exec(t, `UPDATE app.messages SET moved_mailbox_id = $2 WHERE id = $1`, g.String(), f.genericInbox.String())
	if err := f.st.UserTx(ctx, f.user, func(tx *UserTx) error {
		_, err := tx.ReapMailbox(ctx, ReapObservation{MailboxID: f.genericInbox, UIDValidity: 1, ObservedUIDs: []int64{}, UIDBound: 8})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if f.moved(t, g) != f.genericInbox.String() {
		t.Errorf("a generic account's placement was cleared: %q", f.moved(t, g))
	}
}

// THE WINDOW NEVER UNDER-COUNTS (#907 security review). A full-budget burst
// at HH:01 must still count against an attempt at HH+24:00 — exactly 23h59m
// later, inside one true 24 hours — and be free at HH+25:00. With 24 hourly
// buckets the burst's bucket was cleared at HH+24:00, allowing twice the
// budget inside 24 hours.
func TestGmailMeterNeverUnderCounts(t *testing.T) {
	f := newGmailFixture(t)
	ctx := context.Background()
	const budget = 2_000_000_000
	burst := time.Date(2026, 10, 27, 10, 1, 0, 0, time.UTC) // HH:01
	window := func(at time.Time) int64 {
		var n int64
		if err := f.st.UserTx(ctx, f.user, func(tx *UserTx) error {
			var err error
			n, err = tx.DownloadedLast24h(ctx, f.gmail, at)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if err := f.st.UserTx(ctx, f.user, func(tx *UserTx) error {
		_, err := tx.AddDownloadBytes(ctx, f.gmail, burst, budget)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	refuseAt := time.Date(2026, 10, 28, 10, 0, 0, 0, time.UTC) // HH+24:00, 23h59m after the burst
	if w := window(refuseAt); budget-w > 0 {
		t.Errorf("at HH+24:00 the window holds %d of a %d burst 23h59m old: another burst would be allowed", w, int64(budget))
	}
	allowAt := time.Date(2026, 10, 28, 11, 0, 0, 0, time.UTC) // HH+25:00
	if w := window(allowAt); w != 0 {
		t.Errorf("at HH+25:00 (24h after the burst's hour ended) the window still holds %d", w)
	}
}

// TWO CONCURRENT FIRST WRITES BOTH COUNT (#907 re-review). Transaction A
// meters 100 bytes on an account with no meter row and holds its
// transaction open; B meters 7 bytes meanwhile. Before the fix both read "no
// row" and B's upsert replaced A's ring, losing A's bytes.
func TestGmailMeterConcurrentFirstWrites(t *testing.T) {
	f := newGmailFixture(t)
	ctx := context.Background()
	at := time.Date(2026, 10, 27, 10, 30, 0, 0, time.UTC)
	aWrote, release := make(chan struct{}), make(chan struct{})
	aErr := make(chan error, 1)
	go func() {
		aErr <- f.st.UserTx(ctx, f.user, func(tx *UserTx) error {
			if _, err := tx.AddDownloadBytes(ctx, f.gmail, at, 100); err != nil {
				return err
			}
			close(aWrote)
			<-release // hold A open while B runs
			return nil
		})
	}()
	select {
	case <-aWrote:
	case err := <-aErr:
		t.Fatalf("A: %v", err)
	}
	bErr := make(chan error, 1)
	go func() {
		bErr <- f.st.UserTx(ctx, f.user, func(tx *UserTx) error {
			_, err := tx.AddDownloadBytes(ctx, f.gmail, at, 7)
			return err
		})
	}()
	time.Sleep(300 * time.Millisecond) // let B reach the row while A is open
	close(release)
	if err := <-aErr; err != nil {
		t.Fatalf("A: %v", err)
	}
	if err := <-bErr; err != nil {
		t.Fatalf("B: %v", err)
	}
	var total int64
	if err := f.st.UserTx(ctx, f.user, func(tx *UserTx) error {
		var err error
		total, err = tx.DownloadedLast24h(ctx, f.gmail, at)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if total != 107 {
		t.Errorf("two concurrent first writes metered %d, want 107 (one was lost)", total)
	}
}
