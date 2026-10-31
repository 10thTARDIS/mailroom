// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package mailsync

// #740 end to end, against a real Dovecot, through the PRODUCTION writeback
// path (writeback.Runner, which dials its own session exactly as the sync
// runner's pre-sync hook does):
//
//  1. before Mailroom has written anywhere, every mailbox's accepts_keywords is
//     the honest default, false — the sync pass's EXAMINE cannot say otherwise
//     and no longer tries;
//  2. the first rescue out of Junk SELECTs Junk read-write to take $Junk off,
//     and that SELECT's PERMANENTFLAGS `\*` is recorded: Junk becomes true;
//  3. the sync passes that follow EXAMINE Junk again and do NOT downgrade it;
//  4. so the NEXT rescue's $NotJunk is enqueued and actually reaches the server.
//
// The first rescue sends no $NotJunk — the probe is learned by writing, and a
// mailbox Mailroom has never written to is not assumed to accept keywords. That
// is the design, asserted here rather than left to be discovered.

import (
	"context"
	"testing"
	"time"

	"mailroom/internal/store"
	"mailroom/internal/sync"
	"mailroom/internal/writeback"
)

func (f *liveFixture) acceptsKeywords(t *testing.T) map[string]bool {
	t.Helper()
	rows, err := f.super.Query(context.Background(),
		`SELECT imap_name, accepts_keywords FROM app.mailboxes WHERE user_id = $1 ORDER BY imap_name`,
		f.userID.String())
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var name string
		var accepts bool
		if err := rows.Scan(&name, &accepts); err != nil {
			t.Fatal(err)
		}
		out[name] = accepts
	}
	return out
}

// notJunkIntents counts the +kw $NotJunk intents ever enqueued for the user, by
// state, so "sent" is read from the queue and not inferred.
func (f *liveFixture) notJunkIntents(t *testing.T) map[string]int {
	t.Helper()
	rows, err := f.super.Query(context.Background(),
		`SELECT state::text, count(*) FROM app.writeback_intents
		  WHERE user_id = $1 AND op = '+kw' AND payload->>'keyword' = $2
		  GROUP BY state`, f.userID.String(), store.KeywordNotJunk)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var s string
		var n int
		if err := rows.Scan(&s, &n); err != nil {
			t.Fatal(err)
		}
		out[s] = n
	}
	return out
}

func TestLiveKeywordProbeIsLearnedByWritingAndUsed(t *testing.T) {
	ctx := context.Background()
	f := newLiveFixture(t) // the shipping reap cadence
	_, uids, err := f.sess.LiveUIDs(ctx, liveJunk)
	if err != nil {
		t.Fatalf("Junk: %v (is it missing? scripts/mailsync-test.sh creates it)", err)
	}
	for _, uid := range uids {
		f.expungeOnServer(t, liveJunk, uid)
	}
	runner := writeback.NewRunner(f.st, func(ctx context.Context, cfg sync.Config) (sync.SyncSession, error) {
		return sync.Dial(ctx, cfg)
	}, nil, nil)

	junkMsg := func(subject string) {
		t.Helper()
		body := []byte("From: sender@example.com\r\nTo: " + f.userID.String() + "@example.com\r\n" +
			"Subject: " + subject + "\r\nMessage-ID: <" + subject + "@example.com>\r\n" +
			"Date: Mon, 6 Jan 2025 10:00:00 +0000\r\n\r\nmisjudged\r\n")
		if _, err := f.sess.Append(ctx, liveJunk, []sync.Flag{"$Junk"}, time.Time{}, body); err != nil {
			t.Fatalf("append %s: %v", subject, err)
		}
	}
	rescue := func(subject string) {
		t.Helper()
		msg, ok := f.messageIDBySubject(t, subject)
		if !ok {
			t.Fatalf("%s was never ingested", subject)
		}
		no := false
		if err := f.st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
			_, _, err := tx.ApplyEmailUpdate(ctx, msg, store.EmailUpdate{Junk: &no, MoveToRole: "inbox"})
			return err
		}); err != nil {
			t.Fatalf("rescue %s: %v", subject, err)
		}
		res, err := runner.DrainAccount(ctx, f.userID, f.accountID, f.cfg)
		if err != nil || res.DeadLettered != 0 {
			t.Fatalf("drain after rescuing %s = %+v, %v", subject, res, err)
		}
	}

	// 1. THE REPORT, as the code now stands, before any write: every mailbox
	// the sync pass has seen, and what it says. Logged in full, asserted false.
	junkMsg("probe-740-first")
	f.syncOnce(t)
	before := f.acceptsKeywords(t)
	t.Logf("accepts_keywords before any write (%d mailboxes): %v", len(before), before)
	if len(before) == 0 {
		t.Fatal("no mailboxes recorded — the report below would be about nothing")
	}
	for name, accepts := range before {
		if accepts {
			t.Errorf("%s accepts_keywords = true before Mailroom wrote to it: an EXAMINE cannot know that", name)
		}
	}

	// 2. The first rescue writes in Junk, and so learns it. Errorf, here and in
	// 3, so a broken probe still reaches step 4 and shows what it costs.
	rescue("probe-740-first")
	if got := f.acceptsKeywords(t)[liveJunk]; !got {
		t.Errorf("after a write in Junk, accepts_keywords = false: the read-write SELECT's probe was not recorded")
	}
	if n := f.notJunkIntents(t); len(n) != 0 {
		t.Errorf("the FIRST rescue enqueued $NotJunk (%v) before anything had probed Junk", n)
	}

	// 3. Sync passes EXAMINE Junk again; the probe must survive them.
	f.syncOnce(t)
	junkMsg("probe-740-second")
	f.syncOnce(t)
	if got := f.acceptsKeywords(t)[liveJunk]; !got {
		t.Error("a sync pass downgraded Junk's accepts_keywords to false: an EXAMINE overwrote the read-write probe")
	}

	// 4. The next rescue's $NotJunk is enqueued AND applied, and the server has it.
	rescue("probe-740-second")
	if n := f.notJunkIntents(t); n["applied"] != 1 || len(n) != 1 {
		t.Errorf("$NotJunk intents by state = %v, want exactly one, applied", n)
	}
	var withNotJunk int
	for _, fl := range f.serverFlags(t, f.inbox) {
		if hasFlag(fl, "$NotJunk") {
			withNotJunk++
		}
	}
	if withNotJunk != 1 {
		t.Errorf("INBOX copies carrying $NotJunk = %d, want 1 (the second rescue only)", withNotJunk)
	}
}
