// SPDX-License-Identifier: AGPL-3.0-only

package ml

import (
	"context"
	"fmt"
	"math"
	"sort"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// Grouped k-fold cross-validation for label models (#169).
//
// WHY THIS EXISTS RATHER THAN A SINGLE SCORE. A single holdout gives one number
// and no way to know what it is worth. On a real corpus the first
// grouped run produced AUCs from 0.000 to 0.923 across labels whose sender pools
// ranged from very few to many — and the HIGHEST scores came from the SMALLEST pools,
// which is exactly the shape of small-sample optimism. Choosing a promotion
// floor from numbers like that means picking the maximum of a noisy objective
// and calling it a standard, which is the error #175 was already pulled up for.
//
// So: partition the SENDERS into k folds, hold each fold out in turn, and report
// the spread. A label whose AUC barely moves across folds has an estimate that
// can carry a threshold decision. One that swings 0.3 does not, whatever its
// mean happens to be.
//
// IT IS A DIAGNOSTIC AND PROMOTES NOTHING. The promotion path still uses the
// single grouped split, because a promotion decision has to be reproducible from
// one pass; this is for deciding what that pass's numbers are worth.

// LabelCVResult reports one label's cross-validation.
type LabelCVResult struct {
	LabelID   ulid.ULID
	Positives int
	Negatives int
	// Groups is the number of distinct senders among the positives — the
	// population being sampled, and the thing that bounds how much any of this
	// can be trusted.
	Groups int
	// AUCs is one score per USABLE fold. A fold whose holdout ends up with only
	// one class is skipped rather than scored, because AUC is undefined there and
	// a zero would read as a measurement.
	AUCs []float64
	Mean float64
	Min  float64
	Max  float64
	// Spread is Max-Min: how much the answer depends on which senders happened to
	// land in the holdout. This is the number the floor decision actually needs.
	Spread float64
	// Decision is what the fixed abstention band would DO with the pooled
	// out-of-fold scores — precision, coverage, abstention and confident-reject
	// rates at the operating thresholds. AUC asks "does it rank?"; a suggest-only
	// surface asks "of the suggestions shown, how many are right?", and that is a
	// threshold question AUC cannot answer. Pooled out-of-fold scores are the
	// only honest place to compute it: the folds now partition EVERY example, so
	// each one is scored exactly once, by a model that never trained on its
	// sender's fold.
	Decision DecisionStats
	// Gate is the promotion verdict these numbers produce. It is computed HERE,
	// in the diagnostic, and consumed unchanged by the promotion path — so
	// `--folds` shows the decision `--apply` would make rather than a second
	// opinion about it (#155's Rule/preview precedent).
	Gate   LabelGate
	Reason string
}

// CrossValidateLabels runs grouped k-fold CV for every label with enough
// hand-applied evidence.
func (t *Trainer) CrossValidateLabels(ctx context.Context, userID ulid.ULID, folds int) ([]LabelCVResult, error) {
	if folds < 2 {
		folds = 2
	}
	var labels []ulid.ULID
	if err := t.st.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		labels, e = tx.LabelsWithEnoughEvidence(ctx, MinLabelTrainSamples)
		return e
	}); err != nil {
		return nil, fmt.Errorf("ml: label cv: list labels: %w", err)
	}
	sort.Slice(labels, func(i, j int) bool { return labels[i].String() < labels[j].String() })

	out := make([]LabelCVResult, 0, len(labels))
	for _, labelID := range labels {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		out = append(out, t.crossValidateLabel(ctx, userID, labelID, folds))
	}
	return out, nil
}

func (t *Trainer) crossValidateLabel(ctx context.Context, userID, labelID ulid.ULID, folds int) LabelCVResult {
	examples, positives, negatives, err := t.loadLabelExamples(ctx, userID, labelID)
	if err != nil {
		return LabelCVResult{LabelID: labelID, Reason: "load evidence: " + err.Error()}
	}
	return crossValidateExamples(ctx, labelID, examples, positives, negatives, folds)
}

// crossValidateExamples is the body, split out from the loading so the promotion
// path can run the SAME cross-validation over evidence it has already loaded
// rather than a second, differently-obtained one. There is one implementation of
// this measurement and both callers use it.
func crossValidateExamples(ctx context.Context, labelID ulid.ULID, examples []LabelExample, positives, negatives, folds int) LabelCVResult {
	if folds < 2 {
		folds = 2
	}
	res := LabelCVResult{LabelID: labelID}
	res.Positives, res.Negatives = positives, negatives
	if res.Positives < MinLabelTrainSamples || res.Negatives == 0 {
		res.Reason = "insufficient evidence"
		res.Gate = labelPromotionGate(res, folds)
		return res
	}

	keys := groupKeys(examples)
	foldOf, posGroupCount := labelCVFolds(examples, keys, folds)
	res.Groups = posGroupCount
	if res.Groups < folds {
		res.Reason = fmt.Sprintf("only %d sender(s); need at least %d for %d folds", res.Groups, folds, folds)
		res.Gate = labelPromotionGate(res, folds)
		return res
	}

	var oofScores, oofLabels []float64
	for f := 0; f < folds; f++ {
		if err := ctx.Err(); err != nil {
			res.Reason = "cancelled"
			return res
		}
		var fit, holdout []LabelExample
		for i, e := range examples {
			if foldOf[keys[i]] == f {
				holdout = append(holdout, e)
			} else {
				fit = append(fit, e)
			}
		}
		if !hasBothClasses(fit) || !hasBothClasses(holdout) {
			continue
		}
		m, err := TrainLabeler(labelID, fit, labelTrainEpochs)
		if err != nil {
			continue
		}
		scores, labels := labelScores(m, holdout)
		res.AUCs = append(res.AUCs, auc(scores, labels))
		oofScores = append(oofScores, scores...)
		oofLabels = append(oofLabels, labels...)
	}
	res.Decision = decisionStatsAt(oofScores, oofLabels, LabelApplyAbove, LabelApplyBelow)

	if len(res.AUCs) == 0 {
		res.Reason = "no fold could be scored (every holdout was single-class)"
		res.Gate = labelPromotionGate(res, folds)
		return res
	}
	res.Min, res.Max = res.AUCs[0], res.AUCs[0]
	sum := 0.0
	for _, a := range res.AUCs {
		sum += a
		res.Min = math.Min(res.Min, a)
		res.Max = math.Max(res.Max, a)
	}
	res.Mean = sum / float64(len(res.AUCs))
	res.Spread = res.Max - res.Min
	res.Gate = labelPromotionGate(res, folds)
	return res
}

// labelCVFolds assigns EVERY sender group to a fold: groups that contribute
// positives are round-robined over the sorted group list, and groups that
// contribute ONLY negatives are round-robined independently. It returns the
// assignment and how many positive-bearing groups there are.
//
// The negative-only groups used to be left out of the folds entirely — "they
// always train, never score" — and that was a defect, not a convenience: if a
// negative only ever reaches a holdout by sharing a domain with that fold's
// positives, then every scored negative is WITHIN-DOMAIN by construction, and
// the CV measures "is this Patreon-domain mail the Patreon kind?" instead of
// the question the model will actually be asked, "is this unseen sender's mail
// Patreon?". It also starved holdouts outright — a fold whose positive domains
// had no other mail held zero negatives, was single-class, and was skipped —
// which is where the skipped folds and single-digit pair counts (an AUC of
// exactly 1/6 is one positive against six negatives) came from.
//
// With every group assigned, the folds PARTITION the examples: each one lands
// in exactly one holdout, each holdout carries ~1/k of the cross-domain
// negatives, and pooling the out-of-fold scores gives every example exactly
// one honest score — the property the decision metrics above depend on.
//
// Round-robin over the SORTED lists, as before: deterministic from the
// evidence, and no fold systematically gets the alphabetically-early senders.
func labelCVFolds(examples []LabelExample, keys []string, folds int) (foldOf map[string]int, posGroupCount int) {
	posGroups := map[string]bool{}
	for i, e := range examples {
		if e.Positive {
			posGroups[keys[i]] = true
		}
	}
	negOnly := map[string]bool{}
	for i := range examples {
		if !posGroups[keys[i]] {
			negOnly[keys[i]] = true
		}
	}

	sorted := func(set map[string]bool) []string {
		out := make([]string, 0, len(set))
		for g := range set {
			out = append(out, g)
		}
		sort.Strings(out)
		return out
	}

	foldOf = make(map[string]int, len(posGroups)+len(negOnly))
	pos := sorted(posGroups)
	for i, g := range pos {
		foldOf[g] = i % folds
	}
	for i, g := range sorted(negOnly) {
		foldOf[g] = i % folds
	}
	return foldOf, len(pos)
}
