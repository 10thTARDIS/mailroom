// SPDX-License-Identifier: AGPL-3.0-only

package mailsync

// SELECTIVE SYNC (#62): what `sync_scope = 'off'` must and must not do.
//
// Turning a folder off is a PREFERENCE, and the reaper is the one part of this
// package that can destroy information. Put those together and the whole feature
// rests on a single property:
//
//	AN OFF MAILBOX IS NOT A REAP CANDIDATE.
//
// It falls out of where the skip sits — before SELECT, so the mailbox never
// becomes a reapTarget — but "it falls out of the structure" is exactly the kind
// of guarantee a later refactor removes without noticing.
//
// TestSyncAccountScopeOffSkips already asserts the mailbox is not polled, and it
// is worth being precise about why that is not enough, because a probe settled
// it. Simulating the plausible refactor — keep the poll skip, add the mailbox to
// the reap targets anyway — makes THIS file report the actual damage
// (`ProvenanceReaped: 2, MessagesVanished: 1` on a folder the user turned off).
// The older test also goes red, but incidentally: its fixture never sets
// uidvalidity, so the reap refuses on a stale coordinate space and the refusal
// surfaces as an unexpected error. Give that fixture a matching uidvalidity and
// it would pass while mail was being deleted. It asserts "not polled"; nothing
// asserted "not destroyed" until here.
//
// The second test is the other half of "non-destructive": off has to be
// REVERSIBLE. The cursors survive, so turning the folder back on must catch up on
// everything that happened while nobody was looking — new mail, expunges, and a
// UIDVALIDITY roll — rather than leaving a permanently stale folder.

import (
	"context"
	"testing"

	"mailroom/internal/store"
	"mailroom/internal/sync"
)

// AN OFF MAILBOX IS NEVER REAPED, even when it is overdue and the server has
// visibly dropped mail we hold.
//
// The setup is deliberately the strongest case FOR reaping: the reaper is
// enabled, the mailbox was last reaped two hours ago (well past the 1h cadence),
// and we hold three UIDs of which the server now has one. A mailbox that reached
// the reaper here would lose two provenance rows and a message.
func TestScopeOffIsNeverReaped(t *testing.T) {
	sess := &fakeSession{
		mailboxes: []sync.Mailbox{inbox()},
		boxes: map[string]*fakeBox{
			"INBOX": {
				status: sync.MailboxStatus{UIDValidity: 10, UIDNext: 4, NumMessages: 1},
				msgs:   []sync.Message{msg(1, 0, "a")},
			},
		},
	}
	st, ing := newFakeStore(), newFakeIngester()
	st.seedRow("INBOX", &storedRow{
		id: newULID(), syncScope: "off",
		uidValidity: 10, lastSeenUID: 3, lastReapedAt: hoursAgo(2),
	}, 1, 2, 3)
	// If anything did reach the reaper, this is what it would have reported —
	// so a failure below shows real losses rather than an empty struct.
	st.reapResult = store.ReapResult{ProvenanceDropped: 2, MembershipChanged: 2, MessagesVanished: 1}
	st.retireCount = 1

	sum, err := reapEngine(sess, st, ing).SyncAccount(context.Background(), testReq())
	if err != nil {
		t.Fatal(err)
	}

	// NOT ASKED. LiveUIDs is the reaper's source of truth; never issuing it is
	// what makes "we are not looking" incapable of becoming "it is gone".
	if len(sess.liveCalls) != 0 {
		t.Errorf("LiveUIDs called %v for an off mailbox", sess.liveCalls)
	}
	if len(st.reapCalls) != 0 {
		t.Errorf("an off mailbox was reaped: %+v", st.reapCalls)
	}
	if st.retireCalls != 0 {
		t.Errorf("the retirement sweep ran for an off mailbox: %d calls", st.retireCalls)
	}
	// And nothing is reported as destroyed, because nothing was.
	if sum.MailboxesReaped != 0 || sum.ProvenanceReaped != 0 || sum.MessagesVanished != 0 {
		t.Errorf("summary claims destruction for an off mailbox: %+v", sum)
	}
	if sum.MailboxesSkipped != 1 {
		t.Errorf("skipped = %d, want 1", sum.MailboxesSkipped)
	}
}

// The gap backfill is ADDITIVE and runs even when the reaper is disabled
// (doc.go), so it is worth stating separately that `off` also stops it: the point
// of off is not "do not delete", it is DO NOT LOOK. A backfill would re-fetch and
// ingest mail from a folder the user asked us to leave alone.
func TestScopeOffAlsoSkipsTheAdditiveBackfill(t *testing.T) {
	sess := &fakeSession{
		mailboxes: []sync.Mailbox{inbox()},
		boxes: map[string]*fakeBox{
			// The server holds a UID below our cursor that we have no provenance
			// for — exactly the gap the backfill exists to close.
			"INBOX": {
				status: sync.MailboxStatus{UIDValidity: 10, UIDNext: 5, NumMessages: 3},
				msgs:   []sync.Message{msg(1, 0, "a"), msg(2, 0, "b"), msg(4, 0, "d")},
			},
		},
	}
	st, ing := newFakeStore(), newFakeIngester()
	st.seedRow("INBOX", &storedRow{
		id: newULID(), syncScope: "off",
		uidValidity: 10, lastSeenUID: 4, lastReapedAt: hoursAgo(2),
	}, 1, 4) // UID 2 is a gap

	if _, err := reapEngine(sess, st, ing).SyncAccount(context.Background(), testReq()); err != nil {
		t.Fatal(err)
	}
	if len(ing.calls) != 0 {
		t.Errorf("ingested %d messages from an off mailbox", len(ing.calls))
	}
	if len(sess.liveCalls) != 0 {
		t.Errorf("LiveUIDs called %v for an off mailbox", sess.liveCalls)
	}
}

// OFF IS REVERSIBLE. Everything that happened while the folder was off is picked
// up when it goes back to full: new mail arrives (the poll resumes from the
// surviving cursor), and mail that vanished meanwhile is reaped.
//
// This is what makes off a preference rather than a slow way to lose a folder,
// and it is worth a test because nothing in the engine treats re-enabling as an
// event — it simply stops matching the skip, and the ordinary machinery has to be
// enough on its own.
func TestScopeBackToFullCatchesUp(t *testing.T) {
	sess := &fakeSession{
		mailboxes: []sync.Mailbox{inbox()},
		boxes: map[string]*fakeBox{
			// While we were off: UID 2 was expunged, UIDs 4 and 5 arrived.
			"INBOX": {
				status: sync.MailboxStatus{UIDValidity: 10, UIDNext: 6, NumMessages: 4},
				msgs: []sync.Message{
					msg(1, 0, "a"), msg(3, 0, "c"), msg(4, 0, "d"), msg(5, 0, "e"),
				},
			},
		},
	}
	st, ing := newFakeStore(), newFakeIngester()
	// The cursors are exactly where the folder was frozen: we hold 1..3 and last
	// saw UID 3.
	st.seedRow("INBOX", &storedRow{
		id: newULID(), syncScope: "full", // just turned back on
		uidValidity: 10, lastSeenUID: 3, lastReapedAt: hoursAgo(2),
	}, 1, 2, 3)
	st.reapResult = store.ReapResult{ProvenanceDropped: 1, MembershipChanged: 1, MessagesVanished: 1}
	st.retireCount = 1

	sum, err := reapEngine(sess, st, ing).SyncAccount(context.Background(), testReq())
	if err != nil {
		t.Fatal(err)
	}

	// THE NEW MAIL. The poll asks lastSeen+1:* — the cursor survived being off, so
	// it resumes rather than re-reading the folder or skipping past it.
	if len(ing.calls) != 2 {
		t.Fatalf("ingested %d messages, want 2 (UIDs 4 and 5)", len(ing.calls))
	}
	// THE VANISHED MAIL. It is not lost forever just because we were not watching
	// when it went.
	if len(st.reapCalls) != 1 {
		t.Fatalf("reap calls = %d, want 1", len(st.reapCalls))
	}
	if sum.MessagesVanished != 1 {
		t.Errorf("vanished = %d, want 1 (UID 2, expunged while off)", sum.MessagesVanished)
	}
	if sum.MailboxesSkipped != 0 {
		t.Errorf("skipped = %d, want 0 — the mailbox is on again", sum.MailboxesSkipped)
	}
}

// A UIDVALIDITY ROLL WHILE OFF is the nastiest version of catching up: every UID
// we hold refers to a different message now, so resuming from the old cursor
// would ingest the wrong mail. Re-enabling must roll the generation rather than
// poll from a cursor that no longer means anything.
func TestScopeBackToFullAfterUIDValidityRoll(t *testing.T) {
	sess := &fakeSession{
		mailboxes: []sync.Mailbox{inbox()},
		boxes: map[string]*fakeBox{
			// The folder was recreated on the server while we were off: new
			// generation, UIDs restart.
			"INBOX": {
				status: sync.MailboxStatus{UIDValidity: 99, UIDNext: 3, NumMessages: 2},
				msgs:   []sync.Message{msg(1, 0, "x"), msg(2, 0, "y")},
			},
		},
	}
	st, ing := newFakeStore(), newFakeIngester()
	st.seedRow("INBOX", &storedRow{
		id: newULID(), syncScope: "full", // just turned back on
		uidValidity: 10, lastSeenUID: 3, lastReapedAt: hoursAgo(2),
	}, 1, 2, 3)

	sum, err := reapEngine(sess, st, ing).SyncAccount(context.Background(), testReq())
	if err != nil {
		t.Fatal(err)
	}
	// The old generation is retired and the mailbox re-read from scratch, which is
	// the existing UIDVALIDITY machinery doing its job — the point of the test is
	// that being off did not put the mailbox into a state that machinery cannot
	// recognise.
	if len(st.staleCalls) == 0 {
		t.Error("the stale generation was not retired after a roll that happened while off")
	}
	if len(ing.calls) != 2 {
		t.Errorf("ingested %d messages, want 2 from the new generation", len(ing.calls))
	}
	if len(sum.Errors) != 0 {
		t.Errorf("errors after re-enabling across a roll: %+v", sum.Errors)
	}
}
