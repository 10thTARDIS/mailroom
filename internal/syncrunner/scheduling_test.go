// SPDX-License-Identifier: AGPL-3.0-only

package syncrunner

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/provider"
	"mailroom/internal/store"
)

// ---- scheduling test helpers ------------------------------------------------

// fakeClock is a manually-advanced clock. The Runner's `now` seam reads it, so a
// test can drive per-account scheduling deterministically without real sleeps.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Unix(1_700_000_000, 0)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// setFail flips whether the fake syncer fails for one account id, so a test can
// make a failing account recover between passes.
func (f *fakeSyncer) setFail(id ulid.ULID, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failFor == nil {
		f.failFor = map[string]error{}
	}
	if err == nil {
		delete(f.failFor, id.String())
		return
	}
	f.failFor[id.String()] = err
}

// countFor returns how many times the fake syncer has synced one account id.
func (f *fakeSyncer) countFor(id ulid.ULID) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == id {
			n++
		}
	}
	return n
}

// ---- tests ------------------------------------------------------------------

// A failing account backs off on its OWN timeline (doubling each consecutive
// failure) while a healthy account keeps syncing on its base interval — and,
// crucially, once the failing account is backed off past a tick, the healthy one
// still syncs at that tick. That is the whole point of per-account scheduling:
// one account's backoff must never delay another.
func TestScheduledBackoffIsolatesFailingAccount(t *testing.T) {
	const base = 10 * time.Minute
	good := store.Account{ID: ulid.Make(), Name: "good"}
	bad := store.Account{ID: ulid.Make(), Name: "bad"}
	syncer := &fakeSyncer{failFor: map[string]error{bad.ID.String(): errors.New("imap unreachable")}}

	clock := newFakeClock()
	r := New(&fakeLister{accts: []store.Account{good, bad}}, syncer, idPrepare,
		WithInterval(base), WithMaxBackoff(10*time.Hour), WithLogger(discardLogger()))
	r.now = clock.Now
	uid := ulid.Make()
	ctx := context.Background()

	// Pass 1 @ t0: both are newly-scheduled (due now). good succeeds, bad fails.
	if _, err := r.runScheduledPass(ctx, uid, 0); err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	if got := syncer.countFor(good.ID); got != 1 {
		t.Fatalf("good synced %d times after pass 1, want 1", got)
	}
	if got := syncer.countFor(bad.ID); got != 1 {
		t.Fatalf("bad synced %d times after pass 1, want 1", got)
	}
	if _, bo, _ := r.scheduleState(uid, good.ID); bo != 0 {
		t.Errorf("good backoff = %v after success, want 0", bo)
	}
	if _, bo, _ := r.scheduleState(uid, bad.ID); bo != base {
		t.Errorf("bad backoff = %v after first failure, want %v", bo, base)
	}

	// Pass 2 @ t0+2*base: both due again. good succeeds; bad fails and its backoff
	// doubles to 2*base, pushing its next-due well past the next base tick.
	clock.Advance(2 * base)
	if _, err := r.runScheduledPass(ctx, uid, 1); err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	if got := syncer.countFor(good.ID); got != 2 {
		t.Fatalf("good synced %d times after pass 2, want 2", got)
	}
	if got := syncer.countFor(bad.ID); got != 2 {
		t.Fatalf("bad synced %d times after pass 2, want 2", got)
	}
	if _, bo, _ := r.scheduleState(uid, bad.ID); bo != 2*base {
		t.Errorf("bad backoff = %v after second failure, want %v (doubled)", bo, 2*base)
	}

	// Pass 3 @ t0+3*base: good is due again (last synced at 2*base, +base = 3*base),
	// but bad's next-due is ~4*base out, so ONLY good syncs. Proof that bad's
	// backoff does not delay good.
	clock.Advance(base)
	if _, err := r.runScheduledPass(ctx, uid, 2); err != nil {
		t.Fatalf("pass 3: %v", err)
	}
	if got := syncer.countFor(good.ID); got != 3 {
		t.Errorf("good synced %d times after pass 3, want 3 (a backed-off peer must not stall it)", got)
	}
	if got := syncer.countFor(bad.ID); got != 2 {
		t.Errorf("bad synced %d times after pass 3, want 2 (still backed off, not yet due)", got)
	}
	if _, bo, _ := r.scheduleState(uid, bad.ID); bo != 2*base {
		t.Errorf("bad backoff = %v after a pass it was not due for, want it unchanged at %v", bo, 2*base)
	}
}

// A recovered account (fail, then succeed) resets its backoff to zero and returns
// to the plain base cadence.
func TestScheduledRecoveryResetsBackoff(t *testing.T) {
	const base = 10 * time.Minute
	acct := store.Account{ID: ulid.Make(), Name: "flaky"}
	syncer := &fakeSyncer{failFor: map[string]error{acct.ID.String(): errors.New("temporary")}}

	clock := newFakeClock()
	r := New(&fakeLister{accts: []store.Account{acct}}, syncer, idPrepare,
		WithInterval(base), WithLogger(discardLogger()))
	r.now = clock.Now
	uid := ulid.Make()
	ctx := context.Background()

	// Fail once → backoff = base.
	if _, err := r.runScheduledPass(ctx, uid, 0); err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	if _, bo, _ := r.scheduleState(uid, acct.ID); bo != base {
		t.Fatalf("backoff = %v after failure, want %v", bo, base)
	}

	// Recover the account, advance past the backoff, and sync again.
	syncer.setFail(acct.ID, nil)
	clock.Advance(2 * base)
	if _, err := r.runScheduledPass(ctx, uid, 1); err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	nextDue, bo, ok := r.scheduleState(uid, acct.ID)
	if !ok {
		t.Fatal("schedule entry vanished")
	}
	if bo != 0 {
		t.Errorf("backoff = %v after recovery, want 0", bo)
	}
	// Back on the plain base cadence: next-due is exactly one base interval out
	// from the (fake) now, with no jitter on success.
	if want := clock.Now().Add(base); !nextDue.Equal(want) {
		t.Errorf("nextDue = %v after recovery, want %v (now + base)", nextDue, want)
	}
}

// effectiveInterval raises the base interval to a provider's minimum-interval
// floor, and only ever upward: a "gmail" account with a tiny base still waits at
// least the floor, while a generic account (and a base already above the floor)
// is governed by the base.
func TestEffectiveIntervalRespectsProviderFloor(t *testing.T) {
	const tinyBase = time.Minute
	r := New(&fakeLister{}, &fakeSyncer{}, idPrepare,
		WithInterval(tinyBase), WithLogger(discardLogger()))

	gmail := store.Account{ID: ulid.Make(), Name: "g", ProviderProfile: "gmail"}
	floor := provider.IMAPMinSyncInterval("gmail")
	if floor <= tinyBase {
		t.Fatalf("test precondition: gmail floor %v must exceed the tiny base %v", floor, tinyBase)
	}
	if got := r.effectiveInterval(gmail); got != floor {
		t.Errorf("gmail effectiveInterval = %v with a %v base, want the floor %v", got, tinyBase, floor)
	}

	generic := store.Account{ID: ulid.Make(), Name: "x", ProviderProfile: "generic"}
	if got := r.effectiveInterval(generic); got != tinyBase {
		t.Errorf("generic effectiveInterval = %v, want the base %v (no floor)", got, tinyBase)
	}

	// A base already above the floor governs — the floor never speeds an account up.
	bigBase := 2 * floor
	r2 := New(&fakeLister{}, &fakeSyncer{}, idPrepare,
		WithInterval(bigBase), WithLogger(discardLogger()))
	if got := r2.effectiveInterval(gmail); got != bigBase {
		t.Errorf("gmail effectiveInterval = %v with a %v base, want the base (floor is lower)", got, bigBase)
	}
}

// A scheduled pass syncs only accounts that are DUE: an account synced this
// instant is not re-synced by an immediate second pass at the same clock time.
func TestScheduledPassSkipsNotDueAccounts(t *testing.T) {
	const base = 10 * time.Minute
	acct := store.Account{ID: ulid.Make(), Name: "a"}
	syncer := &fakeSyncer{}

	clock := newFakeClock()
	r := New(&fakeLister{accts: []store.Account{acct}}, syncer, idPrepare,
		WithInterval(base), WithLogger(discardLogger()))
	r.now = clock.Now
	uid := ulid.Make()
	ctx := context.Background()

	if _, err := r.runScheduledPass(ctx, uid, 0); err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	if got := syncer.countFor(acct.ID); got != 1 {
		t.Fatalf("account synced %d times on the catch-up pass, want 1", got)
	}

	// Same clock time: the account's next-due is base out, so a second pass skips it.
	if _, err := r.runScheduledPass(ctx, uid, 1); err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	if got := syncer.countFor(acct.ID); got != 1 {
		t.Errorf("account synced %d times, want 1 (not yet due for a re-sync)", got)
	}

	// Once its interval elapses it is due again.
	clock.Advance(base)
	if _, err := r.runScheduledPass(ctx, uid, 2); err != nil {
		t.Fatalf("pass 3: %v", err)
	}
	if got := syncer.countFor(acct.ID); got != 2 {
		t.Errorf("account synced %d times after its interval elapsed, want 2", got)
	}
}

// SyncOnce (the manual/CLI one-shot) syncs every account regardless of the
// per-account schedule — even an account that a scheduled pass just synced and
// marked not-due.
func TestSyncOnceIgnoresSchedule(t *testing.T) {
	const base = 10 * time.Minute
	acct := store.Account{ID: ulid.Make(), Name: "a"}
	syncer := &fakeSyncer{}

	clock := newFakeClock()
	r := New(&fakeLister{accts: []store.Account{acct}}, syncer, idPrepare,
		WithInterval(base), WithLogger(discardLogger()))
	r.now = clock.Now
	uid := ulid.Make()
	ctx := context.Background()

	// Scheduled pass marks the account not-due for another base interval.
	if _, err := r.runScheduledPass(ctx, uid, 0); err != nil {
		t.Fatalf("scheduled pass: %v", err)
	}
	if got := syncer.countFor(acct.ID); got != 1 {
		t.Fatalf("account synced %d times, want 1", got)
	}

	// SyncOnce at the SAME clock time still syncs it — it ignores the schedule.
	sum, err := r.SyncOnce(ctx, uid)
	if err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}
	if sum.Synced != 1 {
		t.Errorf("SyncOnce summary Synced = %d, want 1", sum.Synced)
	}
	if got := syncer.countFor(acct.ID); got != 2 {
		t.Errorf("account synced %d times, want 2 (SyncOnce must ignore not-due)", got)
	}
}
