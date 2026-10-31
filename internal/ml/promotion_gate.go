// SPDX-License-Identifier: AGPL-3.0-only

package ml

import (
	"fmt"
	"math"
)

// The promotion gate for label models (#169, #220).
//
// WHAT THIS REPLACES, AND WHY. The gate used to be "holdout AUC >= labelMinAUC
// (0.65) over a single grouped split". Both halves of that were wrong, and #220
// measured how:
//
//   - WRONG QUANTITY. AUC asks "does the model rank?"; a suggest-only surface
//     asks "of the suggestions actually shown, how many are right?". Those come
//     apart badly here — AUC 0.62-0.72 coexisted with precision 0.20-0.60 at the
//     threshold that decides what a user sees, and on four labels the model
//     confidently rejected 77-88% of true positives. Gate on the quantity the
//     feature risks.
//   - WRONG ESTIMATOR. A single split is one draw. Grouped CV over the same
//     corpus showed fold spreads of 0.24-0.35, wide enough that which senders
//     landed in one holdout could carry a label over any floor in the range being
//     argued about. A point estimate from that distribution is not a standard.
//
// So the gate is: the LOWER END of a 95% interval on pooled out-of-fold
// precision at the apply threshold, over a corpus with enough distinct senders
// that no fold rests on one of them. Bottom of the interval, never the point
// estimate — the project rule earned from #220 and #155.
//
// IT USES THE SAME CROSS-VALIDATION THE DIAGNOSTIC REPORTS, deliberately.
// `train-labels --folds N` and `train-labels --apply` used to compute different
// numbers on different splits, which is the shape #155 settled for Rule/preview:
// a preview that can disagree with the engine is worse than none. Now the
// diagnostic prints the verdict the gate would reach, because it is the gate.
//
// The earlier objection to gating on CV — "a promotion decision has to be
// reproducible from one pass" (#169, 2026-07-29) — was answered by #222: the
// fold assignment is deterministic, the shuffle is seeded from the label ULID,
// the restart is cold, and scores now sum in sorted key order. A CV run is
// reproducible from its evidence, so it can carry the decision.
//
// EXPECTED RESULT ON TODAY'S CORPUS: refuses every label. That is the point, not
// a defect — see LabelGate.Pass.

const (
	// labelPromotionPrecision is the precision a candidate must clear at the
	// apply threshold before it may serve.
	//
	// This is a POLICY number, not a measured one, and it is stated as such. The
	// harm is a wrong suggestion shown to a person, so the bar is "wrong less
	// than one time in five". What keeps it from being a number picked out of
	// noise is that the decision is not sensitive to it: the measured bounds on
	// a real corpus sit around 0.2-0.55, so every label is refused at 0.70, 0.80 or
	// 0.85 alike. If a future corpus makes the exact value load-bearing, that is
	// the moment to argue it from evidence rather than now from none.
	labelPromotionPrecision = 0.80

	// labelPromotionZ is the two-sided 95% z. The LOWER endpoint of that interval
	// is deliberately more conservative than a one-sided 95% bound (1.645) would
	// be: the asymmetry is intended, because promoting a bad model and refusing a
	// good one are not equally costly here.
	labelPromotionZ = 1.96

	// labelPromotionFolds is k for the cross-validation the gate runs.
	labelPromotionFolds = 5

	// labelPromotionSendersPerFold is the minimum positive-bearing senders PER
	// FOLD, so the sender floor is labelPromotionFolds * this.
	//
	// Expressed per fold rather than as a bare count because that is where the
	// requirement actually comes from: at one sender per fold, a fold's entire
	// holdout estimate rests on a single sender, and #169 measured that the
	// highest scores came from the smallest sender pools — the signature of
	// small-sample optimism, not of quality. Two per fold is the least that makes
	// a fold an average rather than an anecdote.
	labelPromotionSendersPerFold = 2
)

// LabelGate is the promotion verdict for one label: what was measured, what was
// required, and what it means.
type LabelGate struct {
	// Target is the precision bar (labelPromotionPrecision), echoed so a report
	// never has to restate it and drift.
	Target float64
	// PrecisionLCB is the lower end of the 95% Wilson interval on pooled
	// out-of-fold precision at the apply threshold. NaN when nothing reached the
	// threshold — no evidence, which is not the same as measured zero.
	PrecisionLCB float64
	// Applied / AppliedPositives are the binomial denominator and numerator the
	// bound is computed from, carried so a reader can check the arithmetic.
	Applied          int
	AppliedPositives int
	// Senders is the number of distinct positive-bearing senders, and MinSenders
	// what was required.
	Senders    int
	MinSenders int
	// Pass is whether this label may be promoted.
	Pass bool
	// Reason states the verdict in words, and is shown whether or not it passed.
	// A gate that only explains itself when it refuses teaches nobody why it
	// accepted.
	Reason string
}

// labelPromotionGate turns a cross-validation result into a promotion verdict.
//
// Order of the checks is deliberate: EVIDENCE FIRST, then quality. "Too few
// senders to judge" and "judged and not good enough" are different answers, and
// collapsing them into one refusal is how a label that simply needs more mail
// gets read as a label the model is bad at.
func labelPromotionGate(cv LabelCVResult, folds int) LabelGate {
	minSenders := folds * labelPromotionSendersPerFold
	g := LabelGate{
		Target:           labelPromotionPrecision,
		PrecisionLCB:     math.NaN(),
		Applied:          cv.Decision.Applied,
		AppliedPositives: cv.Decision.AppliedPositives,
		Senders:          cv.Groups,
		MinSenders:       minSenders,
	}

	if len(cv.AUCs) == 0 {
		g.Reason = "not evaluable: " + naOr(cv.Reason, "no fold could be scored")
		return g
	}
	if g.Senders < minSenders {
		g.Reason = fmt.Sprintf(
			"too few senders to judge: %d positive-bearing, need %d (%d per fold across %d folds)",
			g.Senders, minSenders, labelPromotionSendersPerFold, folds)
		return g
	}
	if g.Applied == 0 {
		// NOT a precision of zero. Nothing crossed the apply threshold, so the
		// model would show no suggestions at all — there is no precision to have.
		g.Reason = "nothing reached the apply threshold: this model would make no suggestions, so there is nothing to measure"
		return g
	}

	g.PrecisionLCB = wilsonLowerBound(g.AppliedPositives, g.Applied, labelPromotionZ)
	if g.PrecisionLCB < g.Target {
		g.Reason = fmt.Sprintf(
			"precision %d/%d at the apply threshold, but the 95%% interval reaches down to %.3f — below the %.2f bar",
			g.AppliedPositives, g.Applied, g.PrecisionLCB, g.Target)
		return g
	}

	g.Pass = true
	g.Reason = fmt.Sprintf(
		"precision %d/%d over %d senders, 95%% interval bottoms at %.3f — clears the %.2f bar",
		g.AppliedPositives, g.Applied, g.Senders, g.PrecisionLCB, g.Target)
	return g
}

// naOr returns s, or fallback when s is empty — so a refusal never renders as a
// sentence with a hole in it.
func naOr(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// LabelPromotionTarget and LabelPromotionMinSenders expose the gate's bar for
// reporting, so a CLI header states the same numbers the gate enforces instead
// of a copy that can drift from them.
func LabelPromotionTarget() float64 { return labelPromotionPrecision }
func LabelPromotionMinSenders() int { return labelPromotionFolds * labelPromotionSendersPerFold }
