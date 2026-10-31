// SPDX-License-Identifier: AGPL-3.0-only

package mailsync

import (
	"context"
	"testing"
	"time"

	"mailroom/internal/sync"
)

// recordingObserver captures the engine's counters.
type recordingObserver struct {
	recovered map[string]int // "path/outcome" -> n
	absent    map[string]int
	aborted   map[string]int
	sweepRead int
	sweepApp  int
}

func newRecordingObserver() *recordingObserver {
	return &recordingObserver{recovered: map[string]int{}, absent: map[string]int{}, aborted: map[string]int{}}
}

func (o *recordingObserver) RecoveredArrival(path, outcome string, n int) {
	o.recovered[path+"/"+outcome] += n
}
func (o *recordingObserver) FlagsAbsent(path string, n int)        { o.absent[path] += n }
func (o *recordingObserver) FlagRefreshAborted(path string, n int) { o.aborted[path] += n }
func (o *recordingObserver) FlagSweep(read, applied int)           { o.sweepRead += read; o.sweepApp += applied }

// ---- #908: recovered arrivals carry notify and blocked ----------------------

// A message the #93 gap backfill recovers for INBOX is a new arrival like any
// other: it must produce a notify id (a push), and one from a blocked sender
// must count as blocked. Before #908, ingestBatch.add dropped both, so the
// gap path produced neither.
//
// PROBE (#908): delete the two `b.blocked`/`b.notify` lines from
// ingestBatch.add and this test goes red (NotifyIDs 0, MessagesBlocked 0).
func TestRecoveredArrivalsCarryNotifyAndBlocked(t *testing.T) {
	sess := &fakeSession{
		mailboxes: []sync.Mailbox{inbox()},
		boxes: map[string]*fakeBox{
			"INBOX": {
				status: sync.MailboxStatus{UIDValidity: 10, UIDNext: 5, NumMessages: 4},
				msgs: []sync.Message{msg(1, 0, "a"), msg(2, 0, "lost-normal"),
					msg(3, 0, "lost-blocked"), msg(4, 0, "d")},
			},
		},
	}
	st, ing := newFakeStore(), newFakeIngester()
	ing.blockedBodies = map[string]bool{"lost-blocked": true}
	st.seedRow("INBOX", &storedRow{id: newULID(), syncScope: "full", uidValidity: 10, lastSeenUID: 4}, 1, 4)
	obs := newRecordingObserver()
	ing.st = st
	e := New(st, ing, factoryFor(sess), WithObserver(obs))

	sum, err := e.SyncAccount(context.Background(), testReq())
	if err != nil {
		t.Fatal(err)
	}
	if sum.GapsBackfilled != 2 {
		t.Fatalf("GapsBackfilled = %d, want 2 (summary %+v)", sum.GapsBackfilled, sum)
	}
	if len(sum.NotifyIDs) != 1 {
		t.Errorf("NotifyIDs = %d, want 1: a gap-recovered INBOX arrival must push (#908)", len(sum.NotifyIDs))
	}
	if sum.MessagesBlocked != 1 {
		t.Errorf("MessagesBlocked = %d, want 1: a gap-recovered blocked arrival must count (#908)", sum.MessagesBlocked)
	}
	if obs.recovered["gap/notify"] != 1 || obs.recovered["gap/blocked"] != 1 {
		t.Errorf("recovered counters = %v, want gap/notify 1 and gap/blocked 1", obs.recovered)
	}
}

// The CONDSTORE pass's body fallback is the other path through
// fetchAndIngestUIDs: a UID it finds no live row for is ingested with its
// body, and a NEW message there must push too.
func TestFlagFallbackArrivalNotifies(t *testing.T) {
	sess := &fakeSession{
		caps:      map[sync.Capability]bool{sync.CapabilityCondStore: true},
		mailboxes: []sync.Mailbox{inbox()},
		boxes: map[string]*fakeBox{
			"INBOX": {
				status: sync.MailboxStatus{UIDValidity: 10, UIDNext: 4, HighestModSeq: 12},
				msgs:   []sync.Message{msg(1, 12, "one"), msg(3, 12, "three-gap")},
			},
		},
	}
	st, ing := newFakeStore(), newFakeIngester()
	st.seedRow("INBOX", &storedRow{id: newULID(), syncScope: "full", uidValidity: 10, lastSeenUID: 3, highestModSeq: 10}, 1)
	obs := newRecordingObserver()
	ing.st = st
	sum, err := New(st, ing, factoryFor(sess), WithObserver(obs)).SyncAccount(context.Background(), testReq())
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.NotifyIDs) != 1 {
		t.Errorf("NotifyIDs = %d, want 1 (the fallback's new arrival)", len(sum.NotifyIDs))
	}
	if obs.recovered["flag_fallback/notify"] != 1 {
		t.Errorf("recovered counters = %v, want flag_fallback/notify 1", obs.recovered)
	}
}

// ---- #909: a FETCH response without FLAGS is no reading ---------------------

// The CONDSTORE flag pass meets a message whose response carries no FLAGS
// (the callback's view of a truncated or split response). It must not be
// refreshed — before #909 it was refreshed with flags=[], clearing \Seen —
// and it must not fall into the body fallback either.
//
// PROBE (#909): remove the `items.Flags && m.Flags == nil` skip in
// refreshFlagsCounting and this goes red (UID 2 refreshed with no flags).
func TestRefreshSkipsAMessageWithoutFlags(t *testing.T) {
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
	row.provFlags = map[int64][]string{1: {`\Seen`}, 2: {`\Seen`}}
	obs := newRecordingObserver()
	ing.st = st
	sum, err := New(st, ing, factoryFor(sess), WithObserver(obs)).SyncAccount(context.Background(), testReq())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range ing.refreshCalls {
		if p.UID == 2 {
			t.Fatalf("UID 2 was refreshed with flags %q although its response carried no FLAGS (#909)", p.RemoteFlags)
		}
	}
	if got := row.provFlags[2]; len(got) != 1 || got[0] != `\Seen` {
		t.Errorf("UID 2's recorded flags = %q, want untouched [\\Seen]", got)
	}
	if len(ing.calls) != 0 {
		t.Errorf("Ingest calls = %d: a flag-less reading must not fall back to a body fetch", len(ing.calls))
	}
	if sum.FlagsAbsent != 1 || obs.absent["refresh"] != 1 {
		t.Errorf("FlagsAbsent = %d / counter %v, want 1", sum.FlagsAbsent, obs.absent)
	}
}

// A NEW message whose response carried a body but no FLAGS is ingested (the
// mail must not be held back), and its flags are re-read once the command has
// finished.
func TestIngestRereadsFlagsTheResponseLacked(t *testing.T) {
	box := &fakeBox{
		status:      sync.MailboxStatus{UIDValidity: 10, UIDNext: 2},
		msgs:        []sync.Message{msg(1, 0, "one", `\Seen`)},
		flagsAbsent: map[sync.UID]bool{1: true},
	}
	sess := &fakeSession{mailboxes: []sync.Mailbox{inbox()}, boxes: map[string]*fakeBox{"INBOX": box}}
	st, ing := newFakeStore(), newFakeIngester()
	obs := newRecordingObserver()
	ing.st = st

	// The re-read FETCH (FLAGS only) is answered properly: a split response's
	// FLAGS normally arrives on the next command.
	reread := &rereadSession{fakeSession: sess, box: box}
	sum, err := New(st, ing, func(context.Context, sync.Config) (sync.SyncSession, error) { return reread, nil },
		WithObserver(obs)).SyncAccount(context.Background(), testReq())
	if err != nil {
		t.Fatal(err)
	}
	if sum.MessagesIngested != 1 {
		t.Fatalf("MessagesIngested = %d, want 1 (the body was real)", sum.MessagesIngested)
	}
	if len(ing.refreshCalls) != 1 || len(ing.refreshCalls[0].RemoteFlags) != 1 {
		t.Fatalf("re-read refreshes = %+v, want one carrying \\Seen", ing.refreshCalls)
	}
	if sum.FlagsAbsent != 1 || obs.absent["ingest"] != 1 {
		t.Errorf("FlagsAbsent = %d / counter %v, want 1 on the ingest path", sum.FlagsAbsent, obs.absent)
	}
}

// rereadSession answers a FLAGS-only FETCH with the flags its box withheld
// from the body FETCH.
type rereadSession struct {
	*fakeSession
	box *fakeBox
}

func (s *rereadSession) FetchByUID(ctx context.Context, uids sync.UIDSet, items sync.FetchItems, fn func(sync.Message) error) error {
	if items.Flags && !items.BodyFull {
		saved := s.box.flagsAbsent
		s.box.flagsAbsent = nil
		defer func() { s.box.flagsAbsent = saved }()
	}
	return s.fakeSession.FetchByUID(ctx, uids, items, fn)
}

// A refresh that applied readings and then saw its command fail reports how
// many it applied (#909's report counter).
func TestAbortedRefreshIsCounted(t *testing.T) {
	sess := &fakeSession{
		caps:      map[sync.Capability]bool{sync.CapabilityCondStore: true},
		mailboxes: []sync.Mailbox{inbox()},
		boxes: map[string]*fakeBox{
			"INBOX": {
				status:       sync.MailboxStatus{UIDValidity: 10, UIDNext: 3, HighestModSeq: 12},
				msgs:         []sync.Message{msg(1, 12, "one", `\Seen`), msg(2, 12, "two")},
				failAfterUID: 1,
			},
		},
	}
	st, ing := newFakeStore(), newFakeIngester()
	st.seedRow("INBOX", &storedRow{id: newULID(), syncScope: "full", uidValidity: 10, lastSeenUID: 2, highestModSeq: 10}, 1, 2)
	obs := newRecordingObserver()
	ing.st = st
	sum, _ := New(st, ing, factoryFor(sess), WithObserver(obs)).SyncAccount(context.Background(), testReq())
	if sum.FlagRefreshAborted != 1 || obs.aborted["refresh"] != 1 {
		t.Errorf("FlagRefreshAborted = %d / counter %v, want 1", sum.FlagRefreshAborted, obs.aborted)
	}
}

// ---- the CONDSTORE-less flag sweep (ADR-046 §12) ----------------------------

// sweepFixture is a mailbox on a server WITHOUT CONDSTORE whose UID 1 is
// recorded unread and UID 2 recorded read; on the server UID 1 has since been
// read on a phone.
func sweepFixture() (*fakeSession, *fakeStore, *fakeIngester, *storedRow) {
	sess := &fakeSession{
		mailboxes: []sync.Mailbox{inbox()},
		boxes: map[string]*fakeBox{
			"INBOX": {
				status: sync.MailboxStatus{UIDValidity: 10, UIDNext: 3, NumMessages: 2},
				msgs:   []sync.Message{msg(1, 0, "one", `\Seen`), msg(2, 0, "two", `\Seen`)},
			},
		},
	}
	st, ing := newFakeStore(), newFakeIngester()
	row := st.seedRow("INBOX", &storedRow{id: newULID(), syncScope: "full", uidValidity: 10, lastSeenUID: 2}, 1, 2)
	row.provFlags = map[int64][]string{1: {}, 2: {`\Seen`}}
	ing.st = st
	return sess, st, ing, row
}

var sweepOn = FlagSweepConfig{Interval: 5 * time.Minute, Window: 500, FullInterval: 6 * time.Hour, FullChunk: 2000}

// The remote \Seen on UID 1 is observed and applied; UID 2, unchanged, is
// not written at all (the comparison is what keeps an unchanged reading from
// overwriting a local edit whose writeback is still pending).
func TestFlagSweepAppliesARemoteSeen(t *testing.T) {
	sess, st, ing, row := sweepFixture()
	obs := newRecordingObserver()
	sum, err := New(st, ing, factoryFor(sess), WithFlagSweep(sweepOn), WithObserver(obs)).
		SyncAccount(context.Background(), testReq())
	if err != nil {
		t.Fatal(err)
	}
	if got := row.provFlags[1]; len(got) != 1 || got[0] != `\Seen` {
		t.Fatalf("UID 1 flags = %q, want [\\Seen] — the phone's read never reached Mailroom", got)
	}
	if len(ing.refreshCalls) != 1 || ing.refreshCalls[0].UID != 1 {
		t.Errorf("refreshes = %+v, want only UID 1 (UID 2 did not change)", ing.refreshCalls)
	}
	if sum.FlagSweepApplied != 1 || sum.FlagSweepRead != 2 || sum.FlagSweeps != 1 {
		t.Errorf("sweep summary read=%d applied=%d sweeps=%d, want 2/1/1", sum.FlagSweepRead, sum.FlagSweepApplied, sum.FlagSweeps)
	}
	for _, f := range sess.fetches {
		if f.items.BodyFull {
			t.Errorf("the sweep fetched a body: %+v", f)
		}
	}
	if obs.sweepRead != 2 || obs.sweepApp != 1 {
		t.Errorf("observer read=%d applied=%d", obs.sweepRead, obs.sweepApp)
	}
}

// NEGATIVE CONTROL (design increment 6): with the sweep disabled, the same
// remote \Seen change is never observed — no flag FETCH at all over held UIDs.
// It is what proves the test above is about the sweep and not about some
// other path.
func TestFlagSweepDisabledNeverObservesARemoteSeen(t *testing.T) {
	sess, st, ing, row := sweepFixture()
	if _, err := New(st, ing, factoryFor(sess)).SyncAccount(context.Background(), testReq()); err != nil {
		t.Fatal(err)
	}
	if got := row.provFlags[1]; len(got) != 0 {
		t.Fatalf("UID 1 flags = %q with the sweep OFF; something else read them", got)
	}
	if len(ing.refreshCalls) != 0 {
		t.Errorf("refreshes = %+v with the sweep off", ing.refreshCalls)
	}
}

// The sweep never runs where CONDSTORE (or QRESYNC) already reports changes.
func TestFlagSweepSkipsCondStoreServers(t *testing.T) {
	for _, c := range []sync.Capability{sync.CapabilityCondStore, sync.CapabilityQResync} {
		sess, st, ing, _ := sweepFixture()
		sess.caps = map[sync.Capability]bool{c: true}
		sum, err := New(st, ing, factoryFor(sess), WithFlagSweep(sweepOn)).SyncAccount(context.Background(), testReq())
		if err != nil {
			t.Fatal(err)
		}
		if sum.FlagSweeps != 0 || st.liveFlagCalls != 0 {
			t.Errorf("%s: the sweep ran (sweeps=%d, baseline reads=%d)", c, sum.FlagSweeps, st.liveFlagCalls)
		}
	}
}

// The recent window re-reads only [cursor-Window+1, cursor], and not again
// until Interval has passed.
func TestFlagSweepWindowAndCadence(t *testing.T) {
	sess, st, ing, _ := sweepFixture()
	sess.boxes["INBOX"].status.UIDNext = 1001
	st.rows["INBOX"].lastSeenUID = 1000
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	cfg := sweepOn
	cfg.Window, cfg.FullInterval = 100, 0
	e := New(st, ing, factoryFor(sess), WithFlagSweep(cfg), WithClock(func() time.Time { return now }))
	if _, err := e.SyncAccount(context.Background(), testReq()); err != nil {
		t.Fatal(err)
	}
	var sweeps []sync.UIDSet
	for _, f := range sess.fetches {
		if f.items.Flags && !f.items.BodyFull {
			sweeps = append(sweeps, f.uids)
		}
	}
	if len(sweeps) != 1 || sweeps[0][0].Start != 901 || sweeps[0][0].Stop != 1000 {
		t.Fatalf("sweep ranges = %v, want exactly [901:1000]", sweeps)
	}
	sess.fetches = nil
	now = now.Add(time.Minute)
	if _, err := e.SyncAccount(context.Background(), testReq()); err != nil {
		t.Fatal(err)
	}
	for _, f := range sess.fetches {
		if f.items.Flags && !f.items.BodyFull {
			t.Errorf("swept again after 1 minute (interval 5m): %v", f.uids)
		}
	}
}

// The full pass walks the whole mailbox FullChunk UIDs per pass and then
// rests for FullInterval.
func TestFlagSweepFullPassWalksInChunks(t *testing.T) {
	sess, st, ing, _ := sweepFixture()
	sess.boxes["INBOX"].status.UIDNext = 2501
	st.rows["INBOX"].lastSeenUID = 2500
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	cfg := FlagSweepConfig{Interval: 5 * time.Minute, Window: 10, FullInterval: 6 * time.Hour, FullChunk: 1000}
	e := New(st, ing, factoryFor(sess), WithFlagSweep(cfg), WithClock(func() time.Time { return now }))
	var full []sync.UIDRange
	var done int
	for pass := 0; pass < 4; pass++ {
		sess.fetches = nil
		sum, err := e.SyncAccount(context.Background(), testReq())
		if err != nil {
			t.Fatal(err)
		}
		done += sum.FlagFullSweeps
		recent := sync.UIDRange{Start: 2491, Stop: 2500} // the Window-10 re-read
		for _, f := range sess.fetches {
			if f.items.Flags && !f.items.BodyFull && f.uids[0] != recent {
				full = append(full, f.uids[0])
			}
		}
		now = now.Add(time.Minute)
	}
	want := []sync.UIDRange{{Start: 1, Stop: 1000}, {Start: 1001, Stop: 2000}, {Start: 2001, Stop: 2490}}
	if len(full) != len(want) {
		t.Fatalf("full-pass ranges = %v, want %v", full, want)
	}
	for i := range want {
		if full[i] != want[i] {
			t.Errorf("full-pass range %d = %v, want %v", i, full[i], want[i])
		}
	}
	if done != 1 {
		t.Errorf("FlagFullSweeps = %d, want 1 (and none restarted within FullInterval)", done)
	}
}

// A flag-less reading in the sweep is no reading, and a failed sweep command
// applies nothing.
func TestFlagSweepAbsentAndFailedApplyNothing(t *testing.T) {
	sess, st, ing, row := sweepFixture()
	sess.boxes["INBOX"].flagsAbsent = map[sync.UID]bool{1: true}
	sum, err := New(st, ing, factoryFor(sess), WithFlagSweep(sweepOn)).SyncAccount(context.Background(), testReq())
	if err != nil {
		t.Fatal(err)
	}
	if len(row.provFlags[1]) != 0 || sum.FlagsAbsent != 1 {
		t.Errorf("absent reading: UID 1 flags %q, FlagsAbsent %d", row.provFlags[1], sum.FlagsAbsent)
	}

	sess, st, ing, row = sweepFixture()
	sess.boxes["INBOX"].failAfterUID = 2
	sum, _ = New(st, ing, factoryFor(sess), WithFlagSweep(sweepOn)).SyncAccount(context.Background(), testReq())
	if len(row.provFlags[1]) != 0 || len(ing.refreshCalls) != 0 {
		t.Errorf("failed sweep command applied readings: UID 1 flags %q, refreshes %d", row.provFlags[1], len(ing.refreshCalls))
	}
	if len(sum.Errors) == 0 {
		t.Error("a failed sweep command was not reported")
	}
}

func TestSameFlags(t *testing.T) {
	for _, c := range []struct {
		a, b []string
		want bool
	}{
		{nil, []string{}, true},
		{[]string{`\Seen`, `$Label1`}, []string{`$label1`, `\seen`}, true},
		{[]string{`\Seen`}, []string{`\Seen`, `\Recent`}, true},
		{[]string{`\Seen`}, []string{}, false},
		{[]string{`\Seen`}, []string{`\Flagged`}, false},
	} {
		if got := sameFlags(c.a, c.b); got != c.want {
			t.Errorf("sameFlags(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}
