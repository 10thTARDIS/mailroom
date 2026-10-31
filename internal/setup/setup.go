// SPDX-License-Identifier: AGPL-3.0-only

// Package setup drains a day-one setup run (mailroom#446, Direction 7 "Desk and
// Drawers", #372): the bulk import that turns a folder-sorted mailbox into a
// desk — label the mail older than the window, ARCHIVE IT IN MAILROOM, and
// optionally bring one folder's mail to the desk with a real server-side move.
//
// NOTHING MOVES ON THE SERVER except a folder the operator explicitly marked
// `bringToDesk`. That is the promise the whole feature rests on: the bytes stay
// where the provider put them, no writeback intent is enqueued for a
// labelArchive folder, and every other client sees no change at all. The
// enforcement is in internal/store/setup.go's SetupApplyChunk, not here.
//
// ⚠ THIS WORKER IS ONE-SHOT, NOT RECURRING — the one structural difference from
// internal/drawer, which this package otherwise copies. There is no
// EnsureScheduled and nothing seeds it at boot: a run is enqueued by
// Setup/apply (and its reversal by Setup/undo), it drains, and the job is done.
// A recurring day-one job would be a wizard that reruns itself.
//
// The chunk model is internal/drawer's: a bounded batch per transaction, each
// chunk well under the 15-minute job lease, looping until the run reports Done.
// The batch is EACH CHUNK in its own UserTx rather than each message, because a
// chunk here is atomic by design — the journal row and the writes it records
// have to commit together or the undo journal would describe writes that did
// not happen.
package setup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// JobKind is the durable-job kind this worker registers for.
const JobKind = store.SetupJobKind

// defaultChunk bounds how many messages one transaction touches. 200 is
// internal/drawer's own batch size and for the same reason: a first-run backlog
// of several thousand messages drains in
// a couple of dozen short transactions rather than one long one that would hold
// row locks and block the sequential dispatcher.
const defaultChunk = 200

// Direction is which way a run is being drained. Derived from the run's STATUS
// rather than from the job payload, because the payload carries ids only
// (store.jobPayloadKeys) — and because the status is the authority: Setup/undo
// flips it and enqueues the same kind, so a job that fired before the flip and a
// job that fired after it must do different things.
type Direction string

const (
	// DirectionApply drains the run forwards.
	DirectionApply Direction = "apply"
	// DirectionUndo replays the journal's inverse.
	DirectionUndo Direction = "undo"
	// DirectionNone is a run that is already finished in either direction:
	// nothing to do, and NOT an error — a duplicate job delivery must be a
	// no-op, not a dead letter.
	DirectionNone Direction = "none"
)

// DirectionFor maps a run status to the work this worker owes it. PURE, so the
// one decision that makes a re-delivered job harmless is unit-testable without
// a database.
//
// 'failed' drains FORWARDS: MarkSetupRunFailed leaves the journal untouched, so
// the dispatcher's own retry has to be able to carry on from the cursor rather
// than treat a transient error as terminal.
func DirectionFor(status string) Direction {
	switch status {
	case store.SetupStatusQueued, store.SetupStatusRunning, store.SetupStatusFailed:
		return DirectionApply
	case store.SetupStatusUndoing:
		return DirectionUndo
	default: // done, undone
		return DirectionNone
	}
}

// Worker drains one run per fired job. Construct with NewWorker and register
// Handle with the dispatcher.
type Worker struct {
	st    *store.Store
	chunk int
	log   *slog.Logger
}

// NewWorker builds a day-one setup worker. A nil logger falls back to
// slog.Default().
func NewWorker(st *store.Store, log *slog.Logger) *Worker {
	if log == nil {
		log = slog.Default()
	}
	return &Worker{st: st, chunk: defaultChunk, log: log}
}

// WithChunk overrides the chunk size, for tests that want several chunks over a
// small fixture.
func (w *Worker) WithChunk(n int) *Worker {
	if n > 0 {
		w.chunk = n
	}
	return w
}

// Result tallies one drained run, for logging and tests.
type Result struct {
	Direction Direction
	Chunks    int
	Labels    int
	Archived  int
	Moves     int
	Processed int
}

// setupPayload is the job payload: THE RUN ID AND NOTHING ELSE
// (store.jobPayloadKeys). The run, its plan and every message it touches are
// re-read under the target user's identity, which is the only context RLS can
// check — the dispatcher itself has no identity and sees whole job rows across
// users through a SECURITY DEFINER.
type setupPayload struct {
	RunID string `json:"run_id"`
}

// Handle drains one run to completion (or to context cancellation). It is the
// dispatcher entry point.
//
// ⚠ ON ERROR THE RUN IS MARKED FAILED **AND** THE ERROR IS RETURNED. Both,
// deliberately: the mark is what stops the client's progress bar spinning
// forever with no explanation, and the returned error is what lets the
// dispatcher's retry/dead-letter machinery do its job. A retry then RESUMES
// WHERE THIS LEFT OFF, because the chunk query skips messages that already have
// a journal row for this run — the journal is the cursor, and it lives in the
// same transaction as the writes it records.
func (w *Worker) Handle(ctx context.Context, job store.Job) error {
	if job.UserID == nil {
		// A setup run is inherently per-user (it walks that user's RLS-scoped
		// folders and mail). A system-scoped one is a wiring bug; dead-letter it
		// rather than spinning.
		return errors.New("setup: job has no user id")
	}
	userID := *job.UserID

	var payload setupPayload
	if len(job.Payload) > 0 {
		if err := json.Unmarshal(job.Payload, &payload); err != nil {
			return fmt.Errorf("setup: job payload: %w", err)
		}
	}
	runID, err := ulid.Parse(payload.RunID)
	if err != nil {
		return fmt.Errorf("setup: job payload run_id %q: %w", payload.RunID, err)
	}

	res, drainErr := w.Drain(ctx, userID, runID)
	if drainErr != nil {
		if markErr := w.st.UserTx(ctx, userID, func(tx *store.UserTx) error {
			return tx.MarkSetupRunFailed(ctx, runID, drainErr.Error())
		}); markErr != nil {
			w.log.Error("setup: could not record the run as failed; the client will keep waiting on it",
				"user_id", userID.String(), "run_id", runID.String(), "err", markErr)
		}
		w.log.Error("setup run failed", "user_id", userID.String(), "run_id", runID.String(),
			"direction", string(res.Direction), "chunks", res.Chunks, "err", drainErr)
		return drainErr
	}
	w.log.Info("setup run drained: nothing moved on the server except an explicit bringToDesk folder",
		"user_id", userID.String(), "run_id", runID.String(), "direction", string(res.Direction),
		"chunks", res.Chunks, "labels", res.Labels, "archived", res.Archived, "moves", res.Moves)
	return nil
}

// Drain loops chunks — each in its OWN UserTx — until the run reports Done or
// the context is cancelled. The direction is read ONCE, before the loop, from
// the run's status; a run that flips to 'undoing' mid-apply is picked up by the
// second job Setup/undo enqueued, not by this one changing its mind halfway.
func (w *Worker) Drain(ctx context.Context, userID, runID ulid.ULID) (Result, error) {
	var res Result

	var status string
	if err := w.st.UserTx(ctx, userID, func(tx *store.UserTx) error {
		run, found, e := tx.GetSetupRun(ctx, runID)
		if e != nil {
			return e
		}
		if !found {
			// RLS makes another user's run indistinguishable from a missing one.
			// Either way there is nothing this job can legitimately do.
			return fmt.Errorf("%w: %s", store.ErrSetupRunNotFound, runID)
		}
		status = run.Status
		return nil
	}); err != nil {
		return res, err
	}
	res.Direction = DirectionFor(status)
	if res.Direction == DirectionNone {
		w.log.Info("setup run needs no work; ignoring a duplicate job delivery",
			"user_id", userID.String(), "run_id", runID.String(), "status", status)
		return res, nil
	}

	for {
		if err := ctx.Err(); err != nil {
			// A cancelled drain leaves the run mid-flight ON PURPOSE: every
			// committed chunk is journaled, so the next job (a retry, or the
			// operator's own re-apply) resumes from the cursor. Nothing is half
			// written — a chunk is one transaction.
			return res, err
		}
		var p store.SetupProgress
		if err := w.st.UserTx(ctx, userID, func(tx *store.UserTx) error {
			var e error
			if res.Direction == DirectionUndo {
				p, e = tx.SetupUndoChunk(ctx, runID, w.chunk)
			} else {
				p, e = tx.SetupApplyChunk(ctx, runID, w.chunk)
			}
			return e
		}); err != nil {
			return res, err
		}
		res.Chunks++
		res.Labels += p.Labels
		res.Archived += p.Archived
		res.Moves += p.Moves
		res.Processed += p.Processed
		if p.Done {
			return res, nil
		}
		if p.Processed == 0 {
			// A chunk that reported neither Done nor progress would loop forever.
			// It cannot happen — the chunk query is bounded by the journal cursor
			// and every examined message gets a row — so treat it as the bug it
			// would be and say so, rather than spinning under a job lease.
			return res, fmt.Errorf("setup: run %s made no progress and did not report done "+
				"after %d chunk(s); refusing to loop", runID, res.Chunks)
		}
	}
}
