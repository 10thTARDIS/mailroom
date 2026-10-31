// SPDX-License-Identifier: AGPL-3.0-only

package syncrunner

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/mailsync"
	"mailroom/internal/store"
	mrsync "mailroom/internal/sync"
)

// ---- fakes for the IDLE watcher --------------------------------------------

// fakeIdleSession implements just the SyncSession surface the watcher touches
// (Supports/Select/Notifications/Idle/Close). Every other method is nil on the
// embedded interface and would panic if the watcher ever called it — a guard
// that the watcher really does only listen, never fetch or ingest.
type fakeIdleSession struct {
	mrsync.SyncSession
	supportsIdle bool
	notifs       chan mrsync.Notification
	selectErr    error
	idleErr      error
}

func (f *fakeIdleSession) Supports(c mrsync.Capability) bool {
	return f.supportsIdle && c == mrsync.CapabilityIdle
}

func (f *fakeIdleSession) Select(context.Context, string, mrsync.SelectOptions) (*mrsync.MailboxStatus, error) {
	if f.selectErr != nil {
		return nil, f.selectErr
	}
	return &mrsync.MailboxStatus{}, nil
}

func (f *fakeIdleSession) Notifications() <-chan mrsync.Notification { return f.notifs }

func (f *fakeIdleSession) Idle(context.Context) (mrsync.IdleSession, error) {
	if f.idleErr != nil {
		return nil, f.idleErr
	}
	return &fakeIdle{}, nil
}

func (f *fakeIdleSession) Close() error { return nil }

// fakeIdle is a no-op IdleSession: Stop/Wait return at once, so the watcher
// re-enters IDLE promptly after each event.
type fakeIdle struct{}

func (fakeIdle) Stop() error { return nil }
func (fakeIdle) Wait() error { return nil }

// concurrencySyncer records the peak number of SyncAccount calls in flight at
// once, so a test can assert the gate never lets two overlap for one account.
type concurrencySyncer struct {
	mu        sync.Mutex
	active    int
	maxActive int
	calls     int
}

func (c *concurrencySyncer) SyncAccount(_ context.Context, _ mailsync.AccountSync) (mailsync.Summary, error) {
	c.mu.Lock()
	c.active++
	if c.active > c.maxActive {
		c.maxActive = c.active
	}
	c.mu.Unlock()

	time.Sleep(time.Millisecond) // hold the "connection" so a real overlap would show

	c.mu.Lock()
	c.active--
	c.calls++
	c.mu.Unlock()
	return mailsync.Summary{}, nil
}

func (c *concurrencySyncer) SyncAccountWith(ctx context.Context, req mailsync.AccountSync, _ mrsync.SyncSession) (mailsync.Summary, error) {
	return c.SyncAccount(ctx, req)
}

func (c *concurrencySyncer) peak() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.maxActive
}

func (c *concurrencySyncer) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}

// ---- tests -----------------------------------------------------------------

// TestIdleWatcherTriggersSyncOnExists proves the end-to-end near-instant path
// with no IMAP and no DB: an EXISTS hint on the IDLE channel drives
// TriggerAccountSync, which runs one gated SyncAccount for that account.
func TestIdleWatcherTriggersSyncOnExists(t *testing.T) {
	acct := store.Account{ID: ulid.Make(), Name: "acct"}
	lister := &fakeLister{accts: []store.Account{acct}}
	syncer := &fakeSyncer{}
	notifs := make(chan mrsync.Notification, 4)
	fake := &fakeIdleSession{supportsIdle: true, notifs: notifs}

	r := New(lister, syncer, idPrepare,
		WithLogger(discardLogger()),
		WithIdle(true, time.Hour), // large reissue so only the EXISTS drives a trigger
		WithIdleDialer(func(context.Context, mrsync.Config) (mrsync.SyncSession, error) { return fake, nil }),
	)

	userID := ulid.Make()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() { r.idleWatcher(ctx, userID, acct); close(done) }()

	num := uint32(1)
	notifs <- mrsync.Notification{Kind: mrsync.NotificationExists, NumMessages: &num}

	waitFor(t, 2*time.Second, func() bool { return syncer.callCount() >= 1 })

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watcher did not unwind after ctx cancel")
	}
}

// TestIdleWatcherIgnoresNonExists proves a flag/expunge hint does NOT trigger a
// sync — only new mail (EXISTS) does.
func TestIdleWatcherIgnoresNonExists(t *testing.T) {
	acct := store.Account{ID: ulid.Make(), Name: "acct"}
	syncer := &fakeSyncer{}
	notifs := make(chan mrsync.Notification, 4)
	fake := &fakeIdleSession{supportsIdle: true, notifs: notifs}

	r := New(&fakeLister{accts: []store.Account{acct}}, syncer, idPrepare,
		WithLogger(discardLogger()),
		WithIdle(true, time.Hour),
		WithIdleDialer(func(context.Context, mrsync.Config) (mrsync.SyncSession, error) { return fake, nil }),
	)

	userID := ulid.Make()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.idleWatcher(ctx, userID, acct)

	notifs <- mrsync.Notification{Kind: mrsync.NotificationFlags}
	notifs <- mrsync.Notification{Kind: mrsync.NotificationExpunge, SeqNum: 3}

	// Give the watcher time to process them; it must not have triggered a sync.
	time.Sleep(100 * time.Millisecond)
	if got := syncer.callCount(); got != 0 {
		t.Fatalf("syncer called %d times on non-EXISTS hints, want 0", got)
	}
}

// TestIdleWatcherExitsWhenUnsupported proves a server without IDLE makes the
// watcher exit ON ITS OWN (leaving the account poll-only) rather than spinning
// — with NO context cancellation.
func TestIdleWatcherExitsWhenUnsupported(t *testing.T) {
	acct := store.Account{ID: ulid.Make(), Name: "acct"}
	fake := &fakeIdleSession{supportsIdle: false, notifs: make(chan mrsync.Notification)}

	r := New(&fakeLister{accts: []store.Account{acct}}, &fakeSyncer{}, idPrepare,
		WithLogger(discardLogger()),
		WithIdle(true, time.Hour),
		WithIdleDialer(func(context.Context, mrsync.Config) (mrsync.SyncSession, error) { return fake, nil }),
	)

	done := make(chan struct{})
	go func() { r.idleWatcher(context.Background(), ulid.Make(), acct); close(done) }()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watcher did not exit on an IDLE-less server (should fall back to poll-only)")
	}
}

// TestRunIdleDisabledIsNoOp proves the config toggle: with IDLE off, RunIdle
// returns immediately and dials nothing.
func TestRunIdleDisabledIsNoOp(t *testing.T) {
	dialed := false
	r := New(&fakeLister{accts: accounts("a")}, &fakeSyncer{}, idPrepare,
		WithLogger(discardLogger()),
		WithIdle(false, time.Hour),
		WithIdleDialer(func(context.Context, mrsync.Config) (mrsync.SyncSession, error) {
			dialed = true
			return &fakeIdleSession{}, nil
		}),
	)

	done := make(chan error, 1)
	go func() { done <- r.RunIdle(context.Background(), ulid.Make()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunIdle(disabled) = %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("RunIdle(disabled) did not return immediately")
	}
	if dialed {
		t.Fatal("RunIdle(disabled) dialed a session; it must be a pure no-op")
	}
}

// TestTriggerVanishedAccountIsNoOp: a trigger for an account the lister no
// longer returns does nothing (no sync, no panic).
func TestTriggerVanishedAccountIsNoOp(t *testing.T) {
	syncer := &fakeSyncer{}
	r := New(&fakeLister{accts: accounts("present")}, syncer, idPrepare,
		WithLogger(discardLogger()), WithIdle(true, time.Hour))

	r.TriggerAccountSync(context.Background(), ulid.Make(), ulid.Make()) // unknown account
	if got := syncer.callCount(); got != 0 {
		t.Fatalf("syncer called %d times for a vanished account, want 0", got)
	}
}

// TestTriggerDebouncedWithinWindow: two triggers in quick succession for one
// account collapse to a single sync — the second is inside the debounce window.
func TestTriggerDebouncedWithinWindow(t *testing.T) {
	acct := store.Account{ID: ulid.Make(), Name: "acct"}
	syncer := &fakeSyncer{}
	r := New(&fakeLister{accts: []store.Account{acct}}, syncer, idPrepare,
		WithLogger(discardLogger()), WithIdle(true, time.Hour))

	userID := ulid.Make()
	r.TriggerAccountSync(context.Background(), userID, acct.ID)
	r.TriggerAccountSync(context.Background(), userID, acct.ID)

	// The default debounce is several seconds; the second call lands inside it.
	if got := syncer.callCount(); got != 1 {
		t.Fatalf("syncer called %d times, want 1 (second trigger debounced)", got)
	}
}

// TestScheduledAndTriggeredNeverOverlap is the concurrency safety proof: with
// the scheduled pass (SyncOnce -> dispatch -> gate) and the IDLE trigger
// (TriggerAccountSync -> gate) both hammering ONE account, SyncAccount is never
// called concurrently for it — the engine's one-pass-at-a-time invariant holds,
// so cursors cannot be corrupted by an overlap.
func TestScheduledAndTriggeredNeverOverlap(t *testing.T) {
	acct := store.Account{ID: ulid.Make(), Name: "acct"}
	syncer := &concurrencySyncer{}
	r := New(&fakeLister{accts: []store.Account{acct}}, syncer, idPrepare,
		WithLogger(discardLogger()), WithIdle(true, time.Hour))

	userID := ulid.Make()
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); _, _ = r.SyncOnce(ctx, userID) }()
		go func() { defer wg.Done(); r.TriggerAccountSync(ctx, userID, acct.ID) }()
	}
	wg.Wait()

	if peak := syncer.peak(); peak > 1 {
		t.Fatalf("SyncAccount ran %d-way concurrent for one account; the gate must serialize it", peak)
	}
	if syncer.callCount() == 0 {
		t.Fatal("no sync passes ran at all")
	}
}
