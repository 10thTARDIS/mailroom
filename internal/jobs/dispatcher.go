// SPDX-License-Identifier: AGPL-3.0-only

package jobs

import (
	"context"
	"log/slog"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// Queue is the store surface the dispatcher needs. StoreQueue is the
// production implementation; tests supply a fake.
type Queue interface {
	// DequeueDue claims up to limit due jobs for worker, moving them to
	// 'running' with attempts incremented.
	DequeueDue(ctx context.Context, worker string, limit int) ([]store.Job, error)
	// Complete transitions a claimed job to 'done'. Returns false if the
	// worker no longer holds the job.
	Complete(ctx context.Context, id ulid.ULID, worker string) (bool, error)
	// Fail retries (back to 'pending' at retryAt) or dead-letters a claimed
	// job, returning the resulting state.
	Fail(ctx context.Context, id ulid.ULID, worker, errMsg string, retryAt time.Time) (store.JobState, error)
	// CountDue reports how many jobs are pending and already due across all
	// users. Observability only — the dispatcher never gates work on it.
	CountDue(ctx context.Context) (int64, error)
}

// Handler runs one job. A nil error means success (the job is completed); a
// non-nil error triggers retry-or-dead-letter.
type Handler func(ctx context.Context, job store.Job) error

// Metrics receives dispatcher observability signals. All methods must be safe
// to call from the dispatcher goroutine. NopMetrics is the default.
type Metrics interface {
	// ObserveClaimed reports how many jobs a pass claimed (queue pressure).
	//
	// ⚠ THIS IS SATURATION, NOT DEPTH: it is bounded by Config.BatchSize, so it
	// cannot exceed the batch however deep the queue is. Use ObserveQueueDepth
	// for backlog.
	ObserveClaimed(n int)
	// ObserveQueueDepth reports the number of jobs that are pending and already
	// due across ALL users — the real backlog, unbounded by the batch size.
	ObserveQueueDepth(n int64)
	// DeadLettered reports a job of kind that exhausted its retries.
	DeadLettered(kind string)
}

// NopMetrics is a Metrics that discards everything.
type NopMetrics struct{}

func (NopMetrics) ObserveClaimed(int)      {}
func (NopMetrics) ObserveQueueDepth(int64) {}
func (NopMetrics) DeadLettered(string)     {}

// Config configures a Dispatcher. Zero values are replaced with sane defaults.
type Config struct {
	// Worker identifies this dispatcher instance (stored as locked_by).
	Worker string
	// PollInterval is the delay between passes (default 5s).
	PollInterval time.Duration
	// BatchSize bounds jobs claimed per pass (default 50).
	BatchSize int
	// Backoff maps a job's attempt count to the retry delay (default
	// exponential, capped at 1h).
	Backoff func(attempts int) time.Duration
	// Logger receives structured logs (default slog.Default()).
	Logger *slog.Logger
	// Metrics receives observability signals (default NopMetrics).
	Metrics Metrics
	// Now supplies the current time; injectable for tests (default time.Now).
	Now func() time.Time
}

// Dispatcher claims and runs durable jobs. Construct with New, Register
// handlers, then Run.
type Dispatcher struct {
	q        Queue
	cfg      Config
	handlers map[string]Handler
}

// New builds a Dispatcher over q, applying defaults to cfg.
func New(q Queue, cfg Config) *Dispatcher {
	if cfg.Worker == "" {
		cfg.Worker = "dispatcher-" + ulid.Make().String()
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 5 * time.Second
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 50
	}
	if cfg.Backoff == nil {
		cfg.Backoff = defaultBackoff
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Metrics == nil {
		cfg.Metrics = NopMetrics{}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Dispatcher{q: q, cfg: cfg, handlers: map[string]Handler{}}
}

// Register associates a handler with a job kind. Registering the same kind
// twice replaces the handler. Call before Run.
func (d *Dispatcher) Register(kind string, h Handler) { d.handlers[kind] = h }

// Run drives the dispatcher until ctx is cancelled. It runs one pass
// immediately (catch-up on boot), then one pass per PollInterval. Run returns
// nil on a clean shutdown (ctx cancelled) and only errors on an unexpected
// failure it cannot recover from.
func (d *Dispatcher) Run(ctx context.Context) error {
	d.cfg.Logger.Info("dispatcher starting", "worker", d.cfg.Worker,
		"poll_interval", d.cfg.PollInterval, "batch_size", d.cfg.BatchSize)

	// Catch-up pass on boot for any timers that fired while we were down.
	d.runOnce(ctx)

	ticker := time.NewTicker(d.cfg.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			d.cfg.Logger.Info("dispatcher stopped", "worker", d.cfg.Worker)
			return nil
		case <-ticker.C:
			d.runOnce(ctx)
		}
	}
}

// runOnce claims one batch of due jobs and processes each. A dequeue error is
// logged and the pass ends (the next tick retries); per-job errors never abort
// the pass.
func (d *Dispatcher) runOnce(ctx context.Context) {
	jobsBatch, err := d.q.DequeueDue(ctx, d.cfg.Worker, d.cfg.BatchSize)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		d.cfg.Logger.Error("dispatcher dequeue failed", "worker", d.cfg.Worker, "err", err)
		return
	}
	d.cfg.Metrics.ObserveClaimed(len(jobsBatch))
	// Backlog is reported AFTER the claim, so the gauge reflects what is still
	// owed rather than counting the batch we are about to run. A failure here is
	// logged and dropped: observability must never stop the dispatcher, and a
	// stale gauge is a smaller problem than a stalled queue.
	if n, err := d.q.CountDue(ctx); err != nil {
		if ctx.Err() == nil {
			d.cfg.Logger.Warn("dispatcher queue-depth count failed; the gauge is stale", "err", err)
		}
	} else {
		d.cfg.Metrics.ObserveQueueDepth(n)
	}
	for _, job := range jobsBatch {
		if ctx.Err() != nil {
			return
		}
		d.process(ctx, job)
	}
}

func (d *Dispatcher) process(ctx context.Context, job store.Job) {
	log := d.cfg.Logger.With("job_id", job.ID.String(), "kind", job.Kind, "attempt", job.Attempts)

	handler, ok := d.handlers[job.Kind]
	if !ok {
		// No handler registered — treat as a (retryable) error so a job whose
		// handler ships in a later deploy recovers, rather than silently
		// dead-lettering it now.
		log.Warn("no handler registered for job kind")
		d.fail(ctx, job, "no handler registered for kind "+job.Kind, log)
		return
	}

	if err := handler(ctx, job); err != nil {
		if ctx.Err() != nil {
			// Shutting down: leave the job 'running' with its lock. A later
			// dispatcher pass reclaims it once the lease ages out — 15 minutes,
			// enforced inside app.dequeue_due_jobs (db/migrations/00041).
			//
			// ⚠ That sentence was ASPIRATIONAL until #351. locked_at was written
			// on every dequeue and read by nothing, so nothing aged out and every
			// restart during a job stranded it permanently — taking its recurring
			// chain with it, silently. Do not burn a retry on shutdown.
			return
		}
		log.Warn("job handler failed", "err", err)
		d.fail(ctx, job, err.Error(), log)
		return
	}

	done, err := d.q.Complete(ctx, job.ID, d.cfg.Worker)
	if err != nil {
		log.Error("marking job done failed", "err", err)
		return
	}
	if !done {
		log.Warn("job lock lost before completion")
	}
}

// fail records a job error and reports a dead-letter if it exhausted retries.
func (d *Dispatcher) fail(ctx context.Context, job store.Job, msg string, log *slog.Logger) {
	retryAt := d.cfg.Now().Add(d.cfg.Backoff(job.Attempts))
	state, err := d.q.Fail(ctx, job.ID, d.cfg.Worker, msg, retryAt)
	if err != nil {
		log.Error("marking job failed errored", "err", err)
		return
	}
	switch state {
	case store.JobDead:
		log.Error("job dead-lettered after exhausting retries", "attempts", job.Attempts, "last_error", msg)
		d.cfg.Metrics.DeadLettered(job.Kind)
	case store.JobPending:
		log.Info("job scheduled for retry", "retry_at", retryAt)
	}
}

// defaultBackoff is exponential from 30s, doubling per attempt, capped at 1h.
func defaultBackoff(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	const base = 30 * time.Second
	const cap = time.Hour
	d := base
	for i := 1; i < attempts && d < cap; i++ {
		d *= 2
	}
	if d > cap {
		d = cap
	}
	return d
}

// StoreQueue adapts *store.Store to Queue, running every operation through a
// SystemTx (identity unset; cross-user job access via the narrow SECURITY
// DEFINER functions only).
type StoreQueue struct{ Store *store.Store }

// DequeueDue implements Queue.
func (q StoreQueue) DequeueDue(ctx context.Context, worker string, limit int) ([]store.Job, error) {
	var out []store.Job
	err := q.Store.SystemTx(ctx, func(tx *store.SystemTx) error {
		var e error
		out, e = tx.DequeueDueJobs(ctx, worker, limit)
		return e
	})
	return out, err
}

// CountDue implements Queue via app.count_due_jobs, the aggregate-only SECURITY
// DEFINER. A plain count in this SystemTx would see SYSTEM jobs only and
// silently undercount every per-user job — see store.CountDueJobs.
func (q StoreQueue) CountDue(ctx context.Context) (int64, error) {
	var n int64
	err := q.Store.SystemTx(ctx, func(tx *store.SystemTx) error {
		var e error
		n, e = tx.CountDueJobs(ctx)
		return e
	})
	return n, err
}

// Complete implements Queue.
func (q StoreQueue) Complete(ctx context.Context, id ulid.ULID, worker string) (bool, error) {
	var done bool
	err := q.Store.SystemTx(ctx, func(tx *store.SystemTx) error {
		var e error
		done, e = tx.CompleteJob(ctx, id, worker)
		return e
	})
	return done, err
}

// Fail implements Queue.
func (q StoreQueue) Fail(ctx context.Context, id ulid.ULID, worker, errMsg string, retryAt time.Time) (store.JobState, error) {
	var state store.JobState
	err := q.Store.SystemTx(ctx, func(tx *store.SystemTx) error {
		var e error
		state, e = tx.FailJob(ctx, id, worker, errMsg, retryAt)
		return e
	})
	return state, err
}
