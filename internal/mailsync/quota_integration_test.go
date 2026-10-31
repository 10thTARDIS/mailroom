// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

// Live end-to-end coverage for the MID-SYNC STORAGE-QUOTA PAUSE (mailroom#97,
// spec §10). This is the mail-loss-adjacent enforcement point — the same family
// as the reaper (#86), the UIDVALIDITY roll (#88) and the gap backfill (#93) — so
// it is proven against a real Dovecot and a real RLS-enforced Postgres, exactly
// like the reaper suite, with no doubles.
//
// THE PROPERTY: when a message the provider still holds does not fit under the
// user's storage cap, sync must PAUSE and leave that mail on the server, never
// silently drop it. "No room" is an explicit, retryable failure — mail stays put,
// cursors do not advance, the reaper does not run — so that once the user frees
// space (or the operator raises the cap) the very next pass ingests it. A silent
// drop here would lose real mail forever.
//
// Run via scripts/mailsync-test.sh, which brings up both servers. Without them,
// this skips like the rest of the suite.
package mailsync

import (
	"context"
	"errors"
	"testing"
	"time"

	"mailroom/internal/ingest"
)

// setQuota sets the fixture user's storage cap (app.users.quota_storage_bytes).
// A cap of 1 byte guarantees the next message does not fit (a 0 cap would read as
// "unlimited" — store.QuotaAdmits — which is the opposite of what we want here).
func (f *liveFixture) setQuota(t *testing.T, limit int64) {
	t.Helper()
	if _, err := f.super.Exec(context.Background(),
		`UPDATE app.users SET quota_storage_bytes = $2 WHERE id = $1`,
		f.userID.String(), limit); err != nil {
		t.Fatalf("set quota: %v", err)
	}
}

// emailChangeCount counts Email change_log rows for a canonical message id — the
// stream that drives /changes and the search index. Zero means the message is, to
// every JMAP-visible surface, as if never seen.
func (f *liveFixture) emailChangeCountBySubject(t *testing.T, subject string) int {
	t.Helper()
	var n int
	if err := f.super.QueryRow(context.Background(),
		`SELECT count(*) FROM app.change_log cl
		   JOIN app.messages m ON m.id = cl.object_id AND m.user_id = cl.user_id
		  WHERE cl.user_id = $1 AND cl.object_type = 'Email' AND m.subject = $2`,
		f.userID.String(), subject).Scan(&n); err != nil {
		t.Fatalf("email change count: %v", err)
	}
	return n
}

// onServer reports whether a subject's message is still present on the Dovecot
// server (by scanning the live UID set and its envelopes is overkill; the reaper
// suite proves what LiveUIDs returns, so a non-empty INBOX with the expected count
// is the honest check here).
func (f *liveFixture) inboxUIDCount(t *testing.T) int {
	t.Helper()
	_, uids, err := f.sess.LiveUIDs(context.Background(), f.inbox)
	if err != nil {
		t.Fatalf("live uids: %v", err)
	}
	return len(uids)
}

// THE BUG THIS ISSUE EXISTS FOR (the pause half of #97). A message that will not
// fit under the storage cap must PAUSE the account and stay on the server — never
// be silently dropped — and must be ingested on a later pass once there is room.
func TestLiveQuotaExhaustionPausesAndKeepsMailOnServer(t *testing.T) {
	f := newLiveFixture(t, WithReapInterval(time.Nanosecond)) // reaper always due
	const subject = "no-room-for-me"
	f.appendMsg(t, f.inbox, subject)

	// One byte of quota: the message cannot possibly fit.
	f.setQuota(t, 1)

	// The sync pass FAILS: SyncAccount returns non-nil, and the error is the quota
	// pause sentinel — which is exactly what the runner turns into a raised account
	// health ("paused: over storage quota") rather than the SUCCESS a nil return
	// would record. We assert at the engine boundary (SyncAccount), the seam the
	// runner keys on; the runner->#43 health wiring is unit-tested in syncrunner.
	sum, err := f.engine.SyncAccount(context.Background(), AccountSync{
		UserID: f.userID, AccountID: f.accountID, Config: f.cfg, DEK: f.dek, DEKID: f.dekID,
	})
	if err == nil {
		t.Fatalf("SyncAccount returned nil across a quota exhaustion — the pause would be recorded as SUCCESS and hidden; summary=%+v", sum)
	}
	if !errors.Is(err, ingest.ErrQuotaExhausted) {
		t.Fatalf("SyncAccount error = %v, want it to wrap ingest.ErrQuotaExhausted so the runner records the quota PAUSE", err)
	}

	// (a) NOTHING was ingested: no canonical row, no live provenance, no change_log.
	if _, ok := f.messageIDBySubject(t, subject); ok {
		t.Error("the over-quota message was ingested — a quota pause must store NOTHING (mail-loss ban)")
	}
	if n := f.liveProvenanceCount(t); n != 0 {
		t.Errorf("live provenance rows = %d, want 0 — the paused message must leave no provenance", n)
	}
	if n := f.emailChangeCountBySubject(t, subject); n != 0 {
		t.Errorf("Email change_log rows for the paused message = %d, want 0", n)
	}

	// (b) The mail is STILL on the server — untouched, un-expunged. The ingest path
	// is a read-only FETCH and the pause adds no IMAP write, so the message the user
	// still has on their provider is exactly where it was.
	if n := f.inboxUIDCount(t); n != 1 {
		t.Fatalf("INBOX has %d messages on the server, want the paused message still sitting there (1)", n)
	}

	// (d) Cursors did NOT advance: raise the cap and the VERY NEXT pass ingests the
	// message. If the pause had advanced the cursor past this UID, no later poll
	// would ever ask for it again and the mail would be lost forever.
	f.setQuota(t, 100<<20)
	sum2 := f.syncOnce(t)
	if sum2.MessagesIngested < 1 {
		t.Fatalf("after raising the cap, MessagesIngested = %d, want the previously-paused message ingested (cursors must not have advanced)", sum2.MessagesIngested)
	}
	msg, ok := f.messageIDBySubject(t, subject)
	if !ok {
		t.Fatal("the previously-paused message was never ingested after the cap was raised — cursors advanced past it, i.e. the mail was lost")
	}
	inboxID := f.mailboxID(t, f.inbox)
	if got := f.mailboxIDs(t, msg); len(got) != 1 || got[0] != inboxID.String() {
		t.Errorf("after recovery the message is in %v, want exactly the inbox", got)
	}
	if n := f.emailChangeCountBySubject(t, subject); n == 0 {
		t.Error("no Email change_log row after recovery — /changes and the index would never learn about the message")
	}
	// And it is still on the server, still exactly once — the pause never touched it.
	if n := f.inboxUIDCount(t); n != 1 {
		t.Errorf("INBOX server count = %d after recovery, want 1 (the pause+recovery must not duplicate or drop it)", n)
	}
}
