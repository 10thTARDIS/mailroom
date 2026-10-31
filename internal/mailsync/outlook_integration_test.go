// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package mailsync

// THE MICROSOFT PROFILE END TO END (ADR-046 §12): the real engine, ingest and
// store over PostgreSQL, syncing an Outlook-shaped scripted server — no
// CONDSTORE, no SPECIAL-USE, Exchange's English folder names — through the
// real sync.Dial.

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oklog/ulid/v2"

	"mailroom/internal/ingest"
	"mailroom/internal/store"
	"mailroom/internal/sync"
	"mailroom/internal/sync/gmailtest"
)

func TestLiveOutlookProfileFoldersAndFlagSweep(t *testing.T) {
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
		{`INSERT INTO app.accounts (id, user_id, name, provider_profile, imap_host, smtp_host, credentials_ct, credentials_dek)
		  VALUES ($1, $2, 'outlook', 'microsoft', 'outlook.office365.com', 'smtp.office365.com', '\x00', $3)`,
			[]any{accountID.String(), userID.String(), ulid.Make().String()}},
	} {
		if _, err := super.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	g := gmailtest.New(t)
	g.GmailExt, g.CondStore = false, false
	for _, n := range []string{"INBOX", "Sent Items", "Deleted Items", "Junk Email", "Drafts", "Archive", "Calendar", "Notes"} {
		g.AddMailbox(&gmailtest.Mailbox{Name: n, UIDValidity: 7, Attrs: []string{`\HasNoChildren`}})
	}
	msg := func(subj string) []byte {
		return []byte("From: a@example.com\r\nTo: you@example.com\r\nSubject: " + subj +
			"\r\nMessage-ID: <" + subj + "@example.com>\r\nDate: Mon, 26 Oct 2026 10:00:00 +0000\r\n\r\nx\r\n")
	}
	g.Append("INBOX", &gmailtest.Message{Body: msg("unread-then-read"), Flags: []string{}})
	g.Append("INBOX", &gmailtest.Message{Body: msg("read"), Flags: []string{`\Seen`}})
	// Calendar is empty (Exchange's calendar store over IMAP); "Notes" is a
	// non-mail NAME holding real mail (review M3 of #910): it must keep syncing.
	g.Append("Notes", &gmailtest.Message{Body: msg("note-to-self")})

	clock := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	sweep := FlagSweepConfig{Interval: 5 * time.Minute, Window: 500, FullInterval: 6 * time.Hour, FullChunk: 2000}
	e := New(NewStoreAdapter(st), ingest.New(st, mustBlobs(t)), sync.Dial,
		WithClock(func() time.Time { return clock }), WithFlagSweep(sweep))
	req := AccountSync{UserID: userID, AccountID: accountID, Config: g.Listen(sync.TLSInsecure),
		DEK: randomKey(t), DEKID: ulid.Make(), ProviderProfile: "microsoft"}

	sum, err := e.SyncAccount(ctx, req)
	if err != nil || len(sum.Errors) != 0 {
		t.Fatalf("first sync: %v %v", err, sum.Errors)
	}
	// Roles from the profile's name map (SPECIAL-USE absent).
	roles := map[string]string{}
	rows, err := super.Query(ctx, `SELECT imap_name, special_use, sync_scope FROM app.mailboxes WHERE account_id = $1`, accountID.String())
	if err != nil {
		t.Fatal(err)
	}
	scopes := map[string]string{}
	for rows.Next() {
		var name, scope string
		var su *string
		if err := rows.Scan(&name, &su, &scope); err != nil {
			t.Fatal(err)
		}
		if su != nil {
			roles[name] = *su
		}
		scopes[name] = scope
	}
	rows.Close()
	for name, want := range map[string]string{"Sent Items": `\Sent`, "Deleted Items": `\Trash`,
		"Junk Email": `\Junk`, "Drafts": `\Drafts`, "Archive": `\Archive`} {
		if roles[name] != want {
			t.Errorf("%s special_use = %q, want %q", name, roles[name], want)
		}
	}
	if scopes["Calendar"] != "off" {
		t.Errorf("Calendar sync_scope = %q, want off", scopes["Calendar"])
	}
	if scopes["Notes"] != "full" {
		t.Errorf("Notes (holding mail) sync_scope = %q, want full: a name alone switched it off", scopes["Notes"])
	}
	// Calendar's one SELECT (which saw it empty) was the evidence; no later
	// pass selects it.
	g.ResetCounters()

	seen := func(subj string) bool {
		var v bool
		if err := super.QueryRow(ctx, `SELECT is_seen FROM app.messages WHERE user_id = $1 AND subject = $2`,
			userID.String(), subj).Scan(&v); err != nil {
			t.Fatalf("read %s: %v", subj, err)
		}
		return v
	}
	if seen("unread-then-read") || !seen("read") {
		t.Fatal("initial flags not as on the server")
	}

	// Read on the phone (no MODSEQ exists to report it), and marked unread.
	g.SetFlagsQuietly("INBOX", 1, `\Seen`)
	g.SetFlagsQuietly("INBOX", 2)
	changes0 := countChanges(t, super, userID)
	clock = clock.Add(6 * time.Minute)
	if sum, err = e.SyncAccount(ctx, req); err != nil || len(sum.Errors) != 0 {
		t.Fatalf("second sync: %v %v", err, sum.Errors)
	}
	for _, s := range g.Selects() {
		if s == "Calendar" {
			t.Error("the not-synced Calendar was selected again")
		}
	}
	if !seen("unread-then-read") || seen("read") {
		t.Fatalf("the sweep did not apply the phone's changes (read=%v unread=%v)", seen("unread-then-read"), !seen("read"))
	}
	if sum.FlagSweepApplied != 2 {
		t.Errorf("FlagSweepApplied = %d, want 2", sum.FlagSweepApplied)
	}
	if n := countChanges(t, super, userID) - changes0; n != 2 {
		t.Errorf("Email change rows = %d, want 2 (one per changed message)", n)
	}

	// Nothing changed: the next sweep writes nothing at all.
	changes0 = countChanges(t, super, userID)
	clock = clock.Add(6 * time.Minute)
	if sum, err = e.SyncAccount(ctx, req); err != nil {
		t.Fatal(err)
	}
	if sum.FlagSweepApplied != 0 || countChanges(t, super, userID) != changes0 {
		t.Errorf("an unchanged sweep applied %d / appended change rows", sum.FlagSweepApplied)
	}

	// Review L6 of #910, through the real store's SQL: a message with a flag
	// writeback still pending is left alone, even though the server's reading
	// differs; once the intent is applied the next sweep converges.
	var msgID string
	if err := super.QueryRow(ctx, `SELECT id FROM app.messages WHERE user_id = $1 AND subject = 'read'`,
		userID.String()).Scan(&msgID); err != nil {
		t.Fatal(err)
	}
	intent := ulid.Make().String()
	if _, err := super.Exec(ctx, `INSERT INTO app.writeback_intents (id, user_id, message_id, account_id, op, idempotency_key, state)
		VALUES ($1, $2, $3, $4, '+flag', $5, 'pending')`, intent, userID.String(), msgID, accountID.String(), "sweep-test-"+intent); err != nil {
		t.Fatal(err)
	}
	g.SetFlagsQuietly("INBOX", 2, `\Seen`)
	clock = clock.Add(6 * time.Minute)
	if sum, err = e.SyncAccount(ctx, req); err != nil {
		t.Fatal(err)
	}
	if sum.FlagSweepDeferred != 1 || seen("read") {
		t.Fatalf("pending writeback: deferred=%d seen=%v, want the copy left alone", sum.FlagSweepDeferred, seen("read"))
	}
	if _, err := super.Exec(ctx, `UPDATE app.writeback_intents SET state = 'applied' WHERE id = $1`, intent); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(6 * time.Minute)
	if sum, err = e.SyncAccount(ctx, req); err != nil {
		t.Fatal(err)
	}
	if !seen("read") {
		t.Error("after the writeback was applied the sweep did not converge")
	}
}

func countChanges(t *testing.T, super *pgxpool.Pool, userID ulid.ULID) int {
	t.Helper()
	var n int
	if err := super.QueryRow(context.Background(),
		`SELECT count(*) FROM app.change_log WHERE user_id = $1 AND object_type = 'Email'`, userID.String()).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
