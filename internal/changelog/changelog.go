// SPDX-License-Identifier: AGPL-3.0-only

// Package changelog runs the weekly change_log compaction sweep (mailroom#526,
// ADR-003 Consequences, mailroom-data-model.md §3.3): the durable, recurring
// per-user job that deletes change_log history older than the configured
// retention window and raises app.change_floor to match, so the log does not
// grow forever.
//
// Modelled on internal/drawer (itself modelled on internal/purge): a per-user
// recurring durable job, seeded at boot and self-rescheduling from inside the
// handler, draining the backlog across bounded batches — each batch its own
// transaction (store.UserTx via CompactChangeLog) — rather than one huge
// DELETE.
//
// It deletes rows, but it is not a delete of anything JMAP-visible: see the
// ⚠ on store.CompactChangeLog for why that does not touch the load-bearing
// change_log invariant.
package changelog

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// JobKind is the durable-job kind this worker registers for.
const JobKind = "change_log_compact"

// defaultInterval is the ordinary sweep cadence (ADR-003: "weekly").
const defaultInterval = 7 * 24 * time.Hour

// retryInterval is how soon a run that made progress but left work behind
// (a batch capped by maxDeletePerType, with more still eligible) comes back —
// short enough to drain a large first-run backlog in a handful of passes
// rather than waiting a full week between chunks, the same "drain quickly,
// then settle into the ordinary cadence" shape drawer.nextFireIn and
// rescan's own retry interval already take.
const retryInterval = time.Hour

// Worker compacts one user's due change_log backlog per fired job and
// reschedules the next sweep. Construct with NewWorker and register Handle
// with the dispatcher.
type Worker struct {
	st        *store.Store
	interval  time.Duration
	retention time.Duration
	batch     int64
	now       func() time.Time
	log       *slog.Logger
}

// NewWorker builds a compaction worker. retention is how far back change_log
// history is kept (MAILROOM_CHANGE_LOG_RETENTION / config.DefaultChangeLogRetention);
// config.Load already refuses anything below its 7-day floor, so a
// non-positive value here only guards a zero-value Config reaching this
// constructor directly, e.g. in a test, and falls back to 90 days. A
// non-positive interval falls back to defaultInterval (weekly); a nil logger
// falls back to slog.Default().
func NewWorker(st *store.Store, retention, interval time.Duration, log *slog.Logger) *Worker {
	if interval <= 0 {
		interval = defaultInterval
	}
	if retention <= 0 {
		retention = 90 * 24 * time.Hour
	}
	if log == nil {
		log = slog.Default()
	}
	return &Worker{
		st: st, interval: interval, retention: retention,
		batch: store.DefaultChangeLogCompactBatch, now: time.Now, log: log,
	}
}

// WithClock overrides the clock, for tests that need to control "now" without
// sleeping through a real timer.
func (w *Worker) WithClock(now func() time.Time) *Worker {
	w.now = now
	return w
}

// SweepResult tallies one sweep for logging and tests: the per-type results
// from every CompactChangeLog batch this run performed, and whether any batch
// hit its cap (meaning more may remain for the next run).
type SweepResult struct {
	Batches int
	Types   []store.CompactionResult // accumulated across every batch this run
	Capped  bool
}

// Handle runs one compaction job: it drains the user's due change_log backlog
// in bounded batches, then reschedules the next sweep. It is the dispatcher
// entry point.
//
// The reschedule runs even when the sweep errored, so the timer never stops
// ticking on a transient failure — the same posture internal/drawer and
// internal/purge take, for the same reason.
func (w *Worker) Handle(ctx context.Context, job store.Job) error {
	if job.UserID == nil {
		// Compaction is inherently per-user (it walks that user's own
		// RLS-scoped change_log). A system-scoped one is a wiring bug;
		// dead-letter it rather than spinning.
		return errors.New("changelog: job has no user id")
	}
	userID := *job.UserID

	res, sweepErr := w.Sweep(ctx, userID)
	if sweepErr != nil {
		w.log.Error("change log compaction failed", "user_id", userID.String(), "err", sweepErr)
	}
	for _, r := range res.Types {
		if r.Deleted == 0 {
			continue
		}
		w.log.Info("change log compacted",
			"user_id", userID.String(), "object_type", string(r.ObjectType),
			"deleted", r.Deleted, "rows_before", r.RowsBefore,
			"old_floor", r.OldFloor, "new_floor", r.NewFloor)
	}

	next := w.interval
	if res.Capped {
		next = retryInterval
	}
	if rerr := w.reschedule(ctx, userID, job.ID, next); rerr != nil {
		w.log.Error("change log compaction reschedule failed", "user_id", userID.String(), "err", rerr)
		if sweepErr == nil {
			return rerr
		}
	}
	return sweepErr
}

// Sweep drains every change_log type's due backlog for userID in batches of
// w.batch rows per type per call, stopping once a call deletes nothing at
// all — the same "re-scan from scratch each pass" shape
// cmd/mailroom-admin/drawerreport.go's applyDrawerSweep uses, adapted to a
// call that (unlike a per-message loop) already covers every type per batch.
func (w *Worker) Sweep(ctx context.Context, userID ulid.ULID) (SweepResult, error) {
	var res SweepResult
	cutoff := w.now().UTC().Add(-w.retention)

	for {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		var batchResults []store.CompactionResult
		if err := w.st.UserTx(ctx, userID, func(tx *store.UserTx) error {
			var e error
			batchResults, e = tx.CompactChangeLog(ctx, cutoff, w.batch, true)
			return e
		}); err != nil {
			return res, fmt.Errorf("changelog: compact batch: %w", err)
		}
		res.Batches++

		var deletedThisBatch int64
		for _, r := range batchResults {
			deletedThisBatch += r.Deleted
			if r.Deleted > 0 && r.Deleted >= w.batch {
				// This type may have more still eligible above the cap.
				res.Capped = true
			}
		}
		res.Types = append(res.Types, batchResults...)

		if deletedThisBatch == 0 {
			break
		}
	}

	w.log.Info("change log compaction summary",
		"user_id", userID.String(), "batches", res.Batches, "capped", res.Capped)
	return res, nil
}

// reschedule enqueues the next sweep, excluding this still-running job — the
// same "a declined reschedule means the chain just stopped" trap
// internal/drawer and internal/purge already carry the comment for.
func (w *Worker) reschedule(ctx context.Context, userID, currentJobID ulid.ULID, in time.Duration) error {
	return w.st.UserTx(ctx, userID, func(tx *store.UserTx) error {
		scheduled, err := tx.EnsureRecurringUserJob(ctx, JobKind, w.now().Add(in), currentJobID)
		if err == nil && !scheduled {
			w.log.Warn("change log compaction did not reschedule: another job of this kind is already outstanding; "+
				"if none is actually running, the compaction chain has stopped and change_log will grow unbounded",
				"user_id", userID.String(), "kind", JobKind)
		}
		return err
	})
}

// EnsureScheduled seeds the first compaction sweep for a user at boot when
// none is outstanding. Idempotent across restarts; the zero job id excludes
// nothing.
func EnsureScheduled(ctx context.Context, st *store.Store, userID ulid.ULID, fireAt time.Time) (bool, error) {
	var scheduled bool
	err := st.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		scheduled, e = tx.EnsureRecurringUserJob(ctx, JobKind, fireAt, ulid.ULID{})
		return e
	})
	return scheduled, err
}
