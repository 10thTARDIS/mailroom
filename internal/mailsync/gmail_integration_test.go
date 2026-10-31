// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package mailsync

// THE GMAIL READ PATH END TO END (#52, ADR-047): the real engine, the real
// ingest and the real store over PostgreSQL, syncing the scripted fake Gmail
// (internal/sync/gmailtest) through the real sync.Dial and the vendored
// go-imap parser. Only PostgreSQL is needed — no Dovecot — so this runs in
// the ordinary integration lane (scripts/integration-test.sh).

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oklog/ulid/v2"

	"mailroom/internal/ingest"
	"mailroom/internal/store"
	"mailroom/internal/sync"
	"mailroom/internal/sync/gmailtest"
)

func TestLiveGmailReadPathEndToEnd(t *testing.T) {
	ctx := context.Background()
	super, err := pgxpool.New(ctx, envOrSkip(t, "MAILROOM_TEST_SUPER_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(super.Close)
	st, err := store.Open(ctx, envOrSkip(t, "MAILROOM_TEST_APP_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)

	userID, accountID := ulid.Make(), ulid.Make()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO app.users (id, oidc_subject, email) VALUES ($1, $2, $3)`,
			[]any{userID.String(), "sub-" + userID.String(), userID.String() + "@example.com"}},
		{`INSERT INTO app.accounts (id, user_id, name, provider_profile, auth_kind, imap_host, smtp_host,
		     credentials_ct, credentials_dek, writeback_enabled)
		  VALUES ($1, $2, 'gmail', 'gmail', 'oauth2', 'imap.gmail.com', 'smtp.gmail.com', '\x00', $3, false)`,
			[]any{accountID.String(), userID.String(), ulid.Make().String()}},
	} {
		if _, err := super.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	g := gmailtest.New(t)
	g.StandardMailboxes()
	body := func(subj, mid, refs string) []byte {
		h := "From: a@example.com\r\nTo: you@example.com\r\nSubject: " + subj + "\r\nMessage-ID: <" + mid + ">\r\n"
		if refs != "" {
			h += "References: <" + refs + ">\r\n"
		}
		return []byte(h + "Date: Mon, 26 Oct 2026 10:00:00 +0000\r\n\r\nbody of " + subj + "\r\n")
	}
	g.Append(allMail, &gmailtest.Message{Body: body("in inbox", "1@example.com", ""), Labels: []string{`\Inbox`, `\Important`, "Work", "Été"}, MsgID: 101, ThrID: 900})
	g.Append(allMail, &gmailtest.Message{Body: body("archived", "2@example.com", ""), Labels: []string{"Receipts"}, MsgID: 102, ThrID: 901})
	// No References to message 1, but the same X-GM-THRID: threaded by the hint.
	g.Append(allMail, &gmailtest.Message{Body: body("Re: in inbox (no refs)", "3@example.com", ""), Labels: []string{`\Sent`}, MsgID: 103, ThrID: 900})
	g.Append("[Gmail]/Spam", &gmailtest.Message{Body: body("spam", "4@example.com", ""), MsgID: 104, ThrID: 902})
	// A draft whose second label decodes to a NUL ("&AAA-x" -> "\x00x"):
	// before the review fix PostgreSQL refused it and the message failed to
	// ingest on every pass.
	g.Append(allMail, &gmailtest.Message{Body: body("a draft", "5@example.com", ""), WireLabels: `\Draft "&AAA-x"`, MsgID: 105, ThrID: 903})

	clock := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	e := New(NewStoreAdapter(st), ingest.New(st, mustBlobs(t)), sync.Dial,
		WithClock(func() time.Time { return clock }), WithReapInterval(time.Nanosecond))
	req := AccountSync{UserID: userID, AccountID: accountID, Config: g.Listen(sync.TLSInsecure), DEK: randomKey(t), DEKID: ulid.Make(),
		Gmail: &GmailSync{DailyBudgetBytes: 10_000_000, LabelSweepInterval: time.Hour}}
	sum, err := e.SyncAccount(ctx, req)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(sum.Errors) != 0 {
		t.Fatalf("errors: %v", sum.Errors)
	}
	if sum.MessagesIngested != 5 {
		t.Errorf("ingested %d, want 5", sum.MessagesIngested)
	}
	for _, sel := range g.Selects() {
		if !allowedSelect(sel) {
			t.Errorf("selected label folder %q", sel)
		}
	}

	// Membership by the JMAP derivation: where does each subject show?
	where := func(subject string) []string {
		rows, err := super.Query(ctx, `
			SELECT mb.imap_name FROM app.messages m
			  JOIN app.mailboxes mb ON mb.id = COALESCE(m.moved_mailbox_id,
			       (SELECT p.mailbox_id FROM app.message_provenance p WHERE p.message_id = m.id AND p.state = 'live' LIMIT 1))
			 WHERE m.user_id = $1 AND m.subject = $2`, userID.String(), subject)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var n string
			_ = rows.Scan(&n)
			out = append(out, n)
		}
		return out
	}
	for subj, want := range map[string]string{
		"in inbox": "INBOX", "archived": allMail, "Re: in inbox (no refs)": "[Gmail]/Sent Mail", "spam": "[Gmail]/Spam",
		"a draft": "[Gmail]/Drafts",
	} {
		if got := where(subj); len(got) != 1 || got[0] != want {
			t.Errorf("%q is in %v, want %s", subj, got, want)
		}
	}

	labels := func(subject string) []string {
		rows, err := super.Query(ctx, `
			SELECT l.name || ':' || ml.applied_by FROM app.message_labels ml
			  JOIN app.labels l ON l.id = ml.label_id JOIN app.messages m ON m.id = ml.message_id
			 WHERE m.user_id = $1 AND m.subject = $2`, userID.String(), subject)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var n string
			_ = rows.Scan(&n)
			out = append(out, n)
		}
		// Sorted HERE, by byte order, not by the database: ORDER BY follows
		// the database collation, which put "Été" after "Work" under C and
		// before it under en_US.UTF-8 (CI's), so the test passed on one and
		// failed on the other (#907's first CI run).
		sort.Strings(out)
		return out
	}
	if got := labels("in inbox"); len(got) != 2 || got[0] != "Work:user" || got[1] != "Été:user" {
		t.Errorf("labels = %v", got)
	}

	var t1, t3 string
	_ = super.QueryRow(ctx, `SELECT thread_id FROM app.messages WHERE user_id = $1 AND subject = 'in inbox'`, userID.String()).Scan(&t1)
	_ = super.QueryRow(ctx, `SELECT thread_id FROM app.messages WHERE user_id = $1 AND subject = 'Re: in inbox (no refs)'`, userID.String()).Scan(&t3)
	if t1 == "" || t1 != t3 {
		t.Errorf("X-GM-THRID hint did not thread the reply: %q vs %q", t1, t3)
	}

	// A label change made in Gmail's own apps: archive "in inbox" and drop Été.
	g.SetLabels(allMail, 1, "Work")
	clock = clock.Add(time.Minute)
	if _, err := e.SyncAccount(ctx, req); err != nil {
		t.Fatal(err)
	}
	if got := where("in inbox"); len(got) != 1 || got[0] != allMail {
		t.Errorf("after archiving in Gmail: %v", got)
	}
	if got := labels("in inbox"); len(got) != 1 || got[0] != "Work:user" {
		t.Errorf("after removing a label in Gmail: %v", got)
	}

	// Every visible change carries its change_log row; no writeback was queued.
	var changes, intents int
	_ = super.QueryRow(ctx, `SELECT count(*) FROM app.change_log c JOIN app.messages m ON m.id = c.object_id
	                          WHERE m.user_id = $1 AND m.subject = 'in inbox' AND c.object_type = 'Email'`, userID.String()).Scan(&changes)
	_ = super.QueryRow(ctx, `SELECT count(*) FROM app.writeback_intents WHERE user_id = $1`, userID.String()).Scan(&intents)
	if changes < 3 || intents != 0 {
		t.Errorf("email change rows = %d (want >= 3: created, labelled, relabelled), writeback intents = %d", changes, intents)
	}

	// The meter recorded what the server served.
	served, _ := g.BodyBytesServed()
	var metered int64
	if err := super.QueryRow(ctx, `SELECT COALESCE((SELECT sum(x) FROM unnest(download_hours) x), 0)::bigint
	                                 FROM app.account_sync_status WHERE account_id = $1`, accountID.String()).Scan(&metered); err != nil {
		t.Fatal(err)
	}
	if metered != served || served == 0 {
		t.Errorf("metered %d bytes, the server served %d", metered, served)
	}
	var floors []int64
	rows, _ := super.Query(ctx, `SELECT backfill_floor_uid FROM app.mailboxes WHERE account_id = $1 AND backfill_floor_uid IS NOT NULL`, accountID.String())
	for rows.Next() {
		var f int64
		_ = rows.Scan(&f)
		floors = append(floors, f)
	}
	rows.Close()
	sort.Slice(floors, func(i, j int) bool { return floors[i] < floors[j] })
	if len(floors) != 3 {
		t.Errorf("floors = %v, want one per synced mailbox", floors)
	}

	if got := labels("a draft"); len(got) != 1 || got[0] != "x:user" {
		t.Errorf("the NUL label was not sanitised: %q", got)
	}

	// NO GHOST DRAFT (#52 review H3): the draft is sent or discarded in Gmail,
	// so its All Mail copy is expunged and it appears nowhere else. It must
	// leave Drafts, not sit there forever on its placement.
	g.Expunge(allMail, 4)
	// Well past the observation cadence, which the store stamps in real time.
	clock = time.Now().Add(48 * time.Hour)
	sum, err = e.SyncAccount(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.Errors) != 0 || sum.MailboxesReaped == 0 {
		t.Errorf("the reap pass: errors=%v reaped=%d vanished=%d", sum.Errors, sum.MailboxesReaped, sum.MessagesVanished)
	}
	if got := where("a draft"); len(got) != 0 {
		var moved *string
		_ = super.QueryRow(ctx, `SELECT moved_mailbox_id FROM app.messages WHERE user_id = $1 AND subject = 'a draft'`, userID.String()).Scan(&moved)
		t.Errorf("the expunged draft still shows in %v (moved_mailbox_id %v) — a ghost", got, moved)
	}
	var vanished int
	_ = super.QueryRow(ctx, `SELECT count(*) FROM app.messages WHERE user_id = $1 AND subject = 'a draft' AND vanished_at IS NOT NULL`, userID.String()).Scan(&vanished)
	if vanished != 1 {
		t.Errorf("the expunged draft is not vanished")
	}
}
