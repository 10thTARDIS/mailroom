// SPDX-License-Identifier: AGPL-3.0-only

package jobs

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// fakeQueue serves a preset batch once, then nothing, and records the
// transitions the dispatcher requests. Fail returns failState.
type fakeQueue struct {
	batch         []store.Job
	served        bool
	completed     []ulid.ULID
	failed        []failRec
	failState     store.JobState
	completeR     bool // what Complete returns
	dueCount      int64
	dueErr        error
	countDueCalls int
}

type failRec struct {
	id      ulid.ULID
	msg     string
	retryAt time.Time
}

func (q *fakeQueue) DequeueDue(_ context.Context, _ string, _ int) ([]store.Job, error) {
	if q.served {
		return nil, nil
	}
	q.served = true
	return q.batch, nil
}

// dueCount is what CountDue reports; dueErr makes it fail, which must degrade
// the gauge and never the dispatcher.
func (q *fakeQueue) CountDue(_ context.Context) (int64, error) {
	q.countDueCalls++
	return q.dueCount, q.dueErr
}

func (q *fakeQueue) Complete(_ context.Context, id ulid.ULID, _ string) (bool, error) {
	q.completed = append(q.completed, id)
	return q.completeR, nil
}

func (q *fakeQueue) Fail(_ context.Context, id ulid.ULID, _, msg string, retryAt time.Time) (store.JobState, error) {
	q.failed = append(q.failed, failRec{id, msg, retryAt})
	return q.failState, nil
}

type fakeMetrics struct {
	claimed  int
	depth    []int64
	deadKind []string
}

func (m *fakeMetrics) ObserveClaimed(n int)      { m.claimed += n }
func (m *fakeMetrics) ObserveQueueDepth(n int64) { m.depth = append(m.depth, n) }
func (m *fakeMetrics) DeadLettered(kind string)  { m.deadKind = append(m.deadKind, kind) }

func testJob(kind string, attempts int) store.Job {
	return store.Job{ID: ulid.Make(), Kind: kind, Attempts: attempts, MaxAttempts: 5}
}

func quietDispatcher(q Queue, m Metrics, now time.Time) *Dispatcher {
	return New(q, Config{
		Worker:  "test",
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Metrics: m,
		Now:     func() time.Time { return now },
		Backoff: func(attempts int) time.Duration { return time.Duration(attempts) * time.Minute },
	})
}

func TestDispatcherCompletesOnSuccess(t *testing.T) {
	job := testJob("noop", 1)
	q := &fakeQueue{batch: []store.Job{job}, completeR: true}
	m := &fakeMetrics{}
	d := quietDispatcher(q, m, time.Unix(1000, 0))

	var ran bool
	d.Register("noop", func(context.Context, store.Job) error { ran = true; return nil })
	d.runOnce(context.Background())

	if !ran {
		t.Fatal("handler did not run")
	}
	if len(q.completed) != 1 || q.completed[0] != job.ID {
		t.Errorf("job not completed: %v", q.completed)
	}
	if len(q.failed) != 0 {
		t.Errorf("unexpected failures: %v", q.failed)
	}
	if m.claimed != 1 {
		t.Errorf("claimed = %d, want 1", m.claimed)
	}
}

func TestDispatcherRetriesOnError(t *testing.T) {
	job := testJob("boom", 2) // attempts < max, so fail_job would retry
	q := &fakeQueue{batch: []store.Job{job}, failState: store.JobPending}
	m := &fakeMetrics{}
	now := time.Unix(1000, 0)
	d := quietDispatcher(q, m, now)

	d.Register("boom", func(context.Context, store.Job) error { return errors.New("kaboom") })
	d.runOnce(context.Background())

	if len(q.completed) != 0 {
		t.Errorf("job should not have completed: %v", q.completed)
	}
	if len(q.failed) != 1 {
		t.Fatalf("want 1 fail call, got %d", len(q.failed))
	}
	f := q.failed[0]
	if f.msg != "kaboom" {
		t.Errorf("fail msg = %q, want kaboom", f.msg)
	}
	// Backoff(2) = 2 minutes from the injected clock.
	if want := now.Add(2 * time.Minute); !f.retryAt.Equal(want) {
		t.Errorf("retryAt = %v, want %v", f.retryAt, want)
	}
	if len(m.deadKind) != 0 {
		t.Errorf("should not dead-letter on retry: %v", m.deadKind)
	}
}

func TestDispatcherDeadLettersWhenExhausted(t *testing.T) {
	job := testJob("boom", 5) // attempts == max
	q := &fakeQueue{batch: []store.Job{job}, failState: store.JobDead}
	m := &fakeMetrics{}
	d := quietDispatcher(q, m, time.Unix(1000, 0))

	d.Register("boom", func(context.Context, store.Job) error { return errors.New("still broken") })
	d.runOnce(context.Background())

	if len(q.failed) != 1 {
		t.Fatalf("want 1 fail call, got %d", len(q.failed))
	}
	if len(m.deadKind) != 1 || m.deadKind[0] != "boom" {
		t.Errorf("dead-letter metric = %v, want [boom]", m.deadKind)
	}
}

func TestDispatcherUnknownKindFails(t *testing.T) {
	job := testJob("mystery", 1)
	q := &fakeQueue{batch: []store.Job{job}, failState: store.JobPending}
	d := quietDispatcher(q, &fakeMetrics{}, time.Unix(1000, 0))
	// No handler registered for "mystery".
	d.runOnce(context.Background())

	if len(q.failed) != 1 {
		t.Fatalf("unknown kind should fail the job, got %d fail calls", len(q.failed))
	}
	if len(q.completed) != 0 {
		t.Errorf("unknown kind should not complete: %v", q.completed)
	}
}

func TestDispatcherLostLockDoesNotPanic(t *testing.T) {
	job := testJob("noop", 1)
	q := &fakeQueue{batch: []store.Job{job}, completeR: false} // lock lost
	d := quietDispatcher(q, &fakeMetrics{}, time.Unix(1000, 0))
	d.Register("noop", func(context.Context, store.Job) error { return nil })
	d.runOnce(context.Background()) // must not panic; simply logs
}

func TestDispatcherRunStopsOnCancel(t *testing.T) {
	q := &fakeQueue{}
	d := New(q, Config{Worker: "test", PollInterval: time.Hour,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled
	if err := d.Run(ctx); err != nil {
		t.Errorf("Run returned %v, want nil on clean cancel", err)
	}
}

// Queue depth is reported from a COUNT, not from the size of the claimed batch
// (#524). The batch is bounded by BatchSize, so a claimed-count flattens at the
// batch exactly when backlog is worst; the whole point of app.count_due_jobs is
// to see past it. Here the queue is far deeper than the batch, and the reported
// depth must be the deeper number.
func TestQueueDepthComesFromTheCountNotTheBatch(t *testing.T) {
	q := &fakeQueue{batch: []store.Job{testJob("purge_trash", 0)}, completeR: true, dueCount: 4096}
	m := &fakeMetrics{}
	d := quietDispatcher(q, m, time.Unix(1000, 0))
	d.Register("purge_trash", func(context.Context, store.Job) error { return nil })

	d.runOnce(context.Background())

	if len(m.depth) != 1 || m.depth[0] != 4096 {
		t.Errorf("reported depth = %v, want [4096]", m.depth)
	}
	// The saturation signal is still its own number and must not have been
	// conflated with depth.
	if m.claimed != 1 {
		t.Errorf("claimed = %d, want 1", m.claimed)
	}
}

// A failing count degrades the GAUGE and never the dispatcher: observability
// must not stop mail work. Without this, a broken definer or a lost connection
// would take the job queue down with it.
func TestAFailingQueueDepthCountDoesNotStopThePass(t *testing.T) {
	q := &fakeQueue{
		batch:     []store.Job{testJob("purge_trash", 0)},
		completeR: true,
		dueErr:    errors.New("definer exploded"),
	}
	m := &fakeMetrics{}
	d := quietDispatcher(q, m, time.Unix(1000, 0))

	ran := false
	d.Register("purge_trash", func(context.Context, store.Job) error { ran = true; return nil })
	d.runOnce(context.Background())

	if !ran {
		t.Error("the job did not run; a metrics failure stopped the dispatcher")
	}
	if len(q.completed) != 1 {
		t.Errorf("completed %d jobs, want 1", len(q.completed))
	}
	if len(m.depth) != 0 {
		t.Errorf("depth was reported despite the count failing: %v", m.depth)
	}
}
