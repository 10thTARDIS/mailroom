// SPDX-License-Identifier: AGPL-3.0-only

package ml

import (
	"math"
	"testing"

	"github.com/oklog/ulid/v2"
)

// scoreTolerance is what "the same model" means for trained-score assertions,
// and it is now ZERO: two identical training runs must agree BIT FOR BIT.
//
// It used to be 1e-9. That tolerance was never a statement about training — it
// was a workaround for #222, where Score summed the feature map in Go's
// randomized iteration order and float addition is not associative, so identical
// runs drifted by accumulated ulps. Score sums in sorted key order now, the drift
// is gone, and the tolerance would only hide its return. A pass being
// reproducible FROM ITS EVIDENCE is the property the label-ULID-seeded shuffle,
// the cold restart and the sorted sum all exist to give the promotion gate; an
// approximate version of that property is not the property.
const scoreTolerance = 0

func TestKindLabelRoundTrips(t *testing.T) {
	id := ulid.MustParse("01KYJ2CC0HQHJFG2W885DFNHGN")
	kind := KindLabel(id)
	if kind == KindPriority {
		t.Fatal("a label kind collided with the priority kind")
	}
	got, ok := LabelIDFromKind(kind)
	if !ok || got != id {
		t.Errorf("LabelIDFromKind(%q) = %v, %v; want %v, true", kind, got, ok, id)
	}
	// The priority model's kind must NOT parse as a label, or a priority model
	// could be served as a label classifier.
	if _, ok := LabelIDFromKind(KindPriority); ok {
		t.Error("the priority kind parsed as a label kind")
	}
	if _, ok := LabelIDFromKind("label:not-a-ulid"); ok {
		t.Error("a malformed label kind parsed as valid")
	}
}

// The three outcomes are distinct, and two of them are easy to conflate. A LOW
// confidence is an answer ("not this label"): no label, no question. An
// ABSTENTION is the absence of an answer: it must produce a question.
func TestDecideSeparatesRejectionFromAbstention(t *testing.T) {
	for _, tc := range []struct {
		name       string
		confidence float64
		apply      bool
		abstain    bool
	}{
		{"confidently belongs", 0.95, true, false},
		{"just over the line", LabelApplyAbove, true, false},
		{"uncertain, high side", 0.79, false, true},
		{"genuinely unsure", 0.5, false, true},
		{"uncertain, low side", 0.21, false, true},
		{"just under the line", LabelApplyBelow, false, false},
		{"confidently does not belong", 0.02, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := Decide(tc.confidence, nil)
			if v.Apply != tc.apply {
				t.Errorf("Apply = %v, want %v", v.Apply, tc.apply)
			}
			if v.Abstain != tc.abstain {
				t.Errorf("Abstain = %v, want %v", v.Abstain, tc.abstain)
			}
			// The invariant that matters: never both. Applying a label while
			// also asking a human about it is incoherent, and whichever the
			// caller checks first would silently win.
			if v.Apply && v.Abstain {
				t.Error("verdict both applies the label and abstains")
			}
		})
	}
}

// The band must be wide enough to actually catch uncertainty. A band that only
// abstained at exactly 0.5 would pass a naive test while filing almost
// everything — the failure this whole feature exists to prevent.
func TestAbstentionBandIsWideEnoughToBeMeaningful(t *testing.T) {
	if LabelApplyAbove-LabelApplyBelow < 0.4 {
		t.Errorf("abstention band is %.2f wide; a narrow band files nearly everything and the model's uncertainty never reaches a human",
			LabelApplyAbove-LabelApplyBelow)
	}
	if LabelApplyAbove <= 0.5 {
		t.Error("a label can be applied on less than even odds")
	}
}

func features(pairs ...string) FeatureVector {
	f := FeatureVector{}
	for _, k := range pairs {
		f[k] = 1
	}
	return f
}

// Single-class data must be REFUSED. A model trained only on positives answers
// "yes" to everything with high confidence — it looks trained and is worthless,
// and because it is confident it would never abstain.
func TestTrainLabelerRefusesSingleClassData(t *testing.T) {
	id := ulid.MustParse("01KYJ2CC0HQHJFG2W885DFNHGN")
	var only []LabelExample
	for i := 0; i < MinLabelTrainSamples+4; i++ {
		only = append(only, LabelExample{Features: features("dom:shop.example"), Positive: true})
	}
	if _, err := TrainLabeler(id, only, 4); err == nil {
		t.Fatal("trained a model on positives only; it would answer yes to everything, confidently")
	}
}

func TestTrainLabelerRefusesTooFewExamples(t *testing.T) {
	id := ulid.MustParse("01KYJ2CC0HQHJFG2W885DFNHGN")
	few := []LabelExample{
		{Features: features("dom:a.example"), Positive: true},
		{Features: features("dom:b.example"), Positive: false},
	}
	if _, err := TrainLabeler(id, few, 4); err == nil {
		t.Fatalf("trained on %d examples; below %d there is not enough to be about the category rather than the examples",
			len(few), MinLabelTrainSamples)
	}
}

// Class balance under real imbalance (#169). The training pool is ~20x
// negative, and the unshuffled, unweighted pass ended every epoch with
// hundreds of consecutive negative updates — so a feature present in BOTH
// classes was driven net-negative and the learned base rate collapsed to the
// majority prior. The sharpest pin: give both classes IDENTICAL features, so
// the only thing the model can learn is a prior. Balanced training must land
// that prior near even odds; the old procedure lands it at the positive rate
// (~0.024 here), which the assertion catches by two orders of margin.
func TestTrainLabelerBalancesClassesUnderImbalance(t *testing.T) {
	id := ulid.MustParse("01KYJ2CC0HQHJFG2W885DFNHGN")
	var ex []LabelExample
	for i := 0; i < 12; i++ {
		ex = append(ex, LabelExample{Features: features("unsub:1"), Positive: true})
	}
	for i := 0; i < 480; i++ {
		ex = append(ex, LabelExample{Features: features("unsub:1"), Positive: false})
	}
	m, err := TrainLabeler(id, ex, labelTrainEpochs)
	if err != nil {
		t.Fatalf("train: %v", err)
	}
	if s := m.Score(features("unsub:1")); s < 0.35 || s > 0.65 {
		t.Errorf("indistinguishable classes scored %.3f; balanced training must answer ~0.5, not the majority prior", s)
	}
}

// Reproducible from the evidence alone: the shuffle must be seeded from the
// label id, so two passes over the same examples build the same model. Time-
// or globally-seeded randomness would make promotion decisions unrepeatable.
func TestTrainLabelerIsDeterministic(t *testing.T) {
	id := ulid.MustParse("01KYJ2CC0HQHJFG2W885DFNHGN")
	var ex []LabelExample
	for i := 0; i < 10; i++ {
		ex = append(ex, LabelExample{Features: features("unsub:1", "list:1"), Positive: true})
		ex = append(ex, LabelExample{Features: features("corr:1"), Positive: false})
		ex = append(ex, LabelExample{Features: features("reply:1"), Positive: false})
	}
	probe := []FeatureVector{
		features("unsub:1", "list:1"), features("corr:1"), features("unsub:1", "reply:1"), {},
	}
	first, err := TrainLabeler(id, ex, labelTrainEpochs)
	if err != nil {
		t.Fatalf("train: %v", err)
	}
	for run := 0; run < 3; run++ {
		again, err := TrainLabeler(id, ex, labelTrainEpochs)
		if err != nil {
			t.Fatalf("train %d: %v", run, err)
		}
		for _, p := range probe {
			if a, b := first.Score(p), again.Score(p); math.Abs(a-b) > scoreTolerance {
				t.Fatalf("run %d: score differs on %v: %.9f vs %.9f — the shuffle is not seeded from the evidence", run, p, a, b)
			}
		}
	}
}

// Load order must not matter. The store hands back positives-first via a UNION
// ALL whose branch order SQL does not even guarantee; the trainer partitions
// by class before shuffling, so ANY interleaving with the same within-class
// order trains the identical model. This is the regression pin for the
// original defect, where positives-first plus no shuffle made every epoch end
// on a run of negatives.
func TestTrainLabelerIgnoresLoadOrder(t *testing.T) {
	id := ulid.MustParse("01KYJ2CC0HQHJFG2W885DFNHGN")
	pos := func() LabelExample { return LabelExample{Features: features("unsub:1", "list:1"), Positive: true} }
	neg := func(i int) LabelExample {
		return LabelExample{Features: features("corr:1", "tocc:"+string(rune('a'+i%3))), Positive: false}
	}
	var grouped, interleaved []LabelExample
	for i := 0; i < 10; i++ {
		grouped = append(grouped, pos())
	}
	for i := 0; i < 30; i++ {
		grouped = append(grouped, neg(i))
	}
	for i := 0; i < 30; i++ {
		if i < 10 {
			interleaved = append(interleaved, pos())
		}
		interleaved = append(interleaved, neg(i))
	}

	a, err := TrainLabeler(id, grouped, labelTrainEpochs)
	if err != nil {
		t.Fatalf("train grouped: %v", err)
	}
	b, err := TrainLabeler(id, interleaved, labelTrainEpochs)
	if err != nil {
		t.Fatalf("train interleaved: %v", err)
	}
	for _, p := range []FeatureVector{features("unsub:1", "list:1"), features("corr:1"), {}} {
		if sa, sb := a.Score(p), b.Score(p); math.Abs(sa-sb) > scoreTolerance {
			t.Errorf("load order changed the model: %.9f vs %.9f on %v", sa, sb, p)
		}
	}
}

// The trainer shuffles internally but must never reorder the caller's slice:
// the naive-split diagnostic and the CV fold keys both read example order.
func TestTrainLabelerDoesNotReorderInput(t *testing.T) {
	id := ulid.MustParse("01KYJ2CC0HQHJFG2W885DFNHGN")
	var ex []LabelExample
	for i := 0; i < 6; i++ {
		ex = append(ex, LabelExample{Features: features("unsub:1"), Positive: true, Group: "p.example"})
		ex = append(ex, LabelExample{Features: features("corr:1"), Positive: false, Group: "n.example"})
	}
	want := make([]bool, len(ex))
	for i, e := range ex {
		want[i] = e.Positive
	}
	if _, err := TrainLabeler(id, ex, 4); err != nil {
		t.Fatalf("train: %v", err)
	}
	for i, e := range ex {
		if e.Positive != want[i] {
			t.Fatalf("example %d changed class order — the trainer reordered its input", i)
		}
	}
}

// Identity features must be invisible to the label model (#169 defect 4). The
// grouped holdout splits on from-DOMAIN, but `alias:` names the delivery alias
// — with per-service aliases, one label's mail keeps its alias across several
// mailer domains, so a held-out domain still shares identity with the fit set.
// Rather than grouping by alias too (which fuses groups through any shared
// alias — a catchall's primary address would collapse the corpus), the model
// simply never consumes `from:`/`dom:`/`alias:`. Trained where identity is the
// ONLY separator, it must learn nothing.
func TestTrainLabelerCannotLearnIdentity(t *testing.T) {
	id := ulid.MustParse("01KYJ2CC0HQHJFG2W885DFNHGN")
	var ex []LabelExample
	for i := 0; i < 10; i++ {
		ex = append(ex, LabelExample{
			Features: features("from:deals@a.example", "dom:a.example", "alias:svc@me.example", "list:1"),
			Positive: true,
		})
		ex = append(ex, LabelExample{
			Features: features("from:x@b.example", "dom:b.example", "alias:other@me.example", "list:1"),
			Positive: false,
		})
	}
	m, err := TrainLabeler(id, ex, labelTrainEpochs)
	if err != nil {
		t.Fatalf("train: %v", err)
	}
	sPos := m.Score(features("from:deals@a.example", "dom:a.example", "alias:svc@me.example", "list:1"))
	sNeg := m.Score(features("from:x@b.example", "dom:b.example", "alias:other@me.example", "list:1"))
	if diff := sPos - sNeg; diff > 0.05 || diff < -0.05 {
		t.Errorf("identity-only classes scored %.3f vs %.3f — the model is consuming identity features", sPos, sNeg)
	}
}

func TestLabelModelFeaturesStripsOnlyIdentity(t *testing.T) {
	in := features("from:a@x.example", "dom:x.example", "alias:svc@me.example",
		"unsub:1", "subtok:sale", "subjclass:promo")
	got := labelModelFeatures(in)
	for _, k := range []string{"from:a@x.example", "dom:x.example", "alias:svc@me.example"} {
		if _, ok := got[k]; ok {
			t.Errorf("identity feature %q survived the strip", k)
		}
	}
	for _, k := range []string{"unsub:1", "subtok:sale", "subjclass:promo"} {
		if _, ok := got[k]; !ok {
			t.Errorf("non-identity feature %q was stripped", k)
		}
	}
	if len(in) != 6 {
		t.Errorf("the input vector was mutated (now %d keys)", len(in))
	}
}

// The model must actually separate the classes it was given — otherwise every
// message lands in the abstention band and the feature is inert while looking
// healthy.
func TestTrainLabelerSeparatesTheClassesItWasGiven(t *testing.T) {
	id := ulid.MustParse("01KYJ2CC0HQHJFG2W885DFNHGN")
	var ex []LabelExample
	for i := 0; i < 12; i++ {
		ex = append(ex, LabelExample{Features: features("unsub:1", "list:1"), Positive: true})
		ex = append(ex, LabelExample{Features: features("corr:1", "reply:1"), Positive: false})
	}
	m, err := TrainLabeler(id, ex, 30)
	if err != nil {
		t.Fatalf("train: %v", err)
	}

	promo := m.Score(features("unsub:1", "list:1"))
	personal := m.Score(features("corr:1", "reply:1"))
	if promo <= personal {
		t.Fatalf("model does not separate the classes: promo %.3f, personal %.3f", promo, personal)
	}
	if !Decide(promo, nil).Apply {
		t.Errorf("a message identical to the positive examples scored %.3f and did not clear the apply threshold (%.2f) — the model is trained but every message would abstain",
			promo, LabelApplyAbove)
	}
	if Decide(personal, nil).Apply {
		t.Errorf("a message identical to the NEGATIVE examples scored %.3f and would be labelled", personal)
	}
}
