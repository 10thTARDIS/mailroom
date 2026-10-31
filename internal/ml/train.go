// SPDX-License-Identifier: AGPL-3.0-only

package ml

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// This file is the LEARNING loop for the T1 priority scorer (spec §8,
// mailroom#33 PR2, rebuilt for #842). The daily pass trains a candidate on the
// user's recent engagement and ACTIVATES IT ONLY IF IT PASSES THE GATE against
// the cold-start heuristic (priority_gate.go, ADR-043). When it does not, the
// cold start serves, and if a trained model is active it is REVERTED to the
// cold start: the model in service is the product of the same procedure on
// older evidence, and has no better claim than the candidate it just lost with.
//
// What changed in #842, and why (the audit is in ADR-043):
//
//   - the corpus is one example per message, labelled from engagement, with
//     mark_read_no_open no longer a label and features rebuilt as of arrival
//     (priority_train.go);
//   - the fit is class-balanced;
//   - the judgement is sender-grouped cross-validation against the cold start
//     at the threshold the user sees, not a time-split AUC against whatever was
//     in service. A holdout with no positives scored AUC 0.5 by convention, and
//     0.5 >= 0.5 promoted v1 with a handful of positives in the corpus.
//
// Still true from the original design: COLD RESTART (each pass fits a fresh
// model, so a pass is reproducible from its evidence) and a hard floor on data
// (MinTrainSamples, both classes) before anything is fitted at all.
//
// KNOWN LIMITATION, unchanged: the bulk signals come from the row (migration
// 00016); mail ingested before it reads as not-bulk until `mailroom-admin
// rescore` backfills them.

const (
	// trainWindow bounds how far back a pass replays feedback. Long enough to
	// accumulate signal for a low-volume personal mailbox, short enough that stale
	// preferences age out.
	trainWindow = 90 * 24 * time.Hour
	// MinTrainSamples is the floor below which the pass does not even fit: too few
	// labelled messages (of either class) to cross-validate. Both classes must
	// ALSO be present. The gate's own evidence floor (MinPositives) is stricter
	// and is what actually decides promotion.
	MinTrainSamples = 50
	// trainKeepN is how many historical snapshots Prune retains per user after a
	// promotion (plus the active one).
	trainKeepN = DefaultKeepN
)

// Gate decision actions, as written to the audit log.
const (
	gateActionPromote = "priority.gate.promote"
	gateActionRefuse  = "priority.gate.refuse"
	gateActionRevert  = "priority.gate.revert"
)

// TrainResult is the structured outcome of one TrainAndMaybePromote pass, for
// logging and tests. Trained is false when the data floor skipped the fit;
// Promoted is true only when the gate passed and the candidate was activated;
// Reverted is true when a trained model was in service and the pass returned
// the user to the cold start. Version is the resulting ACTIVE version (0 = cold
// start).
type TrainResult struct {
	Samples   int // labelled inbound messages in the window
	Positives int
	Negatives int
	Trained   bool
	Promoted  bool
	Reverted  bool
	// CandidateAUC / IncumbentAUC are the gate's pooled out-of-fold AUCs for
	// the candidate and the cold start (kept under the old names for the log
	// line). Reported, never decided on.
	CandidateAUC float64
	IncumbentAUC float64
	Gate         *PriorityGate
	Version      int
	Reason       string
}

// Trainer runs the per-user training pass over the model Registry. Construct with
// NewTrainer; the durable-job worker (TrainWorker) drives it on a schedule, and
// tests call TrainAndMaybePromote directly.
type Trainer struct {
	st  *store.Store
	reg *Registry
	now func() time.Time
}

// NewTrainer builds a Trainer over the store and model registry.
func NewTrainer(st *store.Store, reg *Registry) *Trainer {
	return &Trainer{st: st, reg: reg, now: time.Now}
}

// TrainAndMaybePromote runs one pass: build the corpus, judge a candidate at the
// gate, and either activate it or keep (or restore) the cold start. Every
// decision is written to the audit log with its numbers. "Not enough data" and
// "not good enough" are ordinary outcomes in the result, not errors; an error
// means a real store/blob failure.
func (t *Trainer) TrainAndMaybePromote(ctx context.Context, userID ulid.ULID) (TrainResult, error) {
	var res TrainResult
	until := t.now().UTC()
	corpus, err := t.loadPriorityCorpus(ctx, userID, corpusOptions{since: until.Add(-trainWindow), until: until})
	if err != nil {
		return res, fmt.Errorf("ml: train: %w", err)
	}
	for _, e := range corpus.Examples {
		if e.Positive {
			res.Positives++
		} else {
			res.Negatives++
		}
	}
	res.Samples = len(corpus.Examples)

	activeVersion, err := t.reg.ActiveVersion(ctx, userID, KindPriority)
	if err != nil {
		return res, fmt.Errorf("ml: train: read active version: %w", err)
	}

	if res.Samples < MinTrainSamples || res.Positives == 0 || res.Negatives == 0 {
		res.Reason = fmt.Sprintf("insufficient data (samples=%d, positives=%d, negatives=%d; need >=%d with both classes)",
			res.Samples, res.Positives, res.Negatives, MinTrainSamples)
		return t.refuse(ctx, userID, res, activeVersion, map[string]any{
			"samples": res.Samples, "positives": res.Positives, "negatives": res.Negatives,
			"minTrainSamples": MinTrainSamples, "pass": false, "reason": res.Reason,
		})
	}

	gate := EvaluatePriorityGate(corpus, userID)
	res.Trained = true
	res.Gate = &gate
	res.CandidateAUC, res.IncumbentAUC = gate.CandAUC, gate.ColdAUC
	if !gate.Pass {
		res.Reason = "gate refused: " + gate.Reason
		return t.refuse(ctx, userID, res, activeVersion, gate.AuditDetail(nil))
	}

	candidate, err := fitPriority(corpus.Examples, userID)
	if err != nil {
		return res, fmt.Errorf("ml: train: fit: %w", err)
	}
	version, err := t.reg.Snapshot(ctx, userID, candidate)
	if err != nil {
		return res, fmt.Errorf("ml: train: snapshot candidate: %w", err)
	}
	if err := t.reg.Activate(ctx, userID, KindPriority, version); err != nil {
		return res, fmt.Errorf("ml: train: activate candidate v%d: %w", version, err)
	}
	res.Promoted = true
	res.Version = version
	res.Reason = fmt.Sprintf("promoted v%d: %s", version, gate.Reason)
	t.audit(ctx, userID, gateActionPromote, version, gate.AuditDetail(map[string]any{"previousVersion": activeVersion}))

	// Trim old snapshots (best-effort: a failed prune leaves extra history, never a
	// wrong active model, so it does not fail the pass).
	if _, err := t.reg.Prune(ctx, userID, KindPriority, trainKeepN); err != nil {
		res.Reason += fmt.Sprintf("; prune failed: %v", err)
	}
	return res, nil
}

// refuse records a non-promotion. With a trained model in service it REVERTS
// to the cold start: the incumbent is the same procedure's output on older
// evidence, and the procedure did not earn a place today, so neither did it.
func (t *Trainer) refuse(ctx context.Context, userID ulid.ULID, res TrainResult, activeVersion int, detail map[string]any) (TrainResult, error) {
	if activeVersion == 0 {
		res.Version = 0
		t.audit(ctx, userID, gateActionRefuse, 0, detail)
		return res, nil
	}
	if err := t.reg.RevertToColdStart(ctx, userID, KindPriority); err != nil {
		return res, fmt.Errorf("ml: train: revert v%d to the cold start: %w", activeVersion, err)
	}
	res.Reverted = true
	res.Version = 0
	res.Reason = fmt.Sprintf("reverted v%d to the cold start: %s", activeVersion, res.Reason)
	detail["revertedVersion"] = activeVersion
	t.audit(ctx, userID, gateActionRevert, activeVersion, detail)
	return res, nil
}

// audit writes the gate decision best-effort: a failed audit row must not undo
// a revert that already happened, and the slog line in TrainWorker.Handle
// carries the same numbers.
func (t *Trainer) audit(ctx context.Context, userID ulid.ULID, action string, version int, detail map[string]any) {
	_ = t.st.UserTx(ctx, userID, func(tx *store.UserTx) error {
		return tx.RecordPriorityGateDecision(ctx, action, version, detail)
	})
}

// TrainJobKind is the durable-job kind the training worker registers for.
const TrainJobKind = "ml_train"

// defaultTrainInterval is the cadence a fresh training pass reschedules itself at.
// Daily is ample for a personal mailbox: feedback accrues slowly and a promotion
// only ever helps if the window moved meaningfully, so a tighter loop would burn
// work for no gain. The Trash purge uses the same seed-then-self-reschedule shape.
const defaultTrainInterval = 24 * time.Hour

// TrainWorker runs one user's training pass per fired job and reschedules the
// next. It mirrors purge.Worker: a per-user recurring timer, seeded once in the
// fan-out and self-rescheduling from inside the handler. Construct with
// NewTrainWorker and register Handle with the dispatcher.
type TrainWorker struct {
	trainer  *Trainer
	interval time.Duration
	now      func() time.Time
	log      *slog.Logger
}

// NewTrainWorker builds a training worker over the store + registry. A
// non-positive interval falls back to daily; a nil logger to slog.Default().
func NewTrainWorker(st *store.Store, reg *Registry, interval time.Duration, log *slog.Logger) *TrainWorker {
	if interval <= 0 {
		interval = defaultTrainInterval
	}
	if log == nil {
		log = slog.Default()
	}
	return &TrainWorker{trainer: NewTrainer(st, reg), interval: interval, now: time.Now, log: log}
}

// Handle runs one training job and reschedules the next. Fail-soft: a training
// error is logged and returned (so the dispatcher applies backoff), but the
// reschedule runs either way so the loop never stops on a transient failure, and
// it excludes THIS running job + no-ops when a successor exists, so retries never
// pile up duplicate timers (the purge worker's contract).
func (w *TrainWorker) Handle(ctx context.Context, job store.Job) error {
	if job.UserID == nil {
		// Training is inherently per-user (it walks that user's RLS-scoped
		// feedback). A system-scoped job is a wiring bug; dead-letter it.
		return fmt.Errorf("ml: train job has no user id")
	}
	userID := *job.UserID

	res, trainErr := w.trainer.TrainAndMaybePromote(ctx, userID)
	if trainErr != nil {
		w.log.Error("ml training pass failed", "user_id", userID.String(), "err", trainErr)
	} else {
		w.log.Info("ml training pass",
			"user_id", userID.String(), "samples", res.Samples,
			"positives", res.Positives, "negatives", res.Negatives, "trained", res.Trained,
			"promoted", res.Promoted, "reverted", res.Reverted, "candidate_auc", res.CandidateAUC,
			"cold_start_auc", res.IncumbentAUC, "version", res.Version, "reason", res.Reason)
	}

	// The per-label classifiers ride the SAME pass (#169): they learn from the
	// same slowly-accruing evidence, and a second timer is a second thing to
	// reason about when one of them stops firing.
	//
	// Their failure is deliberately NOT returned. The priority model is the one
	// this job exists for, and a label model that cannot train must not cause the
	// dispatcher to back off and retry the priority pass with it.
	//
	// TRAINS AND EVALUATES, BUT DOES NOT PROMOTE (dryRun = true). A promoted
	// model is one whose verdicts reach real mail. What decides promotion is the
	// Wilson gate (promotion_gate.go): the LOWER end of a 95% Wilson interval on
	// pooled out-of-fold precision at the apply threshold must reach
	// labelPromotionPrecision (0.80), over a sender-grouped 5-fold
	// cross-validation with at least two positive-bearing senders per fold. It
	// replaced the old AUC floor (labelMinAUC 0.65 on a single tail split), which
	// ranked well while precision at the threshold was unusable, and whose split
	// let sender memorisation pass for skill (#169, #220). On a real corpus the
	// gate refuses every label, and that is its intended answer.
	//
	// This pass stays a dry run regardless, until promotion is switched on
	// deliberately.
	//
	// The daily numbers are still worth having: `mailroom-admin train-labels`
	// and this log line are how the corpus is watched for the point at which
	// a label first passes the gate (#169's data floor). Flip this to false then,
	// with that grouped-CV measurement behind it — not before.
	if labelResults, err := w.trainer.TrainLabelModels(ctx, userID, true); err != nil {
		w.log.Error("label model training failed", "user_id", userID.String(), "err", err)
	} else {
		for _, lr := range labelResults {
			w.log.Info("label model pass",
				"user_id", userID.String(), "label_id", lr.LabelID.String(),
				"positives", lr.Positives, "negatives", lr.Negatives,
				"trained", lr.Trained, "promoted", lr.Promoted,
				"auc", lr.AUC, "version", lr.Version, "reason", lr.Reason)
		}
	}

	if rerr := w.reschedule(ctx, userID, job.ID); rerr != nil {
		w.log.Error("ml training reschedule failed", "user_id", userID.String(), "err", rerr)
		if trainErr == nil {
			return rerr
		}
	}
	return trainErr
}

// reschedule enqueues the next pass at now+interval, excluding this still-running
// job so the reschedule-from-within-the-handler sees a clear slate.
//
// ⚠ A DECLINED RESCHEDULE MEANS THE CHAIN JUST STOPPED — see the twin comment in
// internal/purge for why that was invisible for the life of the project (#351
// B3, #357). With this job excluded there should be nothing left to find.
func (w *TrainWorker) reschedule(ctx context.Context, userID, currentJobID ulid.ULID) error {
	return w.trainer.st.UserTx(ctx, userID, func(tx *store.UserTx) error {
		scheduled, err := tx.EnsureRecurringUserJob(ctx, TrainJobKind, w.now().Add(w.interval), currentJobID)
		if err == nil && !scheduled {
			w.log.Warn("priority-model training did not reschedule: another job of this kind is already "+
				"outstanding; if none is actually running, the training chain has stopped",
				"user_id", userID.String(), "kind", TrainJobKind)
		}
		return err
	})
}

// EnsureTrainingScheduled seeds the first training pass for a user at boot when
// none is outstanding. Idempotent across restarts (the store's NOT EXISTS guard);
// the zero job id excludes nothing (there is no running job at boot).
func EnsureTrainingScheduled(ctx context.Context, st *store.Store, userID ulid.ULID, fireAt time.Time) (bool, error) {
	var scheduled bool
	err := st.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		scheduled, e = tx.EnsureRecurringUserJob(ctx, TrainJobKind, fireAt, ulid.ULID{})
		return e
	})
	return scheduled, err
}
