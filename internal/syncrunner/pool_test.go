// SPDX-License-Identifier: AGPL-3.0-only

package syncrunner

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/mailsync"
	"mailroom/internal/store"
	mrsync "mailroom/internal/sync"
)

// ---- pool test fakes -------------------------------------------------------

// poolConn is a fake SyncSession for the connection pool. Only Noop (the reuse
// health probe) and Close (the discard path) carry behavior; every other method
// is inherited nil from the embedded interface and would panic if called — proof
// the pool touches a session only through the probe/close contract, never by
// driving IMAP itself.
type poolConn struct {
	mrsync.SyncSession
	id      int
	mu      sync.Mutex
	noopErr error
	closed  bool
}

func (c *poolConn) Noop(context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.noopErr
}

func (c *poolConn) setNoopErr(err error) {
	c.mu.Lock()
	c.noopErr = err
	c.mu.Unlock()
}

func (c *poolConn) Close() error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	return nil
}

func (c *poolConn) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// dialCounter is a WithPoolDialer seam that counts dials and hands out a fresh
// poolConn each time. It is the dial-counting factory the reuse tests assert on:
// a reused warm connection means the count does NOT move.
type dialCounter struct {
	mu    sync.Mutex
	count int
	conns []*poolConn
}

func (d *dialCounter) dial(context.Context, mrsync.Config) (mrsync.SyncSession, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.count++
	c := &poolConn{id: d.count}
	d.conns = append(d.conns, c)
	return c, nil
}

func (d *dialCounter) callCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.count
}

// poolSyncer runs passes over caller-owned sessions (SyncAccountWith) and, when
// pooling is off, over its own dial (SyncAccount). It records which sessions it
// was handed, tracks peak concurrency (to prove the gate serializes), and can be
// told to fail the next N SyncAccountWith calls (to drive the retry-once path).
type poolSyncer struct {
	mu         sync.Mutex
	withCalls  int
	plainCalls int
	failNext   int
	sessions   []mrsync.SyncSession
	active     int
	peak       int
	holdFor    time.Duration
}

func (s *poolSyncer) SyncAccount(context.Context, mailsync.AccountSync) (mailsync.Summary, error) {
	s.mu.Lock()
	s.plainCalls++
	s.mu.Unlock()
	return mailsync.Summary{}, nil
}

func (s *poolSyncer) SyncAccountWith(_ context.Context, _ mailsync.AccountSync, sess mrsync.SyncSession) (mailsync.Summary, error) {
	s.mu.Lock()
	s.withCalls++
	s.sessions = append(s.sessions, sess)
	s.active++
	if s.active > s.peak {
		s.peak = s.active
	}
	fail := s.failNext > 0
	if fail {
		s.failNext--
	}
	hold := s.holdFor
	s.mu.Unlock()

	if hold > 0 {
		time.Sleep(hold)
	}

	s.mu.Lock()
	s.active--
	s.mu.Unlock()

	if fail {
		return mailsync.Summary{}, errors.New("pool: simulated connection-fatal pass error")
	}
	return mailsync.Summary{}, nil
}

func (s *poolSyncer) snapshot() (withCalls, plainCalls, peak int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.withCalls, s.plainCalls, s.peak
}

// newPoolRunner wires a pool-enabled Runner over the given syncer/dialer for one
// account, returning the runner, the account, and a fixed user id.
func newPoolRunner(syncer Syncer, dialer *dialCounter, opts ...Option) (*Runner, store.Account, ulid.ULID) {
	acct := store.Account{ID: ulid.Make(), Name: "a"}
	base := []Option{
		WithLogger(discardLogger()),
		WithPool(true, time.Minute),
		WithPoolDialer(dialer.dial),
	}
	r := New(&fakeLister{accts: []store.Account{acct}}, syncer, idPrepare, append(base, opts...)...)
	return r, acct, ulid.Make()
}

// ---- tests -----------------------------------------------------------------

// A warm connection that passes its NOOP probe is reused across passes: the
// dial-counting factory fires exactly once for two passes of one account.
func TestPoolReusesWarmConnection(t *testing.T) {
	dialer := &dialCounter{}
	syncer := &poolSyncer{}
	r, _, userID := newPoolRunner(syncer, dialer)

	ctx := context.Background()
	if _, err := r.SyncOnce(ctx, userID); err != nil {
		t.Fatalf("SyncOnce #1: %v", err)
	}
	if _, err := r.SyncOnce(ctx, userID); err != nil {
		t.Fatalf("SyncOnce #2: %v", err)
	}

	if got := dialer.callCount(); got != 1 {
		t.Errorf("dials = %d, want 1 (the warm connection must be reused)", got)
	}
	withCalls, _, _ := syncer.snapshot()
	if withCalls != 2 {
		t.Errorf("SyncAccountWith calls = %d, want 2 (one per pass)", withCalls)
	}
}

// A pooled connection whose NOOP probe now fails is discarded (Closed) and
// re-dialed at acquire; the pass still completes over the fresh connection.
func TestPoolReDialsOnStaleProbe(t *testing.T) {
	dialer := &dialCounter{}
	syncer := &poolSyncer{}
	r, _, userID := newPoolRunner(syncer, dialer)

	ctx := context.Background()
	if _, err := r.SyncOnce(ctx, userID); err != nil {
		t.Fatalf("SyncOnce #1: %v", err)
	}
	// Kill the warm connection so the next pass's probe fails.
	first := dialer.conns[0]
	first.setNoopErr(errors.New("connection reset"))

	sum, err := r.SyncOnce(ctx, userID)
	if err != nil {
		t.Fatalf("SyncOnce #2: %v", err)
	}
	if sum.Failed != 0 {
		t.Errorf("pass Failed = %d, want 0 (a stale probe must re-dial, not fail the sync)", sum.Failed)
	}
	if got := dialer.callCount(); got != 2 {
		t.Errorf("dials = %d, want 2 (stale probe forces a re-dial)", got)
	}
	if !first.isClosed() {
		t.Error("stale connection was not Closed on discard")
	}
	withCalls, _, _ := syncer.snapshot()
	if withCalls != 2 {
		t.Errorf("SyncAccountWith calls = %d, want 2", withCalls)
	}
}

// A reused connection that PASSES its probe but then fails the pass (the
// probe-to-use race) is discarded and the pass is retried ONCE over a fresh
// dial — the silent-latency guard: pooling must never turn a live-connection
// race into a skipped sync.
func TestPoolRetryOnceOnPassError(t *testing.T) {
	dialer := &dialCounter{}
	syncer := &poolSyncer{}
	r, _, userID := newPoolRunner(syncer, dialer)

	ctx := context.Background()
	if _, err := r.SyncOnce(ctx, userID); err != nil {
		t.Fatalf("SyncOnce #1: %v", err) // warms the connection
	}
	// The next pass's FIRST SyncAccountWith (over the reused connection) fails;
	// the retry over a fresh dial must succeed.
	syncer.mu.Lock()
	syncer.failNext = 1
	syncer.mu.Unlock()

	sum, err := r.SyncOnce(ctx, userID)
	if err != nil {
		t.Fatalf("SyncOnce #2: %v", err)
	}
	if sum.Failed != 0 {
		t.Errorf("pass Failed = %d, want 0 (the retry must rescue the pass)", sum.Failed)
	}
	if got := dialer.callCount(); got != 2 {
		t.Errorf("dials = %d, want 2 (retry re-dials once)", got)
	}
	if !dialer.conns[0].isClosed() {
		t.Error("the failed reused connection was not Closed before the retry")
	}
	withCalls, _, _ := syncer.snapshot()
	if withCalls != 3 {
		t.Errorf("SyncAccountWith calls = %d, want 3 (pass1 + failed reuse + retry)", withCalls)
	}
}

// A SECOND consecutive pass failure (reuse fails, retry also fails) surfaces as
// the pass error and backs off — pooling adds exactly one retry, no more.
func TestPoolRetryOnceThenGivesUp(t *testing.T) {
	dialer := &dialCounter{}
	syncer := &poolSyncer{}
	r, _, userID := newPoolRunner(syncer, dialer)

	ctx := context.Background()
	if _, err := r.SyncOnce(ctx, userID); err != nil {
		t.Fatalf("SyncOnce #1: %v", err)
	}
	syncer.mu.Lock()
	syncer.failNext = 2 // both the reuse AND the retry fail
	syncer.mu.Unlock()

	sum, err := r.SyncOnce(ctx, userID)
	if err != nil {
		t.Fatalf("SyncOnce #2: %v", err)
	}
	if sum.Failed != 1 {
		t.Errorf("pass Failed = %d, want 1 (a second failure stands as the pass error)", sum.Failed)
	}
	withCalls, _, _ := syncer.snapshot()
	if withCalls != 3 {
		t.Errorf("SyncAccountWith calls = %d, want 3 (pass1 + reuse + one retry, never two)", withCalls)
	}
}

// With pooling OFF the runner dials-and-closes every pass via SyncAccount, and
// the pool dialer is never touched.
func TestPoolDisabledDialsEveryPass(t *testing.T) {
	dialer := &dialCounter{}
	syncer := &poolSyncer{}
	acct := store.Account{ID: ulid.Make(), Name: "a"}
	r := New(&fakeLister{accts: []store.Account{acct}}, syncer, idPrepare,
		WithLogger(discardLogger()), WithPool(false, time.Minute), WithPoolDialer(dialer.dial))
	userID := ulid.Make()

	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := r.SyncOnce(ctx, userID); err != nil {
			t.Fatalf("SyncOnce #%d: %v", i, err)
		}
	}

	withCalls, plainCalls, _ := syncer.snapshot()
	if plainCalls != 3 {
		t.Errorf("SyncAccount calls = %d, want 3 (dial-per-pass)", plainCalls)
	}
	if withCalls != 0 {
		t.Errorf("SyncAccountWith calls = %d, want 0 (pool disabled)", withCalls)
	}
	if got := dialer.callCount(); got != 0 {
		t.Errorf("pool dials = %d, want 0 (pool disabled must not touch the pool dialer)", got)
	}
}

// forgetUser closes every warm connection and drops the user's pool map, so a
// shut-down user leaks no connections.
func TestPoolTeardownClosesSessions(t *testing.T) {
	dialer := &dialCounter{}
	syncer := &poolSyncer{}
	r, _, userID := newPoolRunner(syncer, dialer)

	if _, err := r.SyncOnce(context.Background(), userID); err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}
	if dialer.callCount() != 1 {
		t.Fatalf("precondition: want 1 warm connection, got %d dials", dialer.callCount())
	}

	r.forgetUser(userID)

	if !dialer.conns[0].isClosed() {
		t.Error("forgetUser did not Close the warm pooled connection")
	}
	r.scheduleMu.Lock()
	_, ok := r.pools[userID]
	r.scheduleMu.Unlock()
	if ok {
		t.Error("forgetUser did not drop the user's pool map")
	}
}

// A vanished account's warm connection is reaped by dueAccounts (Closed and
// dropped) the next time it reconciles the live account set.
func TestPoolReapsVanishedAccount(t *testing.T) {
	dialer := &dialCounter{}
	syncer := &poolSyncer{}
	lister := &fakeLister{}
	acct := store.Account{ID: ulid.Make(), Name: "a"}
	lister.accts = []store.Account{acct}
	r := New(lister, syncer, idPrepare,
		WithLogger(discardLogger()), WithPool(true, time.Minute), WithPoolDialer(dialer.dial))
	userID := ulid.Make()

	if _, err := r.SyncOnce(context.Background(), userID); err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}
	// The account disappears; a scheduled reconcile must reap its warm connection.
	lister.accts = nil
	r.dueAccounts(userID, nil, time.Now())

	if !dialer.conns[0].isClosed() {
		t.Error("dueAccounts did not Close the vanished account's warm connection")
	}
	r.scheduleMu.Lock()
	_, ok := r.pools[userID][acct.ID]
	r.scheduleMu.Unlock()
	if ok {
		t.Error("dueAccounts did not drop the vanished account's pool entry")
	}
}

// The gate serializes passes for one account, so the pool never hands its single
// warm session to two passes at once: a storm of concurrent gated passes drives
// SyncAccountWith strictly one at a time, over one reused connection.
func TestPoolGateSerializesSharedSession(t *testing.T) {
	dialer := &dialCounter{}
	syncer := &poolSyncer{holdFor: time.Millisecond}
	r, acct, userID := newPoolRunner(syncer, dialer)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var (
				sum PassSummary
				mu  sync.Mutex
			)
			r.runGated(userID, acct.ID, func() {
				_ = r.syncAccount(context.Background(), userID, acct, 0, &sum, &mu)
			})
		}()
	}
	wg.Wait()

	_, _, peak := syncer.snapshot()
	if peak > 1 {
		t.Fatalf("SyncAccountWith peak concurrency = %d; the gate must serialize passes so the pool never shares a session", peak)
	}
	// The gate coalesces the storm into a small number of serial passes, all over
	// the one warm connection — so at most one dial ever happens.
	if got := dialer.callCount(); got != 1 {
		t.Errorf("dials = %d, want 1 (coalesced serial passes reuse one warm connection)", got)
	}
}
