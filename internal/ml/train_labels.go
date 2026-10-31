// SPDX-License-Identifier: AGPL-3.0-only

package ml

import (
	"context"
	"fmt"
	"sort"
	"strconv"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// The training pass for the per-label classifiers (spec §8, #169).
//
// It rides the SAME daily job as the priority trainer rather than adding a
// second schedule: both learn from the same slowly-accruing evidence, and a
// second timer is a second thing to reason about when one of them stops firing.
//
// The shape mirrors TrainAndMaybePromote deliberately, because the reasoning
// that made that pass safe applies unchanged here:
//
//   - COLD-START GUARD: refuse to train on too little data or on one class.
//   - HOLDOUT, not a self-report: score a slice the model did not fit on.
//   - COLD RESTART: a fresh model each pass, so a run is reproducible from the
//     evidence alone and cannot compound toward the majority class.
//   - PROMOTE ONLY ON MERIT: a candidate replaces the incumbent only if it ranks
//     the holdout better, so a noisy window cannot degrade a working model.
//
// The one real difference is what counts as evidence. The priority model learns
// from feedback EVENTS (the user disagreeing with a score); a label model learns
// from the labels themselves, which are state rather than events. So there is no
// time window: every hand-applied label still stands as a statement, however old.

// labelHoldoutFraction is the share of a label's evidence reserved for
// evaluation.
const labelHoldoutFraction = 0.25

// The AUC floor that used to decide promotion (labelMinAUC = 0.65) is GONE, not
// moved: see promotion_gate.go, which replaced both the quantity and the
// estimator. #169's "do not move the floor" still stands — the floor was never
// the binding constraint, and this is not a retuning of it.

// labelNegativeSample bounds how much unlabelled mail is drawn as negatives.
const labelNegativeSample = 400

// labelTrainEpochs is how many SGD passes over the fitted set.
const labelTrainEpochs = 24

// LabelTrainResult reports one label's pass.
type LabelTrainResult struct {
	LabelID   ulid.ULID
	Positives int
	Negatives int
	Trained   bool
	Promoted  bool
	// AUC is measured over a holdout of senders the model never trained on. It
	// is REPORTED, not decided on: see Gate, and promotion_gate.go for why a
	// ranking number cannot answer a threshold question.
	AUC float64
	// Gate is the promotion verdict and the evidence behind it. It is the only
	// thing that decides Promoted.
	Gate LabelGate
	// NaiveAUC is the same model over a class-only split, where its own training
	// senders appear on both sides. Reported ALONGSIDE, never instead: the gap
	// between the two is how much of the model's apparent skill is sender
	// memorisation, and it is the number that decided this classifier may not
	// file mail on its own (#169).
	NaiveAUC float64
	// Groups is how many distinct senders the label's positives span. One is not
	// a model — it is a rule with extra steps, and it cannot be evaluated here at
	// all.
	Groups  int
	Version int
	Reason  string
}

// TrainLabelModels trains (and conditionally promotes) a classifier for every
// label with enough hand-applied evidence. It is fail-soft per label: one
// label's failure never stops the others, because a single malformed or
// pathological label should not cost the user every other category.
// dryRun trains and evaluates but promotes nothing — the shape can be inspected
// before any model starts labelling mail.
func (t *Trainer) TrainLabelModels(ctx context.Context, userID ulid.ULID, dryRun bool) ([]LabelTrainResult, error) {
	var labels []ulid.ULID
	if err := t.st.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		labels, e = tx.LabelsWithEnoughEvidence(ctx, MinLabelTrainSamples)
		return e
	}); err != nil {
		return nil, fmt.Errorf("ml: label training: list labels: %w", err)
	}
	// Deterministic order so a run's logs are comparable with the last one's.
	sort.Slice(labels, func(i, j int) bool { return labels[i].String() < labels[j].String() })

	out := make([]LabelTrainResult, 0, len(labels))
	for _, labelID := range labels {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		out = append(out, t.trainOneLabel(ctx, userID, labelID, dryRun))
	}
	return out, nil
}

// loadLabelExamples builds the example set for one label: its hand-applied
// positives plus a bounded sample of other mail as negatives, each carrying the
// sender domain the holdout groups on.
func (t *Trainer) loadLabelExamples(ctx context.Context, userID, labelID ulid.ULID) (examples []LabelExample, positives, negatives int, err error) {
	var rows []store.LabelTrainingRow
	if err := t.st.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		rows, e = tx.LabelTrainingRows(ctx, labelID, labelNegativeSample)
		return e
	}); err != nil {
		return nil, 0, 0, err
	}

	extractor := NewStoreExtractor(t.st, userID)
	for _, r := range rows {
		if err := ctx.Err(); err != nil {
			return nil, 0, 0, err
		}
		fv, err := extractor.Extract(ctx, r.MessageID)
		if err != nil {
			// A message whose features cannot be rebuilt (purged) is skipped, the
			// same way the priority trainer and the eval harness skip it.
			continue
		}
		examples = append(examples, LabelExample{Features: fv, Positive: r.Positive, Group: r.SenderDomain})
		if r.Positive {
			positives++
		} else {
			negatives++
		}
	}
	return examples, positives, negatives, nil
}

func (t *Trainer) trainOneLabel(ctx context.Context, userID, labelID ulid.ULID, dryRun bool) LabelTrainResult {
	res := LabelTrainResult{LabelID: labelID}

	examples, positives, negatives, err := t.loadLabelExamples(ctx, userID, labelID)
	if err != nil {
		res.Reason = "load evidence: " + err.Error()
		return res
	}
	res.Positives, res.Negatives = positives, negatives

	if res.Positives < MinLabelTrainSamples || res.Negatives == 0 {
		res.Reason = fmt.Sprintf("insufficient evidence (positives=%d, negatives=%d; need >=%d positives and both classes)",
			res.Positives, res.Negatives, MinLabelTrainSamples)
		return res
	}

	// Senders, because both the gate's floor and every diagnostic below are
	// about mail from a sender the model has NOT seen — a sender it already
	// knows is one an induced `alias =`/`fromDomain =` rule handles for free and
	// far more legibly.
	groups := map[string]bool{}
	exKeys := groupKeys(examples)
	for i, e := range examples {
		if e.Positive {
			groups[exKeys[i]] = true
		}
	}
	res.Groups = len(groups)

	// THE GATE COMES FIRST, before any diagnostic can refuse on its own behalf.
	//
	// It used to come last, behind the single grouped split — and that split
	// returns "cannot evaluate" for a corpus whose positives and negatives come
	// from DISJOINT senders, because it only ever holds out positive-bearing
	// groups, so the holdout is single-class and unscoreable. #220 fixed exactly
	// that in the CV (every group is assigned to a fold) and did not fix it here.
	// With the gate behind it, a cleanly separated label was refused by a
	// diagnostic before the decision was ever reached. Caught by
	// TestLabelGatePromotesSeparableEvidence.
	//
	// So: the cross-validation decides, and everything below it reports.
	res.Gate = crossValidateExamples(ctx, labelID, examples, positives, negatives, labelPromotionFolds).Gate

	// The single-split AUCs, purely as the MEMORISATION DIAGNOSTIC this issue
	// exists to keep visible: the grouped number against the naive class-only one,
	// where the model's own training senders appear on both sides. Best-effort —
	// a split that cannot be formed leaves the AUCs at zero and changes no
	// decision, where before it ended the run.
	if fit, holdout := splitLabelExamplesByGroup(examples, labelHoldoutFraction); len(fit) > 0 && len(holdout) > 0 {
		if candidate, err := TrainLabeler(labelID, fit, labelTrainEpochs); err == nil {
			res.Trained = true
			res.AUC = labelAUC(candidate, holdout)
		}
	}
	if naiveFit, naiveHoldout := splitLabelExamples(examples, labelHoldoutFraction); len(naiveFit) > 0 && len(naiveHoldout) > 0 {
		if naive, err := TrainLabeler(labelID, naiveFit, labelTrainEpochs); err == nil {
			res.NaiveAUC = labelAUC(naive, naiveHoldout)
		}
	}

	// VERSION 0 MEANS "no trained model", not "an incumbent scoring 0.5".
	// Registry.ActiveModel always hands back something — a flat baseline for a
	// kind that has never trained — so testing the model for nil would compare a
	// first candidate against a coin toss instead of against the bar.
	_, incumbentVersion, err := t.reg.ActiveModel(ctx, userID, KindLabel(labelID))
	if err != nil {
		incumbentVersion = 0
	}

	if !res.Gate.Pass {
		res.Version = incumbentVersion
		res.Reason = res.Gate.Reason
		return res
	}

	if dryRun {
		res.Version = incumbentVersion
		res.Reason = "dry run — would promote: " + res.Gate.Reason
		return res
	}

	// PROMOTE A MODEL TRAINED ON ALL THE EVIDENCE, not the split-trained
	// candidate. The cross-validation's job was to estimate how this label's
	// model generalises to unseen senders; having answered that, withholding a
	// quarter of the evidence from the model that actually serves would be paying
	// the estimator's cost twice.
	final, err := TrainLabeler(labelID, examples, labelTrainEpochs)
	if err != nil {
		res.Reason = "final fit: " + err.Error()
		res.Version = incumbentVersion
		return res
	}
	version, err := t.reg.Snapshot(ctx, userID, final)
	if err != nil {
		res.Reason = "snapshot: " + err.Error()
		return res
	}
	if err := t.reg.Activate(ctx, userID, KindLabel(labelID), version); err != nil {
		res.Reason = "activate: " + err.Error()
		res.Version = incumbentVersion
		return res
	}
	res.Promoted = true
	res.Version = version
	res.Reason = res.Gate.Reason
	return res
}

// splitLabelExamples holds out `frac` of EACH class, so both sides always carry
// positives and negatives.
//
// IT DOES NOT SEPARATE SENDERS, and that is exactly what makes it the wrong
// metric to promote on — see splitLabelExamplesByGroup, which is what the
// trainer uses. This one is retained because the COMPARISON is the finding: the
// gap between the two AUCs is the share of a model's apparent skill that is
// sender memorisation, and reporting one without the other is how a model that
// has learned nothing gets promoted for recognising `deals@shop.example`.
func splitLabelExamples(examples []LabelExample, frac float64) (fit, holdout []LabelExample) {
	var pos, neg []LabelExample
	for _, e := range examples {
		if e.Positive {
			pos = append(pos, e)
		} else {
			neg = append(neg, e)
		}
	}
	take := func(src []LabelExample) (keep, held []LabelExample) {
		n := int(float64(len(src)) * frac)
		if n <= 0 {
			n = 1
		}
		if n >= len(src) {
			n = len(src) - 1
		}
		if n < 0 {
			return src, nil
		}
		return src[:len(src)-n], src[len(src)-n:]
	}
	keepPos, heldPos := take(pos)
	keepNeg, heldNeg := take(neg)
	return append(keepPos, keepNeg...), append(heldPos, heldNeg...)
}

// splitLabelExamplesByGroup holds out whole SENDERS, so no sender appears on
// both sides of the split.
//
// THIS IS THE ONLY HONEST QUESTION TO ASK A LABEL MODEL. The thing it will be
// used for is mail from senders it has never seen — a sender it already knows is
// one an induced `alias =` or `fromDomain =` rule already handles for free, and
// far more legibly. Scored against a holdout containing its own training
// senders, a model that has memorised a lookup table of From addresses reports
// near-perfect AUC and has generalised nothing.
//
// That is not hypothetical: it is why the classifier's auto-apply was removed
// (#169). A label with 8 messages from 4 senders scored ~0.99 by recall and
// promoted, while a label with 22 messages from 17 senders scored near chance
// and never did — so the labels that earned an automatic model were precisely
// the ones a one-line rule already solved.
//
// Groups are assigned to the holdout whole and in order until the target share
// of POSITIVES is met, then both classes' remaining members follow their group.
// A message with no sender domain is its own group, which is the conservative
// reading: it shares memorisable identity with nothing.
//
// It can fail. If every positive comes from one sender there is no way to hold a
// sender out and still have positives on both sides, and it returns nil — which
// the caller must treat as "this label cannot be evaluated", NOT as "it scored
// badly". A single-sender label is a rule, not a model.
func splitLabelExamplesByGroup(examples []LabelExample, frac float64) (fit, holdout []LabelExample) {
	// Deterministic group order: sorted, so a run is reproducible from the
	// evidence alone (the same property the cold restart exists to preserve).
	keys := groupKeys(examples)
	posByGroup := map[string]int{}
	for i, e := range examples {
		if e.Positive {
			posByGroup[keys[i]]++
		}
	}
	groups := make([]string, 0, len(posByGroup))
	totalPos := 0
	for g, n := range posByGroup {
		groups = append(groups, g)
		totalPos += n
	}
	if len(groups) < 2 {
		return nil, nil // one sender: nothing to hold out that leaves positives behind
	}
	sort.Strings(groups)

	target := int(float64(totalPos) * frac)
	if target < 1 {
		target = 1
	}
	held := map[string]bool{}
	heldPos := 0
	// Smallest groups first, so holding out enough positives costs the fewest
	// senders and the fit side keeps the widest variety to learn from.
	sort.SliceStable(groups, func(i, j int) bool { return posByGroup[groups[i]] < posByGroup[groups[j]] })
	for _, g := range groups {
		if heldPos >= target {
			break
		}
		// Never hold out so much that the fit side loses its positives entirely.
		if heldPos+posByGroup[g] >= totalPos {
			continue
		}
		held[g] = true
		heldPos += posByGroup[g]
	}
	if heldPos == 0 {
		return nil, nil
	}

	for i, e := range examples {
		if held[keys[i]] {
			holdout = append(holdout, e)
		} else {
			fit = append(fit, e)
		}
	}
	// Both sides must carry both classes or AUC is undefined on one of them.
	if !hasBothClasses(fit) || !hasBothClasses(holdout) {
		return nil, nil
	}
	return fit, holdout
}

// groupKeys returns the grouping key for each example, positionally.
//
// An example with no sender domain gets a key of its own. Pooling them would
// invent a "no-sender" cohort that shares no memorisable identity, and holding
// that cohort out would LOOK like a sender split while testing nothing.
//
// Keyed by INDEX rather than by anything about the value: identity is what is
// wanted, and two senderless messages are different messages. (The first version
// used the address of the loop variable, which Go is free to reuse between
// calls — so two senderless examples could collide, and worse, the split stopped
// being reproducible. A test caught it.)
func groupKeys(examples []LabelExample) []string {
	out := make([]string, len(examples))
	for i, e := range examples {
		if e.Group == "" {
			out[i] = "\x00msg:" + strconv.Itoa(i)
			continue
		}
		out[i] = e.Group
	}
	return out
}

func hasBothClasses(es []LabelExample) bool {
	var pos, neg bool
	for _, e := range es {
		if e.Positive {
			pos = true
		} else {
			neg = true
		}
		if pos && neg {
			return true
		}
	}
	return false
}

// labelScores runs a model over held-out examples, returning parallel
// score/label slices. Identity features are stripped exactly as at training
// time (labelModelFeatures), so evaluation asks the model the same question
// training answered — and an incumbent trained before the strip existed cannot
// defend its seat with memorised sender weights the candidate is barred from
// having.
func labelScores(m Model, holdout []LabelExample) (scores, labels []float64) {
	scores = make([]float64, 0, len(holdout))
	labels = make([]float64, 0, len(holdout))
	for _, e := range holdout {
		label := 0.0
		if e.Positive {
			label = 1.0
		}
		scores = append(scores, m.Score(labelModelFeatures(e.Features)))
		labels = append(labels, label)
	}
	return scores, labels
}

// labelAUC scores a model over held-out examples, reusing the SAME ranking
// metric the priority trainer promotes on (eval.go's auc) rather than a second
// implementation that could disagree about what "better" means.
func labelAUC(m Model, holdout []LabelExample) float64 {
	scores, labels := labelScores(m, holdout)
	return auc(scores, labels)
}
