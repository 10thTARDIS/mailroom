// SPDX-License-Identifier: AGPL-3.0-only

package syncrunner

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
	mrsync "mailroom/internal/sync"
)

// NudgeWriteback (#581) is what makes a user's action reach the provider in
// seconds instead of at the next poll. Two properties carry it, and each has a
// test here that fails without the thing it names:
//
//   - it DRAINS and does not SYNC — pushing one flag up must not also pull a
//     full reconcile over every mailbox, which at ~37k messages is the
//     expensive half;
//   - it goes through the per-account gate, so a nudge can never run a second
//     drain beside one already in flight (the engine requires one pass per
//     account at a time, and a second IMAP session per account is what spec §4
//     tells us not to open).

// recorder counts writeback drains per account and can block inside one, so a
// test can hold a drain open and fire more nudges at it.
type recorder struct {
	mu       sync.Mutex
	calls    []ulid.ULID
	inFlight int
	maxSeen  int // the most drains ever inside this account at one instant
	err      error
	hold     chan struct{} // when non-nil, the first drain waits on it
	entered  chan struct{} // closed once the first drain is inside
	once     sync.Once
}

func (w *recorder) drain(_ context.Context, _ ulid.ULID, acct store.Account, _ mrsync.Config) error {
	w.mu.Lock()
	w.calls = append(w.calls, acct.ID)
	w.inFlight++
	if w.inFlight > w.maxSeen {
		w.maxSeen = w.inFlight
	}
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		w.inFlight--
		w.mu.Unlock()
	}()
	if w.hold != nil {
		w.once.Do(func() { close(w.entered) })
		<-w.hold
	}
	return w.err
}

func (w *recorder) concurrencyPeak() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.maxSeen
}

func (w *recorder) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.calls)
}

func TestNudgeWritebackDrainsEveryAccountWithoutSyncing(t *testing.T) {
	accts := accounts("a", "b")
	wb := &recorder{}
	syncer := &fakeSyncer{}
	r := New(&fakeLister{accts: accts}, syncer, idPrepare,
		WithLogger(discardLogger()), WithWriteback(wb.drain))

	r.NudgeWriteback(context.Background(), ulid.Make())

	waitFor(t, 2*time.Second, func() bool { return wb.count() == 2 })

	// THE HALF THAT MATTERS: no sync ran. A nudge that quietly triggered a full
	// pass would work in this test and cost a CONDSTORE reconcile per action in
	// production.
	syncer.mu.Lock()
	defer syncer.mu.Unlock()
	if len(syncer.calls) != 0 {
		t.Errorf("sync passes = %d, want 0: a nudge drains writeback, it does not sync", len(syncer.calls))
	}
}

func TestNudgeWritebackCoalescesWhileADrainIsRunning(t *testing.T) {
	accts := accounts("only")
	wb := &recorder{hold: make(chan struct{}), entered: make(chan struct{})}
	r := New(&fakeLister{accts: accts}, &fakeSyncer{}, idPrepare,
		WithLogger(discardLogger()), WithWriteback(wb.drain))
	user := ulid.Make()

	r.NudgeWriteback(context.Background(), user)
	<-wb.entered // a drain is now in flight and parked

	// Twenty more actions while it runs — a batch label, say.
	for i := 0; i < 20; i++ {
		r.NudgeWriteback(context.Background(), user)
	}

	// At least one trigger has registered on the gate before we release the
	// parked drain, so the follow-up path is genuinely exercised.
	g := r.gateFor(user, accts[0].ID)
	waitFor(t, 2*time.Second, func() bool {
		g.mu.Lock()
		defer g.mu.Unlock()
		return g.pending
	})
	close(wb.hold)

	// A follow-up runs: a trigger that arrived mid-drain refers to work that
	// drain may have been too early to see, so it must not be dropped.
	waitFor(t, 2*time.Second, func() bool { return wb.count() >= 2 })
	time.Sleep(100 * time.Millisecond) // let every straggler goroutine finish

	// ⚠ THE ASSERTION IS THE CONCURRENCY CAP, NOT A TOTAL, and the difference
	// cost an hour. The first version asserted "exactly 2 drains", which passed
	// without -race and failed with it: nudges whose goroutine had not yet run
	// when the gate was released legitimately drain afterwards, so the total
	// climbs past 2 and is a property of scheduling, not of the design. Those
	// extra drains are the cheap no-op case in production (DrainAccount returns
	// without dialing when nothing is pending). What must NEVER happen — a
	// second IMAP session for one account, which the engine forbids and spec §4
	// warns about — is two drains at once.
	//
	// PROBE, and note WHICH mutation proves this: dropping runGated from
	// NudgeWriteback fails at the `g.pending` wait on line ~114, not here —
	// bypassing the gate removes the bookkeeping this test steers by, so it
	// never reaches the assertion and proves nothing about it. The mutation that
	// does is inside runGated: keep `g.pending = true` and `run()` anyway
	// instead of returning. That fails HERE, with peak = 9.
	if peak := wb.concurrencyPeak(); peak != 1 {
		t.Errorf("peak concurrent drains for one account = %d, want 1: a nudge must go through the gate", peak)
	}
	if got := wb.count(); got < 2 {
		t.Errorf("drains = %d, want at least 2: the trigger that arrived mid-drain was dropped", got)
	}
}

func TestNudgeWritebackWithoutAWritebackEngineDoesNothing(t *testing.T) {
	// A deployment with no writeback wired must not dial, log an error, or panic
	// when a user acts — it simply has nothing to push.
	lister := &fakeLister{accts: accounts("a")}
	r := New(lister, &fakeSyncer{}, idPrepare, WithLogger(discardLogger()))
	r.NudgeWriteback(context.Background(), ulid.Make()) // no WithWriteback
	time.Sleep(20 * time.Millisecond)
}

func TestNudgeWritebackSurvivesAListerFailure(t *testing.T) {
	// The user's action already succeeded; the intents are durable. Failing to
	// list accounts must not panic or block — the scheduled pass still drains.
	wb := &recorder{}
	r := New(&fakeLister{err: errors.New("db away")}, &fakeSyncer{}, idPrepare,
		WithLogger(discardLogger()), WithWriteback(wb.drain))
	r.NudgeWriteback(context.Background(), ulid.Make())
	time.Sleep(20 * time.Millisecond)
	if got := wb.count(); got != 0 {
		t.Errorf("drains = %d, want 0 when the account list could not be read", got)
	}
}

func TestNudgeWritebackReportsADrainFailureWithoutRetrying(t *testing.T) {
	// A failed drain leaves the intents queued for the next pass — the same
	// contract the pre-sync hook has. It must not spin.
	wb := &recorder{err: errors.New("imap down")}
	r := New(&fakeLister{accts: accounts("a")}, &fakeSyncer{}, idPrepare,
		WithLogger(discardLogger()), WithWriteback(wb.drain))
	r.NudgeWriteback(context.Background(), ulid.Make())
	waitFor(t, 2*time.Second, func() bool { return wb.count() == 1 })
	time.Sleep(50 * time.Millisecond)
	if got := wb.count(); got != 1 {
		t.Errorf("drains = %d, want 1: a failure waits for the next pass rather than retrying here", got)
	}
}
