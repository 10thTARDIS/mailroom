// SPDX-License-Identifier: AGPL-3.0-only

package mailsync

// Skipping what did not change (#620). Two skips, decided from one LIST-STATUS,
// against TWO baselines:
//
//   - phase 1 skips the SELECT when the tuple equals what the last POLL saw;
//   - phase 2 skips a due observation when it equals what the last OBSERVATION
//     saw — never the last poll, because a poll whose FETCH silently lost a
//     message (#93) still advances the poll cursors.
//
// Every test below that asserts a skip has a sibling that asserts the work IS
// done when one precondition is removed: a skip that fires too often loses mail,
// and one that never fires is only a missed optimisation, so the fail-safe
// direction is the one pinned hardest.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
	"mailroom/internal/sync"
)

func i64(v int64) *int64 { return &v }

// skipCase is one CONDSTORE + LIST-STATUS account with a single INBOX whose
// stored baselines — poll-time AND observation-time — match the server exactly,
// and whose poll has nothing outstanding (last-seen 3, UIDNEXT 4). As built it is
// NOT due for observation; due() makes it due.
type skipCase struct {
	now  time.Time
	sess *fakeSession
	st   *fakeStore
	ing  *fakeIngester
	box  *fakeBox
	row  *storedRow
}

func newSkipCase() *skipCase {
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	box := &fakeBox{
		status: sync.MailboxStatus{UIDValidity: 10, UIDNext: 4, NumMessages: 3, HighestModSeq: 50},
		msgs:   []sync.Message{msg(1, 10, "one"), msg(2, 20, "two"), msg(3, 30, "three")},
	}
	sess := &fakeSession{
		caps: map[sync.Capability]bool{sync.CapabilityCondStore: true},
		mailboxes: []sync.Mailbox{{Name: "INBOX", Delimiter: '/',
			Status: &sync.ListStatus{UIDValidity: 10, UIDNext: 4, NumMessages: 3, HighestModSeq: 50}}},
		boxes: map[string]*fakeBox{"INBOX": box},
	}
	st, ing := newFakeStore(), newFakeIngester()
	fresh := now.Add(-time.Minute) // observed a minute ago: not due
	fullObs := now.Add(-time.Hour)
	row := st.seedRow("INBOX", &storedRow{
		id: newULID(), syncScope: "full",
		uidValidity: 10, lastSeenUID: 3, highestModSeq: 50,
		pollUIDNext: i64(4), pollMessages: i64(3),
		lastReapedAt: &fresh,
		observed:     &store.ObservedStatus{UIDValidity: 10, UIDNext: 4, Messages: 3, HighestModSeq: 50},
		lastFullObs:  &fullObs,
	}, 1, 2, 3)
	return &skipCase{now: now, sess: sess, st: st, ing: ing, box: box, row: row}
}

// due backdates the cadence cursor two intervals: due whatever its jitter.
func (c *skipCase) due() *skipCase {
	old := c.now.Add(-2 * time.Hour)
	c.row.lastReapedAt = &old
	return c
}

// setServer moves the server's status AND what its LIST-STATUS reports, together.
func (c *skipCase) setServer(fn func(st *sync.MailboxStatus)) {
	fn(&c.box.status)
	c.sess.mailboxes[0].Status = &sync.ListStatus{
		UIDValidity: c.box.status.UIDValidity, UIDNext: c.box.status.UIDNext,
		NumMessages: c.box.status.NumMessages, HighestModSeq: c.box.status.HighestModSeq,
	}
}

func (c *skipCase) run(t *testing.T) Summary {
	t.Helper()
	c.ing.st = c.st
	e := New(c.st, c.ing, factoryFor(c.sess), WithReapInterval(time.Hour), WithClock(func() time.Time { return c.now }))
	sum, err := e.SyncAccount(context.Background(), testReq())
	if err != nil {
		t.Fatalf("SyncAccount: %v", err)
	}
	return sum
}

func (c *skipCase) selected() bool {
	for _, s := range c.sess.selects {
		if s == "INBOX" {
			return true
		}
	}
	return false
}

// ---- phase 1: the poll -----------------------------------------------------

// THE MECHANISM. Nothing about the folder moved since the last poll's SELECT, so
// the SELECT is not sent — and the folder is still upserted and still counted in
// good standing.
func TestUnchangedFolderIsNotSelected(t *testing.T) {
	c := newSkipCase()
	sum := c.run(t)

	if c.selected() {
		t.Fatalf("INBOX was SELECTed (selects %v) although LIST-STATUS matched the last poll exactly", c.sess.selects)
	}
	if len(c.sess.fetches) != 0 {
		t.Errorf("fetches = %+v, want none", c.sess.fetches)
	}
	if sum.MailboxesUnchanged != 1 || sum.MailboxesSynced != 1 {
		t.Errorf("unchanged/synced = %d/%d, want 1/1 (a skipped folder is a SUBSET of synced)",
			sum.MailboxesUnchanged, sum.MailboxesSynced)
	}
	if _, wrote := c.st.cursorCalls[c.row.id]; wrote {
		t.Error("the cursors were rewritten for a folder that was never SELECTed")
	}
	if len(sum.Errors) != 0 {
		t.Errorf("errors: %v", sum.Errors)
	}
}

// Each of the four numbers, moved on its own, forces the SELECT: every one of
// them is the only witness to some change (a UIDVALIDITY roll, an arrival, an
// expunge, a flag change).
func TestAnyMovedStatusFieldForcesTheSelect(t *testing.T) {
	for name, move := range map[string]func(*sync.MailboxStatus){
		"UIDVALIDITY":   func(s *sync.MailboxStatus) { s.UIDValidity = 11 },
		"UIDNEXT":       func(s *sync.MailboxStatus) { s.UIDNext = 5 },
		"MESSAGES":      func(s *sync.MailboxStatus) { s.NumMessages = 2 },
		"HIGHESTMODSEQ": func(s *sync.MailboxStatus) { s.HighestModSeq = 51 },
	} {
		t.Run(name, func(t *testing.T) {
			c := newSkipCase()
			c.setServer(move)
			sum := c.run(t)
			if !c.selected() {
				t.Fatalf("%s moved but INBOX was not SELECTed", name)
			}
			if sum.MailboxesUnchanged != 0 {
				t.Errorf("MailboxesUnchanged = %d, want 0", sum.MailboxesUnchanged)
			}
		})
	}
}

// Without LIST-STATUS there is no status to compare: today's behaviour exactly.
func TestNoListStatusSelectsEveryFolder(t *testing.T) {
	c := newSkipCase()
	c.sess.mailboxes[0].Status = nil
	if sum := c.run(t); !c.selected() || sum.MailboxesUnchanged != 0 {
		t.Fatalf("selected=%v unchanged=%d, want SELECTed and 0 — no status must mean no skip",
			c.selected(), sum.MailboxesUnchanged)
	}
}

// Without CONDSTORE a flag change moves none of the other three numbers, so an
// "unchanged" tuple proves nothing about flags.
func TestNoCondStoreSelectsEveryFolder(t *testing.T) {
	c := newSkipCase()
	c.sess.caps = nil
	if sum := c.run(t); !c.selected() || sum.MailboxesUnchanged != 0 {
		t.Fatalf("selected=%v unchanged=%d, want SELECTed and 0 without CONDSTORE",
			c.selected(), sum.MailboxesUnchanged)
	}
}

// A zero HIGHESTMODSEQ is "unknown" (a server that did not report it, or a
// NOMODSEQ mailbox), even when the stored cursor is zero too: equal zeros prove
// nothing about flags.
func TestZeroModSeqSelectsTheFolder(t *testing.T) {
	c := newSkipCase()
	c.setServer(func(s *sync.MailboxStatus) { s.HighestModSeq = 0 })
	c.row.highestModSeq = 0
	if sum := c.run(t); !c.selected() || sum.MailboxesUnchanged != 0 {
		t.Fatalf("selected=%v unchanged=%d, want SELECTed and 0 with no modseq on either side",
			c.selected(), sum.MailboxesUnchanged)
	}
}

// NULL poll baselines — every folder on the first pass after migration 00058 —
// are unknown, and unknown means SELECT. That SELECT then records the baseline
// from its OWN status, which is what lets the next pass skip.
func TestUnknownPollBaselineSelectsAndRecordsIt(t *testing.T) {
	for name, unset := range map[string]func(*storedRow){
		"no poll UIDNEXT":  func(r *storedRow) { r.pollUIDNext = nil },
		"no poll MESSAGES": func(r *storedRow) { r.pollMessages = nil },
	} {
		t.Run(name, func(t *testing.T) {
			c := newSkipCase()
			unset(c.row)
			sum := c.run(t)
			if !c.selected() || sum.MailboxesUnchanged != 0 {
				t.Fatalf("selected=%v unchanged=%d, want SELECTed and 0", c.selected(), sum.MailboxesUnchanged)
			}
			got, ok := c.st.cursorCalls[c.row.id]
			if !ok {
				t.Fatal("the poll wrote no cursors")
			}
			if got.UIDNext != 4 || got.Messages != 3 {
				t.Errorf("poll baseline written = (UIDNEXT %d, MESSAGES %d), want (4, 3) from the SELECT",
					got.UIDNext, got.Messages)
			}
		})
	}
}

// THE TOP OF THE RANGE (an addition to #620's design; see pollOwesNothing). The
// last observation saw UIDs 1–2 (UIDNEXT 3). Then UID 3 arrived, and the poll's
// FETCH of 3:* silently dropped it — the highest UID it asked for — so the cursor
// stayed at 2 while that SELECT's status (UIDNEXT 4) was recorded. Nothing on
// the server has changed since, so the tuple matches the POLL baseline. Today's
// poll re-asks for 3:* on the next pass, and that is the ONLY thing that ever
// will: the observation leaves every UID at or above lastSeen+1 to the poll
// (gapBound). Skipping the SELECT here would leave UID 3 unfetched until the
// folder next changed.
func TestUnchangedTupleWithAnOutstandingTopIsStillPolled(t *testing.T) {
	c := newSkipCase()
	c.row.lastSeenUID = 2
	delete(c.row.prov, 3) // we never ingested it
	c.row.observed = &store.ObservedStatus{UIDValidity: 10, UIDNext: 3, Messages: 2, HighestModSeq: 40}

	sum := c.run(t)

	if !c.selected() {
		t.Fatal("INBOX was not SELECTed although UID 3 is inside the range the poll still owes")
	}
	if sum.MailboxesUnchanged != 0 {
		t.Errorf("MailboxesUnchanged = %d, want 0", sum.MailboxesUnchanged)
	}
	if !c.row.prov[3] {
		t.Error("UID 3 was not fetched — the dropped top of the last FETCH is lost")
	}
}

// ...but a cursor below UIDNEXT is not by itself a debt. Two folders whose poll
// range [lastSeen+1, UIDNEXT) provably holds no message are skipped, or the
// guard above would keep them SELECTed every pass forever.
func TestUnchangedFolderWithNothingAboveTheCursorIsSkipped(t *testing.T) {
	t.Run("emptied before our first sync", func(t *testing.T) {
		// UIDs up to 56 were used and are all gone; we never ingested any, so the
		// cursor is 0 and will stay 0 until new mail arrives.
		c := newSkipCase()
		c.box.msgs = nil
		c.setServer(func(s *sync.MailboxStatus) { s.UIDNext, s.NumMessages = 57, 0 })
		c.row.lastSeenUID, c.row.pollUIDNext, c.row.pollMessages = 0, i64(57), i64(0)
		c.row.prov = map[int64]bool{}
		c.row.observed = nil // not even observed yet: MESSAGES 0 is enough
		if sum := c.run(t); c.selected() || sum.MailboxesUnchanged != 1 {
			t.Errorf("selected=%v unchanged=%d, want skipped: an empty folder owes the poll nothing",
				c.selected(), sum.MailboxesUnchanged)
		}
	})
	for _, tc := range []struct {
		name     string
		observed *store.ObservedStatus
		skipped  bool
	}{
		// UID 3 was expunged before we ever fetched it: the cursor is 2, UIDNEXT
		// 4. The last observation saw {1, 2} — all at or below the cursor — and
		// the tuple has not moved since, so nothing exists above the cursor.
		{"newest expunged unfetched, observed since", &store.ObservedStatus{UIDValidity: 10, UIDNext: 4, Messages: 2, HighestModSeq: 50}, true},
		// The same folder with no observation baseline: nothing proves the range
		// empty, so it is polled as it always was.
		{"newest expunged unfetched, never observed", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newSkipCase()
			c.box.msgs = c.box.msgs[:2]
			c.setServer(func(s *sync.MailboxStatus) { s.NumMessages = 2 })
			c.row.lastSeenUID, c.row.pollMessages = 2, i64(2)
			delete(c.row.prov, 3)
			c.row.observed = tc.observed
			sum := c.run(t)
			if c.selected() == tc.skipped || (sum.MailboxesUnchanged == 1) != tc.skipped {
				t.Errorf("selected=%v unchanged=%d, want skipped=%v", c.selected(), sum.MailboxesUnchanged, tc.skipped)
			}
		})
	}
}

// ---- phase 2: the observation ----------------------------------------------

// THE MECHANISM. Due, and LIST-STATUS equals what the last REAL observation saw:
// no EXAMINE/SEARCH, the cadence is re-stamped, and nothing is verified, stamped
// as observed, or reaped on the strength of a question we did not ask.
func TestUnchangedObservationIsRestampedNotAsked(t *testing.T) {
	c := newSkipCase().due()
	sum := c.run(t)

	if len(c.sess.liveCalls) != 0 {
		t.Fatalf("LiveUIDs called for %v, want no SEARCH at all", c.sess.liveCalls)
	}
	if sum.ObservationsSkipped != 1 {
		t.Errorf("ObservationsSkipped = %d, want 1", sum.ObservationsSkipped)
	}
	if sum.MailboxesObserved != 0 {
		t.Errorf("MailboxesObserved = %d, want 0: a skipped observation is not an answered SEARCH", sum.MailboxesObserved)
	}
	if len(c.st.unchangedCalls) != 1 || c.st.unchangedCalls[0] != c.row.id {
		t.Errorf("unchanged re-stamps = %v, want exactly INBOX's", c.st.unchangedCalls)
	}
	if len(c.st.reapCalls) != 0 || len(c.st.stampCalls) != 0 || sum.MailboxesVerified != 0 {
		t.Errorf("a skipped observation reaped/stamped/verified: reaps=%d stamps=%d verified=%d",
			len(c.st.reapCalls), len(c.st.stampCalls), sum.MailboxesVerified)
	}
}

// ⚠ THE #93 CASE, and the reason the baseline is the OBSERVATION. The last pass
// polled UIDs 4 and 5, its FETCH silently dropped 4, and its cursors moved on to
// the new status anyway (last-seen 5, UIDNEXT 6). So LIST-STATUS matches the
// POLL baseline exactly — the poll is rightly skipped — but not the observation
// baseline, which still describes the folder before 4 and 5 arrived. The
// observation MUST run, and it is what finds and recovers UID 4.
func TestObservationRunsWhenOnlyThePollBaselineIsCurrent(t *testing.T) {
	c := newSkipCase().due()
	c.box.msgs = append(c.box.msgs, msg(4, 40, "four — dropped by the last FETCH"), msg(5, 60, "five"))
	c.setServer(func(s *sync.MailboxStatus) { s.UIDNext, s.NumMessages, s.HighestModSeq = 6, 5, 60 })
	// The poll cursors moved with the lossy pass...
	c.row.lastSeenUID, c.row.highestModSeq = 5, 60
	c.row.pollUIDNext, c.row.pollMessages = i64(6), i64(5)
	c.row.prov[5] = true // ...which ingested 5 but not 4.
	// ...while the observation baseline (UIDNEXT 4, MESSAGES 3) is from before.

	sum := c.run(t)

	if c.selected() || sum.MailboxesUnchanged != 1 {
		t.Fatalf("precondition: the poll should be skipped (tuple equals the poll baseline); selected=%v unchanged=%d",
			c.selected(), sum.MailboxesUnchanged)
	}
	if len(c.sess.liveCalls) != 1 {
		t.Fatalf("LiveUIDs called %d times, want 1 — the folder changed since its last OBSERVATION and was not observed; "+
			"UID 4 would stay lost", len(c.sess.liveCalls))
	}
	if sum.ObservationsSkipped != 0 {
		t.Errorf("ObservationsSkipped = %d, want 0", sum.ObservationsSkipped)
	}
	if sum.GapsDetected != 1 || sum.GapsBackfilled != 1 || !c.row.prov[4] {
		t.Errorf("gaps detected/backfilled = %d/%d, UID 4 held = %v; want the lost message recovered",
			sum.GapsDetected, sum.GapsBackfilled, c.row.prov[4])
	}
	// And the real observation records the NEW baseline, so the next pass may skip.
	want := store.ObservedStatus{UIDValidity: 10, UIDNext: 6, Messages: 5, HighestModSeq: 60}
	if got := c.st.seenCalls[c.row.id]; got == nil || *got != want {
		t.Errorf("observation baseline written = %+v, want %+v", got, want)
	}
}

// THE BACKSTOP. However long a folder reads as unchanged, a real observation runs
// once its last one is 24 hours old — and it is the REAL-observation time that
// counts, which a skip's re-stamp does not move.
func TestObservationBackstopForcesARealObservation(t *testing.T) {
	for _, tc := range []struct {
		age      time.Duration
		observed bool
	}{
		{24*time.Hour - time.Second, false},
		{24 * time.Hour, true},
		{72 * time.Hour, true},
	} {
		c := newSkipCase().due()
		full := c.now.Add(-tc.age)
		c.row.lastFullObs = &full
		sum := c.run(t)
		if got := len(c.sess.liveCalls) == 1; got != tc.observed {
			t.Errorf("last real observation %v ago: observed=%v, want %v (skipped=%d)",
				tc.age, got, tc.observed, sum.ObservationsSkipped)
		}
		if tc.observed && c.st.seenCalls[c.row.id] == nil {
			t.Errorf("last real observation %v ago: the forced observation recorded no baseline", tc.age)
		}
	}
}

// Every precondition of the observation skip, removed one at a time, means the
// observation runs. "Unknown" is never "unchanged".
func TestObservationSkipNeedsEveryCondition(t *testing.T) {
	for name, spoil := range map[string]func(*skipCase){
		"no CONDSTORE":   func(c *skipCase) { c.sess.caps = nil },
		"no LIST-STATUS": func(c *skipCase) { c.sess.mailboxes[0].Status = nil },
		// Both sides zero, so the comparison alone WOULD match: only the guard
		// that treats a zero modseq as unknown keeps this observed.
		"LIST-STATUS without a modseq": func(c *skipCase) {
			c.sess.mailboxes[0].Status.HighestModSeq = 0
			c.row.observed.HighestModSeq = 0
		},
		"never observed (NULL baseline)": func(c *skipCase) { c.row.observed = nil },
		"no real-observation time":       func(c *skipCase) { c.row.lastFullObs = nil },
		"observed UIDVALIDITY differs":   func(c *skipCase) { c.row.observed.UIDValidity = 9 },
		"observed UIDNEXT differs":       func(c *skipCase) { c.row.observed.UIDNext = 3 },
		"observed MESSAGES differs":      func(c *skipCase) { c.row.observed.Messages = 4 },
		"observed HIGHESTMODSEQ differs": func(c *skipCase) { c.row.observed.HighestModSeq = 49 },
	} {
		t.Run(name, func(t *testing.T) {
			c := newSkipCase().due()
			spoil(c)
			sum := c.run(t)
			if len(c.sess.liveCalls) != 1 || sum.ObservationsSkipped != 0 {
				t.Errorf("observations=%d skipped=%d, want 1 and 0", len(c.sess.liveCalls), sum.ObservationsSkipped)
			}
		})
	}
}

// A mailbox the server refuses to SEARCH is DEFERRED exactly as before #620: not
// stamped, not re-stamped, due next pass. The skip changes nothing about it — and
// an unchanged mailbox the pass never reached after the refusal is deferred too,
// not quietly re-stamped.
func TestDeferredObservationIsUnaffectedBySkipping(t *testing.T) {
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	old, full := now.Add(-2*time.Hour), now.Add(-time.Hour)
	status := sync.MailboxStatus{UIDValidity: 10, UIDNext: 2, NumMessages: 1, HighestModSeq: 5}
	listed := sync.ListStatus{UIDValidity: 10, UIDNext: 2, NumMessages: 1, HighestModSeq: 5}
	baseline := store.ObservedStatus{UIDValidity: 10, UIDNext: 2, Messages: 1, HighestModSeq: 5}
	stale := store.ObservedStatus{UIDValidity: 10, UIDNext: 1, Messages: 0, HighestModSeq: 1}

	sess := &fakeSession{caps: map[sync.Capability]bool{sync.CapabilityCondStore: true}, boxes: map[string]*fakeBox{}}
	st, ing := newFakeStore(), newFakeIngester()
	ids := map[string]ulid.ULID{}
	for _, f := range []struct {
		name    string
		obs     store.ObservedStatus
		refused bool
	}{
		{"INBOX", baseline, false}, // unchanged: skipped
		{"Busy", stale, true},      // changed: asked, and refused
		{"Later", baseline, false}, // unchanged, but after the refusal: deferred
	} {
		box := &fakeBox{status: status, msgs: []sync.Message{msg(1, 5, f.name)}}
		if f.refused {
			box.liveErr = rateLimitedSearch()
		}
		sess.boxes[f.name] = box
		l := listed
		sess.mailboxes = append(sess.mailboxes, sync.Mailbox{Name: f.name, Status: &l})
		o := f.obs
		ids[f.name] = newULID()
		st.seedRow(f.name, &storedRow{
			id: ids[f.name], syncScope: "full", uidValidity: 10, lastSeenUID: 1, highestModSeq: 5,
			pollUIDNext: i64(2), pollMessages: i64(1), lastReapedAt: &old, observed: &o, lastFullObs: &full,
		}, 1)
	}
	ing.st = st
	e := New(st, ing, factoryFor(sess), WithReapInterval(time.Hour), WithClock(func() time.Time { return now }))
	sum, err := e.SyncAccount(context.Background(), testReq())
	if err != nil {
		t.Fatalf("SyncAccount: %v", err)
	}

	if len(sess.liveCalls) != 1 || sess.liveCalls[0] != "Busy" {
		t.Fatalf("LiveUIDs calls = %v, want exactly the changed folder", sess.liveCalls)
	}
	if sum.MailboxesDeferred != 2 {
		t.Errorf("MailboxesDeferred = %d, want 2 (the refused folder and the one never reached)", sum.MailboxesDeferred)
	}
	if sum.ObservationsSkipped != 1 {
		t.Errorf("ObservationsSkipped = %d, want 1 (only the folder before the refusal)", sum.ObservationsSkipped)
	}
	if len(st.unchangedCalls) != 1 || st.unchangedCalls[0] != ids["INBOX"] {
		t.Errorf("re-stamped %v, want only INBOX — a deferred folder must stay due", st.unchangedCalls)
	}
	if len(st.stampCalls) != 0 || len(st.reapCalls) != 0 {
		t.Errorf("stamped %v / reaped %d after a refusal, want nothing", st.stampCalls, len(st.reapCalls))
	}
	if len(sum.Errors) != 0 {
		t.Errorf("errors = %v, want none: a refusal is a deferral", sum.Errors)
	}
}

// The baseline is only recorded when every UID the observation saw is at or
// below the cursor — the proof covered all of them (an addition to #620's
// design; see observedBaseline). In the first case UID 4 arrived between this
// pass's poll (UIDNEXT 4) and its observation (UIDNEXT 5): the gap proof stops
// below last-seen+1 = 4 and leaves UID 4 to the next poll. If that poll's FETCH
// silently drops it and moves the cursor past it, the tuple will not have
// changed — so recording (…, UIDNEXT 5, …) here would let the next observation
// be skipped over exactly that loss. The other cases are covered and must
// record one, or the skips could never fire for them.
func TestObservationRecordsNoBaselineItsProofDidNotCover(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(c *skipCase)
		want  *store.ObservedStatus
	}{
		{"mail arrived after the poll", func(c *skipCase) {
			c.box.liveUIDs, c.box.liveUIDsSet = []sync.UID{1, 2, 3, 4}, true
			c.box.liveStatus = &sync.MailboxStatus{UIDValidity: 10, UIDNext: 5, NumMessages: 4, HighestModSeq: 60}
		}, nil},
		{"nothing arrived (control)", func(*skipCase) {},
			&store.ObservedStatus{UIDValidity: 10, UIDNext: 4, Messages: 3, HighestModSeq: 50}},
		{"newest expunged before we fetched it", func(c *skipCase) {
			c.box.msgs = c.box.msgs[:2]
			c.box.status.NumMessages = 2
			c.row.lastSeenUID = 2
			delete(c.row.prov, 3)
		}, &store.ObservedStatus{UIDValidity: 10, UIDNext: 4, Messages: 2, HighestModSeq: 50}},
		{"emptied before our first sync", func(c *skipCase) {
			c.box.msgs = nil
			c.box.status.UIDNext, c.box.status.NumMessages = 57, 0
			c.row.lastSeenUID = 0
			c.row.prov = map[int64]bool{}
		}, &store.ObservedStatus{UIDValidity: 10, UIDNext: 57, Messages: 0, HighestModSeq: 50}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newSkipCase().due()
			c.row.pollUIDNext = nil // the poll runs
			c.row.observed = nil    // and so does the observation
			tc.setup(c)
			sum := c.run(t)
			if len(c.sess.liveCalls) != 1 || sum.MailboxesVerified != 1 {
				t.Fatalf("observations=%d verified=%d errors=%v, want 1/1", len(c.sess.liveCalls), sum.MailboxesVerified, sum.Errors)
			}
			got, stamped := c.st.seenCalls[c.row.id]
			if !stamped {
				t.Fatal("the observation was not stamped at all")
			}
			if (got == nil) != (tc.want == nil) || (got != nil && *got != *tc.want) {
				t.Errorf("baseline recorded = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// A store failure on the re-stamp is reported against the folder and nothing is
// counted as skipped: the folder simply stays due.
func TestUnchangedRestampFailureIsReported(t *testing.T) {
	c := newSkipCase().due()
	boom := errors.New("stamp failed")
	st := &restampFailStore{fakeStore: c.st, err: boom}
	c.ing.st = c.st
	e := New(st, c.ing, factoryFor(c.sess), WithReapInterval(time.Hour), WithClock(func() time.Time { return c.now }))
	sum, err := e.SyncAccount(context.Background(), testReq())
	if err != nil {
		t.Fatalf("SyncAccount: %v", err)
	}
	if sum.ObservationsSkipped != 0 || len(sum.Errors) != 1 || !errors.Is(sum.Errors[0], boom) {
		t.Errorf("skipped=%d errors=%v, want 0 and the stamp failure", sum.ObservationsSkipped, sum.Errors)
	}
}

type restampFailStore struct {
	*fakeStore
	err error
}

func (s *restampFailStore) StampMailboxUnchanged(context.Context, ulid.ULID, ulid.ULID) error {
	return s.err
}

// Summary.Duration is SyncAccountWith's wall time — the figure #620 is judged
// on, so it must actually be measured, not left at zero.
func TestSyncAccountReportsItsDuration(t *testing.T) {
	c := newSkipCase()
	slow := &slowListSession{fakeSession: c.sess, delay: 20 * time.Millisecond}
	c.ing.st = c.st
	e := New(c.st, c.ing, nil, WithReapInterval(time.Hour), WithClock(func() time.Time { return c.now }))
	sum, err := e.SyncAccountWith(context.Background(), testReq(), slow)
	if err != nil {
		t.Fatalf("SyncAccountWith: %v", err)
	}
	if sum.Duration < slow.delay {
		t.Errorf("Duration = %v, want at least the %v the LIST took", sum.Duration, slow.delay)
	}
}

// slowListSession is a fakeSession whose LIST takes a known, measurable time.
type slowListSession struct {
	*fakeSession
	delay time.Duration
}

func (s *slowListSession) ListMailboxes(ctx context.Context) ([]sync.Mailbox, error) {
	time.Sleep(s.delay)
	return s.fakeSession.ListMailboxes(ctx)
}
