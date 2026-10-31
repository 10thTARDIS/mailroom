// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// The Gmail read path's store surface against PostgreSQL (#52, ADR-047):
// ApplyGmailObservation (labels, folder placement, the change_log invariant,
// no writeback intents), the forced ingest-only hold, the daily meter, the
// backfill floor, progress and the census's dedup count.

import (
	"context"
	"crypto/sha256"
	"sort"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
)

type gmailFixture struct {
	h                                         *harness
	st                                        *Store
	user, other                               ulid.ULID
	gmail, generic                            ulid.ULID
	inbox, allMail, sent, drafts, spam, trash ulid.ULID
	genericInbox                              ulid.ULID
}

func newGmailFixture(t *testing.T) *gmailFixture {
	t.Helper()
	h := newHarness(t)
	f := &gmailFixture{h: h, st: h.appStore(), user: NewULID(), other: NewULID(), gmail: NewULID(), generic: NewULID(),
		inbox: NewULID(), allMail: NewULID(), sent: NewULID(), drafts: NewULID(), spam: NewULID(), trash: NewULID(),
		genericInbox: NewULID()}
	for _, u := range []ulid.ULID{f.user, f.other} {
		f.exec(t, `INSERT INTO app.users (id, oidc_subject, email) VALUES ($1, $2, $3)`,
			u.String(), "sub-"+u.String(), u.String()+"@example.com")
	}
	f.exec(t, `INSERT INTO app.accounts (id, user_id, name, provider_profile, auth_kind, imap_host, smtp_host,
	             credentials_ct, credentials_dek, writeback_enabled)
	           VALUES ($1, $2, 'gmail', 'gmail', 'oauth2', 'imap.gmail.com', 'smtp.gmail.com', '\x00', $3, true)`,
		f.gmail.String(), f.user.String(), NewULID().String())
	f.exec(t, `INSERT INTO app.accounts (id, user_id, name, imap_host, smtp_host, credentials_ct, credentials_dek)
	           VALUES ($1, $2, 'other', 'mail.example.com', 'smtp.example.com', '\x00', $3)`,
		f.generic.String(), f.user.String(), NewULID().String())
	for _, m := range []struct {
		id      ulid.ULID
		account ulid.ULID
		name    string
		role    string
	}{
		{f.inbox, f.gmail, "INBOX", ""}, {f.allMail, f.gmail, "[Gmail]/All Mail", `\All`},
		{f.sent, f.gmail, "[Gmail]/Sent Mail", `\Sent`}, {f.drafts, f.gmail, "[Gmail]/Drafts", `\Drafts`},
		{f.spam, f.gmail, "[Gmail]/Spam", `\Junk`}, {f.trash, f.gmail, "[Gmail]/Trash", `\Trash`},
		{f.genericInbox, f.generic, "INBOX", ""},
	} {
		f.exec(t, `INSERT INTO app.mailboxes (id, user_id, account_id, imap_name, display_name, special_use)
		           VALUES ($1, $2, $3, $4, $4, NULLIF($5, ''))`,
			m.id.String(), f.user.String(), m.account.String(), m.name, m.role)
	}
	return f
}

func (f *gmailFixture) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := f.h.super.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func (f *gmailFixture) message(t *testing.T, subject, msgIDHint string) ulid.ULID {
	t.Helper()
	id, thread := NewULID(), NewULID()
	hash := sha256.Sum256([]byte(subject + id.String()))
	f.exec(t, `INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id) VALUES ($1, $2, 10, 20, $3)`,
		f.user.String(), hash[:], NewULID().String())
	f.exec(t, `INSERT INTO app.threads (id, user_id, subject) VALUES ($1, $2, $3)`, thread.String(), f.user.String(), subject)
	f.exec(t, `INSERT INTO app.messages (id, user_id, content_hash, thread_id, subject, received_at, size_bytes, message_id_hint)
	           VALUES ($1, $2, $3, $4, $5, now(), 1000, NULLIF($6, ''))`,
		id.String(), f.user.String(), hash[:], thread.String(), subject, msgIDHint)
	return id
}

func (f *gmailFixture) prov(t *testing.T, msg, account, mailbox ulid.ULID, uid int64) {
	t.Helper()
	f.exec(t, `INSERT INTO app.message_provenance (id, user_id, message_id, account_id, mailbox_id, uid, uidvalidity)
	           VALUES ($1, $2, $3, $4, $5, $6, 1)`,
		NewULID().String(), f.user.String(), msg.String(), account.String(), mailbox.String(), uid)
}

func (f *gmailFixture) observe(t *testing.T, msg, mailbox ulid.ULID, uid int64, labels ...string) GmailApplyResult {
	t.Helper()
	if labels == nil {
		labels = []string{}
	}
	var r GmailApplyResult
	if err := f.st.UserTx(context.Background(), f.user, func(tx *UserTx) error {
		var err error
		r, err = tx.ApplyGmailObservation(context.Background(), msg, GmailObservation{
			MailboxID: mailbox, UIDValidity: 1, UID: uid, MsgID: 42, ThrID: 77, Labels: labels})
		return err
	}); err != nil {
		t.Fatalf("observe: %v", err)
	}
	return r
}

// labelsOf is ground truth: (name, applied_by) pairs on a message.
func (f *gmailFixture) labelsOf(t *testing.T, msg ulid.ULID) map[string]string {
	t.Helper()
	rows, err := f.h.super.Query(context.Background(), `
		SELECT l.name, ml.applied_by FROM app.message_labels ml JOIN app.labels l ON l.id = ml.label_id
		 WHERE ml.message_id = $1`, msg.String())
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var n, by string
		if err := rows.Scan(&n, &by); err != nil {
			t.Fatal(err)
		}
		out[n] = by
	}
	return out
}

func (f *gmailFixture) moved(t *testing.T, msg ulid.ULID) string {
	t.Helper()
	var m *string
	if err := f.h.super.QueryRow(context.Background(), `SELECT moved_mailbox_id FROM app.messages WHERE id = $1`, msg.String()).Scan(&m); err != nil {
		t.Fatal(err)
	}
	if m == nil {
		return ""
	}
	return *m
}

func (f *gmailFixture) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := f.h.super.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (f *gmailFixture) emailChanges(t *testing.T, msg ulid.ULID) int {
	return f.count(t, `SELECT count(*) FROM app.change_log WHERE object_type = 'Email' AND object_id = $1`, msg.String())
}

func TestApplyGmailObservationLabelsAndFolder(t *testing.T) {
	f := newGmailFixture(t)
	ctx := context.Background()
	// "Work" already exists as a Mailroom label: user-wide, the same label.
	var work ulid.ULID
	if err := f.st.UserTx(ctx, f.user, func(tx *UserTx) error {
		var err error
		work, err = tx.CreateLabel(ctx, LabelRow{Name: "Work", MirrorToIMAP: true})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	m := f.message(t, "hello", "")
	f.prov(t, m, f.gmail, f.allMail, 10)

	// First reading: \Inbox places it in INBOX; Work (existing) and Été (new)
	// are applied as the user's own filings; \Important and \Starred are not
	// imported at all.
	r := f.observe(t, m, f.allMail, 10, `\Inbox`, `\Important`, `\Starred`, "Work", "Été")
	if !r.Found || !r.FirstSeen || r.LabelsAdded != 2 || r.LabelsCreated != 1 || !r.FolderChanged || !r.Changed {
		t.Fatalf("first reading = %+v", r)
	}
	got := f.labelsOf(t, m)
	want := map[string]string{"Work": "user", "Été": "user"}
	if len(got) != 2 || got["Work"] != "user" || got["Été"] != "user" {
		t.Errorf("labels = %v, want %v", got, want)
	}
	if f.moved(t, m) != f.inbox.String() {
		t.Errorf("placement = %q, want INBOX", f.moved(t, m))
	}
	if n := f.count(t, `SELECT count(*) FROM app.labels WHERE user_id = $1 AND name IN ('\Important', '\Starred', 'Important', 'Starred')`, f.user.String()); n != 0 {
		t.Errorf("%d Gmail system label(s) became Mailroom labels", n)
	}
	if n := f.count(t, `SELECT count(*) FROM app.labels WHERE user_id = $1 AND name = 'Work'`, f.user.String()); n != 1 {
		t.Errorf("Work duplicated: %d", n)
	}
	_ = work
	if f.emailChanges(t, m) != 1 {
		t.Errorf("Email change rows = %d, want 1", f.emailChanges(t, m))
	}
	var gm []string
	var msgid, thrid string
	if err := f.h.super.QueryRow(ctx, `SELECT gm_labels, gm_msgid, gm_thrid FROM app.message_provenance WHERE mailbox_id = $1 AND uid = 10`,
		f.allMail.String()).Scan(&gm, &msgid, &thrid); err != nil {
		t.Fatal(err)
	}
	if len(gm) != 5 || msgid != "42" || thrid != "77" {
		t.Errorf("recorded gm_labels=%v msgid=%q thrid=%q", gm, msgid, thrid)
	}

	// The same reading again changes nothing and appends nothing.
	if r := f.observe(t, m, f.allMail, 10, `\Inbox`, `\Important`, `\Starred`, "Work", "Été"); r.Changed {
		t.Errorf("an unchanged reading changed something: %+v", r)
	}
	if f.emailChanges(t, m) != 1 {
		t.Errorf("an unchanged reading appended a change row")
	}

	// A label the user added in MAILROOM survives the next reading of an
	// unchanged Gmail: only Gmail's CHANGES are applied.
	if err := f.st.UserTx(ctx, f.user, func(tx *UserTx) error {
		lid, err := tx.CreateLabel(ctx, LabelRow{Name: "Mine"})
		if err != nil {
			return err
		}
		_, err = tx.tx.Exec(ctx, `INSERT INTO app.message_labels (user_id, message_id, label_id) VALUES ($1, $2, $3)`,
			f.user.String(), m.String(), lid.String())
		return err
	}); err != nil {
		t.Fatal(err)
	}

	// Archived in Gmail, and Été removed: INBOX placement goes, Été goes,
	// Work and the user's own Mine stay.
	r = f.observe(t, m, f.allMail, 10, "Work")
	if r.LabelsRemoved != 1 || !r.FolderChanged || !r.Changed {
		t.Errorf("archive reading = %+v", r)
	}
	got = f.labelsOf(t, m)
	names := make([]string, 0, len(got))
	for n := range got {
		names = append(names, n)
	}
	sort.Strings(names)
	if len(names) != 2 || names[0] != "Mine" || names[1] != "Work" {
		t.Errorf("labels after archive = %v", names)
	}
	if f.moved(t, m) != "" {
		t.Errorf("an archived message kept placement %q (All Mail membership is its provenance)", f.moved(t, m))
	}
	// Moved in Gmail to Sent (sent to self): placement follows.
	f.observe(t, m, f.allMail, 10, `\Sent`, "Work")
	if f.moved(t, m) != f.sent.String() {
		t.Errorf("placement = %q, want Sent", f.moved(t, m))
	}

	// NO WRITEBACK INTENT came from any of this: a change Gmail reported is
	// not a change to push back to Gmail.
	if n := f.count(t, `SELECT count(*) FROM app.writeback_intents WHERE message_id = $1`, m.String()); n != 0 {
		t.Errorf("%d writeback intent(s) from Gmail readings", n)
	}
}

// A placement overrides every location, so a message that also lives in
// another of the user's accounts is not placed by Gmail's labels.
func TestApplyGmailObservationLeavesSharedMessagesUnplaced(t *testing.T) {
	f := newGmailFixture(t)
	m := f.message(t, "to myself", "")
	f.prov(t, m, f.generic, f.genericInbox, 5)
	f.prov(t, m, f.gmail, f.allMail, 11)
	r := f.observe(t, m, f.allMail, 11, `\Inbox`, "Work")
	if r.FolderChanged || f.moved(t, m) != "" {
		t.Errorf("a shared message was placed: %+v moved=%q", r, f.moved(t, m))
	}
	if f.labelsOf(t, m)["Work"] != "user" {
		t.Errorf("labels still apply to a shared message: %v", f.labelsOf(t, m))
	}
}

// A copy in Gmail's Trash or Spam takes away a placement made from All Mail's
// labels, so the message shows where it is.
func TestApplyGmailObservationTrashClearsPlacement(t *testing.T) {
	f := newGmailFixture(t)
	m := f.message(t, "deleted in gmail", "")
	f.prov(t, m, f.gmail, f.allMail, 12)
	f.observe(t, m, f.allMail, 12, `\Inbox`)
	if f.moved(t, m) != f.inbox.String() {
		t.Fatal("setup: not placed in INBOX")
	}
	f.prov(t, m, f.gmail, f.trash, 3)
	r := f.observe(t, m, f.trash, 3)
	if !r.FolderChanged || f.moved(t, m) != "" {
		t.Errorf("Trash copy: %+v moved=%q", r, f.moved(t, m))
	}
}

// INGEST-ONLY IS FORCED for a Gmail OAuth account (ADR-047 §3, ADR-028):
// even with writeback_enabled = true on its row, every intent is suppressed.
// The generic account beside it is untouched — the negative control.
func TestGmailWritebackHeldInStore(t *testing.T) {
	f := newGmailFixture(t)
	ctx := context.Background()
	m := f.message(t, "x", "")
	state := func(account ulid.ULID, key string) string {
		var id ulid.ULID
		if err := f.st.UserTx(ctx, f.user, func(tx *UserTx) error {
			var err error
			id, err = tx.EnqueueIntent(ctx, IntentRow{MessageID: &m, AccountID: account, Op: "flag_add",
				Payload: []byte(`{"flag":"\\Seen"}`), IdempotencyKey: key})
			return err
		}); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		var s string
		if err := f.h.super.QueryRow(ctx, `SELECT state FROM app.writeback_intents WHERE id = $1`, id.String()).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	if s := state(f.gmail, "k-gmail"); s != "suppressed" {
		t.Errorf("Gmail OAuth intent state = %q, want suppressed", s)
	}
	if s := state(f.generic, "k-generic"); s != "pending" {
		t.Errorf("generic account intent state = %q, want pending (the hold reached it)", s)
	}
}

func TestGmailMeterFloorProgressAndDedup(t *testing.T) {
	f := newGmailFixture(t)
	ctx := context.Background()
	d1 := time.Date(2026, 10, 27, 23, 0, 0, 0, time.UTC)
	d2 := d1.Add(2 * time.Hour) // the next UTC day
	run := func(fn func(tx *UserTx) error) {
		t.Helper()
		if err := f.st.UserTx(ctx, f.user, fn); err != nil {
			t.Fatal(err)
		}
	}
	var a, b, c, today int64
	run(func(tx *UserTx) error {
		var err error
		if a, err = tx.AddDownloadBytes(ctx, f.gmail, d1, 100); err != nil {
			return err
		}
		if b, err = tx.AddDownloadBytes(ctx, f.gmail, d1, 50); err != nil {
			return err
		}
		if c, err = tx.AddDownloadBytes(ctx, f.gmail, d2, 7); err != nil {
			return err
		}
		today, err = tx.DownloadedLast24h(ctx, f.gmail, d2)
		return err
	})
	// ROLLING, NOT A CALENDAR DAY (#52 review M2): d2 is two hours after d1
	// and on the next UTC day, and the 23:00 bytes still count against it.
	if a != 100 || b != 150 || c != 157 || today != 157 {
		t.Errorf("meter: %d %d %d last24h=%d, want 100 150 157 157", a, b, c, today)
	}
	window := func(at time.Time) int64 {
		var n int64
		run(func(tx *UserTx) error {
			var err error
			n, err = tx.DownloadedLast24h(ctx, f.gmail, at)
			return err
		})
		return n
	}
	if w := window(d1.Add(25 * time.Hour)); w != 7 {
		t.Errorf("25h after d1 the window = %d, want 7 (d1's hour has rolled out)", w)
	}
	if w := window(d2.Add(25 * time.Hour)); w != 0 {
		t.Errorf("25h after d2 the window = %d, want 0", w)
	}
	// A write long after the last one starts the ring over.
	run(func(tx *UserTx) error {
		var err error
		c, err = tx.AddDownloadBytes(ctx, f.gmail, d2.Add(72*time.Hour), 5)
		return err
	})
	if c != 5 {
		t.Errorf("after 3 days the window = %d, want 5", c)
	}

	// Progress: two messages imported of 5 on the server, All Mail floored at 3.
	f.exec(t, `UPDATE app.mailboxes SET poll_messages = 5 WHERE id = $1`, f.allMail.String())
	run(func(tx *UserTx) error { return tx.SetBackfillFloor(ctx, f.allMail, 3) })
	m1, m2 := f.message(t, "a", "<a@example.com>"), f.message(t, "b", "b@example.com")
	f.prov(t, m1, f.gmail, f.allMail, 3)
	f.prov(t, m2, f.gmail, f.allMail, 4)
	var p GmailImportProgress
	run(func(tx *UserTx) error {
		var err error
		p, err = tx.GmailImportProgress(ctx, f.gmail, d2.Add(72*time.Hour))
		return err
	})
	if p.Mailboxes != 3 || p.MessagesOnServer == nil || *p.MessagesOnServer != 5 || p.MessagesImported != 2 ||
		p.BytesImported != 2000 || p.HistoryComplete || p.BytesLast24h != 5 {
		t.Errorf("progress = %+v", p)
	}
	run(func(tx *UserTx) error { return tx.SetBackfillFloor(ctx, f.allMail, 1) })
	run(func(tx *UserTx) error {
		var err error
		p, err = tx.GmailImportProgress(ctx, f.gmail, d2)
		return err
	})
	if !p.HistoryComplete {
		t.Errorf("floor at 1 everywhere is a complete history: %+v", p)
	}

	// Dedup preview: x@ lives only in the OTHER account (counts), a@ lives in
	// this Gmail account (does not), b@ stored without brackets (matched).
	mx := f.message(t, "x", "<x@example.com>")
	f.prov(t, mx, f.generic, f.genericInbox, 9)
	mb := f.message(t, "b2", "<b2@example.com>")
	_ = mb // imported: no provenance at all — counts as elsewhere
	var n int
	run(func(tx *UserTx) error {
		var err error
		n, err = tx.CountMessageIDsElsewhere(ctx, f.gmail, []string{"x@example.com", "<a@example.com>", "b2@example.com", "nope@example.com"})
		return err
	})
	if n != 2 {
		t.Errorf("dedup elsewhere = %d, want 2 (x in the other account, b2 imported)", n)
	}
}

// RLS: another user sees nothing and changes nothing.
func TestApplyGmailObservationIsolated(t *testing.T) {
	f := newGmailFixture(t)
	m := f.message(t, "mine", "")
	f.prov(t, m, f.gmail, f.allMail, 20)
	var r GmailApplyResult
	if err := f.st.UserTx(context.Background(), f.other, func(tx *UserTx) error {
		var err error
		r, err = tx.ApplyGmailObservation(context.Background(), m, GmailObservation{MailboxID: f.allMail, UIDValidity: 1, UID: 20, Labels: []string{`\Inbox`, "X"}})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if r.Found || f.moved(t, m) != "" || len(f.labelsOf(t, m)) != 0 {
		t.Errorf("another user's reading reached the message: %+v", r)
	}
}
