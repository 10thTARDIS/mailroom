// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// The Gmail writeback's store surface against PostgreSQL (#52, ADR-047 §3,
// increment 5b): the echo suppression (RecordGmailLabelDelta against the read
// path's diff), All Mail as a Gmail account's Archive, a Mailroom archive
// that Gmail can still undo, the label name carried on a label intent, and
// the hold that keeps every Gmail intent suppressed while the switch is off.

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/oklog/ulid/v2"
)

func (f *gmailFixture) provID(t *testing.T, msg, mailbox ulid.ULID) ulid.ULID {
	t.Helper()
	var id string
	if err := f.h.super.QueryRow(context.Background(),
		`SELECT id FROM app.message_provenance WHERE message_id = $1 AND mailbox_id = $2`,
		msg.String(), mailbox.String()).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return ulid.MustParse(id)
}

func (f *gmailFixture) baseline(t *testing.T, prov ulid.ULID) []string {
	t.Helper()
	var got []string
	if err := f.h.super.QueryRow(context.Background(),
		`SELECT gm_labels FROM app.message_provenance WHERE id = $1`, prov.String()).Scan(&got); err != nil {
		t.Fatal(err)
	}
	return got
}

func (f *gmailFixture) recordDelta(t *testing.T, prov ulid.ULID, add, remove []string) bool {
	t.Helper()
	var moved bool
	if err := f.st.UserTx(context.Background(), f.user, func(tx *UserTx) error {
		var err error
		moved, err = tx.RecordGmailLabelDelta(context.Background(), prov, add, remove)
		return err
	}); err != nil {
		t.Fatalf("record delta: %v", err)
	}
	return moved
}

// labelMessage gives msg the Mailroom label `name` (creating it), as the
// user doing it in Mailroom would.
func (f *gmailFixture) labelMessage(t *testing.T, msg ulid.ULID, name string) ulid.ULID {
	t.Helper()
	var lid ulid.ULID
	if err := f.st.UserTx(context.Background(), f.user, func(tx *UserTx) error {
		ids, _, err := tx.ensureLabelsByName(context.Background(), []string{name})
		if err != nil {
			return err
		}
		lid = ulid.MustParse(ids[0])
		_, _, err = tx.ApplyEmailUpdate(context.Background(), msg, EmailUpdate{AddLabelIDs: []ulid.ULID{lid}})
		return err
	}); err != nil {
		t.Fatalf("label: %v", err)
	}
	return lid
}

func (f *gmailFixture) unlabelMessage(t *testing.T, msg, lid ulid.ULID) {
	t.Helper()
	if err := f.st.UserTx(context.Background(), f.user, func(tx *UserTx) error {
		_, _, err := tx.ApplyEmailUpdate(context.Background(), msg, EmailUpdate{RemoveLabelIDs: []ulid.ULID{lid}})
		return err
	}); err != nil {
		t.Fatalf("unlabel: %v", err)
	}
}

// TestGmailEchoSuppression: a label Mailroom wrote to Gmail does not come
// back from the next reading as a Gmail change that fights a newer Mailroom
// state. With the confirmed delta on the baseline, the user's later removal
// (its own writeback still queued) survives a sweep of the not-yet-updated
// Gmail. NEGATIVE CONTROL: without the delta, the very same reading re-adds
// the label the user just removed — the fight this exists to prevent.
func TestGmailEchoSuppression(t *testing.T) {
	for _, withDelta := range []bool{true, false} {
		name := "with the confirmed delta"
		if !withDelta {
			name = "negative control: no delta recorded"
		}
		t.Run(name, func(t *testing.T) {
			f := newGmailFixture(t)
			m := f.message(t, "echo", "")
			f.prov(t, m, f.gmail, f.allMail, 5)
			f.observe(t, m, f.allMail, 5, `\Inbox`) // first reading
			prov := f.provID(t, m, f.allMail)

			// The user labels it Work in Mailroom; the engine writes it to
			// Gmail and, the STORE confirmed, records the delta.
			lid := f.labelMessage(t, m, "Work")
			if withDelta && !f.recordDelta(t, prov, []string{"Work"}, nil) {
				t.Fatal("the delta moved no row")
			}
			// Then the user takes it off again; that removal's writeback is
			// still queued, so Gmail still has Work at the next sweep.
			f.unlabelMessage(t, m, lid)

			r := f.observe(t, m, f.allMail, 5, `\Inbox`, "Work")

			_, has := f.labelsOf(t, m)["Work"]
			if withDelta {
				if has || r.Changed {
					t.Errorf("the reading re-applied Mailroom's own write over the user's removal (changed=%v)", r.Changed)
				}
			} else if !has {
				t.Error("negative control: without the delta the reading should have re-added Work; " +
					"the test cannot observe the fight it claims to prevent")
			}
		})
	}
}

// TestGmailLabelDeltaKeepsAGmailSideChange: the baseline moves by the delta
// only. A label added in Gmail's own app since the last reading is not
// absorbed into the baseline, so the next reading still imports it.
func TestGmailLabelDeltaKeepsAGmailSideChange(t *testing.T) {
	f := newGmailFixture(t)
	m := f.message(t, "both", "")
	f.prov(t, m, f.gmail, f.allMail, 6)
	f.observe(t, m, f.allMail, 6, `\Inbox`)
	prov := f.provID(t, m, f.allMail)
	f.recordDelta(t, prov, []string{"Work"}, []string{`\INBOX`}) // system labels: any case
	if got := f.baseline(t, prov); !reflect.DeepEqual(got, []string{"Work"}) {
		t.Fatalf("baseline = %q, want [Work]", got)
	}
	// Gmail: Work (ours) plus Travel (added in Gmail's app meanwhile).
	r := f.observe(t, m, f.allMail, 6, "Work", "Travel")
	if _, ok := f.labelsOf(t, m)["Travel"]; !ok || r.LabelsAdded != 1 {
		t.Errorf("the Gmail-side label was lost: labels %v, result %+v", f.labelsOf(t, m), r)
	}
}

// TestGmailLabelDeltaLeavesANeverReadCopy: a copy with no baseline keeps none,
// so its first reading is still a first reading.
func TestGmailLabelDeltaLeavesANeverReadCopy(t *testing.T) {
	f := newGmailFixture(t)
	m := f.message(t, "unread", "")
	f.prov(t, m, f.gmail, f.allMail, 7)
	prov := f.provID(t, m, f.allMail)
	if f.recordDelta(t, prov, []string{"Work"}, nil) {
		t.Error("a never-read copy got a baseline")
	}
	if got := f.baseline(t, prov); got != nil {
		t.Errorf("baseline = %q, want NULL", got)
	}
}

// TestGmailArchiveRoundTrip: archiving in Mailroom places the message in All
// Mail (a Gmail account's Archive) and queues the move there; once its
// writeback is confirmed, the next reading changes nothing; and a later
// "back to the inbox" made in Gmail still applies (the M3 rule treats
// Mailroom's All Mail placement as the one Gmail's Archive implies).
func TestGmailArchiveRoundTrip(t *testing.T) {
	f := newGmailFixture(t)
	m := f.message(t, "archive me", "")
	f.prov(t, m, f.gmail, f.allMail, 8)
	f.observe(t, m, f.allMail, 8, `\Inbox`)
	if f.moved(t, m) != f.inbox.String() {
		t.Fatalf("fixture: not placed in INBOX")
	}
	if err := f.st.UserTx(context.Background(), f.user, func(tx *UserTx) error {
		_, _, err := tx.ApplyEmailUpdate(context.Background(), m, EmailUpdate{MoveToRole: "archive"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got := f.moved(t, m); got != f.allMail.String() {
		t.Fatalf("archive placed it in %q, want All Mail %s", got, f.allMail)
	}
	var payload []byte
	var state string
	if err := f.h.super.QueryRow(context.Background(), `
		SELECT payload, state::text FROM app.writeback_intents WHERE message_id = $1 AND op = 'move'`,
		m.String()).Scan(&payload, &state); err != nil {
		t.Fatal(err)
	}
	var p map[string]string
	_ = json.Unmarshal(payload, &p)
	if p["mailbox"] != "[Gmail]/All Mail" || p["target_role"] != "archive" {
		t.Errorf("move payload = %s", payload)
	}
	if state != "suppressed" {
		t.Errorf("intent state = %s: the Gmail writeback switch is off, so it must be suppressed", state)
	}

	prov := f.provID(t, m, f.allMail)
	f.recordDelta(t, prov, nil, []string{`\Inbox`}) // the engine confirmed -\Inbox
	if r := f.observe(t, m, f.allMail, 8); r.Changed {
		t.Errorf("reading the archived Gmail changed something: %+v", r)
	}
	// Moved back to the inbox in Gmail's own app.
	f.observe(t, m, f.allMail, 8, `\Inbox`)
	if got := f.moved(t, m); got != f.inbox.String() {
		t.Errorf("Gmail's move back to the inbox was not applied: placement %q", got)
	}
}

// TestArchiveRoleIsAllMailOnlyForGmailOAuth: the All Mail fallback is Gmail
// OAuth's alone. A generic account with a \All mailbox and no \Archive keeps
// "no archive mailbox" (a \All folder is virtual on some servers).
func TestArchiveRoleIsAllMailOnlyForGmailOAuth(t *testing.T) {
	f := newGmailFixture(t)
	f.exec(t, `INSERT INTO app.mailboxes (id, user_id, account_id, imap_name, display_name, special_use)
	           VALUES ($1, $2, $3, 'Virtual/All', 'Virtual/All', '\All')`,
		NewULID().String(), f.user.String(), f.generic.String())
	if err := f.st.UserTx(context.Background(), f.user, func(tx *UserTx) error {
		mb, ok, err := tx.MailboxForRole(context.Background(), f.gmail, "archive")
		if err != nil {
			return err
		}
		if !ok || mb.ID != f.allMail || mb.IMAPName != "[Gmail]/All Mail" {
			t.Errorf("gmail archive = %+v, %v", mb, ok)
		}
		mb, ok, err = tx.MailboxForRole(context.Background(), f.generic, "archive")
		if err != nil {
			return err
		}
		if ok {
			t.Errorf("a generic account's \\All became its Archive: %+v", mb)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestGmailLabelIntentCarriesTheName: a label's ±kw intent carries the label
// NAME (what Gmail writeback writes) beside its keyword atom, and on a Gmail
// OAuth account it is born suppressed while the switch is off.
func TestGmailLabelIntentCarriesTheName(t *testing.T) {
	if GmailWritebackReady {
		t.Skip("the switch is on; the suppression half of this test describes the held state")
	}
	f := newGmailFixture(t)
	m := f.message(t, "named", "")
	f.prov(t, m, f.gmail, f.allMail, 9)
	f.observe(t, m, f.allMail, 9, `\Inbox`)
	f.labelMessage(t, m, "Été 2026")
	var payload []byte
	var state string
	if err := f.h.super.QueryRow(context.Background(), `
		SELECT payload, state::text FROM app.writeback_intents WHERE message_id = $1 AND op = '+kw'`,
		m.String()).Scan(&payload, &state); err != nil {
		t.Fatal(err)
	}
	var p map[string]string
	_ = json.Unmarshal(payload, &p)
	if p["label"] != "Été 2026" || p["keyword"] == "" {
		t.Errorf("payload = %s, want the label name and its atom", payload)
	}
	if state != "suppressed" {
		t.Errorf("state = %s, want suppressed (GmailWritebackReady is false)", state)
	}
}

// TestPurgeNeverPromotesAnExpungeForAHeldGmailAccount (#911 review): the
// Gmail writeback hold counts as writeback off for purge, whatever the
// account's column says.
func TestPurgeNeverPromotesAnExpungeForAHeldGmailAccount(t *testing.T) {
	if GmailWritebackReady {
		t.Skip("the switch is on; this test describes the held state")
	}
	f := newGmailFixture(t) // the Gmail account has writeback_enabled = true
	m := f.message(t, "trashed", "")
	f.prov(t, m, f.gmail, f.trash, 3)
	var (
		ids  []ulid.ULID
		skip ExpungeSkip
	)
	if err := f.st.UserTx(context.Background(), f.user, func(tx *UserTx) error {
		var err error
		ids, skip, err = tx.ExpungeableAccountIDs(context.Background(), m)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(ids) != 0 || skip != ExpungeSkipShadowed {
		t.Errorf("expungeable = %v (skip %q), want none, shadowed", ids, skip)
	}
}

// TestGmailWritebackFoldersRefusesAmbiguousRoles (#911 review F9).
func TestGmailWritebackFoldersRefusesAmbiguousRoles(t *testing.T) {
	f := newGmailFixture(t)
	read := func() error {
		return f.st.UserTx(context.Background(), f.user, func(tx *UserTx) error {
			_, err := tx.GmailWritebackFolders(context.Background(), f.gmail)
			return err
		})
	}
	if err := read(); err != nil {
		t.Fatalf("unambiguous folders refused: %v", err)
	}
	f.exec(t, `INSERT INTO app.mailboxes (id, user_id, account_id, imap_name, display_name, special_use)
	           VALUES ($1, $2, $3, '[Gmail]/Bin', '[Gmail]/Bin', '\Trash')`,
		NewULID().String(), f.user.String(), f.gmail.String())
	if err := read(); !errors.Is(err, ErrGmailAmbiguousFolders) {
		t.Errorf("two Trash mailboxes: err = %v, want ErrGmailAmbiguousFolders", err)
	}
}

// TestGmailLabelDeltaIsCaseInsensitive (#911 review F7): removing "work"
// takes out "Work", and adding "WORK" over "Work" adds no duplicate.
func TestGmailLabelDeltaIsCaseInsensitive(t *testing.T) {
	f := newGmailFixture(t)
	m := f.message(t, "case", "")
	f.prov(t, m, f.gmail, f.allMail, 4)
	f.observe(t, m, f.allMail, 4, `\Inbox`, "Work")
	prov := f.provID(t, m, f.allMail)
	f.recordDelta(t, prov, []string{"WORK"}, nil)
	if got := f.baseline(t, prov); !reflect.DeepEqual(got, []string{`\Inbox`, "Work"}) {
		t.Errorf("after adding WORK: %q", got)
	}
	f.recordDelta(t, prov, nil, []string{"work"})
	if got := f.baseline(t, prov); !reflect.DeepEqual(got, []string{`\Inbox`}) {
		t.Errorf("after removing work: %q", got)
	}
}

// TestKeywordAndLabelIntentsDoNotCollide (#911 review F8): a raw keyword
// equal to a label's atom, in the same update as that label, enqueues both
// intents instead of the second being swallowed by the first's key.
func TestKeywordAndLabelIntentsDoNotCollide(t *testing.T) {
	f := newGmailFixture(t)
	m := f.message(t, "collide", "")
	f.prov(t, m, f.generic, f.genericInbox, 11)
	var atom string
	if err := f.st.UserTx(context.Background(), f.user, func(tx *UserTx) error {
		ids, _, err := tx.ensureLabelsByName(context.Background(), []string{"Clash"})
		if err != nil {
			return err
		}
		lid := ulid.MustParse(ids[0])
		a, _, _, err := tx.labelKeywordAtom(context.Background(), lid)
		if err != nil {
			return err
		}
		atom = a
		_, _, err = tx.ApplyEmailUpdate(context.Background(), m, EmailUpdate{
			AddKeywords: []string{atom}, AddLabelIDs: []ulid.ULID{lid}})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := f.h.super.QueryRow(context.Background(),
		`SELECT count(*) FROM app.writeback_intents WHERE message_id = $1 AND op = '+kw'`, m.String()).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("+kw intents = %d, want 2 (the keyword's and the label's)", n)
	}
}
