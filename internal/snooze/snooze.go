// SPDX-License-Identifier: AGPL-3.0-only

// Package snooze fires snooze wake-ups (spec §6, mailroom#51).
//
// A snoozed message leaves the inbox view until its wake time passes, then
// returns. The wake rides the durable job model (spec §16/§14) as
// store.SnoozeJobKind, so timers survive restarts, and internal/jobs' dispatcher
// runs a catch-up pass on boot — a wake whose time passed while the server was
// down fires at next start rather than being lost. That is §6's durability
// requirement, and it is satisfied by machinery that already existed.
//
// THE WORKER IS DELIBERATELY ALMOST EMPTY. All the care lives in
// store.WakeSnoozed, which wakes by TIME rather than by an id list carried in
// the job payload:
//
//   - it is idempotent, so a job that fires twice (a crash between the work and
//     the completion write, two dispatchers racing a batch) wakes nothing the
//     second time and appends no duplicate change_log rows;
//   - it cannot act on a stale premise, because a payload list would go stale
//     the moment a message was un-snoozed, re-snoozed, trashed or purged between
//     enqueue and fire.
//
// Together those mean this handler needs no locking, no dedup and no
// reconciliation of its own — the correctness is in the query.
package snooze

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// Worker handles snooze_wake jobs. Construct with NewWorker and register Handle
// with the dispatcher.
type Worker struct {
	st  *store.Store
	now func() time.Time
	log *slog.Logger
}

// NewWorker builds a snooze wake worker. A nil logger is rejected at Handle
// time rather than at construction, matching internal/purge.
func NewWorker(st *store.Store, log *slog.Logger) *Worker {
	return &Worker{st: st, now: time.Now, log: log}
}

// WithClock overrides the clock, for tests that need to control "now" without
// sleeping through a real timer.
func (w *Worker) WithClock(now func() time.Time) *Worker {
	w.now = now
	return w
}

// Handle wakes everything due for the job's user and, if that user still has a
// later snooze pending, schedules the next wake.
//
// The reschedule is what keeps a long snooze cheap: a message snoozed for a
// month costs ONE timer a month away, not a wake-up every minute for a month.
// It is also what repairs a lost timer — if a wake job were ever dropped, the
// next fire for that user re-derives the next wake from the data rather than
// from the job queue's memory of it.
func (w *Worker) Handle(ctx context.Context, job store.Job) error {
	if job.UserID == nil {
		// A snooze wake is inherently per-user: it walks that user's RLS-scoped
		// messages. A system-scoped one is a wiring bug — fail so the dispatcher
		// eventually dead-letters it rather than spinning forever.
		return errors.New("snooze: job has no user id")
	}
	if w.log == nil {
		return errors.New("snooze: no logger configured")
	}
	userID := *job.UserID

	var (
		woke []ulid.ULID
		next time.Time
		more bool
	)
	if err := w.st.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var err error
		if woke, err = tx.WakeSnoozed(ctx, w.now()); err != nil {
			return err
		}
		next, more, err = tx.NextSnoozeWake(ctx)
		return err
	}); err != nil {
		return fmt.Errorf("snooze: wake for user %s: %w", userID, err)
	}

	if len(woke) > 0 {
		w.log.Info("snooze wake", "user_id", userID.String(), "messages", len(woke))
	}

	if more {
		// Scheduled in its own transaction: the wake above is the part that must
		// not be lost, and it is already committed. A failure to schedule the
		// NEXT one is recoverable — it is logged and returned so the dispatcher
		// retries this job, and the wake itself stays done because WakeSnoozed is
		// idempotent.
		if err := w.st.UserTx(ctx, userID, func(tx *store.UserTx) error {
			_, err := tx.EnsureSnoozeWakeJob(ctx, next)
			return err
		}); err != nil {
			return fmt.Errorf("snooze: reschedule for user %s: %w", userID, err)
		}
	}
	return nil
}

// EnsureScheduled seeds a wake at boot for a user who has snoozed mail but no
// outstanding timer — the repair path for a job queue that lost one (a dropped
// job, a restore from a backup taken before the job was enqueued). It is
// idempotent: with no pending snooze it schedules nothing.
func EnsureScheduled(ctx context.Context, st *store.Store, userID ulid.ULID) (bool, error) {
	var scheduled bool
	err := st.UserTx(ctx, userID, func(tx *store.UserTx) error {
		next, more, err := tx.NextSnoozeWake(ctx)
		if err != nil || !more {
			return err
		}
		scheduled, err = tx.EnsureSnoozeWakeJob(ctx, next)
		return err
	})
	return scheduled, err
}
