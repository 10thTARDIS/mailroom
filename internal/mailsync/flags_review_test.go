// SPDX-License-Identifier: AGPL-3.0-only

package mailsync

import (
	"context"
	"errors"
	"testing"

	"mailroom/internal/sync"
)

// The review of PR #910 (M4, M5, L6, L7). Each test was red against the code
// it reviewed.

// M4: a server that does not keep user keywords (no PERMANENTFLAGS \*) never
// reports them, so its reading must not wipe the keywords Mailroom holds for
// the copy. Only system flags are compared and applied; a server that DOES
// keep keywords is still believed about them.
//
// PROBE: drop the withRecordedKeywords call in sweepRange and the first case
// goes red (the keyword is wiped).
func TestFlagSweepKeepsKeywordsTheServerCannotStore(t *testing.T) {
	for _, accepts := range []bool{false, true} {
		sess, st, ing, row := sweepFixture()
		row.provFlags = map[int64][]string{1: {`\Seen`, `$Label1`}, 2: {`\Seen`, `$Label1`}}
		row.acceptsKeywords = accepts
		// On the server: UID 1 \Seen (keyword never stored), UID 2 now unread.
		sess.boxes["INBOX"].msgs = []sync.Message{msg(1, 0, "one", `\Seen`), msg(2, 0, "two")}
		if _, err := New(st, ing, factoryFor(sess), WithFlagSweep(sweepOn)).SyncAccount(context.Background(), testReq()); err != nil {
			t.Fatal(err)
		}
		if !accepts {
			if got := row.provFlags[1]; !sameFlags(got, []string{`\Seen`, `$Label1`}) {
				t.Errorf("no-keyword server: UID 1 flags = %q, want the keyword kept and nothing applied", got)
			}
			if got := row.provFlags[2]; !sameFlags(got, []string{`$Label1`}) {
				t.Errorf("no-keyword server: UID 2 flags = %q, want \\Seen removed and the keyword kept", got)
			}
		} else {
			if got := row.provFlags[1]; !sameFlags(got, []string{`\Seen`}) {
				t.Errorf("keyword server: UID 1 flags = %q, want the server's word on the keyword", got)
			}
		}
	}
}

// M5: a server that refuses the sweep's FLAGS FETCH must still deliver new
// mail; the failure is recorded, not fatal.
//
// PROBE: make the sweep's error return from syncMailbox again and this goes
// red (no new message ingested).
func TestFlagSweepFailureStillDeliversNewMail(t *testing.T) {
	sess, st, ing, _ := sweepFixture()
	box := sess.boxes["INBOX"]
	box.status.UIDNext = 4
	box.msgs = append(box.msgs, msg(3, 0, "brand new"))
	bad := &badFlagsSession{fakeSession: sess}
	sum, err := New(st, ing, func(context.Context, sync.Config) (sync.SyncSession, error) { return bad, nil },
		WithFlagSweep(sweepOn)).SyncAccount(context.Background(), testReq())
	if err != nil {
		t.Fatal(err)
	}
	if sum.MessagesIngested != 1 {
		t.Fatalf("MessagesIngested = %d, want 1: a refused sweep blocked new mail", sum.MessagesIngested)
	}
	if len(sum.Errors) == 0 {
		t.Error("the sweep's failure was not recorded")
	}
}

// badFlagsSession answers BAD to every FLAGS-only FETCH (the sweep's shape).
type badFlagsSession struct{ *fakeSession }

func (s *badFlagsSession) FetchByUID(ctx context.Context, uids sync.UIDSet, items sync.FetchItems, fn func(sync.Message) error) error {
	if items.Flags && !items.BodyFull {
		return errors.New("imap: BAD Could not parse command")
	}
	return s.fakeSession.FetchByUID(ctx, uids, items, fn)
}

// L6: a copy whose message has a local flag edit still on its way to the
// server is not touched by the sweep — the server's reading is older than
// Mailroom's. The next sweep after the writeback drains converges.
//
// PROBE: drop the PendingWriteback skip and this goes red.
func TestFlagSweepSkipsPendingWriteback(t *testing.T) {
	sess, st, ing, row := sweepFixture()
	// UID 1 is recorded unread; the server still says \Seen because the
	// local "mark unread" (a -flag intent) is still pending.
	row.pendingWriteback = map[int64]bool{1: true}
	sum, err := New(st, ing, factoryFor(sess), WithFlagSweep(sweepOn)).SyncAccount(context.Background(), testReq())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range ing.refreshCalls {
		if p.UID == 1 {
			t.Fatalf("the sweep applied the server's older reading over a pending local edit: %q", p.RemoteFlags)
		}
	}
	if sum.FlagSweepDeferred != 1 {
		t.Errorf("FlagSweepDeferred = %d, want 1", sum.FlagSweepDeferred)
	}
}

// L7: a CONDSTORE pass that met a reply without FLAGS keeps the old
// HIGHESTMODSEQ, so the next pass asks about that message again.
//
// PROBE: write status.HighestModSeq into the cursors unconditionally and this
// goes red.
func TestAbsentFlagsHoldTheModSeqCursor(t *testing.T) {
	sess := &fakeSession{
		caps:      map[sync.Capability]bool{sync.CapabilityCondStore: true},
		mailboxes: []sync.Mailbox{inbox()},
		boxes: map[string]*fakeBox{
			"INBOX": {
				status:      sync.MailboxStatus{UIDValidity: 10, UIDNext: 3, HighestModSeq: 12},
				msgs:        []sync.Message{msg(1, 12, "one", `\Seen`), msg(2, 12, "two", `\Seen`)},
				flagsAbsent: map[sync.UID]bool{2: true},
			},
		},
	}
	st, ing := newFakeStore(), newFakeIngester()
	row := st.seedRow("INBOX", &storedRow{id: newULID(), syncScope: "full", uidValidity: 10, lastSeenUID: 2, highestModSeq: 10}, 1, 2)
	ing.st = st
	if _, err := New(st, ing, factoryFor(sess)).SyncAccount(context.Background(), testReq()); err != nil {
		t.Fatal(err)
	}
	if c := st.cursorCalls[row.id]; c.HighestModSeq != 10 {
		t.Errorf("HIGHESTMODSEQ cursor = %d, want the old 10 held (UID 2 had no FLAGS)", c.HighestModSeq)
	}

	// Control: with every reply complete, the cursor advances.
	sess.boxes["INBOX"].flagsAbsent = nil
	if _, err := New(st, ing, factoryFor(sess)).SyncAccount(context.Background(), testReq()); err != nil {
		t.Fatal(err)
	}
	if c := st.cursorCalls[row.id]; c.HighestModSeq != 12 {
		t.Errorf("control: HIGHESTMODSEQ cursor = %d, want 12", c.HighestModSeq)
	}
}
