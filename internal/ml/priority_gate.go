// SPDX-License-Identifier: AGPL-3.0-only

package ml

import (
	"encoding/json"
	"fmt"
	"math"

	"mailroom/internal/store"
)

// The promotion gate for the PRIORITY model (#842, ADR-043).
//
// WHAT IT REPLACES. The daily pass promoted a candidate whenever its AUC on the
// newest 20% of feedback EVENTS was at least the incumbent's and at least 0.5.
// On a real instance that let v1 replace the cold-start heuristic with a handful of positives
// against thousands of negatives, and every version since scored all mail at or
// below a low ceiling — nothing reached the 0.6 line, so nothing was ever important
// again. Three defects made that possible, and each is answered here:
//
//   - AUC ON A HOLDOUT WITH NO POSITIVES IS 0.5 BY CONVENTION (eval.go's auc),
//     and 0.5 >= 0.5 passed both the floor and the tie with an incumbent that
//     was ALSO 0.5 for the same reason. With a handful of positives in thousands of events the
//     newest slice usually held none, so the comparison measured nothing and
//     promoted anyway.
//   - WRONG QUANTITY. A ranking number cannot see that every score sits below
//     the threshold the user actually looks at. The harm here is engaged mail
//     NOT shown as important, so the gate measures recall of engaged mail AT
//     THE THRESHOLD — the same lesson #220 taught the label models.
//   - WRONG COMPARATOR AND SPLIT. A time split shares senders between train
//     and holdout, which is how sender memorisation passes for skill (#169,
//     #220). The gate cross-validates over SENDER DOMAINS, and compares against
//     the cold start — the model that is serving when nothing is promoted —
//     on exactly the same out-of-fold examples.

const (
	// MinPositives is the least positive evidence a priority candidate needs
	// before it is judged at all.
	//
	// From the interval, not from taste: the gate's question is a recall, and
	// the 95% Wilson interval on a proportion from n trials is at most
	// ±1.96·sqrt(0.25/n) wide. At n = 100 that is ±0.098 — the first round
	// number at which a recall estimate is known to within ten points
	// anywhere in [0,1]. Below it the interval is wide enough that "better than
	// the cold start" cannot be told from noise on any realistic gap. The
	// incident it answers trained on 13 (±0.27).
	//
	// On a real instance at the time of #842 the positives were a few events plus the
	// unrecorded replies `priority-report` counts; the explicit `open` signal
	// is what is expected to carry the corpus past this, and until it does the
	// cold start serves — which is the intended answer, not a failure.
	MinPositives = 100

	// priorityGateFolds is k for the sender-grouped cross-validation.
	priorityGateFolds = 5

	// priorityGateSendersPerFold: at least this many positive-bearing sender
	// domains per fold, for the reason labelPromotionSendersPerFold gives — at
	// one per fold a fold's estimate is an anecdote about one sender.
	priorityGateSendersPerFold = 2

	// PriorityGateThreshold is the line the gate measures at. It MUST equal
	// api.ImportantThreshold (the presentation cutoff); a test in internal/api
	// pins the two together, because a gate that measures recall at a line the
	// user never sees would be measuring the wrong harm.
	PriorityGateThreshold = 0.6

	// priorityGateZ is the two-sided 95% z, as the label gate uses.
	priorityGateZ = 1.96

	// priorityPairedFloor: of the engaged messages the two models DISAGREE on
	// at the threshold, the Wilson lower bound on the share the candidate gets
	// right must exceed one half. See PriorityGate.PairedLCB for why the
	// comparison is paired.
	priorityPairedFloor = 0.5

	// PriorityPrecisionMargin is how much worse than the cold start the
	// candidate's precision at the threshold may be. A POLICY number: recall is
	// the harm this gate exists for, and a recall gain is allowed to cost a
	// little precision (a few more not-quite-important messages above the
	// line), but not a large amount — otherwise "mark everything important"
	// would pass on recall alone.
	PriorityPrecisionMargin = 0.05
)

// PriorityGate is one promotion verdict for a priority candidate, with every
// number it was decided on.
type PriorityGate struct {
	Threshold float64

	// Evidence.
	Positives       int
	Negatives       int
	PositiveSenders int // distinct sender domains among the positives
	MinPositives    int
	MinSenders      int
	Folds           int
	UsableFolds     int // folds whose holdout held both classes

	// Pooled out-of-fold decision stats at the threshold. Cold is the cold-start
	// heuristic over the SAME examples (it has nothing to fit, so every example
	// is out-of-sample for it by construction).
	Cand DecisionStats
	Cold DecisionStats

	CandRecallLCB    float64
	ColdRecallLCB    float64
	CandPrecisionLCB float64
	ColdPrecisionLCB float64
	CandAUC          float64
	ColdAUC          float64

	// Wins / Losses are the engaged (positive) messages exactly one model puts
	// at or above the threshold: Wins the candidate catches and the cold start
	// misses, Losses the reverse.
	Wins   int
	Losses int
	// PairedLCB is the Wilson lower bound on Wins/(Wins+Losses). PAIRED because
	// both models score the same messages: comparing two independent recall
	// intervals would discard that pairing and need far more evidence for the
	// same conclusion; McNemar's reasoning is that only the disagreements carry
	// information about which model is better. NaN when they never disagree.
	PairedLCB float64

	Pass   bool
	Reason string
}

// priorityGate decides from pooled out-of-fold candidate scores and cold-start
// scores over the same labelled examples. It is a pure function of its inputs so
// every branch can be pinned by a unit test without a database.
//
// Checks run EVIDENCE FIRST, then quality, as the label gate's do: "too little
// evidence to judge" and "judged and worse" are different answers.
func priorityGate(cand, cold []float64, positive []bool, positiveSenders, folds, usableFolds int) PriorityGate {
	g := PriorityGate{
		Threshold:        PriorityGateThreshold,
		MinPositives:     MinPositives,
		MinSenders:       priorityGateFolds * priorityGateSendersPerFold,
		Folds:            folds,
		UsableFolds:      usableFolds,
		PositiveSenders:  positiveSenders,
		CandRecallLCB:    math.NaN(),
		ColdRecallLCB:    math.NaN(),
		CandPrecisionLCB: math.NaN(),
		ColdPrecisionLCB: math.NaN(),
		PairedLCB:        math.NaN(),
	}
	labels := make([]float64, len(positive))
	for i, p := range positive {
		if p {
			labels[i] = 1
			g.Positives++
		} else {
			g.Negatives++
		}
	}
	if len(cand) == len(positive) && len(cold) == len(positive) {
		g.Cand = bandStats(cand, labels)
		g.Cold = bandStats(cold, labels)
		g.CandAUC = auc(cand, labels)
		g.ColdAUC = auc(cold, labels)
		g.CandRecallLCB = wilsonLowerBound(g.Cand.AppliedPositives, g.Cand.Positives, priorityGateZ)
		g.ColdRecallLCB = wilsonLowerBound(g.Cold.AppliedPositives, g.Cold.Positives, priorityGateZ)
		if g.Cand.Applied > 0 {
			g.CandPrecisionLCB = wilsonLowerBound(g.Cand.AppliedPositives, g.Cand.Applied, priorityGateZ)
		}
		if g.Cold.Applied > 0 {
			g.ColdPrecisionLCB = wilsonLowerBound(g.Cold.AppliedPositives, g.Cold.Applied, priorityGateZ)
		}
		for i, p := range positive {
			if !p {
				continue
			}
			c, k := important(cand[i]), important(cold[i])
			switch {
			case c && !k:
				g.Wins++
			case k && !c:
				g.Losses++
			}
		}
		if g.Wins+g.Losses > 0 {
			g.PairedLCB = wilsonLowerBound(g.Wins, g.Wins+g.Losses, priorityGateZ)
		}
	}

	switch {
	case g.Positives < g.MinPositives:
		g.Reason = fmt.Sprintf("too few positives to judge: %d engaged messages, need %d", g.Positives, g.MinPositives)
	case g.PositiveSenders < g.MinSenders:
		g.Reason = fmt.Sprintf("too few senders to judge: positives span %d sender domains, need %d (%d per fold across %d folds)",
			g.PositiveSenders, g.MinSenders, priorityGateSendersPerFold, priorityGateFolds)
	case len(cand) != len(positive) || g.UsableFolds == 0:
		g.Reason = "not evaluable: no sender-grouped fold held both classes"
	case g.Cand.Applied == 0:
		g.Reason = fmt.Sprintf("the candidate puts nothing at or above %.2f: it would mark no mail important", g.Threshold)
	case g.Wins+g.Losses == 0:
		g.Reason = fmt.Sprintf("the candidate and the cold start agree on every engaged message at %.2f: nothing to gain (recall %d/%d each)",
			g.Threshold, g.Cand.AppliedPositives, g.Cand.Positives)
	case !(g.PairedLCB > priorityPairedFloor):
		g.Reason = fmt.Sprintf("recall not better than the cold start: of %d engaged messages the models disagree on, the candidate catches %d; "+
			"the 95%% lower bound %.3f does not exceed %.2f (recall %d/%d vs cold start %d/%d)",
			g.Wins+g.Losses, g.Wins, g.PairedLCB, priorityPairedFloor,
			g.Cand.AppliedPositives, g.Cand.Positives, g.Cold.AppliedPositives, g.Cold.Positives)
	case g.Cold.Applied > 0 && g.Cand.Precision < g.Cold.Precision-PriorityPrecisionMargin:
		g.Reason = fmt.Sprintf("precision worse than the cold start by more than %.2f: %d/%d (%.3f) vs %d/%d (%.3f)",
			PriorityPrecisionMargin, g.Cand.AppliedPositives, g.Cand.Applied, g.Cand.Precision,
			g.Cold.AppliedPositives, g.Cold.Applied, g.Cold.Precision)
	default:
		g.Pass = true
		g.Reason = fmt.Sprintf("recall %d/%d vs cold start %d/%d (wins %d, losses %d, paired lower bound %.3f > %.2f); precision %.3f vs %.3f",
			g.Cand.AppliedPositives, g.Cand.Positives, g.Cold.AppliedPositives, g.Cold.Positives,
			g.Wins, g.Losses, g.PairedLCB, priorityPairedFloor, g.Cand.Precision, g.Cold.Precision)
	}
	return g
}

// important is the band decision at the threshold, on the value the score
// column will actually hold (store.StoredScore): a score of exactly 0.6 must
// land on the same side here as in Email/query's SQL comparison.
func important(score float64) bool {
	return store.StoredScore(score) >= PriorityGateThreshold
}

// bandStats is decisionStatsAt with a single line (no abstention band): Applied
// is "at or above the threshold", on the stored-precision score.
func bandStats(scores, labels []float64) DecisionStats {
	stored := make([]float64, len(scores))
	for i, s := range scores {
		stored[i] = store.StoredScore(s)
	}
	return decisionStatsAt(stored, labels, PriorityGateThreshold, -1)
}

// AuditDetail renders the gate as a JSON-safe map for the audit log: NaN (no
// evidence) becomes null rather than failing json.Marshal.
func (g PriorityGate) AuditDetail(extra map[string]any) map[string]any {
	num := func(f float64) any {
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return nil
		}
		return f
	}
	stats := func(d DecisionStats) map[string]any {
		return map[string]any{
			"scored": d.Scored, "positives": d.Positives, "applied": d.Applied,
			"appliedPositives": d.AppliedPositives,
			"precision":        num(d.Precision), "recall": num(d.Coverage),
		}
	}
	out := map[string]any{
		"threshold": g.Threshold, "positives": g.Positives, "negatives": g.Negatives,
		"positiveSenders": g.PositiveSenders, "minPositives": g.MinPositives, "minSenders": g.MinSenders,
		"folds": g.Folds, "usableFolds": g.UsableFolds,
		"candidate": stats(g.Cand), "coldStart": stats(g.Cold),
		"candRecallLCB": num(g.CandRecallLCB), "coldRecallLCB": num(g.ColdRecallLCB),
		"candAUC": num(g.CandAUC), "coldAUC": num(g.ColdAUC),
		"wins": g.Wins, "losses": g.Losses, "pairedLCB": num(g.PairedLCB),
		"precisionMargin": PriorityPrecisionMargin,
		"pass":            g.Pass, "reason": g.Reason,
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

// MarshalJSON keeps a PriorityGate printable (the admin report emits it).
func (g PriorityGate) MarshalJSON() ([]byte, error) { return json.Marshal(g.AuditDetail(nil)) }
