// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

// Package mailsync's live end-to-end coverage for the UIDVALIDITY roll (#88).
//
// A UIDVALIDITY roll is what an IMAP server does when a mailbox's UID space is no
// longer the one it was: the folder was deleted and recreated, restored from
// backup, or migrated. Every UID we hold for it becomes meaningless. Mailroom
// retires the old generation and re-fetches the mailbox from UID 1 — and, until
// #88, a message that did not come back was simply left holding retired rows,
// which are invisible to membership, to Email/query, to mailboxIds and to the
// mailbox counts, with trashed_at NULL and not one change_log row anywhere. It
// stopped existing and nobody was told. Mail that vanishes with no trace is the
// failure that destroys trust in a mail client, so this is proved against a REAL
// SERVER, not a double: the roll is performed on Dovecot by deleting and
// recreating the folder, exactly as an operator or another client would.
//
// Run via scripts/mailsync-test.sh. Without a live Dovecot + PostgreSQL, every
// test here skips (newLiveFixture).
package mailsync

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/oklog/ulid/v2"

	"mailroom/internal/ingest"
	"mailroom/internal/store"
	"mailroom/internal/sync"
)

// ---- server-side surgery ----------------------------------------------------

// adminClient opens a raw go-imap connection to the fixture's server. Mailroom's
// own SyncSession deliberately cannot CREATE or DELETE a mailbox (it is a sync
// client, not a mailbox manager), so the harness reaches past it to do what only
// an operator or another client can. This is the only place any test drives
// go-imap directly, and it is test-only by construction.
func (f *liveFixture) adminClient(t *testing.T) *imapclient.Client {
	t.Helper()
	c, err := imapclient.DialInsecure(fmt.Sprintf("%s:%d", f.cfg.Host, f.cfg.Port), nil)
	if err != nil {
		t.Fatalf("admin dial: %v", err)
	}
	if err := c.Login(f.cfg.Username, f.cfg.Password).Wait(); err != nil {
		_ = c.Close()
		t.Fatalf("admin login: %v", err)
	}
	t.Cleanup(func() {
		_ = c.Logout().Wait()
		_ = c.Close()
	})
	return c
}

// rollUIDValidity recreates a mailbox on the server, which is how a real IMAP
// server's UIDVALIDITY moves: the new folder is a new UID space. It returns the
// old and new UIDVALIDITY, and fails if they are equal — a test that silently
// failed to roll would "pass" while proving nothing.
func (f *liveFixture) rollUIDValidity(t *testing.T, mailbox string) (before, after uint32) {
	t.Helper()
	ctx := context.Background()

	st, _, err := f.sess.LiveUIDs(ctx, mailbox)
	if err != nil {
		t.Fatalf("read UIDVALIDITY of %q: %v", mailbox, err)
	}
	before = st.UIDValidity

	c := f.adminClient(t)
	if err := c.Delete(mailbox).Wait(); err != nil {
		t.Fatalf("delete %q: %v", mailbox, err)
	}
	if err := c.Create(mailbox, nil).Wait(); err != nil {
		t.Fatalf("recreate %q: %v", mailbox, err)
	}

	st, _, err = f.sess.LiveUIDs(ctx, mailbox)
	if err != nil {
		t.Fatalf("read new UIDVALIDITY of %q: %v", mailbox, err)
	}
	after = st.UIDValidity
	if after == before {
		t.Fatalf("recreating %q did not move UIDVALIDITY (still %d) — this test proves nothing", mailbox, before)
	}
	return before, after
}

// storedUIDValidity is the generation app.mailboxes currently believes it is on.
// It is how these tests check that an UNFINISHED roll is still pending: the cursor
// must stay on the OLD generation until the reconcile actually lands.
func (f *liveFixture) storedUIDValidity(t *testing.T, imapName string) int64 {
	t.Helper()
	var v int64
	if err := f.super.QueryRow(context.Background(),
		`SELECT uidvalidity FROM app.mailboxes WHERE user_id = $1 AND imap_name = $2`,
		f.userID.String(), imapName).Scan(&v); err != nil {
		t.Fatalf("stored uidvalidity: %v", err)
	}
	return v
}

// staleProvenance counts a mailbox's retired provenance rows — the outstanding
// work of a roll. Zero means the roll has been reconciled.
func (f *liveFixture) staleProvenance(t *testing.T, imapName string) int {
	t.Helper()
	var n int
	if err := f.super.QueryRow(context.Background(),
		`SELECT count(*) FROM app.message_provenance p
		   JOIN app.mailboxes m ON m.id = p.mailbox_id AND m.user_id = p.user_id
		  WHERE p.user_id = $1 AND m.imap_name = $2 AND p.state = 'stale_uidvalidity'`,
		f.userID.String(), imapName).Scan(&n); err != nil {
		t.Fatalf("stale provenance: %v", err)
	}
	return n
}

// emailChanges counts a message's Email change_log rows — what /changes and the
// search indexer actually consume.
func (f *liveFixture) emailChanges(t *testing.T, msg ulid.ULID) int {
	t.Helper()
	var n int
	if err := f.super.QueryRow(context.Background(),
		`SELECT count(*) FROM app.change_log
		  WHERE user_id = $1 AND object_type = 'Email' AND object_id = $2`,
		f.userID.String(), msg.String()).Scan(&n); err != nil {
		t.Fatalf("change count: %v", err)
	}
	return n
}

// trashSnapshot is the deletion lifecycle of one message (ADR-009).
type trashSnapshot struct {
	trashed    bool
	vanished   bool
	purgeAfter *time.Time
}

func (f *liveFixture) trashState(t *testing.T, msg ulid.ULID) trashSnapshot {
	t.Helper()
	var s trashSnapshot
	if err := f.super.QueryRow(context.Background(),
		`SELECT trashed_at IS NOT NULL, vanished_at IS NOT NULL, purge_after
		   FROM app.messages WHERE user_id = $1 AND id = $2`,
		f.userID.String(), msg.String()).Scan(&s.trashed, &s.vanished, &s.purgeAfter); err != nil {
		t.Fatalf("trash state: %v", err)
	}
	return s
}

// ---- tests ------------------------------------------------------------------

// THE BUG #88 EXISTS FOR. The Archive folder is recreated on the server. One
// message is put back into it; the other is not. The one that is not must NOT
// silently cease to exist: it leaves the folder, it is soft-deleted (retractably,
// ADR-009), and the change stream carries exactly one row saying so. The one that
// IS back is untouched.
func TestLiveUIDValidityRollSoftDeletesTheOrphanAndAnnouncesIt(t *testing.T) {
	f := newLiveFixture(t, WithReapInterval(time.Nanosecond))

	f.appendMsg(t, f.archive, "roll-survivor")
	f.appendMsg(t, f.archive, "roll-casualty")
	f.syncOnce(t)

	archiveID := f.mailboxID(t, f.archive)
	survivor, ok := f.messageIDBySubject(t, "roll-survivor")
	if !ok {
		t.Fatal("roll-survivor was never ingested")
	}
	casualty, ok := f.messageIDBySubject(t, "roll-casualty")
	if !ok {
		t.Fatal("roll-casualty was never ingested")
	}
	if got := f.mailboxIDs(t, casualty); len(got) != 1 || got[0] != archiveID.String() {
		t.Fatalf("precondition: the casualty is not in the Mailroom Archive: %v", got)
	}
	beforeSurvivor := f.emailChanges(t, survivor)
	beforeCasualty := f.emailChanges(t, casualty)

	// THE ROLL. The folder is deleted and recreated on the server — a brand-new UID
	// space — and only the survivor is put back. (Same bytes, so content-hash dedup
	// lands it on the SAME canonical message: exactly what happens when a mailbox is
	// restored from a backup that was missing one message.)
	oldV, newV := f.rollUIDValidity(t, f.archive)
	t.Logf("UIDVALIDITY %d -> %d", oldV, newV)
	f.appendMsg(t, f.archive, "roll-survivor")

	sum := f.syncOnce(t)
	if len(sum.Errors) != 0 {
		t.Fatalf("sync errors: %v", sum.Errors)
	}
	if sum.GenerationsReconciled != 1 {
		t.Fatalf("GenerationsReconciled = %d, want 1 — the roll was never finished: %+v", sum.GenerationsReconciled, sum)
	}
	if sum.MessagesVanished != 1 {
		t.Errorf("MessagesVanished = %d, want 1", sum.MessagesVanished)
	}

	// The casualty: gone from every folder, honestly soft-deleted, and ANNOUNCED.
	if got := f.mailboxIDs(t, casualty); len(got) != 0 {
		t.Errorf("the orphaned message is still in %v", got)
	}
	if !f.trashed(t, casualty) {
		t.Fatal("THE ORPHANED MESSAGE WAS NOT SOFT-DELETED — it is in no folder at all, invisible, with no trace. That is #88.")
	}
	if !f.trashState(t, casualty).vanished {
		t.Error("no vanished_at: the soft-delete is not retractable, so a message that turns out to still be there could never come back")
	}
	// TWO change rows, not one — the #94 fix. Retiring the old generation
	// (MarkMailboxProvenanceStale) drops the casualty out of the folder the instant
	// it commits, so a client polling mid-roll must be told THEN (row 1); the
	// reconcile then soft-deletes it (row 2). The old behaviour emitted only the
	// second, so a client that polled between the retire and the reconcile saw a
	// briefly-empty folder with no /changes notification at all.
	if got := f.emailChanges(t, casualty) - beforeCasualty; got != 2 {
		t.Errorf("Email change_log rows for the orphan = %d, want 2 (left the folder at retire-time, soft-deleted at reconcile-time — #94)", got)
	}

	// The survivor: still in Archive, not deleted. It too gets two rows, and for the
	// same #94 reason: it LEAVES the folder when the generation is retired (row 1),
	// and RETURNS when the re-fetch re-ingests it under the new generation (row 2).
	// A client polling in the window between the two really did see it gone, so it
	// has to be told both that it left and that it came back — the accepted 2N burst.
	if got := f.mailboxIDs(t, survivor); len(got) != 1 || got[0] != archiveID.String() {
		t.Errorf("the surviving message is in %v, want {Archive}", got)
	}
	if f.trashed(t, survivor) {
		t.Fatal("SOFT-DELETED A MESSAGE THAT IS SITTING IN THE RECREATED FOLDER")
	}
	if got := f.emailChanges(t, survivor) - beforeSurvivor; got != 2 {
		t.Errorf("Email change_log rows for the survivor = %d, want 2 (left at retire-time, returned at re-fetch-time — #94)", got)
	}

	// Email/query agrees with Email/get — a message list and its folder counts
	// cannot disagree — and the roll left no unfinished business behind.
	if got := f.inMailbox(t, archiveID); len(got) != 1 || got[0] != survivor.String() {
		t.Errorf("inMailbox(Archive) = %v, want only the survivor", got)
	}
	if n := f.staleProvenance(t, f.archive); n != 0 {
		t.Errorf("%d retired provenance rows left behind; the roll was not finished", n)
	}
	if v := f.storedUIDValidity(t, f.archive); v != int64(newV) {
		t.Errorf("stored uidvalidity = %d, want %d — the cursor never adopted the new generation", v, newV)
	}
}

// A PARTIAL RE-FETCH SOFT-DELETES NOTHING. This is the mail-destroying case, and
// the reason "our fetch loop returned no error" is not allowed to count as proof:
// the server is entirely healthy and still holds both messages, but the FETCH
// silently brings back only one of them. Our provenance then looks EXACTLY as it
// would if the other had been deleted. Nothing may be concluded from it.
func TestLiveUIDValidityRollWithAPartialRefetchSoftDeletesNothing(t *testing.T) {
	f := newLiveFixture(t, WithReapInterval(time.Nanosecond))

	f.appendMsg(t, f.archive, "partial-a")
	f.appendMsg(t, f.archive, "partial-b")
	f.syncOnce(t)

	archiveID := f.mailboxID(t, f.archive)
	a, ok := f.messageIDBySubject(t, "partial-a")
	if !ok {
		t.Fatal("partial-a was never ingested")
	}
	b, ok := f.messageIDBySubject(t, "partial-b")
	if !ok {
		t.Fatal("partial-b was never ingested")
	}

	// Roll the folder and put BOTH messages back on the server. Nothing is gone.
	f.rollUIDValidity(t, f.archive)
	f.appendMsg(t, f.archive, "partial-a")
	lostUID := f.appendMsg(t, f.archive, "partial-b")

	// Now break the re-fetch, and only the re-fetch: the SELECT, the UID SEARCH and
	// the ingest all run against the real Dovecot, but the FETCH quietly drops one
	// message — a truncated response, a library bug, a half-read connection. It
	// returns no error, which is the entire point.
	lossy := New(NewStoreAdapter(f.st), ingest.New(f.st, f.blobs),
		func(ctx context.Context, cfg sync.Config) (sync.SyncSession, error) {
			s, err := sync.Dial(ctx, cfg)
			if err != nil {
				return nil, err
			}
			return &lossyFetch{SyncSession: s, drop: lostUID}, nil
		}, WithReapInterval(time.Nanosecond))

	sum, err := lossy.SyncAccount(context.Background(), AccountSync{
		UserID: f.userID, AccountID: f.accountID, Config: f.cfg, DEK: f.dek, DEKID: f.dekID,
	})
	if err != nil {
		t.Fatalf("SyncAccount: %v", err)
	}
	if len(sum.Errors) == 0 {
		t.Fatal("the incomplete re-fetch was not surfaced as an error")
	}
	if sum.GenerationsReconciled != 0 {
		t.Fatalf("RECONCILED A RETIRED GENERATION ON AN INCOMPLETE RE-FETCH: %+v", sum)
	}
	if sum.MessagesVanished != 0 {
		t.Errorf("MessagesVanished = %d, want 0", sum.MessagesVanished)
	}
	if f.trashed(t, b) {
		t.Fatal("SOFT-DELETED A MESSAGE BECAUSE OUR OWN FETCH MISSED IT — it is sitting on the server")
	}
	if f.trashed(t, a) {
		t.Fatal("soft-deleted the message the fetch DID bring back")
	}
	// The roll is still PENDING, not lost: the retired rows are still retired and
	// the cursor still names the old generation, so the next pass retries it.
	if n := f.staleProvenance(t, f.archive); n == 0 {
		t.Error("the retired rows were dropped by a reconcile that was refused")
	}

	// And a HEALTHY pass finishes it — which proves the refusal above was caused by
	// the injected fault and not by the messages simply being there.
	sum = f.syncOnce(t)
	if len(sum.Errors) != 0 {
		t.Fatalf("healthy sync errors: %v", sum.Errors)
	}
	if sum.GenerationsReconciled != 1 {
		t.Fatalf("the pending roll was never retried: %+v", sum)
	}
	if sum.MessagesVanished != 0 {
		t.Errorf("MessagesVanished = %d after a complete re-fetch of a folder that lost nothing, want 0", sum.MessagesVanished)
	}
	for name, id := range map[string]ulid.ULID{"partial-a": a, "partial-b": b} {
		if f.trashed(t, id) {
			t.Errorf("%s was soft-deleted even though it is on the server", name)
		}
		if got := f.mailboxIDs(t, id); len(got) != 1 || got[0] != archiveID.String() {
			t.Errorf("%s mailboxIds = %v, want {Archive}", name, got)
		}
	}
	if n := f.staleProvenance(t, f.archive); n != 0 {
		t.Errorf("%d retired provenance rows survived the successful reconcile", n)
	}
}

// A roll whose corroborating live-UID read FAILS reconciles nothing and — the
// part that makes it retryable rather than merely safe — leaves the mailbox's
// cursor on the OLD generation, so the next pass re-detects the roll. A roll
// "finished" without being finished would strand the orphans invisibly forever.
func TestLiveUIDValidityRollWithAFailedCorroborationStaysPending(t *testing.T) {
	f := newLiveFixture(t, WithReapInterval(time.Nanosecond))

	f.appendMsg(t, f.archive, "corroboration-casualty")
	f.syncOnce(t)
	casualty, ok := f.messageIDBySubject(t, "corroboration-casualty")
	if !ok {
		t.Fatal("message was never ingested")
	}

	oldV, _ := f.rollUIDValidity(t, f.archive) // recreated EMPTY: the message is gone

	broken := New(NewStoreAdapter(f.st), ingest.New(f.st, f.blobs),
		func(ctx context.Context, cfg sync.Config) (sync.SyncSession, error) {
			s, err := sync.Dial(ctx, cfg)
			if err != nil {
				return nil, err
			}
			return &brokenLiveUIDs{SyncSession: s, mailbox: f.archive}, nil
		}, WithReapInterval(time.Nanosecond))

	sum, err := broken.SyncAccount(context.Background(), AccountSync{
		UserID: f.userID, AccountID: f.accountID, Config: f.cfg, DEK: f.dek, DEKID: f.dekID,
	})
	if err != nil {
		t.Fatalf("SyncAccount: %v", err)
	}
	if len(sum.Errors) == 0 {
		t.Fatal("the failed corroboration was not surfaced")
	}
	if sum.GenerationsReconciled != 0 || sum.MessagesVanished != 0 {
		t.Fatalf("concluded something about a mailbox we could not ask about: %+v", sum)
	}
	if f.trashed(t, casualty) {
		t.Fatal("SOFT-DELETED MAIL BECAUSE WE COULD NOT ASK THE SERVER ABOUT IT")
	}
	if v := f.storedUIDValidity(t, f.archive); v != int64(oldV) {
		t.Fatalf("the cursor advanced to %d over an UNFINISHED roll (was %d) — the next pass will never re-detect it and the orphan stays invisible forever", v, oldV)
	}

	// The next healthy pass finishes what the broken one could not.
	sum = f.syncOnce(t)
	if sum.GenerationsReconciled != 1 {
		t.Fatalf("the pending roll was never retried: %+v (errors %v)", sum, sum.Errors)
	}
	if !f.trashed(t, casualty) {
		t.Error("the orphan was still not soft-deleted after a healthy pass")
	}
	if got := f.mailboxIDs(t, casualty); len(got) != 0 {
		t.Errorf("the orphan is still in %v", got)
	}
}

// A message the USER trashed is not the roll's business. Its trashed_at and its
// 30-day purge_after stay exactly as the user set them, and it never acquires
// vanished_at — which is what stops the IMAP Trash sync from resurrecting it
// (store.RetractVanished).
func TestLiveUIDValidityRollDoesNotDisturbAUserTrashedMessage(t *testing.T) {
	f := newLiveFixture(t, WithReapInterval(time.Nanosecond))
	ctx := context.Background()

	f.appendMsg(t, f.archive, "user-deleted-this")
	f.syncOnce(t)
	msg, ok := f.messageIDBySubject(t, "user-deleted-this")
	if !ok {
		t.Fatal("message was never ingested")
	}

	// The user hits Delete in Mailroom: Email/set trashes it canonically (ADR-009).
	if err := f.st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
		found, _, err := tx.ApplyEmailUpdate(ctx, msg, store.EmailUpdate{Trash: true})
		if err != nil {
			return err
		}
		if !found {
			t.Fatal("message not found for Email/set")
		}
		return nil
	}); err != nil {
		t.Fatalf("Email/set trash: %v", err)
	}
	before := f.trashState(t, msg)
	if !before.trashed || before.purgeAfter == nil {
		t.Fatalf("precondition: the message was not trashed: %+v", before)
	}
	changesBefore := f.emailChanges(t, msg)

	// Now the folder it came from is recreated empty. The roll must not touch it.
	f.rollUIDValidity(t, f.archive)

	sum := f.syncOnce(t)
	if len(sum.Errors) != 0 {
		t.Fatalf("sync errors: %v", sum.Errors)
	}
	if sum.GenerationsReconciled != 1 {
		t.Fatalf("the roll was not finished: %+v", sum)
	}
	if sum.MessagesVanished != 0 {
		t.Fatalf("RE-TRASHED A MESSAGE THE USER DELETED: %+v", sum)
	}

	after := f.trashState(t, msg)
	if !after.trashed {
		t.Fatal("THE USER'S DELETE WAS UNDONE BY A UIDVALIDITY ROLL")
	}
	if after.vanished {
		t.Fatal("STAMPED vanished_at ON A MESSAGE THE USER DELETED — syncing the IMAP Trash folder would now resurrect it")
	}
	if after.purgeAfter == nil || !after.purgeAfter.Equal(*before.purgeAfter) {
		t.Errorf("purge_after moved from %v to %v — the user's 30-day window was silently restarted",
			before.purgeAfter, after.purgeAfter)
	}
	// It is canonically placed in Trash (moved_mailbox_id), so provenance says
	// nothing a client can see and the roll owes it no change_log row at all.
	if got := f.emailChanges(t, msg) - changesBefore; got != 0 {
		t.Errorf("Email change_log rows = %d, want 0 — nothing a client can observe changed", got)
	}
}

// ---- fault injection --------------------------------------------------------

// lossyFetch is a real IMAP session with exactly one fault injected: the FETCH
// silently omits one UID. It returns NO error — which is precisely the failure
// mode "our fetch loop succeeded" cannot detect, and the reason the reconcile
// demands an independent proof of completeness instead.
type lossyFetch struct {
	sync.SyncSession
	drop sync.UID
}

func (l *lossyFetch) FetchByUID(ctx context.Context, uids sync.UIDSet, items sync.FetchItems, fn func(sync.Message) error) error {
	return l.SyncSession.FetchByUID(ctx, uids, items, func(m sync.Message) error {
		if m.UID == l.drop {
			return nil // the message the server "loses": omitted, with no error
		}
		return fn(m)
	})
}

var _ sync.SyncSession = (*lossyFetch)(nil)
