// SPDX-License-Identifier: AGPL-3.0-only

package mailsync

// Bounded body fetches (#656). After a very large number of messages were moved into INBOX on
// the provider, the new-message pass asked for `lastSeen+1:*` as ONE command of
// enormous size; it died mid-stream twice and the provider then refused it on
// every pass. These tests pin that no body FETCH names more than the chunk size,
// that a pass which fails part-way resumes where it stopped, and — the invariant
// that keeps the rest of a backlog from being stranded — that a partly fetched
// folder never records the #620 poll baseline that would let it be skipped.

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"testing"

	"mailroom/internal/sync"
)

// bodyFetches is every FETCH that downloaded bodies, in order.
func bodyFetches(s *fakeSession) []sync.UIDSet {
	var out []sync.UIDSet
	for _, f := range s.fetches {
		if f.items.BodyFull {
			out = append(out, f.uids)
		}
	}
	return out
}

// rangeSet is a one-range UIDSet; stop 0 is open-ended (`start:*`).
func rangeSet(start, stop sync.UID) sync.UIDSet { return sync.UIDSetRange(start, stop) }

// numbered is n messages with UIDs 1..n, each with a distinct body.
func numbered(n int, modSeq uint64) []sync.Message {
	out := make([]sync.Message, n)
	for i := range out {
		uid := sync.UID(i + 1)
		out[i] = msg(uid, modSeq, fmt.Sprintf("message %d", uid))
	}
	return out
}

// applyCursors does what the real store does with the last cursor write for
// the named mailbox — including poll_uidnext = NULLIF(UIDNext, 0) — so a test
// can run the NEXT pass against the state the previous one left. (The fake
// records writes without applying them.)
func applyCursors(t *testing.T, st *fakeStore, name string) {
	t.Helper()
	row := st.rows[name]
	c, ok := st.cursorCalls[row.id]
	if !ok {
		t.Fatalf("no cursor write recorded for %s", name)
	}
	row.uidValidity = c.UIDValidity
	row.lastSeenUID = c.LastSeenUID
	row.highestModSeq = c.HighestModSeq
	row.pollUIDNext = nil
	if c.UIDNext != 0 {
		row.pollUIDNext = i64(c.UIDNext)
	}
	row.pollMessages = i64(c.Messages)
	delete(st.cursorCalls, row.id)
}

func TestNewMessageChunks(t *testing.T) {
	cases := []struct {
		name          string
		from, uidNext sync.UID
		n             int
		want          []sync.UIDSet
	}{
		{"unknown UIDNEXT is one open command", 1, 0, 500, []sync.UIDSet{rangeSet(1, 0)}},
		{"a poll's handful is one open command", 41, 44, 500, []sync.UIDSet{rangeSet(41, 0)}},
		{"exactly n new UIDs is one command", 1, 501, 500, []sync.UIDSet{rangeSet(1, 0)}},
		{"n+1 new UIDs is two", 1, 502, 500, []sync.UIDSet{rangeSet(1, 500), rangeSet(501, 0)}},
		{"1,234 new UIDs is three", 1, 1235, 500,
			[]sync.UIDSet{rangeSet(1, 500), rangeSet(501, 1000), rangeSet(1001, 0)}},
		{"the walk starts at the cursor", 101, 110, 3,
			[]sync.UIDSet{rangeSet(101, 103), rangeSet(104, 106), rangeSet(107, 0)}},
		// Never asked for by syncMailbox (it skips the pass), but must not
		// underflow into an endless walk.
		{"an empty range does not underflow", 9, 5, 3, []sync.UIDSet{rangeSet(9, 0)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := newMessageChunks(tc.from, tc.uidNext, tc.n)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("newMessageChunks(%d, %d, %d) = %v, want %v", tc.from, tc.uidNext, tc.n, got, tc.want)
			}
		})
	}
}

func TestUIDChunks(t *testing.T) {
	got := uidChunks([]int64{9, 2, 3, 4, 11, 12, 20}, 3)
	want := []sync.UIDSet{
		{{Start: 2, Stop: 4}},
		{{Start: 9, Stop: 9}, {Start: 11, Stop: 12}},
		{{Start: 20, Stop: 20}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("uidChunks = %v, want %v", got, want)
	}
}

// The #656 shape at unit scale: 1,234 new messages and the DEFAULT chunk size
// give exactly three commands, the last one open-ended, and every message is
// ingested with the cursor and poll baseline recorded as before.
func TestNewMessagePassFetchesInBoundedChunks(t *testing.T) {
	if DefaultFetchChunk != 500 {
		t.Fatalf("DefaultFetchChunk = %d, want 500", DefaultFetchChunk)
	}
	sess := &fakeSession{
		mailboxes: []sync.Mailbox{inbox()},
		boxes: map[string]*fakeBox{"INBOX": {
			status: sync.MailboxStatus{UIDValidity: 10, UIDNext: 1235, NumMessages: 1234},
			msgs:   numbered(1234, 0),
		}},
	}
	st, ing := newFakeStore(), newFakeIngester()
	sum, err := newEngine(sess, st, ing).SyncAccount(context.Background(), testReq())
	if err != nil {
		t.Fatal(err)
	}
	assertCounts(t, sum, 1, 0, 1234, 0)

	want := []sync.UIDSet{rangeSet(1, 500), rangeSet(501, 1000), rangeSet(1001, 0)}
	if got := bodyFetches(sess); !reflect.DeepEqual(got, want) {
		t.Fatalf("body FETCH commands = %v, want %v", got, want)
	}
	c := st.cursorCalls[st.rows["INBOX"].id]
	if c.LastSeenUID != 1234 || c.UIDNext != 1235 || c.Messages != 1234 {
		t.Errorf("cursors = %+v, want last-seen 1234 and the poll baseline 1235/1234", c)
	}
}

// Without UIDNEXT there is nothing to walk towards: today's single open-ended
// command, unchanged.
func TestNewMessagePassWithoutUIDNextIsOneOpenFetch(t *testing.T) {
	sess := &fakeSession{
		mailboxes: []sync.Mailbox{inbox()},
		boxes: map[string]*fakeBox{"INBOX": {
			status: sync.MailboxStatus{UIDValidity: 10},
			msgs:   numbered(1234, 0),
		}},
	}
	st, ing := newFakeStore(), newFakeIngester()
	sum, err := newEngine(sess, st, ing).SyncAccount(context.Background(), testReq())
	if err != nil {
		t.Fatal(err)
	}
	assertCounts(t, sum, 1, 0, 1234, 0)
	if got, want := bodyFetches(sess), []sync.UIDSet{rangeSet(1, 0)}; !reflect.DeepEqual(got, want) {
		t.Fatalf("body FETCH commands = %v, want %v", got, want)
	}
}

// An explicit UID list — here the flag pass's body fallback, for 1,100 UIDs
// with no live provenance (the shape of the vanished messages #656 left behind)
// — is split into commands of at most 500.
func TestFlagFallbackBodyFetchIsBounded(t *testing.T) {
	sess := &fakeSession{
		caps:      map[sync.Capability]bool{sync.CapabilityCondStore: true},
		mailboxes: []sync.Mailbox{inbox()},
		boxes: map[string]*fakeBox{"INBOX": {
			status: sync.MailboxStatus{UIDValidity: 10, UIDNext: 1101, NumMessages: 1100, HighestModSeq: 12},
			msgs:   numbered(1100, 12),
		}},
	}
	st, ing := newFakeStore(), newFakeIngester()
	st.seedRow("INBOX", &storedRow{id: newULID(), syncScope: "full", uidValidity: 10, lastSeenUID: 1100, highestModSeq: 10})

	sum, err := newEngine(sess, st, ing).SyncAccount(context.Background(), testReq())
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.Errors) != 0 {
		t.Fatalf("errors: %v", sum.Errors)
	}
	want := []sync.UIDSet{rangeSet(1, 500), rangeSet(501, 1000), rangeSet(1001, 1100)}
	if got := bodyFetches(sess); !reflect.DeepEqual(got, want) {
		t.Fatalf("body FETCH commands = %v, want %v", got, want)
	}
	if len(ing.calls) != 1100 {
		t.Errorf("ingested %d, want all 1100", len(ing.calls))
	}
}

// The gap backfill's list goes through the same split.
func TestGapBackfillBodyFetchIsBounded(t *testing.T) {
	sess := &fakeSession{
		mailboxes: []sync.Mailbox{inbox()},
		boxes: map[string]*fakeBox{"INBOX": {
			status: sync.MailboxStatus{UIDValidity: 10, UIDNext: 7, NumMessages: 6},
			msgs:   numbered(6, 0),
		}},
	}
	st, ing := newFakeStore(), newFakeIngester()
	st.seedRow("INBOX", &storedRow{id: newULID(), syncScope: "full", uidValidity: 10, lastSeenUID: 6}, 1)
	ing.st = st
	e := New(st, ing, factoryFor(sess), WithFetchChunk(2))

	sum, err := e.SyncAccount(context.Background(), testReq())
	if err != nil {
		t.Fatal(err)
	}
	if sum.GapsDetected != 5 || sum.GapsBackfilled != 5 {
		t.Fatalf("gaps detected/backfilled = %d/%d, want 5/5 (errors %v)", sum.GapsDetected, sum.GapsBackfilled, sum.Errors)
	}
	want := []sync.UIDSet{rangeSet(2, 3), rangeSet(4, 5), rangeSet(6, 6)}
	if got := bodyFetches(sess); !reflect.DeepEqual(got, want) {
		t.Fatalf("body FETCH commands = %v, want %v", got, want)
	}
}

// ⚠ THE INVARIANT. Chunk 2 of 4 dies part-way. The chunk before it stays
// ingested and the cursor records it; the next pass RESUMES after it; and the
// partial pass records NO poll baseline, so the next LIST-STATUS pass polls the
// folder instead of skipping it.
//
// Pass 3 is the positive control that makes pass 2's "it was polled" mean
// something: with a complete poll behind it and nothing changed on the server,
// this same fixture DOES skip the folder.
func TestAChunkThatFailsResumesAndRecordsNoPollBaseline(t *testing.T) {
	status := sync.MailboxStatus{UIDValidity: 10, UIDNext: 11, NumMessages: 10, HighestModSeq: 50,
		PermanentFlagsAllowNew: true}
	box := &fakeBox{status: status, msgs: numbered(10, 40), failAfterUID: 5}
	sess := &fakeSession{
		caps: map[sync.Capability]bool{sync.CapabilityCondStore: true},
		mailboxes: []sync.Mailbox{{Name: "INBOX", Delimiter: '/', Status: &sync.ListStatus{
			UIDValidity: 10, UIDNext: 11, NumMessages: 10, HighestModSeq: 50}}},
		boxes: map[string]*fakeBox{"INBOX": box},
	}
	st, ing := newFakeStore(), newFakeIngester()
	ing.st = st
	e := New(st, ing, factoryFor(sess), WithFetchChunk(3))
	req := testReq()

	// ---- pass 1: [1:3] completes, [4:6] dies after UID 5 ----
	sum, err := e.SyncAccount(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.Errors) != 1 {
		t.Fatalf("pass 1 errors = %v, want the mid-chunk failure", sum.Errors)
	}
	if got, want := bodyFetches(sess), []sync.UIDSet{rangeSet(1, 3), rangeSet(4, 6)}; !reflect.DeepEqual(got, want) {
		t.Fatalf("pass 1 body FETCH commands = %v, want %v", got, want)
	}
	if len(ing.calls) != 5 || sum.MessagesIngested != 5 {
		t.Fatalf("pass 1 ingested %d (summary %d), want UIDs 1..5 kept", len(ing.calls), sum.MessagesIngested)
	}
	row := st.rows["INBOX"]
	c, ok := st.cursorCalls[row.id]
	if !ok {
		t.Fatal("pass 1 recorded no progress; the next pass would restart from UID 1")
	}
	if c.LastSeenUID != 3 {
		t.Errorf("partial last-seen = %d, want 3 (the end of the last COMPLETED chunk, not 5)", c.LastSeenUID)
	}
	if c.UIDNext != 0 || c.Messages != 0 {
		t.Errorf("partial pass recorded poll baseline UIDNext=%d Messages=%d, want none (0/0 → NULL): "+
			"a half-fetched folder must never look polled", c.UIDNext, c.Messages)
	}
	if c.UIDValidity != 10 || c.HighestModSeq != 50 {
		t.Errorf("partial cursors = %+v, want this SELECT's uidvalidity 10 / modseq 50", c)
	}
	// A failed poll is not a phase-2 target: not observed, verified or reaped.
	if len(sess.liveCalls) != 0 || len(st.reapCalls) != 0 || len(st.gapCalls) != 0 || len(st.stampCalls) != 0 {
		t.Errorf("partly fetched INBOX reached phase 2: live=%v reaps=%d gaps=%d stamps=%d",
			sess.liveCalls, len(st.reapCalls), len(st.gapCalls), len(st.stampCalls))
	}

	// ---- pass 2: the server recovers; nothing else changed on it ----
	applyCursors(t, st, "INBOX")
	box.failAfterUID = 0
	sess.fetches, sess.selects = nil, nil
	sum, err = e.SyncAccount(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.Errors) != 0 {
		t.Fatalf("pass 2 errors: %v", sum.Errors)
	}
	if sum.MailboxesUnchanged != 0 || len(sess.selects) != 1 {
		t.Fatalf("pass 2 skipped the partly fetched folder (unchanged=%d selects=%v): "+
			"the rest of its backlog is stranded", sum.MailboxesUnchanged, sess.selects)
	}
	if got, want := bodyFetches(sess), []sync.UIDSet{rangeSet(4, 6), rangeSet(7, 9), rangeSet(10, 0)}; !reflect.DeepEqual(got, want) {
		t.Fatalf("pass 2 body FETCH commands = %v, want a resume at UID 4: %v", got, want)
	}
	uids := map[int64]bool{}
	for _, p := range ing.calls {
		uids[p.UID] = true
	}
	if len(uids) != 10 {
		keys := make([]int64, 0, len(uids))
		for u := range uids {
			keys = append(keys, u)
		}
		sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
		t.Fatalf("ingested UIDs after pass 2 = %v, want all 10", keys)
	}
	if sum.MessagesIngested != 5 {
		t.Errorf("pass 2 created %d, want 5 (UIDs 6..10; 4 and 5 were kept from pass 1 and dedup)", sum.MessagesIngested)
	}
	c = st.cursorCalls[row.id]
	if c.LastSeenUID != 10 || c.UIDNext != 11 || c.Messages != 10 {
		t.Errorf("pass 2 cursors = %+v, want last-seen 10 and the poll baseline 11/10", c)
	}

	// ---- pass 3 (positive control): complete, unchanged → skipped ----
	applyCursors(t, st, "INBOX")
	sess.fetches, sess.selects = nil, nil
	sum, err = e.SyncAccount(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if sum.MailboxesUnchanged != 1 || len(sess.selects) != 0 {
		t.Errorf("pass 3: unchanged=%d selects=%v; want the fully polled folder skipped — "+
			"otherwise pass 2's 'polled' proves nothing", sum.MailboxesUnchanged, sess.selects)
	}
}

// A UIDVALIDITY roll records NOTHING part-way: its cursors are committed only
// with the reconcile (#88), and writing this SELECT's UIDVALIDITY would declare
// the roll finished.
func TestAPartialRollRecordsNoCursors(t *testing.T) {
	sess := &fakeSession{
		mailboxes: []sync.Mailbox{inbox()},
		boxes: map[string]*fakeBox{"INBOX": {
			status:       sync.MailboxStatus{UIDValidity: 11, UIDNext: 11, NumMessages: 10},
			msgs:         numbered(10, 0),
			failAfterUID: 5,
		}},
	}
	st, ing := newFakeStore(), newFakeIngester()
	row := st.seedRow("INBOX", &storedRow{id: newULID(), syncScope: "full", uidValidity: 10, lastSeenUID: 10})
	ing.st = st
	e := New(st, ing, factoryFor(sess), WithFetchChunk(3))

	sum, err := e.SyncAccount(context.Background(), testReq())
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.Errors) != 1 || len(st.staleCalls) != 1 {
		t.Fatalf("errors=%v staleCalls=%d, want the roll detected and its re-fetch failed", sum.Errors, len(st.staleCalls))
	}
	if len(ing.calls) != 5 {
		t.Errorf("ingested %d, want 5 kept", len(ing.calls))
	}
	if c, ok := st.cursorCalls[row.id]; ok {
		t.Errorf("a partial roll wrote cursors %+v; the roll must stay pending on the OLD generation", c)
	}
}
