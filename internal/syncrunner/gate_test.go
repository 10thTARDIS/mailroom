// SPDX-License-Identifier: AGPL-3.0-only

package syncrunner

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
)

// newGateRunner builds a Runner wired only enough to exercise runGated: no
// IMAP, no DB, no scheduler. The gate lives entirely in memory.
func newGateRunner(t *testing.T) *Runner {
	t.Helper()
	return New(&fakeLister{}, &fakeSyncer{}, idPrepare, WithLogger(discardLogger()))
}

// TestGateSerializesOneAccount: many concurrent triggers for ONE account never
// run their work concurrently. The work asserts an in-flight counter never
// exceeds one.
func TestGateSerializesOneAccount(t *testing.T) {
	r := newGateRunner(t)
	userID, acctID := ulid.Make(), ulid.Make()

	var (
		active  int32
		maxSeen int32
		runs    int32
	)
	work := func() {
		n := atomic.AddInt32(&active, 1)
		for {
			old := atomic.LoadInt32(&maxSeen)
			if n <= old || atomic.CompareAndSwapInt32(&maxSeen, old, n) {
				break
			}
		}
		time.Sleep(time.Millisecond) // widen the window a real overlap would use
		atomic.AddInt32(&runs, 1)
		atomic.AddInt32(&active, -1)
	}

	const goroutines = 32
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.runGated(userID, acctID, work)
		}()
	}
	wg.Wait()

	if maxSeen > 1 {
		t.Fatalf("max concurrent runs = %d, want 1 (gate must serialize one account)", maxSeen)
	}
	if got := atomic.LoadInt32(&runs); got < 1 {
		t.Fatalf("runs = %d, want at least 1", got)
	}
}

// TestGateCoalescesExactlyOneFollowup: while a pass is in flight, ANY number of
// triggers collapse into EXACTLY ONE follow-up pass — never zero (the follow-up
// is mandatory), never more (they coalesce).
func TestGateCoalescesExactlyOneFollowup(t *testing.T) {
	for _, nTriggers := range []int{1, 2, 10, 50} {
		t.Run("", func(t *testing.T) {
			r := newGateRunner(t)
			userID, acctID := ulid.Make(), ulid.Make()

			var runs int32
			started := make(chan struct{})
			release := make(chan struct{})
			work := func() {
				if atomic.AddInt32(&runs, 1) == 1 {
					close(started) // first pass is now the holder...
					<-release      // ...and blocks until the triggers have landed
				}
			}

			done := make(chan struct{})
			go func() {
				r.runGated(userID, acctID, work)
				close(done)
			}()

			<-started // holder is inside pass #1
			// Fire N triggers while pass #1 is blocked. Each returns immediately
			// (pending), so they all collapse into one follow-up.
			for i := 0; i < nTriggers; i++ {
				r.runGated(userID, acctID, work)
			}
			close(release) // let pass #1 finish; the holder runs the one follow-up

			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("runGated did not return; gate deadlocked")
			}

			if got := atomic.LoadInt32(&runs); got != 2 {
				t.Fatalf("runs = %d, want 2 (one initial + exactly one coalesced follow-up), triggers=%d", got, nTriggers)
			}
		})
	}
}

// TestGateNoFollowupWithoutTrigger: a lone pass with no concurrent trigger runs
// exactly once — the coalescing loop must not re-run gratuitously.
func TestGateNoFollowupWithoutTrigger(t *testing.T) {
	r := newGateRunner(t)
	userID, acctID := ulid.Make(), ulid.Make()

	var runs int32
	r.runGated(userID, acctID, func() { atomic.AddInt32(&runs, 1) })

	if got := atomic.LoadInt32(&runs); got != 1 {
		t.Fatalf("runs = %d, want 1", got)
	}
}

// TestGateDifferentAccountsRunInParallel: two accounts are independent — their
// passes must be able to run at the same time. The work rendezvouses; if the
// gate serialized across accounts this would deadlock and time out.
func TestGateDifferentAccountsRunInParallel(t *testing.T) {
	r := newGateRunner(t)
	userID := ulid.Make()
	acctA, acctB := ulid.Make(), ulid.Make()

	bothIn := make(chan struct{})
	var arrived sync.WaitGroup
	arrived.Add(2)
	work := func() {
		arrived.Done()
		select {
		case <-bothIn:
		case <-time.After(2 * time.Second):
		}
	}
	go func() { arrived.Wait(); close(bothIn) }()

	done := make(chan struct{})
	go func() {
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); r.runGated(userID, acctA, work) }()
		go func() { defer wg.Done(); r.runGated(userID, acctB, work) }()
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("two accounts did not run in parallel; the gate is serializing across accounts")
	}
}

// TestGateTeardownDoesNotDeadlock: when the work returns promptly (e.g. it
// observed a cancelled context), a holder plus coalesced triggers still unwinds
// — the follow-up also returns promptly, so there is no deadlock or spin.
func TestGateTeardownDoesNotDeadlock(t *testing.T) {
	r := newGateRunner(t)
	userID, acctID := ulid.Make(), ulid.Make()

	var runs int32
	started := make(chan struct{})
	release := make(chan struct{})
	work := func() {
		if atomic.AddInt32(&runs, 1) == 1 {
			close(started)
			<-release
		}
		// Subsequent (follow-up) runs return immediately, simulating a pass that
		// sees a cancelled context and unwinds at once.
	}

	done := make(chan struct{})
	go func() { r.runGated(userID, acctID, work); close(done) }()

	<-started
	r.runGated(userID, acctID, work) // register a follow-up
	close(release)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("gate deadlocked on teardown")
	}
}
