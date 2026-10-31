// SPDX-License-Identifier: AGPL-3.0-only

package sync

import (
	"context"
	"errors"
	stdsync "sync"
	"testing"
	"time"
)

// fakeClock is a manual clock plus a sleep that ADVANCES it rather than
// blocking. That is the whole point: pacing 60 calls at 30/min is two minutes of
// wall time and a few microseconds of test time, and every wait is recorded so
// the test asserts on the pacing rather than on having taken a while.
type fakeClock struct {
	mu    stdsync.Mutex
	now   time.Time
	slept []time.Duration
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.slept = append(c.slept, d)
	c.now = c.now.Add(d)
	return nil
}

// SleepFrozen records the wait without advancing the clock — for the
// concurrency test, where a clock that every goroutine's sleep pushed forward
// would make the requested durations depend on interleaving.
func (c *fakeClock) SleepFrozen(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.slept = append(c.slept, d)
	return nil
}

func (c *fakeClock) waits() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]time.Duration, len(c.slept))
	copy(out, c.slept)
	return out
}

// TestSearchLimiterPacesCalls is the mechanism #638 turns on: N calls at
// perMinute must be spread across at least N/perMinute minutes, and the FIRST
// one must not wait (an idle account should not be penalised for being idle).
func TestSearchLimiterPacesCalls(t *testing.T) {
	clock := newFakeClock()
	start := clock.Now()
	b := newTokenBucket(30, 1, clock.Now, clock.Sleep)

	const calls = 10
	for i := 0; i < calls; i++ {
		if err := b.Wait(context.Background()); err != nil {
			t.Fatalf("Wait #%d: %v", i, err)
		}
	}

	waits := clock.waits()
	if len(waits) != calls-1 {
		t.Fatalf("slept %d times over %d calls (waits: %v); want %d — "+
			"the first call should go immediately and every later one should be paced",
			len(waits), calls, waits, calls-1)
	}
	for i, d := range waits {
		if d != 2*time.Second {
			t.Errorf("wait #%d = %v, want 2s (30 searches/minute = one every 2s)", i+1, d)
		}
	}

	// The measurement that actually matters to the server: elapsed time per call.
	elapsed := clock.Now().Sub(start)
	if want := time.Duration(calls-1) * 2 * time.Second; elapsed != want {
		t.Errorf("%d calls took %v of clock time, want %v", calls, elapsed, want)
	}
}

// TestSearchLimiterNeverExceedsTheRateInAnyWindow is the claim the server
// actually enforces: no rolling 60-second window may hold more than perMinute
// calls. Asserting on the per-call spacing alone would pass for a limiter that
// banked an hour of credit and then fired it in one burst.
func TestSearchLimiterNeverExceedsTheRateInAnyWindow(t *testing.T) {
	clock := newFakeClock()
	b := newTokenBucket(30, 1, clock.Now, clock.Sleep)

	// An idle period first: an unclamped bucket would bank 30 minutes of tokens
	// here and then admit ~900 calls instantly.
	clock.now = clock.now.Add(30 * time.Minute)

	var at []time.Time
	for i := 0; i < 90; i++ {
		if err := b.Wait(context.Background()); err != nil {
			t.Fatalf("Wait #%d: %v", i, err)
		}
		at = append(at, clock.Now())
	}

	for i := range at {
		n := 0
		for j := i; j < len(at) && at[j].Sub(at[i]) < time.Minute; j++ {
			n++
		}
		if n > 30 {
			t.Fatalf("%d calls landed in the 60s window starting at call %d; the server allows 30", n, i)
		}
	}
}

// TestSearchLimiterBurst proves burst is the knob it claims to be: burst N lets
// N calls through back-to-back after an idle period, and no more.
func TestSearchLimiterBurst(t *testing.T) {
	clock := newFakeClock()
	b := newTokenBucket(30, 3, clock.Now, clock.Sleep)

	for i := 0; i < 3; i++ {
		if err := b.Wait(context.Background()); err != nil {
			t.Fatalf("Wait #%d: %v", i, err)
		}
	}
	if got := clock.waits(); len(got) != 0 {
		t.Fatalf("burst of 3: the first 3 calls slept %v, want none", got)
	}
	if err := b.Wait(context.Background()); err != nil {
		t.Fatalf("Wait #4: %v", err)
	}
	if got := clock.waits(); len(got) != 1 || got[0] != 2*time.Second {
		t.Errorf("burst of 3: the 4th call slept %v, want one 2s wait", got)
	}
}

// TestSearchLimiterDisabled is the off switch: perMinute <= 0 means NO PACING,
// and it must be a nil interface value, because nil is what session.go tests for.
func TestSearchLimiterDisabled(t *testing.T) {
	for _, perMinute := range []int{0, -1} {
		l := NewSearchLimiter(perMinute, 1)
		if l != nil {
			t.Errorf("NewSearchLimiter(%d, 1) = %#v, want a nil Limiter (no pacing)", perMinute, l)
		}
	}
	if l := NewSearchLimiter(30, 1); l == nil {
		t.Error("NewSearchLimiter(30, 1) = nil, want a working limiter")
	}
}

// TestSearchLimiterWaitRespectsContext: a cancelled wait is an ordinary error
// the caller propagates. It must NOT return nil — returning nil would let the
// command through unpaced, handing the burst straight back to the server.
func TestSearchLimiterWaitRespectsContext(t *testing.T) {
	clock := newFakeClock()
	b := newTokenBucket(30, 1, clock.Now, clock.Sleep)

	// Spend the immediate token so the next Wait must sleep.
	if err := b.Wait(context.Background()); err != nil {
		t.Fatalf("first Wait: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := b.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Wait on a cancelled context = %v, want context.Canceled", err)
	}
}

// TestSearchLimiterConcurrent: N goroutines must reserve N DISTINCT slots. A
// bucket that read and wrote its state without the lock would hand two
// goroutines the same slot and admit both at once — exactly the burst the
// limiter exists to prevent, and a defect that would otherwise only surface in
// production.
//
// The clock is FROZEN here (sleeps are recorded, not applied) precisely so the
// assertion is about the reservations and not about interleaving: with time
// standing still, the i-th reservation must ask for exactly i*spacing whatever
// order the goroutines ran in, so the set of requested waits is deterministic.
func TestSearchLimiterConcurrent(t *testing.T) {
	clock := newFakeClock()
	b := newTokenBucket(60, 1, clock.Now, clock.SleepFrozen)

	const n = 20
	var wg stdsync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			if err := b.Wait(context.Background()); err != nil {
				t.Errorf("Wait: %v", err)
			}
		}()
	}
	wg.Wait()

	// 60/min = one per second: slots at +0s (immediate), +1s, +2s ... +19s.
	seen := map[time.Duration]bool{0: true} // the un-slept first reservation
	for _, d := range clock.waits() {
		if seen[d] {
			t.Fatalf("two goroutines were admitted for the same slot (%v); waits: %v", d, clock.waits())
		}
		seen[d] = true
	}
	if len(seen) != n {
		t.Errorf("%d distinct slots over %d concurrent calls, want %d", len(seen), n, n)
	}
}

// TestSleepContextIsInterruptible covers the PRODUCTION sleep (the fake clock's
// stands in for it everywhere else). A pace that could not be interrupted would
// hold a shutdown open for the whole spacing.
func TestSleepContextIsInterruptible(t *testing.T) {
	if err := sleepContext(context.Background(), time.Millisecond); err != nil {
		t.Errorf("sleepContext(1ms) = %v, want nil", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if err := sleepContext(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Errorf("sleepContext on a cancelled context = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("a cancelled sleepContext took %v; it waited out the timer", elapsed)
	}
}
