// SPDX-License-Identifier: AGPL-3.0-only

package ml

import (
	"math"
	"strings"
	"testing"
)

// Wilson, against values computed by hand, including the two cases the normal
// approximation gets wrong and this gate depends on getting right.
func TestWilsonLowerBound(t *testing.T) {
	cases := []struct {
		name              string
		succ, trials      int
		want              float64
		wantNaN           bool
		normalWouldReport string
	}{
		// The normal interval has ZERO width at p̂=1 and would report 1.000 — a
		// label promoting on five suggestions. This is the case Wilson exists for.
		{name: "five for five", succ: 5, trials: 5, want: 0.5655, normalWouldReport: "1.000"},
		// Point estimate 0.900, bound 0.596: the gap IS the sample size.
		{name: "nine of ten", succ: 9, trials: 10, want: 0.5958},
		{name: "ninety-five of a hundred", succ: 95, trials: 100, want: 0.8882},
		// A Promotions-style label as #220 measured it.
		{name: "the measured corpus", succ: 83, trials: 139, want: 0.5140},
		{name: "none of ten", succ: 0, trials: 10, want: 0.0},
		{name: "no evidence at all", trials: 0, wantNaN: true},
	}
	for _, c := range cases {
		got := wilsonLowerBound(c.succ, c.trials, labelPromotionZ)
		if c.wantNaN {
			if !math.IsNaN(got) {
				t.Errorf("%s: got %v, want NaN — no trials is 'no evidence', not 'measured zero'", c.name, got)
			}
			continue
		}
		if math.Abs(got-c.want) > 5e-4 {
			t.Errorf("%s: wilson(%d/%d) = %.4f, want %.4f", c.name, c.succ, c.trials, got, c.want)
		}
		if got < 0 || got > 1 {
			t.Errorf("%s: bound %.4f escaped [0,1]", c.name, got)
		}
	}
}

// gateOver builds the minimum LabelCVResult the gate reads, so a test states
// only the numbers it is about.
func gateOver(senders, applied, appliedPos int) LabelGate {
	return labelPromotionGate(LabelCVResult{
		Groups:   senders,
		AUCs:     []float64{0.7, 0.7, 0.7, 0.7, 0.7},
		Decision: DecisionStats{Applied: applied, AppliedPositives: appliedPos},
	}, labelPromotionFolds)
}

// THE POINT OF THE WHOLE GATE: a point estimate that clears the bar must still
// be refused when the interval below it does not. 9 of 10 is precision 0.900
// against a 0.80 target — an AUC-or-point-estimate gate promotes it — and ten
// suggestions cannot support that claim.
func TestGateRefusesAHighPointEstimateOnThinEvidence(t *testing.T) {
	g := gateOver(20, 10, 9)
	if pointEstimate := 9.0 / 10.0; pointEstimate < g.Target {
		t.Fatalf("test is not testing what it claims: point estimate %.3f is already below the %.2f target", pointEstimate, g.Target)
	}
	if g.Pass {
		t.Errorf("promoted on 9/10: the gate is reading the point estimate, not the bottom of the interval")
	}
	if !strings.Contains(g.Reason, "reaches down to") {
		t.Errorf("reason %q does not say the interval is what refused it", g.Reason)
	}
}

// Real numbers (#220): a Promotions label at the apply
// threshold. This is the corpus the whole epic was judged on and
// the gate must refuse it.
func TestGateRefusesTheMeasuredCorpus(t *testing.T) {
	g := gateOver(79, 139, 83)
	if g.Pass {
		t.Errorf("promoted the label #220 measured at 0.597 precision (interval bottom %.3f)", g.PrecisionLCB)
	}
	if g.PrecisionLCB >= g.Target {
		t.Errorf("PrecisionLCB %.3f unexpectedly clears the %.2f target", g.PrecisionLCB, g.Target)
	}
}

// Evidence before quality: a thin sender pool is "cannot judge", not "judged and
// bad". Perfect precision must NOT rescue it — #169 measured that the highest
// scores came from the smallest pools.
func TestGateRefusesThinSenderPoolsRegardlessOfPrecision(t *testing.T) {
	g := gateOver(LabelPromotionMinSenders()-1, 200, 200)
	if g.Pass {
		t.Fatalf("promoted on %d senders with a floor of %d", g.Senders, g.MinSenders)
	}
	if !strings.Contains(g.Reason, "too few senders") {
		t.Errorf("reason %q reads as a quality refusal; a thin pool is an unanswerable question, not a bad answer", g.Reason)
	}
	// One more sender, same flawless evidence, and it passes — proving the sender
	// floor was the only thing refusing it.
	if ok := gateOver(LabelPromotionMinSenders(), 200, 200); !ok.Pass {
		t.Errorf("at the sender floor with 200/200 the gate still refused: %s", ok.Reason)
	}
}

// "Nothing reached the apply threshold" is not "precision zero". A model that
// makes no suggestions has no precision to have, and reporting 0.000 would read
// as measured-and-terrible.
func TestGateSeparatesNoSuggestionsFromZeroPrecision(t *testing.T) {
	none := gateOver(20, 0, 0)
	if none.Pass {
		t.Error("promoted a model that would make no suggestions at all")
	}
	if !math.IsNaN(none.PrecisionLCB) {
		t.Errorf("PrecisionLCB = %v, want NaN when nothing was applied", none.PrecisionLCB)
	}
	if !strings.Contains(none.Reason, "no suggestions") {
		t.Errorf("reason %q does not distinguish 'made none' from 'got them wrong'", none.Reason)
	}

	wrong := gateOver(20, 40, 0)
	if math.IsNaN(wrong.PrecisionLCB) {
		t.Error("40 suggestions all wrong is a measurement; it must not report as 'no evidence'")
	}
}

// A label with genuinely strong, well-spread evidence passes. Without this the
// suite only proves the gate can say no, which a `return false` also does.
func TestGateAcceptsStrongWellSpreadEvidence(t *testing.T) {
	g := gateOver(20, 100, 95)
	if !g.Pass {
		t.Fatalf("refused 95/100 over 20 senders: %s", g.Reason)
	}
	if g.PrecisionLCB < g.Target {
		t.Errorf("passed with PrecisionLCB %.3f below target %.2f", g.PrecisionLCB, g.Target)
	}
	if !strings.Contains(g.Reason, "clears") {
		t.Errorf("a passing gate must say why it accepted; got %q", g.Reason)
	}
}

// An unevaluable label is refused as unevaluable, carrying the CV's own reason
// rather than a manufactured verdict.
func TestGateRefusesAnUnevaluableLabel(t *testing.T) {
	g := labelPromotionGate(LabelCVResult{Reason: "insufficient evidence"}, labelPromotionFolds)
	if g.Pass {
		t.Error("promoted a label that could not be cross-validated at all")
	}
	if !strings.Contains(g.Reason, "insufficient evidence") {
		t.Errorf("reason %q dropped the cross-validation's own explanation", g.Reason)
	}
}
