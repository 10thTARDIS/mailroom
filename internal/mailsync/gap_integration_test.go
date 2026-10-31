// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

// Package mailsync's live end-to-end coverage for the GAP BACKFILL (#93).
//
// #93 is the third time this project has walked into one trap, and the first time
// the trap was baited with the ordinary poll rather than an exotic edge case.
//
// The poll fetches the open-ended UID range above the mailbox's last-seen UID and
// then advances that cursor to the highest UID it INGESTED. A FETCH that silently
// drops a UID in the MIDDLE of the range returns no error — a truncated response,
// a half-read connection, a library bug all look exactly like this — so the cursor
// sails right past the dropped message. The next poll asks for `cursor+1:*`. The
// message is now BELOW the cursor, and nothing will ever ask for it again: not the
// poll, whose range starts above it, and not the #86 reaper, which only ever DROPS
// provenance rows and is blind to a message that has none. The message sits on the
// server forever, and Mailroom never learns it exists. Silent, permanent mail loss.
//
// It cannot be detected from the FETCH. A hole in a FETCH response is what an
// EXPUNGED UID looks like; "we asked for 5:* and got 5,7,8" is indistinguishable
// from "6 was deleted". The only thing that can tell those apart is a second,
// independent question to the server — the same proof #88 already demanded of the
// UIDVALIDITY re-fetch, applied to the poll that never had one:
//
//	EVERY UID THE SERVER REPORTS BELOW OUR OWN CURSOR HAS A LIVE PROVENANCE ROW.
//
// So it is proved against a REAL SERVER, with the fault injected at exactly the
// layer where it really happens: the FETCH returns fewer messages than the server
// holds, and returns no error. Run via scripts/mailsync-test.sh; without a live
// Dovecot + PostgreSQL every test here skips (newLiveFixture).
package mailsync

import (
	"context"
	"testing"
	"time"

	"mailroom/internal/ingest"
	"mailroom/internal/sync"
)

// ---- harness ----------------------------------------------------------------

// lossyEngine is a REAL engine against the REAL server with exactly one fault: its
// FETCH silently omits `drop`, with no error. UID SEARCH ALL still lists that UID,
// because the server still has the message — and that asymmetry is the whole bug.
func (f *liveFixture) lossyEngine(drop sync.UID) *Engine {
	return New(NewStoreAdapter(f.st), ingest.New(f.st, f.blobs),
		func(ctx context.Context, cfg sync.Config) (sync.SyncSession, error) {
			s, err := sync.Dial(ctx, cfg)
			if err != nil {
				return nil, err
			}
			return &lossyFetch{SyncSession: s, drop: drop}, nil
		}, WithReapInterval(time.Nanosecond))
}

// run drives one pass of an arbitrary engine against the fixture's account.
func (f *liveFixture) run(t *testing.T, e *Engine) Summary {
	t.Helper()
	sum, err := e.SyncAccount(context.Background(), AccountSync{
		UserID: f.userID, AccountID: f.accountID, Config: f.cfg, DEK: f.dek, DEKID: f.dekID,
	})
	if err != nil {
		t.Fatalf("SyncAccount: %v", err)
	}
	return sum
}

// storedLastSeenUID is the poll's cursor. It is what makes #93 PERMANENT: once it
// is past a dropped UID, no future poll's range includes that UID again.
func (f *liveFixture) storedLastSeenUID(t *testing.T, imapName string) int64 {
	t.Helper()
	var uid int64
	if err := f.super.QueryRow(context.Background(),
		`SELECT last_seen_uid FROM app.mailboxes WHERE user_id = $1 AND imap_name = $2`,
		f.userID.String(), imapName).Scan(&uid); err != nil {
		t.Fatalf("stored last_seen_uid: %v", err)
	}
	return uid
}

// lastReapedAt is the observation cadence cursor. An UNCLOSED gap must not stamp
// it: a mailbox we know we are missing a message from has to be re-checked on the
// next pass, not in an hour.
func (f *liveFixture) lastReapedAt(t *testing.T, imapName string) *time.Time {
	t.Helper()
	var at *time.Time
	if err := f.super.QueryRow(context.Background(),
		`SELECT last_reaped_at FROM app.mailboxes WHERE user_id = $1 AND imap_name = $2`,
		f.userID.String(), imapName).Scan(&at); err != nil {
		t.Fatalf("stored last_reaped_at: %v", err)
	}
	return at
}

// ---- tests ------------------------------------------------------------------

// THE BUG #93 EXISTS FOR, AND THE TEST THAT FAILS AGAINST TODAY'S CODE.
//
// Three messages arrive. The poll's FETCH silently drops the MIDDLE one and
// reports no error, so the last-seen cursor advances to the third message anyway
// and the second is now below it — unreachable by any future poll. A later pass
// must nevertheless end with that message ingested and visible to a JMAP client.
//
// Against the code before this change, the second pass fetches `cursor+1:*`, that
// range does not contain the dropped UID, and the message is never learned about.
// Ever.
func TestLiveSilentlyTruncatedFetchIsRecovered(t *testing.T) {
	f := newLiveFixture(t, WithReapInterval(time.Nanosecond))

	f.appendMsg(t, f.inbox, "truncated-before")
	lostUID := f.appendMsg(t, f.inbox, "truncated-lost")
	f.appendMsg(t, f.inbox, "truncated-after")

	// --- Pass 1: the FETCH loses the middle message, silently. ---
	sum := f.run(t, f.lossyEngine(lostUID))

	if _, ok := f.messageIDBySubject(t, "truncated-lost"); ok {
		t.Fatal("the injected fault did not fire — the message was ingested, so this test proves nothing")
	}
	// THE THING THAT MAKES IT PERMANENT: the cursor is past the dropped UID.
	if got := f.storedLastSeenUID(t, f.inbox); got <= int64(lostUID) {
		t.Fatalf("last_seen_uid = %d, want > %d — the cursor did not advance past the dropped UID, "+
			"so the next poll would re-request it anyway and this test proves nothing", got, lostUID)
	}
	// The pass noticed. It says so, it fixes nothing (the same lossy FETCH drops the
	// UID again), and — crucially — it destroys nothing and does not mark the mailbox
	// done.
	if sum.GapsDetected != 1 {
		t.Errorf("GapsDetected = %d, want 1 — the server lists a UID below our cursor that we do not hold", sum.GapsDetected)
	}
	if sum.MessagesVanished != 0 || sum.ProvenanceReaped != 0 {
		t.Fatalf("A TRUNCATED FETCH DELETED SOMETHING: %+v", sum)
	}
	if len(sum.Errors) != 1 || sum.Errors[0].Mailbox != f.inbox {
		t.Fatalf("errors = %v, want exactly one, naming the INBOX", sum.Errors)
	}
	if at := f.lastReapedAt(t, f.inbox); at != nil {
		t.Errorf("last_reaped_at = %v over an UNCLOSED gap — the mailbox would not be re-checked for an hour", at)
	}

	// --- Pass 2: a healthy connection. ---
	sum = f.syncOnce(t)
	if len(sum.Errors) != 0 {
		t.Fatalf("healthy sync errors: %v", sum.Errors)
	}

	lost, ok := f.messageIDBySubject(t, "truncated-lost")
	if !ok {
		t.Fatal("THE MESSAGE IS STILL LOST. It is sitting in the server's INBOX, below our own last-seen " +
			"cursor, and no poll will ever ask for it again. That is mailroom#93: silent, permanent mail loss.")
	}
	if sum.GapsBackfilled != 1 || sum.MessagesIngested != 1 {
		t.Errorf("summary = %+v, want exactly one message recovered", sum)
	}

	inboxID := f.mailboxID(t, f.inbox)
	if got := f.mailboxIDs(t, lost); len(got) != 1 || got[0] != inboxID.String() {
		t.Errorf("the recovered message's mailboxIds = %v, want {INBOX}", got)
	}
	if f.trashed(t, lost) {
		t.Error("the recovered message is soft-deleted")
	}
	// Email/query agrees, and every message the server holds is in the folder.
	if got := f.inMailbox(t, inboxID); len(got) != 3 {
		t.Errorf("inMailbox(INBOX) has %d messages, want all 3 the server holds", len(got))
	}
	// And the change stream carried it: /changes and the search index learn through
	// that one row, so a recovered message is not merely in the database.
	if n := f.emailChanges(t, lost); n == 0 {
		t.Error("no Email change_log row for the recovered message — no client and no index would ever see it")
	}
	// The mailbox is now proved complete, so it is finally marked observed.
	if at := f.lastReapedAt(t, f.inbox); at == nil {
		t.Error("the observation cursor was never stamped even though the gap closed")
	}
	// The messages that were never in doubt are untouched.
	for _, subj := range []string{"truncated-before", "truncated-after"} {
		id, ok := f.messageIDBySubject(t, subj)
		if !ok {
			t.Fatalf("%s vanished", subj)
		}
		if f.trashed(t, id) {
			t.Errorf("%s was soft-deleted by the backfill", subj)
		}
	}
}

// FAIL CLOSED, AND KEEP THE TWO CONCLUSIONS APART. A gap is evidence that OUR FETCH
// was incomplete — never that anything was deleted. So while a gap stands open, the
// mailbox is one we have a demonstrably incomplete view of, and an incomplete view
// is not a licence to delete rows from it: the reaper does not touch that mailbox,
// even though the server really has expunged a message from it. Only once the gap
// is closed does the reap run — and then it runs correctly.
func TestLiveOpenGapBlocksTheReap(t *testing.T) {
	f := newLiveFixture(t, WithReapInterval(time.Nanosecond))

	f.appendMsg(t, f.inbox, "gap-bystander")
	lostUID := f.appendMsg(t, f.inbox, "gap-lost")
	doomedUID := f.appendMsg(t, f.inbox, "gap-doomed")

	lossy := f.lossyEngine(lostUID)

	// Pass 1: the bystander and the doomed message are ingested; gap-lost is not.
	f.run(t, lossy)
	doomed, ok := f.messageIDBySubject(t, "gap-doomed")
	if !ok {
		t.Fatal("gap-doomed was never ingested")
	}
	if _, ok := f.messageIDBySubject(t, "gap-lost"); ok {
		t.Fatal("the injected fault did not fire")
	}
	inboxID := f.mailboxID(t, f.inbox)
	before := f.liveProvenanceCount(t)

	// The user deletes gap-doomed on their phone. A reaper that ran would remove it.
	f.expungeOnServer(t, f.inbox, doomedUID)

	// Pass 2, STILL LOSSY: the gap is still open. The reaper must keep its hands off
	// this mailbox — its view of the folder is provably incomplete.
	sum := f.run(t, lossy)
	if sum.ProvenanceReaped != 0 || sum.MessagesVanished != 0 {
		t.Fatalf("REAPED A MAILBOX WE KNOW WE ARE MISSING A MESSAGE FROM: %+v", sum)
	}
	if got := f.liveProvenanceCount(t); got != before {
		t.Fatalf("live provenance went from %d to %d while a gap was open", before, got)
	}
	if got := f.mailboxIDs(t, doomed); len(got) != 1 || got[0] != inboxID.String() {
		t.Fatalf("the expunged message's membership changed while our view was incomplete: %v", got)
	}
	if f.trashed(t, doomed) {
		t.Fatal("SOFT-DELETED A MESSAGE WHILE HOLDING A DEMONSTRABLY INCOMPLETE VIEW OF ITS MAILBOX")
	}

	// Pass 3, HEALTHY: the gap closes — and only now does the reap run. Both
	// conclusions, from one observation, in the right order: ADD first, then REMOVE.
	sum = f.syncOnce(t)
	if len(sum.Errors) != 0 {
		t.Fatalf("healthy sync errors: %v", sum.Errors)
	}
	lost, ok := f.messageIDBySubject(t, "gap-lost")
	if !ok {
		t.Fatal("the message our FETCH dropped was never recovered")
	}
	if sum.GapsBackfilled != 1 {
		t.Errorf("GapsBackfilled = %d, want 1", sum.GapsBackfilled)
	}
	if sum.ProvenanceReaped != 1 || sum.MessagesVanished != 1 {
		t.Errorf("summary = %+v, want the expunged message reaped exactly once", sum)
	}

	// The recovered message is in the inbox and alive; the expunged one is gone from
	// it and honestly soft-deleted. The gap detector added; the reaper removed; the
	// two never touched each other's messages.
	if got := f.mailboxIDs(t, lost); len(got) != 1 || got[0] != inboxID.String() {
		t.Errorf("the recovered message is in %v, want {INBOX}", got)
	}
	if f.trashed(t, lost) {
		t.Fatal("SOFT-DELETED THE MESSAGE WE JUST RECOVERED FROM THE SERVER")
	}
	if got := f.mailboxIDs(t, doomed); len(got) != 0 {
		t.Errorf("the expunged message is still in %v", got)
	}
	if !f.trashed(t, doomed) {
		t.Error("the expunged message was not soft-deleted once the mailbox was verified")
	}
	if got := f.inMailbox(t, inboxID); len(got) != 2 {
		t.Errorf("inMailbox(INBOX) = %d messages, want the 2 the server still holds", len(got))
	}
}

// WE FAILED TO ASK. A live-UID read that fails concludes NOTHING — in either
// direction. It does not reap, and it does not decide that anything is missing
// either. The open gap simply survives to be closed by a healthy pass.
func TestLiveGapDetectionOnAFailedLiveUIDsReadConcludesNothing(t *testing.T) {
	f := newLiveFixture(t, WithReapInterval(time.Nanosecond))

	f.appendMsg(t, f.inbox, "unasked-bystander")
	lostUID := f.appendMsg(t, f.inbox, "unasked-lost")
	f.appendMsg(t, f.inbox, "unasked-after")

	f.run(t, f.lossyEngine(lostUID)) // leaves a gap at lostUID
	if _, ok := f.messageIDBySubject(t, "unasked-lost"); ok {
		t.Fatal("the injected fault did not fire")
	}
	before := f.liveProvenanceCount(t)

	// Now the FETCH is healthy but we cannot ask the server what it holds.
	blind := New(NewStoreAdapter(f.st), ingest.New(f.st, f.blobs),
		func(ctx context.Context, cfg sync.Config) (sync.SyncSession, error) {
			s, err := sync.Dial(ctx, cfg)
			if err != nil {
				return nil, err
			}
			return &brokenLiveUIDs{SyncSession: s, mailbox: f.inbox}, nil
		}, WithReapInterval(time.Nanosecond))

	sum := f.run(t, blind)
	if len(sum.Errors) == 0 {
		t.Fatal("the failed live-UID read was not surfaced")
	}
	if sum.GapsDetected != 0 || sum.GapsBackfilled != 0 {
		t.Fatalf("decided a message was missing from a mailbox we could not ask about: %+v", sum)
	}
	if sum.ProvenanceReaped != 0 || sum.MessagesVanished != 0 {
		t.Fatalf("REAPED A MAILBOX WE COULD NOT ASK ABOUT: %+v", sum)
	}
	if got := f.liveProvenanceCount(t); got != before {
		t.Fatalf("live provenance went from %d to %d across a failed live-UID read", before, got)
	}
	if at := f.lastReapedAt(t, f.inbox); at != nil {
		t.Error("stamped the observation cursor for a mailbox we never actually observed")
	}

	// A healthy pass still finds and recovers the message — proving the pass above
	// was blocked by the injected fault, not by there being nothing to find.
	f.syncOnce(t)
	if _, ok := f.messageIDBySubject(t, "unasked-lost"); !ok {
		t.Fatal("the lost message was never recovered by a healthy pass")
	}
}

// AN ANSWER WE CANNOT TRUST IS NOT AN ANSWER. The server's own SELECT says the
// mailbox holds messages and the SEARCH then names none: a broken response, not an
// emptied folder. The reaper already refuses to read that as "everything here is
// gone"; the gap detector must equally refuse to read it as anything at all. It
// must not reap, must not backfill, and must not mark the mailbox done.
func TestLiveGapDetectionRefusesABrokenUIDSet(t *testing.T) {
	f := newLiveFixture(t, WithReapInterval(time.Nanosecond))

	f.appendMsg(t, f.inbox, "broken-answer-bystander")
	lostUID := f.appendMsg(t, f.inbox, "broken-answer-lost")
	f.appendMsg(t, f.inbox, "broken-answer-after")

	f.run(t, f.lossyEngine(lostUID)) // leaves a gap at lostUID
	before := f.liveProvenanceCount(t)

	broken := New(NewStoreAdapter(f.st), ingest.New(f.st, f.blobs),
		func(ctx context.Context, cfg sync.Config) (sync.SyncSession, error) {
			s, err := sync.Dial(ctx, cfg)
			if err != nil {
				return nil, err
			}
			return &emptyUIDSet{SyncSession: s, mailbox: f.inbox}, nil
		}, WithReapInterval(time.Nanosecond))

	sum := f.run(t, broken)
	if len(sum.Errors) == 0 {
		t.Fatal("an empty UID set from a mailbox the server says holds messages was accepted")
	}
	if sum.GapsDetected != 0 || sum.GapsBackfilled != 0 {
		t.Fatalf("read a broken UID set as a statement about what we are missing: %+v", sum)
	}
	if sum.ProvenanceReaped != 0 || sum.MessagesVanished != 0 {
		t.Fatalf("REAPED THE FOLDER ON A BROKEN UID SET: %+v", sum)
	}
	if got := f.liveProvenanceCount(t); got != before {
		t.Fatalf("live provenance went from %d to %d on an answer we said we refused", before, got)
	}
	if at := f.lastReapedAt(t, f.inbox); at != nil {
		t.Error("stamped the observation cursor on an answer that was refused")
	}

	// And a healthy pass converges, so the refusal above was the guard firing rather
	// than the folder simply being fine.
	f.syncOnce(t)
	if _, ok := f.messageIDBySubject(t, "broken-answer-lost"); !ok {
		t.Fatal("the lost message was never recovered by a healthy pass")
	}
}

// A GAP THAT IS NOT A GAP. A UID above the last-seen cursor has simply not been
// polled yet — the next poll's `lastSeen+1:*` range will ask for it — so it must
// never be counted as missing, and the mailbox that holds it is perfectly verified.
// (Without this bound, every message delivered between the poll and the live-UID
// read would look like a message we had lost, and every busy mailbox would report a
// permanent gap.)
func TestLiveMailArrivingAfterThePollIsNotAGap(t *testing.T) {
	f := newLiveFixture(t, WithReapInterval(time.Nanosecond))
	f.appendMsg(t, f.inbox, "settled")

	// A pass that polls, and DURING which new mail lands: the message is appended
	// between the poll's FETCH and the verification's UID SEARCH. Simulated exactly:
	// poll once, then append, then verify by polling again is NOT the same thing —
	// so instead we inject the append into the session's LiveUIDs call.
	arriving := New(NewStoreAdapter(f.st), ingest.New(f.st, f.blobs),
		func(ctx context.Context, cfg sync.Config) (sync.SyncSession, error) {
			s, err := sync.Dial(ctx, cfg)
			if err != nil {
				return nil, err
			}
			return &appendBeforeLiveUIDs{SyncSession: s, mailbox: f.inbox, f: f, t: t}, nil
		}, WithReapInterval(time.Nanosecond))

	sum := f.run(t, arriving)
	if len(sum.Errors) != 0 {
		t.Fatalf("mail landing mid-pass was treated as an error: %v", sum.Errors)
	}
	if sum.GapsDetected != 0 {
		t.Fatalf("counted mail that arrived AFTER the poll as a gap: %+v — every busy mailbox "+
			"would report a permanent gap and never reap again", sum)
	}
	if sum.MessagesVanished != 0 || sum.ProvenanceReaped != 0 {
		t.Fatalf("deleted something: %+v", sum)
	}

	// The next ordinary poll picks it up through the normal new-message range, which
	// is exactly why it was not a gap.
	f.syncOnce(t)
	if _, ok := f.messageIDBySubject(t, "arrived-mid-pass"); !ok {
		t.Fatal("the message that arrived mid-pass was never ingested by the following poll")
	}
}

// ---- fault injection --------------------------------------------------------

// emptyUIDSet is a real IMAP session whose UID SEARCH ALL comes back empty for one
// mailbox while that mailbox's own SELECT still reports EXISTS > 0 — a truncated
// response, a library type-confusion, a server bug. It is the shape of answer that
// must never be believed in EITHER direction: read as "everything is gone" it wipes
// the folder; read as "nothing is missing" it hides a real gap.
type emptyUIDSet struct {
	sync.SyncSession
	mailbox string
}

func (e *emptyUIDSet) LiveUIDs(ctx context.Context, mailbox string) (*sync.MailboxStatus, []sync.UID, error) {
	status, uids, err := e.SyncSession.LiveUIDs(ctx, mailbox)
	if err != nil || mailbox != e.mailbox {
		return status, uids, err
	}
	return status, nil, nil // the SELECT still says EXISTS > 0
}

var _ sync.SyncSession = (*emptyUIDSet)(nil)

// appendBeforeLiveUIDs delivers a message to the mailbox in the instant BETWEEN the
// poll and the verification's UID SEARCH — the ordinary race a busy mailbox runs
// every pass. The new UID is above the poll's cursor and above the poll's UIDNEXT,
// so it is outside the space the poll claims to have covered, and it must not be
// mistaken for a message the poll lost.
type appendBeforeLiveUIDs struct {
	sync.SyncSession
	mailbox  string
	f        *liveFixture
	t        *testing.T
	appended bool
}

func (a *appendBeforeLiveUIDs) LiveUIDs(ctx context.Context, mailbox string) (*sync.MailboxStatus, []sync.UID, error) {
	if mailbox == a.mailbox && !a.appended {
		a.appended = true
		a.f.appendMsg(a.t, a.mailbox, "arrived-mid-pass") // a separate IMAP client delivers mail
	}
	return a.SyncSession.LiveUIDs(ctx, mailbox)
}

var _ sync.SyncSession = (*appendBeforeLiveUIDs)(nil)
