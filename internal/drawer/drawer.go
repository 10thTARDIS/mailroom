// SPDX-License-Identifier: AGPL-3.0-only

// Package drawer runs the drawer auto-archive sweep (mailroom#443, Direction 7
// "Desk and Drawers", #372): below the line, each area's DRAWER holds mail
// the desk judged unimportant, and this worker MOVES what has sat there
// longer than the area's period to the account's Archive folder — a real
// server-side move, so every other client agrees about what is in the inbox.
//
// NEVER DELETE. NEVER MARK AS READ. Only an explicit user rule may do either
// (a firm requirement, #443). This worker's whole job is a folder
// move; there is no path here that touches is_seen or issues a DELETE — see
// internal/store/drawer_sweep.go's ArchiveByDrawer for where that is actually
// enforced.
//
// Modelled on internal/purge: a per-user recurring durable job (spec §16),
// seeded at boot and self-rescheduling from inside the handler, one bounded
// batch per fired job with EACH MESSAGE in its own transaction so a single
// failure is isolated and logged rather than poisoning the rest of the batch.
//
// ⚠ THIS PACKAGE DOES NOT RUN UNSUPERVISED BY DEFAULT (migration 00048,
// #443). Sweep reads DeskPreferences.DrawerSweepEnabled — a login-level
// MASTER SWITCH, default false — before touching anything else, and archives
// nothing while it is off, whatever any area's own auto_archive says: the
// per-area switch alone was not enough, because the default plan
// materialises five areas, all on, and this job is seeded and self-firing
// from the moment the process boots. `mailroom-admin drawer-report` is the
// preview and `--enable` is the switch.
package drawer

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
const JobKind = store.DrawerJobKind

// defaultInterval is the ordinary sweep cadence once a user's drawer is
// caught up.
const defaultInterval = time.Hour

// defaultBatch bounds how many candidates one sweep processes. Small enough
// that a first-run backlog of a thousand-plus messages drains in a handful of short
// passes rather than one long one that would block `submit` on the
// sequential dispatcher.
const defaultBatch = 200

// fullBatchRetry is how soon a FULL batch comes back — draining a large
// backlog in a few short runs, each comfortably under the 15-minute job
// lease, rather than waiting a full interval between chunks.
const fullBatchRetry = time.Minute

// noneDeletedNoneMarkedRead is the constant sentence every sweep log line
// carries (#443, a firm requirement): this worker moves mail and
// nothing else.
const noneDeletedNoneMarkedRead = "none deleted, none marked read"

// sweepDisabledMessage is Handle's Info line for a run the login-level
// master switch (DeskPreferences.DrawerSweepEnabled, migration 00048, #443)
// turned into a no-op. Named as a constant because it is the one sentence an
// operator watching the logs after a deploy needs to see verbatim: the
// sweep did NOT run, and here is exactly how to make it run.
const sweepDisabledMessage = "drawer sweep is off for this login; nothing archived — " +
	"enable it in the desk preferences after reading mailroom-admin drawer-report"

// Worker archives one user's due drawer candidates per fired job and
// reschedules the next sweep. Construct with NewWorker and register Handle
// with the dispatcher.
type Worker struct {
	st       *store.Store
	interval time.Duration
	batch    int
	now      func() time.Time
	log      *slog.Logger
}

// NewWorker builds a drawer sweep worker. interval is the cadence once the
// drawer is caught up (a non-positive interval falls back to one hour); a
// nil logger falls back to slog.Default().
func NewWorker(st *store.Store, interval time.Duration, log *slog.Logger) *Worker {
	if interval <= 0 {
		interval = defaultInterval
	}
	if log == nil {
		log = slog.Default()
	}
	return &Worker{st: st, interval: interval, batch: defaultBatch, now: time.Now, log: log}
}

// WithClock overrides the clock, for tests that need to control "now"
// without sleeping through a real timer.
func (w *Worker) WithClock(now func() time.Time) *Worker {
	w.now = now
	return w
}

// SweepResult tallies one sweep for logging and tests.
type SweepResult struct {
	// Disabled reports the login-level master switch (DeskPreferences'
	// DrawerSweepEnabled, migration 00048, #443): when true, Sweep read the
	// switch as off and did none of the rest of its work — Candidates,
	// Archived and PerArea are all zero on a disabled result, never a
	// partial count.
	Disabled   bool
	Candidates int            // due messages examined (capped at the batch size)
	Archived   int            // messages actually moved to Archive
	Errors     int            // per-message failures — logged, isolated, retried next sweep
	PerArea    map[string]int // archived count, by area name
}

// Handle runs one drawer sweep job: it archives the user's due batch, then
// reschedules the next sweep. It is the dispatcher entry point.
//
// The reschedule runs even when the sweep errored, so the timer never stops
// ticking on a transient failure — the same posture internal/purge takes, and
// for the same reason.
func (w *Worker) Handle(ctx context.Context, job store.Job) error {
	if job.UserID == nil {
		// A drawer sweep is inherently per-user (it walks that user's
		// RLS-scoped areas and mail). A system-scoped one is a wiring bug;
		// dead-letter it rather than spinning.
		return errors.New("drawer: job has no user id")
	}
	userID := *job.UserID

	res, sweepErr := w.Sweep(ctx, userID)
	if sweepErr != nil {
		w.log.Error("drawer sweep failed", "user_id", userID.String(), "err", sweepErr)
	}
	if res.Disabled {
		// ONCE PER RUN, at Info: an operator watching the logs after a deploy
		// must see plainly that the hourly job is doing nothing and why,
		// without having to know to go read drawer-report first (#443).
		w.log.Info(sweepDisabledMessage, "user_id", userID.String())
	}

	next := w.nextFireIn(res)
	if rerr := w.reschedule(ctx, userID, job.ID, next); rerr != nil {
		w.log.Error("drawer reschedule failed", "user_id", userID.String(), "err", rerr)
		if sweepErr == nil {
			return rerr
		}
	}
	return sweepErr
}

// nextFireIn decides how soon the sweep comes back. DrawerCandidates is
// capped at w.batch, so a FULL batch means more candidates may remain behind
// the ones just processed — the same "drain a backlog quickly, then settle
// into the ordinary cadence" shape internal/rescan's nextFireIn already
// takes, restated for a batch-size signal instead of a remaining-count one:
// DrawerCandidates re-queries from scratch every call (idempotent by the
// app.drawer_archives row, not by a payload list), so there is no separate
// "remaining" figure to ask for — a full batch back IS the sign more work is
// waiting.
//
// ⚠ ONLY IF THE BATCH MADE PROGRESS. A full batch that archived NOTHING is
// not a backlog draining, it is the same candidates coming back every time
// (every re-check failing, or a bug in the predicate) — retrying that every
// minute would be a busy loop that never ends and never logs anything new.
// Such a batch waits the ordinary interval like an idle one.
func (w *Worker) nextFireIn(res SweepResult) time.Duration {
	// A disabled run is decided FIRST and unconditionally: the master switch
	// being off means the sweep did not run at all, so a Candidates/Archived
	// count on such a result — Sweep never populates one, but nothing stops
	// a future caller from handing nextFireIn one directly — must never be
	// read as "a full batch drained quickly." The chain still ticks on the
	// ordinary cadence so the moment the switch flips, the next run acts.
	if res.Disabled {
		return w.interval
	}
	if res.Candidates >= w.batch && res.Archived > 0 {
		return fullBatchRetry
	}
	return w.interval
}

// Sweep processes one bounded batch of the user's due drawer candidates.
// EACH MESSAGE IN ITS OWN TRANSACTION, so a per-message failure is isolated —
// it is logged and the message stays in the drawer to be retried next sweep,
// and it never poisons the rest of the batch (internal/purge's same
// isolation posture, applied per message here too).
func (w *Worker) Sweep(ctx context.Context, userID ulid.ULID) (SweepResult, error) {
	res := SweepResult{PerArea: map[string]int{}}
	now := w.now().UTC()

	// THE MASTER SWITCH, READ FIRST (migration 00048, #443): a per-area
	// auto_archive is not enough on its own — the default plan materialises
	// five areas, all on — so nothing below this check ever runs, and
	// nothing is archived, until the login's own DrawerSweepEnabled is true.
	// Handle logs this once per run and still reschedules on the ordinary
	// interval, so the chain keeps ticking and the moment the switch flips,
	// the very next run does the work.
	var prefs store.DeskPreferences
	if err := w.st.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		prefs, e = tx.GetDeskPreferences(ctx)
		return e
	}); err != nil {
		return res, fmt.Errorf("drawer: read desk preferences: %w", err)
	}
	if !prefs.DrawerSweepEnabled {
		res.Disabled = true
		return res, nil
	}

	var candidates []store.DrawerCandidate
	if err := w.st.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		candidates, e = tx.DrawerCandidates(ctx, now, w.batch)
		return e
	}); err != nil {
		return res, fmt.Errorf("drawer: candidate scan: %w", err)
	}
	res.Candidates = len(candidates)

	for _, c := range candidates {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		var archived bool
		txErr := w.st.UserTx(ctx, userID, func(tx *store.UserTx) error {
			var e error
			archived, e = tx.ArchiveByDrawer(ctx, c, now)
			return e
		})
		if txErr != nil {
			w.log.Warn("drawer: could not archive one message; it stays in the drawer, retried next sweep",
				"user_id", userID.String(), "message_id", c.ID.String(), "area", c.AreaName, "err", txErr)
			res.Errors++
			continue
		}
		if archived {
			res.Archived++
			res.PerArea[c.AreaName]++
		}
	}

	for area, n := range res.PerArea {
		w.log.Info("drawer sweep: "+noneDeletedNoneMarkedRead,
			"user_id", userID.String(), "area", area, "archived", n)
	}
	w.log.Info("drawer sweep summary: "+noneDeletedNoneMarkedRead,
		"user_id", userID.String(), "candidates", res.Candidates, "archived", res.Archived, "errors", res.Errors)
	return res, nil
}

// reschedule enqueues the next sweep, excluding this still-running job — the
// same "a declined reschedule means the chain just stopped" trap
// internal/purge and internal/rescan already carry the comment for.
func (w *Worker) reschedule(ctx context.Context, userID, currentJobID ulid.ULID, in time.Duration) error {
	return w.st.UserTx(ctx, userID, func(tx *store.UserTx) error {
		scheduled, err := tx.EnsureRecurringUserJob(ctx, JobKind, w.now().Add(in), currentJobID)
		if err == nil && !scheduled {
			w.log.Warn("drawer sweep did not reschedule: another job of this kind is already outstanding; "+
				"if none is actually running, the sweep chain has stopped and the drawer will not auto-archive",
				"user_id", userID.String(), "kind", JobKind)
		}
		return err
	})
}

// EnsureScheduled seeds the first drawer sweep for a user at boot when none
// is outstanding. Idempotent across restarts; the zero job id excludes
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
