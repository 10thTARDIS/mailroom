// SPDX-License-Identifier: AGPL-3.0-only

package ml

import (
	"context"
	"math"
	"sort"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/stats"
)

// This file is the feedback-replay eval harness (spec §8, mailroom#35). It is
// MODEL-AGNOSTIC: it works against any Model + FeatureExtractor, so mailroom#33
// uses it to validate a candidate scorer before Activate, and it doubles as
// regression protection. It does NOT split train/eval — the caller builds the
// holdout (e.g. a time-held-out slice from store.ListFeedbackEvents) and passes
// it in, keeping the harness composable.

// FeatureExtractor turns a message id into its feature vector. mailroom#33
// implements it over the message store; the eval harness (and the future
// scorer) take it as a dependency. No real extractor ships here.
//
// An extractor returns an error when it cannot build features for a message
// (e.g. the message was purged, or its body is not yet indexed). The harness
// treats any such error as "skip this sample", so a partial holdout still yields
// a metric over the samples that could be scored.
type FeatureExtractor interface {
	Extract(ctx context.Context, msgID ulid.ULID) (FeatureVector, error)
}

// FeedbackSample is one replayable observation: a feedback event's message plus
// the action the user took. The harness derives the training LABEL from Action
// via LabelForAction, so callers hand over raw actions and never encode the
// label mapping themselves.
type FeedbackSample struct {
	MessageID ulid.ULID
	Action    string
}

// EvalReport summarizes a candidate's ranking quality over a holdout.
type EvalReport struct {
	// AUC is the area under the ROC curve of score-vs-label: the probability
	// that a random positive is scored above a random negative. 1.0 = perfect
	// ranking, 0.5 = no better than chance. It is the primary metric — the
	// question is "does the model rank important mail above unimportant", which
	// is a ranking question, so a threshold-free ranking metric is the right
	// one. Reported as 0.5 (chance) when the holdout has no positives or no
	// negatives, since AUC is undefined without both classes.
	AUC float64
	// N is the number of samples actually scored (included in the metric).
	N int
	// Positives is how many of the N scored samples had a positive label.
	Positives int
	// AccuracyAt50 is the fraction of the N scored samples whose 0.5-thresholded
	// prediction matched the binarized label — a coarse sanity number alongside
	// AUC, not the primary metric.
	AccuracyAt50 float64
	// Skipped counts holdout samples left OUT of the metric, for any reason: an
	// action with no label mapping, a zero message id, or a feature-extraction
	// failure. N + Skipped == len(holdout).
	Skipped int
}

// Action → label map (spec §8). Positive actions signal the mail mattered
// (the user engaged); negative actions signal it did not (dismissed unread /
// deleted / explicitly down-ranked). Labels are the training target in [0,1];
// the baseline yields exactly 1.0 / 0.0, leaving room for a future continuous
// signal. An action NOT in this map has no defined label and its sample is
// skipped — the harness never guesses a label for an unknown action.
var actionLabels = map[string]float64{
	// Positive — engagement.
	"open":           1.0,
	"reply":          1.0,
	"forward":        1.0,
	"star":           1.0,
	"flag":           1.0,
	"mark_important": 1.0,
	// Negative — dismissal / down-rank.
	"archive_unread":     0.0,
	"delete":             0.0,
	"trash":              0.0,
	"mark_not_important": 0.0,
	"spam":               0.0,
	// mark_read_no_open is DELIBERATELY ABSENT (#842). It was mapped to 0.0
	// and the server emitted it for every mark-read, including the one the
	// web client makes when a message is OPENED — so every message the user
	// read, including every one they then replied to, was taught as "did not
	// matter". On a real instance it was most events against a handful of
	// positives. The server cannot tell an open from a bulk mark-read, and a
	// bulk dismissal is already captured by archive_unread, so the action is
	// no longer emitted and the historical rows are no longer a label: an
	// unmapped action is skipped, never guessed. LegacyLabelForAction keeps
	// the old map so `priority-report` can reconstruct what v1..vN trained on.
}

// legacyMarkReadNoOpen is the retired token, named once so the report and the
// tests refer to the same string.
const legacyMarkReadNoOpen = "mark_read_no_open"

// LabelForAction returns the training label for a feedback action and whether
// the action is mapped at all. Callers and the harness treat ok == false as
// "no label — skip".
func LabelForAction(action string) (label float64, ok bool) {
	label, ok = actionLabels[action]
	return label, ok
}

// LegacyLabelForAction is the label map the priority trainer used BEFORE #842,
// when mark_read_no_open was a negative. Report-only: it lets `priority-report`
// reconstruct the training set an existing model version was actually fitted
// on. Nothing trains through it.
func LegacyLabelForAction(action string) (label float64, ok bool) {
	if action == legacyMarkReadNoOpen {
		return 0.0, true
	}
	return LabelForAction(action)
}

// Evaluate replays holdout against candidate: for each sample it derives the
// label from the action, extracts features, and Scores the (unchanged)
// candidate — Evaluate never calls Update, so it measures the model AS GIVEN. It
// then computes AUC (primary), plus n, positive count and accuracy at the 0.5
// threshold. Samples with an unmapped action, a zero message id, or a failed
// extraction are skipped and counted (never fail the whole run). The only error
// Evaluate returns is context cancellation.
func Evaluate(ctx context.Context, candidate Model, fx FeatureExtractor, holdout []FeedbackSample) (EvalReport, error) {
	var (
		scores []float64
		labels []float64
		report EvalReport
	)
	for _, s := range holdout {
		if err := ctx.Err(); err != nil {
			return EvalReport{}, err
		}
		label, ok := LabelForAction(s.Action)
		if !ok || s.MessageID == (ulid.ULID{}) {
			report.Skipped++
			continue
		}
		fv, err := fx.Extract(ctx, s.MessageID)
		if err != nil {
			report.Skipped++
			continue
		}
		scores = append(scores, candidate.Score(fv))
		labels = append(labels, label)
	}

	report.N = len(scores)
	if report.N == 0 {
		report.AUC = 0.5
		return report, nil
	}

	var correct int
	for i, sc := range scores {
		pos := labels[i] >= 0.5
		if pos {
			report.Positives++
		}
		if (sc >= 0.5) == pos {
			correct++
		}
	}
	report.AccuracyAt50 = float64(correct) / float64(report.N)
	report.AUC = auc(scores, labels)
	return report, nil
}

// DecisionStats summarises what a FIXED operating band would actually do with
// a set of scored examples — the threshold-level truth AUC cannot express.
//
// AUC is a ranking number: a model can rank flawlessly and still put every
// score inside the abstention band (a useless model that looks perfect), or
// clear an AUC floor while its above-threshold set is half wrong (a harmful
// model that looks fine). For a suggest-only surface the risk is WRONG
// SUGGESTIONS SHOWN, so the number that matters is precision at the operating
// threshold, with coverage and the abstention/confident-reject rates telling
// the rest of the story: how much of the label the band actually catches, how
// much lands on a human, and how much is silently — confidently — refused.
type DecisionStats struct {
	// Scored is the number of (score, label) pairs the stats are over.
	Scored int
	// Positives is how many of the Scored pairs are true positives.
	Positives int
	// Applied is how many scores reached the apply threshold — the suggestion
	// volume the band would produce.
	Applied int
	// AppliedPositives is how many of the Applied are truly positive: the
	// numerator Precision is the ratio of. Reported as a COUNT because the
	// promotion gate needs a binomial numerator and denominator, and
	// reconstructing one by rounding Precision*Applied would be recovering an
	// integer from a float that was derived from it.
	AppliedPositives int
	// Precision is, of the Applied, the fraction that are truly positive: the
	// share of shown suggestions that would be RIGHT. NaN when nothing reached
	// the threshold, which is "no evidence", not "perfectly precise".
	Precision float64
	// Coverage is, of the true positives, the fraction the apply threshold
	// catches — recall at the operating point.
	Coverage float64
	// AbstainRate is the fraction of all Scored examples inside the band: the
	// share of judged mail that lands on a human instead of an answer.
	AbstainRate float64
	// FalseReject is, of the true positives, the fraction scored at or below
	// the reject threshold — mail that belongs to the label and was CONFIDENTLY
	// told it does not. This is the silent-misfiling number: an abstained
	// positive costs a glance, a confidently rejected one is never seen again.
	FalseReject float64
}

// decisionStatsAt computes DecisionStats over parallel score/label slices at
// the given band. Labels are binarized at 0.5, matching auc.
func decisionStatsAt(scores, labels []float64, above, below float64) DecisionStats {
	s := DecisionStats{
		Scored:      len(scores),
		Precision:   math.NaN(),
		Coverage:    math.NaN(),
		AbstainRate: math.NaN(),
		FalseReject: math.NaN(),
	}
	var appliedPos, rejectedPos, abstained int
	for i, sc := range scores {
		pos := labels[i] >= 0.5
		if pos {
			s.Positives++
		}
		switch {
		case sc >= above:
			s.Applied++
			if pos {
				appliedPos++
			}
		case sc <= below:
			if pos {
				rejectedPos++
			}
		default:
			abstained++
		}
	}
	s.AppliedPositives = appliedPos
	if s.Applied > 0 {
		s.Precision = float64(appliedPos) / float64(s.Applied)
	}
	if s.Scored > 0 {
		s.AbstainRate = float64(abstained) / float64(s.Scored)
	}
	if s.Positives > 0 {
		s.Coverage = float64(appliedPos) / float64(s.Positives)
		s.FalseReject = float64(rejectedPos) / float64(s.Positives)
	}
	return s
}

// auc computes the area under the ROC curve via the Mann-Whitney U (rank-sum)
// identity, using AVERAGE ranks so tied scores are handled correctly:
//
//	AUC = (sum of positive ranks - nPos*(nPos+1)/2) / (nPos * nNeg)
//
// Labels are binarized at 0.5. A model that ranks every positive above every
// negative gives AUC = 1.0; a constant (or otherwise fully-tied) scorer gives
// exactly 0.5 because every sample shares the mid-rank. With no positives or no
// negatives AUC is undefined; we report 0.5.
func auc(scores, labels []float64) float64 {
	n := len(scores)
	idx := make([]int, n)
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool { return scores[idx[a]] < scores[idx[b]] })

	ranks := make([]float64, n)
	for i := 0; i < n; {
		j := i
		for j+1 < n && scores[idx[j+1]] == scores[idx[i]] {
			j++
		}
		// Tie group idx[i..j]: 1-based average rank = ((i+1)+(j+1))/2.
		avg := float64(i+j+2) / 2.0
		for k := i; k <= j; k++ {
			ranks[idx[k]] = avg
		}
		i = j + 1
	}

	var sumPosRanks float64
	var nPos, nNeg int
	for i := 0; i < n; i++ {
		if labels[i] >= 0.5 {
			sumPosRanks += ranks[i]
			nPos++
		} else {
			nNeg++
		}
	}
	if nPos == 0 || nNeg == 0 {
		return 0.5
	}
	return (sumPosRanks - float64(nPos)*float64(nPos+1)/2) / (float64(nPos) * float64(nNeg))
}

// wilsonLowerBound is stats.WilsonLowerBound (#714 moved it to a shared
// package so learned placement's correction gate is the same calculation).
// Kept as a name here so the promotion gate and its tests read as before.
func wilsonLowerBound(successes, trials int, z float64) float64 {
	return stats.WilsonLowerBound(successes, trials, z)
}
