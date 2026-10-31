// SPDX-License-Identifier: AGPL-3.0-only

// Package purge runs the terminal step of the deletion lifecycle (ADR-009,
// data-model §6): the durable, unattended job that hard-deletes messages whose
// 30-day (configurable) Trash window has closed, reclaims their content-addressed
// blobs, and leaves an audit tombstone.
//
// It is the missing half of a lifecycle whose soft-delete end already existed:
// Email/set trashes a message (trashed_at + purge_after, an Email `updated`) and
// enqueues the per-account move to Trash; the reaper soft-deletes a vanished one.
// Nothing, until now, ever finished the job — Trash grew forever and blobs were
// never reclaimed. This worker closes that.
//
// PROPAGATE-FIRST, CANONICAL-LAST (the safety spine, mirrored from the reaper).
// A due message that still has live IMAP provenance has copies on servers we
// sync; its EXPUNGE is enqueued through the writeback path (which dead-letters —
// SURFACED — on failure) and its canonical row is left ALONE. Only once every
// provenance copy is gone — the writeback engine expunged it and the reaper
// dropped the now-absent UID — does a later sweep hard-delete the canonical row.
// So a failed EXPUNGE can never drop the record and the audit trail while a copy
// still sits on the server; it just defers the purge and shows up in account
// health. See store.HardDeleteMessage for the blob-reclaim race proof.
//
// AGENT REACH (spec §13a, ADR-009). There is no trigger for this job on any
// request-driven surface. It is a system timer, dispatched under a SystemTx and
// handed a user id by app.dequeue_due_jobs. The only delete an Email/set (and
// hence any future agent using it) can reach is the TRASH transition; the purge
// is unreachable from any API. That is the seam that makes "agents can only reach
// trash" structural rather than remembered.
package purge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/blob"
	"mailroom/internal/store"
)

// JobKind is the durable-job kind the purge worker registers for. It matches the
// example in db/schema.sql's app.jobs comment ('purge_trash').
const JobKind = "purge_trash"

// defaultBatch bounds how many due messages one sweep processes. A large Trash
// backlog drains across successive (hourly) sweeps rather than in one enormous
// transaction burst; at the 0.5 scale a single bounded pass is almost always the
// whole backlog anyway.
const defaultBatch = 500

// Worker hard-deletes due Trash for one user per fired job and reschedules the
// next sweep. Construct with NewWorker and register Handle with the dispatcher.
type Worker struct {
	st       *store.Store
	blobs    blob.BlobStore
	interval time.Duration
	batch    int
	now      func() time.Time
	log      *slog.Logger
}

// NewWorker builds a purge worker. interval is the sweep cadence (the handler
// reschedules itself this far out); blobs reclaims the raw-message blob FILE
// after its metadata row is deleted, and must be the SAME store the ingest path
// wrote — a purge that could not reach the bytes would leak every file. A
// non-positive interval falls back to one hour; a nil blob store or logger is
// rejected at Handle time.
func NewWorker(st *store.Store, blobs blob.BlobStore, interval time.Duration, log *slog.Logger) *Worker {
	if interval <= 0 {
		interval = time.Hour
	}
	if log == nil {
		log = slog.Default()
	}
	return &Worker{st: st, blobs: blobs, interval: interval, batch: defaultBatch, now: time.Now, log: log}
}

// SweepResult tallies one sweep for logging and tests.
type SweepResult struct {
	Candidates      int // due messages examined
	Purged          int // messages hard-deleted (row gone, `destroyed`, tombstoned)
	BlobsReclaimed  int // subset of Purged whose blob was unreferenced and removed
	ExpungeEnqueued int // due messages still on a server: EXPUNGE queued, not deleted
	Errors          int // per-message failures — logged, isolated, retried next sweep
	// The three skip tallies below count due messages for which phase 1 was NOT
	// attempted, BY CAUSE. Each keeps purge_after and waits rather than
	// enqueueing an EXPUNGE that would destroy the wrong copy or never be dialed
	// for. They are separate from ExpungeEnqueued so a growing wait pile cannot
	// read as progress — and separate from EACH OTHER because they mean
	// different things and only one of them is benign.
	//
	// ⚠ THEY WERE ONE FIELD NAMED ShadowedSkipped, AND THAT NAME WAS A CLAIM
	// ABOUT CAUSE THE NUMBER COULD NOT SUPPORT. On the production shadow run the
	// mode's own count is expected to be large, so a stalled trash move folded
	// into it would look like the mode working correctly — the "one total cannot
	// distinguish nothing-to-do from the-thing-I-measured-is-always-empty"
	// failure, in its most expensive form.

	// ShadowedSkipped: no account holding a live copy has writeback on
	// (ADR-028 §7). EXPECTED during a shadow run — Runner.DrainAccount can
	// structurally never dial for these.
	ShadowedSkipped int
	// UnlandedSkipped: an account with writeback ON still holds a live copy
	// outside its Trash, so the trash move never reached the server. NOT
	// expected. A count that persists across sweeps means retention has stalled
	// on a move that is suppressed, queued, dead-lettered or was never made —
	// and until it lands, expunging would destroy the copy where it actually is.
	UnlandedSkipped int
	// VanishedSkipped: no live copy anywhere — a race against the due scan.
	// Like the other two causes, purge_after is deferred a full retention
	// window (see the unconditional defer below), so phase 2 hard-deletes it
	// one window later, not on the next sweep. A non-zero count here is
	// information, not a problem.
	VanishedSkipped int
}

// Handle runs one purge job: it sweeps the user's due Trash, then reschedules the
// next sweep. It is the dispatcher entry point.
//
// The reschedule runs even when the sweep errored, so the timer never stops
// ticking on a transient failure; because it excludes THIS still-running job and
// no-ops when a successor already exists, retries never pile up duplicate timers.
// A systemic sweep error is returned so the dispatcher applies its backoff and
// retry (the reschedule having already guaranteed forward progress either way).
func (w *Worker) Handle(ctx context.Context, job store.Job) error {
	if job.UserID == nil {
		// A purge job is inherently per-user (it walks that user's RLS-scoped
		// Trash). A system-scoped one is a wiring bug; dead-letter it rather than
		// spinning, by returning a non-retryable-shaped error the dispatcher will
		// eventually exhaust.
		return errors.New("purge: job has no user id")
	}
	if w.blobs == nil {
		return errors.New("purge: no blob store configured; refusing to purge without a way to reclaim blob files")
	}
	userID := *job.UserID

	res, sweepErr := w.Sweep(ctx, userID)
	if sweepErr != nil {
		w.log.Error("purge sweep failed", "user_id", userID.String(), "err", sweepErr)
	}

	if rerr := w.reschedule(ctx, userID, job.ID); rerr != nil {
		w.log.Error("purge reschedule failed", "user_id", userID.String(), "err", rerr)
		if sweepErr == nil {
			return rerr
		}
	}
	_ = res
	return sweepErr
}

// Sweep processes one bounded batch of the user's due Trash. Each message is
// handled in its OWN transaction, so a per-message failure is isolated — it is
// logged and the message stays in Trash to be retried next sweep, and it never
// poisons the rest of the batch (the reaper's "isolation one level up" posture,
// applied per message rather than per mailbox).
func (w *Worker) Sweep(ctx context.Context, userID ulid.ULID) (SweepResult, error) {
	var res SweepResult
	now := w.now().UTC()

	var candidates []store.PurgeCandidate
	if err := w.st.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		candidates, e = tx.DueForPurge(ctx, now, w.batch)
		return e
	}); err != nil {
		return res, fmt.Errorf("purge: due scan: %w", err)
	}
	res.Candidates = len(candidates)

	for _, c := range candidates {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		if c.HasLiveProvenance {
			// Phase 1: copies still on the server — usually. But DueForPurge's
			// liveProv hint does not know about shadowing, and a shadowed account's
			// EXPUNGE can never land (Runner.DrainAccount never dials for it). Left
			// unguarded, this message would fail the same phase-1→phase-2 check on
			// every sweep forever, accumulating stuck Trash with nothing to show for
			// it (ADR-028 §7) — so skip the enqueue entirely when no writeback-
			// enabled account holds a copy, and DEFER purge_after by a full window.
			//
			// ⚠ THE DEFER IS THE LOAD-BEARING HALF, not bookkeeping. Skipping alone
			// leaves the message due forever, so the instant writeback is enabled
			// its EXPUNGE is enqueued 'pending' and applied to every live copy —
			// which is still INBOX, because the trash MOVE is sitting suppressed in
			// the ledger and never reached the server. Lifting the mode would then
			// destroy real inbox mail, in batches, with no --apply and no review.
			// See store.DeferPurgeDeadline for the full argument.
			skip, err := w.enqueueExpunge(ctx, userID, c.ID)
			if err != nil {
				w.log.Warn("purge: could not enqueue EXPUNGE; message stays in Trash, retried next sweep",
					"user_id", userID.String(), "message_id", c.ID.String(), "err", err)
				res.Errors++
				continue
			}
			switch skip {
			case store.ExpungeSkipNone:
				res.ExpungeEnqueued++
			case store.ExpungeSkipShadowed:
				res.ShadowedSkipped++
			case store.ExpungeSkipUnlanded:
				// Per message, at Warn: unlike the shadowed case this is not
				// supposed to happen, and the id is what makes it investigable.
				w.log.Warn("purge: a live copy sits outside Trash on a writeback-enabled "+
					"account; EXPUNGE withheld and retention deferred until the trash move lands",
					"user_id", userID.String(), "message_id", c.ID.String())
				res.UnlandedSkipped++
			case store.ExpungeSkipVanished:
				res.VanishedSkipped++
			}
			continue
		}
		// Phase 2: no copy anywhere we sync. Hard-delete now.
		purged, reclaimed, err := w.hardDelete(ctx, userID, c.ID, now)
		if err != nil {
			w.log.Warn("purge: hard delete failed; message stays in Trash, retried next sweep",
				"user_id", userID.String(), "message_id", c.ID.String(), "err", err)
			res.Errors++
			continue
		}
		if purged {
			res.Purged++
		}
		if reclaimed {
			res.BlobsReclaimed++
		}
	}

	w.log.Info("purge sweep",
		"user_id", userID.String(), "candidates", res.Candidates, "purged", res.Purged,
		"blobs_reclaimed", res.BlobsReclaimed, "expunge_enqueued", res.ExpungeEnqueued,
		"shadowed_skipped", res.ShadowedSkipped, "unlanded_skipped", res.UnlandedSkipped,
		"vanished_skipped", res.VanishedSkipped, "errors", res.Errors)
	return res, nil
}

// enqueueExpunge queues the per-account EXPUNGE of a due message that still has
// live provenance (phase 1), for those accounts where destroying the copy is
// actually the right thing — meaning every live copy the account holds is in
// that account's Trash. When no account qualifies it enqueues nothing, defers
// the deadline, and returns the reason it skipped (ADR-028 §7) — the caller
// tallies each cause separately, because only one of the three is benign.
//
// A returned ExpungeSkipNone means the enqueue happened.
func (w *Worker) enqueueExpunge(ctx context.Context, userID, messageID ulid.ULID) (skip store.ExpungeSkip, err error) {
	err = w.st.UserTx(ctx, userID, func(tx *store.UserTx) error {
		// ONE PREDICATE, ONE EVALUATION, used both to decide and to enqueue.
		//
		// ⚠ THE PREVIOUS SHAPE IS THE BUG THIS SIGNATURE EXISTS TO PREVENT. The
		// worker used to apply a safety check here and then call an enqueue that
		// resolved its OWN account set with a different predicate. The two could
		// disagree, and did: the worker correctly skipped an account whose trash
		// move had not landed, and the enqueue then wrote an EXPUNGE for it
		// anyway, because some OTHER account had made the message a candidate.
		// Three rounds of review each patched one of those predicates in turn.
		accounts, why, aerr := tx.ExpungeableAccountIDs(ctx, messageID)
		if aerr != nil {
			return aerr
		}
		if len(accounts) == 0 {
			skip = why
			// Push the deadline out so the message leaves the due set. This is a
			// BATCHING measure, not the safety property: DueForPurge is
			// `ORDER BY purge_after LIMIT n`, so a backlog of un-expungeable
			// messages would otherwise fill every batch and starve a live account
			// belonging to the same user. Safety is ExpungeableAccountIDs.
			//
			// Applied to ALL THREE causes, deliberately. The vanished case would be
			// fine without it (phase 2 takes the message next sweep), but making the
			// defer conditional would put a branch here that no test can reach — the
			// case only arises from a race between two transactions — and an
			// untestable branch guarding a destructive path is worth less than the
			// retention window it would save.
			_, derr := tx.DeferPurgeDeadline(ctx, messageID)
			return derr
		}
		skip = store.ExpungeSkipNone
		_, e := tx.EnqueuePurgeExpungeIntents(ctx, messageID, accounts)
		return e
	})
	return skip, err
}

// hardDelete performs one message's phase-2 hard delete in a single transaction,
// deleting the reclaimed blob FILE inside that transaction, before it commits.
//
// That ordering is the blob-reclaim race proof (see store.HardDeleteMessage): the
// message-row delete is invisible to a concurrent ingest until this transaction
// commits, so the "not found by content hash" window a byte-identical re-ingest
// needs to re-Put these bytes opens only AFTER the file is already gone. A
// file-delete failure aborts the transaction — nothing is destroyed for this
// message and it is retried next sweep — so the failure mode is "retry", never a
// canonical row that outlives its bytes.
func (w *Worker) hardDelete(ctx context.Context, userID, messageID ulid.ULID, now time.Time) (purged, reclaimed bool, err error) {
	txErr := w.st.UserTx(ctx, userID, func(tx *store.UserTx) error {
		hash, reclaim, ok, e := tx.HardDeleteMessage(ctx, messageID, now)
		if e != nil {
			return e
		}
		if !ok {
			// Not eligible under the row lock (restored, window reopened by a clock
			// skew, or it gained live provenance). Commit a no-op; next sweep
			// reclassifies it.
			return nil
		}
		purged = true
		if !reclaim {
			return nil // bytes still referenced (a shared attachment) — keep the file
		}
		h, e := contentHash(hash)
		if e != nil {
			return e
		}
		if e := w.blobs.Delete(ctx, userID, h); e != nil {
			return fmt.Errorf("reclaim blob file: %w", e)
		}
		reclaimed = true
		return nil
	})
	if txErr != nil {
		return false, false, txErr
	}
	return purged, reclaimed, nil
}

// reschedule enqueues the next sweep at now+interval, excluding this job so the
// reschedule-from-within-the-handler sees a clear slate.
//
// ⚠ A DECLINED RESCHEDULE MEANS THE CHAIN JUST STOPPED, so it is logged rather
// than discarded. The guard inside EnsureRecurringUserJob refuses when it finds
// another outstanding job of this kind; here, with this job excluded, that
// should be impossible — a job stranded in 'running' by a dead process was
// enough to satisfy it, and the recurring chain died silently for the life of
// the deployment because nothing on this path ever asked (#351 B3, #357).
// Migration 00041's lock ageing removes the known cause; this line is what would
// have found it.
func (w *Worker) reschedule(ctx context.Context, userID, currentJobID ulid.ULID) error {
	return w.st.UserTx(ctx, userID, func(tx *store.UserTx) error {
		scheduled, err := tx.EnsureRecurringUserJob(ctx, JobKind, w.now().Add(w.interval), currentJobID)
		if err == nil && !scheduled {
			w.log.Warn("Trash purge did not reschedule: another job of this kind is already outstanding; "+
				"if none is actually running, the sweep chain has stopped and Trash will not be reclaimed",
				"user_id", userID.String(), "kind", JobKind)
		}
		return err
	})
}

// EnsureScheduled seeds the first purge sweep for a user at boot when none is
// outstanding. It is idempotent across restarts (the store's NOT EXISTS guard),
// and passing the zero job id excludes nothing (there is no running job at boot).
func EnsureScheduled(ctx context.Context, st *store.Store, userID ulid.ULID, fireAt time.Time) (bool, error) {
	var scheduled bool
	err := st.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		scheduled, e = tx.EnsureRecurringUserJob(ctx, JobKind, fireAt, ulid.ULID{})
		return e
	})
	return scheduled, err
}

// contentHash narrows a 32-byte hash slice to the blob store's array type.
func contentHash(b []byte) (blob.ContentHash, error) {
	var h blob.ContentHash
	if len(b) != len(h) {
		return h, fmt.Errorf("purge: content hash is %d bytes, want %d", len(b), len(h))
	}
	copy(h[:], b)
	return h, nil
}
