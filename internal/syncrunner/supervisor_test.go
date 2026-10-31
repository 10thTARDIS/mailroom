// SPDX-License-Identifier: AGPL-3.0-only

package syncrunner

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
)

// fakeUserLister returns a caller-controlled set of active user ids.
type fakeUserLister struct {
	mu  sync.Mutex
	ids []ulid.ULID
	err error
}

func (f *fakeUserLister) set(ids ...ulid.ULID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ids = ids
}

func (f *fakeUserLister) ListActiveUserIDs(context.Context) ([]ulid.ULID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	out := make([]ulid.ULID, len(f.ids))
	copy(out, f.ids)
	return out, nil
}

// taskRecorder records how many times a per-user task started and stopped.
type taskRecorder struct {
	mu      sync.Mutex
	started map[ulid.ULID]int
	stopped map[ulid.ULID]int
}

func newTaskRecorder() *taskRecorder {
	return &taskRecorder{started: map[ulid.ULID]int{}, stopped: map[ulid.ULID]int{}}
}

// task is a long-running UserTask: it marks a start, blocks until its context is
// cancelled, then marks a stop — modelling a sync/index loop.
func (r *taskRecorder) task(ctx context.Context, userID ulid.ULID) {
	r.mu.Lock()
	r.started[userID]++
	r.mu.Unlock()
	<-ctx.Done()
	r.mu.Lock()
	r.stopped[userID]++
	r.mu.Unlock()
}

func (r *taskRecorder) count(m map[ulid.ULID]int, id ulid.ULID) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return m[id]
}

// eventually polls cond until it holds or the deadline passes.
func eventually(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition not met within deadline")
}

func quietSupervisor(lister UserLister, tasks []UserTask) *Supervisor {
	return NewSupervisor(lister, tasks,
		WithRefreshInterval(5*time.Millisecond),
		WithSupervisorLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
}

// The supervisor starts a task per active user, picks up a user that appears at
// runtime, tears down a user that vanishes, and — on shutdown — cancels every
// per-user context and only returns once all tasks have unwound (clean-shutdown
// barrier).
func TestSupervisorFansOutAndReconciles(t *testing.T) {
	u1, u2 := ulid.Make(), ulid.Make()
	lister := &fakeUserLister{}
	lister.set(u1)
	rec := newTaskRecorder()
	sup := quietSupervisor(lister, []UserTask{rec.task})

	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- sup.Run(ctx) }()

	// u1's task starts.
	eventually(t, func() bool { return rec.count(rec.started, u1) == 1 })

	// A user provisioned at runtime is picked up without a restart.
	lister.set(u1, u2)
	eventually(t, func() bool { return rec.count(rec.started, u2) == 1 })

	// A user that drops out of the active set has its loop torn down, while the
	// other keeps running.
	lister.set(u1)
	eventually(t, func() bool { return rec.count(rec.stopped, u2) == 1 })
	if got := rec.count(rec.stopped, u1); got != 0 {
		t.Fatalf("u1 stopped %d times while still active, want 0", got)
	}

	// Shutdown cancels everything and Run returns only after tasks unwind.
	cancel()
	select {
	case err := <-runErr:
		if err == nil {
			t.Error("Run returned nil error on cancel, want context.Canceled")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
	if got := rec.count(rec.stopped, u1); got != 1 {
		t.Fatalf("u1 stopped %d times after shutdown, want 1", got)
	}
}

// A listing error is not fatal: the current per-user set is kept rather than torn
// down, so a transient database blip does not stop everyone's sync.
func TestSupervisorKeepsSetOnListError(t *testing.T) {
	u1 := ulid.Make()
	lister := &fakeUserLister{}
	lister.set(u1)
	rec := newTaskRecorder()
	sup := quietSupervisor(lister, []UserTask{rec.task})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = sup.Run(ctx) }()

	eventually(t, func() bool { return rec.count(rec.started, u1) == 1 })

	// The lister now errors on every call; u1 must stay running.
	lister.mu.Lock()
	lister.err = context.DeadlineExceeded
	lister.mu.Unlock()

	// Give several refresh ticks a chance to (wrongly) tear u1 down.
	time.Sleep(40 * time.Millisecond)
	if got := rec.count(rec.stopped, u1); got != 0 {
		t.Fatalf("u1 stopped %d times during a list error, want 0 (set must be kept)", got)
	}
}
