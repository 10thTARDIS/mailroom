// SPDX-License-Identifier: AGPL-3.0-only

package ml

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/oklog/ulid/v2"
)

func TestLabelForAction(t *testing.T) {
	cases := []struct {
		action string
		label  float64
		ok     bool
	}{
		{"open", 1.0, true},
		{"reply", 1.0, true},
		{"star", 1.0, true},
		{"mark_important", 1.0, true},
		{"archive_unread", 0.0, true},
		{"delete", 0.0, true},
		{"mark_not_important", 0.0, true},
		{"spam", 0.0, true},
		{"some_unmapped_action", 0.0, false},
		{"", 0.0, false},
	}
	for _, c := range cases {
		got, ok := LabelForAction(c.action)
		if ok != c.ok {
			t.Errorf("LabelForAction(%q) ok = %v, want %v", c.action, ok, c.ok)
			continue
		}
		if ok && got != c.label {
			t.Errorf("LabelForAction(%q) = %v, want %v", c.action, got, c.label)
		}
	}
}

// AUC on hand-built sets with exact known values (including ties + degenerate).
func TestAUCKnownValues(t *testing.T) {
	cases := []struct {
		name   string
		scores []float64
		labels []float64
		want   float64
	}{
		{"perfect ranking", []float64{0.1, 0.2, 0.3, 0.4}, []float64{0, 0, 1, 1}, 1.0},
		{"worst ranking", []float64{0.4, 0.3, 0.2, 0.1}, []float64{0, 0, 1, 1}, 0.0},
		{"interleaved -> 0.75", []float64{0.1, 0.2, 0.3, 0.4}, []float64{0, 1, 0, 1}, 0.75},
		{"tie group -> 0.75", []float64{0.5, 0.5, 0.1}, []float64{1, 0, 0}, 0.75},
		{"all tied (constant) -> 0.5", []float64{0.5, 0.5, 0.5, 0.5}, []float64{1, 0, 1, 0}, 0.5},
		{"no negatives -> 0.5", []float64{0.1, 0.9}, []float64{1, 1}, 0.5},
		{"no positives -> 0.5", []float64{0.1, 0.9}, []float64{0, 0}, 0.5},
	}
	for _, c := range cases {
		if got := auc(c.scores, c.labels); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("%s: auc = %v, want %v", c.name, got, c.want)
		}
	}
}

// A baseline that scored perfectly-ranked data would need real features; here we
// prove the harness plumbing with a feature-reading fake model, and verify skip
// accounting.
type featureScorer struct{}

func (featureScorer) Score(f FeatureVector) float64  { return f["s"] }
func (featureScorer) Update(FeatureVector, float64)  {}
func (featureScorer) Kind() string                   { return "priority" }
func (featureScorer) MarshalBinary() ([]byte, error) { return nil, nil }
func (featureScorer) UnmarshalBinary([]byte) error   { return nil }

// mapExtractor returns the stored feature vector for a message, or an error when
// the message is unknown (to exercise the skip path).
type mapExtractor map[ulid.ULID]FeatureVector

func (m mapExtractor) Extract(_ context.Context, id ulid.ULID) (FeatureVector, error) {
	fv, ok := m[id]
	if !ok {
		return nil, errors.New("no features")
	}
	return fv, nil
}

func TestEvaluate(t *testing.T) {
	m1, m2, m3, m4 := ulid.Make(), ulid.Make(), ulid.Make(), ulid.Make()
	unextractable := ulid.Make() // deliberately absent from the extractor

	fx := mapExtractor{
		m1: {"s": 0.9},
		m2: {"s": 0.8},
		m3: {"s": 0.2},
		m4: {"s": 0.1},
	}
	holdout := []FeedbackSample{
		{MessageID: m1, Action: "open"},                    // label 1
		{MessageID: m2, Action: "reply"},                   // label 1
		{MessageID: m3, Action: "archive_unread"},          // label 0
		{MessageID: m4, Action: "delete"},                  // label 0
		{MessageID: ulid.Make(), Action: "carrier_pigeon"}, // unmapped action -> skip
		{MessageID: ulid.ULID{}, Action: "open"},           // zero message id -> skip
		{MessageID: unextractable, Action: "open"},         // extraction fails -> skip
	}

	rep, err := Evaluate(context.Background(), featureScorer{}, fx, holdout)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if rep.N != 4 {
		t.Errorf("N = %d, want 4", rep.N)
	}
	if rep.Skipped != 3 {
		t.Errorf("Skipped = %d, want 3", rep.Skipped)
	}
	if rep.Positives != 2 {
		t.Errorf("Positives = %d, want 2", rep.Positives)
	}
	if math.Abs(rep.AUC-1.0) > 1e-9 {
		t.Errorf("AUC = %v, want 1.0 (scores perfectly rank the labels)", rep.AUC)
	}
	if math.Abs(rep.AccuracyAt50-1.0) > 1e-9 {
		t.Errorf("AccuracyAt50 = %v, want 1.0", rep.AccuracyAt50)
	}
	if rep.N+rep.Skipped != len(holdout) {
		t.Errorf("N+Skipped = %d, want %d", rep.N+rep.Skipped, len(holdout))
	}
}

// The decision metrics are threshold arithmetic; pin them with hand-counted
// sets. The band here is [0.2, 0.8] like the label model's.
func TestDecisionStatsAtKnownValues(t *testing.T) {
	scores := []float64{
		0.95, 0.85, // applied: one true positive, one false positive
		0.90,       // applied, positive
		0.50, 0.30, // abstained: one positive, one negative
		0.10,       // confidently rejected positive (the silent misfile)
		0.05, 0.15, // confidently rejected negatives (correct)
	}
	labels := []float64{1, 0, 1, 1, 0, 1, 0, 0}
	s := decisionStatsAt(scores, labels, 0.8, 0.2)

	if s.Scored != 8 || s.Positives != 4 {
		t.Fatalf("Scored=%d Positives=%d, want 8 and 4", s.Scored, s.Positives)
	}
	if s.Applied != 3 {
		t.Errorf("Applied = %d, want 3", s.Applied)
	}
	if math.Abs(s.Precision-2.0/3.0) > 1e-9 {
		t.Errorf("Precision = %v, want 2/3", s.Precision)
	}
	if math.Abs(s.Coverage-2.0/4.0) > 1e-9 {
		t.Errorf("Coverage = %v, want 1/2", s.Coverage)
	}
	if math.Abs(s.AbstainRate-2.0/8.0) > 1e-9 {
		t.Errorf("AbstainRate = %v, want 1/4", s.AbstainRate)
	}
	if math.Abs(s.FalseReject-1.0/4.0) > 1e-9 {
		t.Errorf("FalseReject = %v, want 1/4", s.FalseReject)
	}
}

// An undefined precision must be NaN, not 0 or 1: "nothing cleared the
// threshold" is the absence of evidence, and either number would be read as a
// measurement (the same reasoning as the skipped CV folds).
func TestDecisionStatsAtUndefinedPrecisionIsNaN(t *testing.T) {
	s := decisionStatsAt([]float64{0.5, 0.4, 0.1}, []float64{1, 0, 0}, 0.8, 0.2)
	if !math.IsNaN(s.Precision) {
		t.Errorf("Precision = %v with nothing applied, want NaN", s.Precision)
	}
	if s.Applied != 0 {
		t.Errorf("Applied = %d, want 0", s.Applied)
	}
	// The other rates are still defined here.
	if math.Abs(s.Coverage-0) > 1e-9 || math.Abs(s.FalseReject-0) > 1e-9 {
		t.Errorf("Coverage=%v FalseReject=%v, want 0 and 0", s.Coverage, s.FalseReject)
	}
	if math.Abs(s.AbstainRate-2.0/3.0) > 1e-9 {
		t.Errorf("AbstainRate = %v, want 2/3", s.AbstainRate)
	}

	empty := decisionStatsAt(nil, nil, 0.8, 0.2)
	for name, v := range map[string]float64{
		"Precision":   empty.Precision,
		"Coverage":    empty.Coverage,
		"AbstainRate": empty.AbstainRate,
		"FalseReject": empty.FalseReject,
	} {
		if !math.IsNaN(v) {
			t.Errorf("empty pool: %s = %v, want NaN", name, v)
		}
	}
}

func TestEvaluateEmptyHoldout(t *testing.T) {
	rep, err := Evaluate(context.Background(), NewBaseline("priority"), mapExtractor{}, nil)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if rep.N != 0 || rep.AUC != 0.5 {
		t.Errorf("empty holdout: N=%d AUC=%v, want 0 and 0.5", rep.N, rep.AUC)
	}
}
