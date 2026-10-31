// SPDX-License-Identifier: AGPL-3.0-only

package ml

import (
	"encoding/json"
	"strings"
	"testing"
)

// gateCase builds parallel score slices for the pure gate. Positives come first:
// candHit of them score 0.9 under the candidate (else 0.1), coldHit of them
// 0.9 under the cold start; overlap of the hits are shared (both catch them).
// Negatives: candFP / coldFP of them score 0.9 under each model.
type gateCase struct {
	pos, neg         int
	candHit, coldHit int
	overlap          int
	candFP, coldFP   int
	senders          int
	usableFolds      int
}

func (c gateCase) run() PriorityGate {
	n := c.pos + c.neg
	cand := make([]float64, n)
	cold := make([]float64, n)
	positive := make([]bool, n)
	for i := 0; i < n; i++ {
		cand[i], cold[i] = 0.1, 0.1
	}
	// Positives 0..pos-1: the first `overlap` are caught by both; then
	// candHit-overlap by the candidate only; then coldHit-overlap by the cold
	// start only.
	idx := 0
	for k := 0; k < c.overlap; k++ {
		cand[idx], cold[idx] = 0.9, 0.9
		idx++
	}
	for k := 0; k < c.candHit-c.overlap; k++ {
		cand[idx] = 0.9
		idx++
	}
	for k := 0; k < c.coldHit-c.overlap; k++ {
		cold[idx] = 0.9
		idx++
	}
	for i := 0; i < c.pos; i++ {
		positive[i] = true
	}
	for k := 0; k < c.candFP; k++ {
		cand[c.pos+k] = 0.9
	}
	for k := 0; k < c.coldFP; k++ {
		cold[c.pos+k] = 0.9
	}
	senders := c.senders
	if senders == 0 {
		senders = 20
	}
	usable := c.usableFolds
	if usable == 0 {
		usable = priorityGateFolds
	}
	return priorityGate(cand, cold, positive, senders, priorityGateFolds, usable)
}

// A good candidate passes: it catches clearly more engaged mail than the cold
// start, at no worse precision. Every refusal test below differs from this by
// ONE input, which is what makes each a negative control for the others.
var goodGate = gateCase{pos: 120, neg: 600, candHit: 80, coldHit: 40, overlap: 35, candFP: 20, coldFP: 20}

func TestPriorityGateAcceptsAGoodCandidate(t *testing.T) {
	g := goodGate.run()
	if !g.Pass {
		t.Fatalf("a candidate with recall 80/120 vs 40/120 was refused: %s", g.Reason)
	}
	if g.Wins != 45 || g.Losses != 5 {
		t.Errorf("wins/losses = %d/%d, want 45/5", g.Wins, g.Losses)
	}
}

// Few positives — the v1 shape (13 of them) — is refused before quality is
// even considered, however good the numbers look. Control: the same candidate
// at exactly MinPositives passes.
func TestPriorityGateRefusesTooFewPositives(t *testing.T) {
	c := goodGate
	c.pos, c.candHit, c.coldHit, c.overlap = MinPositives-1, 66, 33, 29
	if g := c.run(); g.Pass || !strings.Contains(g.Reason, "too few positives") {
		t.Fatalf("%d positives passed (or for the wrong reason): pass=%v %q", MinPositives-1, g.Pass, g.Reason)
	}
	c.pos = MinPositives
	if g := c.run(); !g.Pass {
		t.Fatalf("control: the same candidate at %d positives was refused: %s", MinPositives, g.Reason)
	}
}

func TestPriorityGateRefusesTooFewSenders(t *testing.T) {
	c := goodGate
	c.senders = priorityGateFolds*priorityGateSendersPerFold - 1
	if g := c.run(); g.Pass || !strings.Contains(g.Reason, "too few senders") {
		t.Fatalf("pass=%v %q", g.Pass, g.Reason)
	}
}

// Worse recall than the cold start is refused. Swapping the two hit counts of
// the passing case is the whole difference.
func TestPriorityGateRefusesWorseRecall(t *testing.T) {
	c := goodGate
	c.candHit, c.coldHit = goodGate.coldHit, goodGate.candHit
	g := c.run()
	if g.Pass || !strings.Contains(g.Reason, "recall not better") {
		t.Fatalf("a candidate catching 40/120 vs the cold start's 80/120 passed: pass=%v %q", g.Pass, g.Reason)
	}
}

// The paired bound needs real evidence: three uncontested wins are not enough
// (Wilson LB 0.44), four are (0.51). Pins the floor's meaning.
func TestPriorityGatePairedBoundNeedsEvidence(t *testing.T) {
	c := goodGate
	c.candHit, c.coldHit, c.overlap = 43, 40, 40
	if g := c.run(); g.Pass {
		t.Fatalf("3 wins / 0 losses passed (paired LB %.3f)", g.PairedLCB)
	}
	c.candHit = 44
	if g := c.run(); !g.Pass {
		t.Fatalf("control: 4 wins / 0 losses was refused: %s", g.Reason)
	}
}

// Buying recall by marking everything important is refused on precision.
// Control: the same recall gain at a precision within the margin passes.
func TestPriorityGateRefusesAPrecisionCollapse(t *testing.T) {
	c := goodGate
	c.candFP = 400 // precision 80/480 = 0.17 vs cold 40/60 = 0.67
	if g := c.run(); g.Pass || !strings.Contains(g.Reason, "precision worse") {
		t.Fatalf("pass=%v %q", g.Pass, g.Reason)
	}
	c.candFP = 22 // 80/102 = 0.784 vs 40/60 = 0.667: better, passes
	if g := c.run(); !g.Pass {
		t.Fatalf("control: refused at good precision: %s", g.Reason)
	}
}

// The production collapse: a candidate whose every score sits under the line
// (max 0.233 on v1..v9) marks nothing important, and is refused for exactly
// that — never promoted on a ranking number.
func TestPriorityGateRefusesAModelThatMarksNothingImportant(t *testing.T) {
	c := goodGate
	c.candHit, c.overlap, c.candFP = 0, 0, 0
	g := c.run()
	if g.Pass || !strings.Contains(g.Reason, "nothing at or above") {
		t.Fatalf("pass=%v %q", g.Pass, g.Reason)
	}
}

// Good-looking scores from a cross-validation in which no fold held both
// classes measured nothing, and are refused. Control: the passing case.
func TestPriorityGateRefusesWhenNoFoldIsUsable(t *testing.T) {
	ref := goodGate.run()
	if !ref.Pass {
		t.Fatalf("control case does not pass: %s", ref.Reason)
	}
	n := goodGate.pos + goodGate.neg
	cand, cold, positive := make([]float64, n), make([]float64, n), make([]bool, n)
	for i := 0; i < goodGate.pos; i++ {
		positive[i] = true
		cand[i] = 0.9
	}
	g := priorityGate(cand, cold, positive, 20, priorityGateFolds, 0)
	if g.Pass || !strings.Contains(g.Reason, "not evaluable") {
		t.Fatalf("pass=%v %q", g.Pass, g.Reason)
	}
}

// A score of exactly the threshold lands on the same side the stored real does
// in Email/query's SQL comparison.
func TestPriorityGateBandUsesStoredPrecision(t *testing.T) {
	if !important(0.6) {
		t.Error("0.6 is not important; float32(0.6) widened is 0.6000000238 >= 0.6")
	}
	if important(0.5999999) {
		t.Error("0.5999999 is important")
	}
}

// The audit detail must marshal even when a bound is NaN (no evidence).
func TestPriorityGateAuditDetailMarshals(t *testing.T) {
	g := priorityGate(nil, nil, nil, 0, priorityGateFolds, 0)
	if _, err := json.Marshal(g.AuditDetail(map[string]any{"x": 1})); err != nil {
		t.Fatalf("audit detail does not marshal: %v", err)
	}
}
