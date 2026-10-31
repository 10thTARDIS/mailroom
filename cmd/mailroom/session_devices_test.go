// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/auth"
)

// lockedBuf is a log sink safe to read while the revoker's goroutines write.
type lockedBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// concurrencyProbe is a revocation stand-in that holds each call until released
// and records how many ran at once.
type concurrencyProbe struct {
	cur, max atomic.Int64
	started  atomic.Int64
	release  chan struct{}
	done     sync.WaitGroup
}

func (c *concurrencyProbe) work(ctx context.Context, _ *auth.Authenticator, _, _ ulid.ULID, _ []byte) {
	defer c.done.Done()
	n := c.cur.Add(1)
	for {
		m := c.max.Load()
		if n <= m || c.max.CompareAndSwap(m, n) {
			break
		}
	}
	c.started.Add(1)
	<-c.release // holds its slot until the test lets go, whatever the timeout says
	c.cur.Add(-1)
}

func newTestRevoker(c *concurrencyProbe, log *lockedBuf, timeout time.Duration) *providerRevoker {
	r := newProviderRevoker(context.Background(), nil, slog.New(slog.NewJSONHandler(log, nil)))
	r.timeout = timeout
	r.work = c.work
	r.authn.Store(&auth.Authenticator{}) // never used: work is stubbed
	return r
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// "*others*" for a user with many sessions must not start one provider call per
// session at once: no more than deviceRevokeConcurrency are in flight, and every
// revocation still happens.
func TestProviderRevokerBoundsConcurrency(t *testing.T) {
	const fanOut = 3 * deviceRevokeConcurrency
	c := &concurrencyProbe{release: make(chan struct{})}
	c.done.Add(fanOut)
	var logs lockedBuf
	r := newTestRevoker(c, &logs, 30*time.Second)

	for i := 0; i < fanOut; i++ {
		r.revoke(ulid.Make(), ulid.Make(), []byte("sealed"))
	}
	// The limit is reached, and held: the rest are waiting, not running.
	waitFor(t, "the first slots to fill", func() bool { return c.started.Load() == deviceRevokeConcurrency })
	time.Sleep(150 * time.Millisecond)
	if got := c.started.Load(); got != deviceRevokeConcurrency {
		t.Fatalf("%d revocations running with the limit at %d", got, deviceRevokeConcurrency)
	}

	close(c.release)
	c.done.Wait() // every one of them ran
	if got := c.max.Load(); got > deviceRevokeConcurrency {
		t.Fatalf("saw %d revocations in flight at once, want at most %d", got, deviceRevokeConcurrency)
	}
	if got := c.started.Load(); got != fanOut {
		t.Fatalf("%d revocations ran, want all %d", got, fanOut)
	}
	if strings.Contains(logs.String(), "dropped") {
		t.Errorf("a revocation was dropped although the slots freed up in time:\n%s", logs.String())
	}
}

// A revocation that cannot get a slot within the overall bound is dropped with an
// Info line, and the limit holds meanwhile.
func TestProviderRevokerDropsWhatCannotGetASlotInTime(t *testing.T) {
	const fanOut = deviceRevokeConcurrency + 3
	c := &concurrencyProbe{release: make(chan struct{})}
	c.done.Add(deviceRevokeConcurrency) // only the ones that get a slot run
	var logs lockedBuf
	r := newTestRevoker(c, &logs, 200*time.Millisecond)
	for i := 0; i < fanOut; i++ {
		r.revoke(ulid.Make(), ulid.Make(), []byte("sealed"))
	}
	// The slot holders keep their slots past the bound; the three that waited run
	// out of time and are dropped.
	waitFor(t, "the waiting revocations to be dropped", func() bool {
		return strings.Count(logs.String(), "dropped the provider revocation") == fanOut-deviceRevokeConcurrency
	})
	close(c.release)
	c.done.Wait()
	if got := c.max.Load(); got > deviceRevokeConcurrency {
		t.Fatalf("saw %d in flight, want at most %d", got, deviceRevokeConcurrency)
	}
	if got := c.started.Load(); got != deviceRevokeConcurrency {
		t.Fatalf("%d revocations ran, want only the %d that held a slot", got, deviceRevokeConcurrency)
	}
}
