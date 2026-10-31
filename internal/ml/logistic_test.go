// SPDX-License-Identifier: AGPL-3.0-only

package ml

import (
	"bytes"
	"math"
	"testing"
)

// A fresh logistic model Scores exactly 0.5 (bias 0, no weights); training with
// Update moves the score for the trained features and it converges within (0,1).
func TestLogisticScoreAndUpdate(t *testing.T) {
	m := NewLogistic("priority").(*logisticModel)
	if got := m.Score(nil); math.Abs(got-0.5) > 1e-9 {
		t.Fatalf("fresh logistic Score(nil) = %v, want 0.5", got)
	}
	f := FeatureVector{"from:vip@example.com": 1, "corr:1": 1}
	if got := m.Score(f); math.Abs(got-0.5) > 1e-9 {
		t.Fatalf("fresh logistic Score(f) = %v, want 0.5 (no weights yet)", got)
	}

	// Train toward important: the score for f must rise into (0.5, 1).
	for i := 0; i < 300; i++ {
		m.Update(f, 1.0)
	}
	up := m.Score(f)
	if up <= 0.5 || up >= 1.0 {
		t.Fatalf("after positive training Score = %v, want in (0.5, 1)", up)
	}

	// A different, unseen feature set carries no learned weight of its own, so it
	// contributes exactly zero and Scores at the model's current base rate —
	// sigmoid(bias), NOT a fixed 0.5: the intercept has moved with training. This
	// is the real per-feature invariant (the unseen keys add nothing; only the
	// learned intercept, i.e. the base rate, remains).
	if other := m.Score(FeatureVector{"from:spam@nope.test": 1}); math.Abs(other-sigmoid(m.bias)) > 1e-9 {
		t.Errorf("unseen features Score = %v, want sigmoid(bias) = %v (unseen keys add zero)", other, sigmoid(m.bias))
	}

	// Train a fresh model toward unimportant: the score must fall into (0, 0.5).
	down := NewLogistic("priority")
	for i := 0; i < 300; i++ {
		down.Update(f, 0.0)
	}
	if s := down.Score(f); s <= 0.0 || s >= 0.5 {
		t.Fatalf("after negative training Score = %v, want in (0, 0.5)", s)
	}
}

// Score must not mutate the model (it is documented as read-safe for concurrent
// callers): scoring the same vector twice yields the identical value and leaves
// the weights/bias untouched.
func TestLogisticScoreIsReadOnly(t *testing.T) {
	m := NewLogistic("priority").(*logisticModel)
	m.Update(FeatureVector{"a": 1, "b": 1}, 1.0)
	before := map[string]float64{}
	for k, v := range m.weights {
		before[k] = v
	}
	biasBefore := m.bias

	f := FeatureVector{"a": 1}
	s1 := m.Score(f)
	s2 := m.Score(f)
	if s1 != s2 {
		t.Errorf("Score not deterministic: %v vs %v", s1, s2)
	}
	if m.bias != biasBefore {
		t.Errorf("Score mutated bias: %v -> %v", biasBefore, m.bias)
	}
	if len(m.weights) != len(before) {
		t.Fatalf("Score mutated weight count: %d -> %d", len(before), len(m.weights))
	}
	for k, v := range before {
		if m.weights[k] != v {
			t.Errorf("Score mutated weight %q: %v -> %v", k, v, m.weights[k])
		}
	}
}

// MarshalBinary/UnmarshalBinary is an exact round-trip: a trained model survives
// serialization with identical weights, bias, n, and Score.
func TestLogisticMarshalRoundTrip(t *testing.T) {
	m := NewLogistic("priority").(*logisticModel)
	feats := []FeatureVector{
		{"from:a@x.test": 1, "dom:x.test": 1, "hour:9": 1},
		{"list:1": 1, "subtok:sale": 1, "sent:0": 1},
		{"corr:1": 1, "sent:6-10": 1, "reply:1": 1},
	}
	for i := 0; i < 50; i++ {
		m.Update(feats[i%len(feats)], float64(i%2))
	}

	data, err := m.MarshalBinary()
	if err != nil {
		t.Fatalf("MarshalBinary: %v", err)
	}
	if data[0] != modelMagic || data[1] != typeTagLogistic || data[2] != logisticFormatV1 {
		t.Fatalf("framing = %#x %#x %#x, want %#x %#x %#x",
			data[0], data[1], data[2], modelMagic, typeTagLogistic, logisticFormatV1)
	}

	got := &logisticModel{}
	if err := got.UnmarshalBinary(data); err != nil {
		t.Fatalf("UnmarshalBinary: %v", err)
	}
	if got.kind != m.kind {
		t.Errorf("kind = %q, want %q", got.kind, m.kind)
	}
	if got.bias != m.bias {
		t.Errorf("bias = %v, want %v", got.bias, m.bias)
	}
	if got.n != m.n {
		t.Errorf("n = %v, want %v", got.n, m.n)
	}
	if len(got.weights) != len(m.weights) {
		t.Fatalf("weight count = %d, want %d", len(got.weights), len(m.weights))
	}
	for k, v := range m.weights {
		if got.weights[k] != v {
			t.Errorf("weight %q = %v, want %v", k, got.weights[k], v)
		}
	}
	if got.lr == 0 || got.l2 == 0 {
		t.Error("lr/l2 not restored after Unmarshal (would freeze online updates / regularization)")
	}
	for _, f := range feats {
		if got.Score(f) != m.Score(f) {
			t.Errorf("Score(%v) after round-trip = %v, want %v", f, got.Score(f), m.Score(f))
		}
	}

	// A trained model Scores DIFFERENTLY from a fresh one — the whole point of
	// persisting weights.
	if fresh := NewLogistic("priority"); math.Abs(fresh.Score(feats[0])-got.Score(feats[0])) < 1e-6 {
		t.Error("trained model Scores the same as a fresh one; training/round-trip lost the weights")
	}
}

// UnmarshalModel dispatches on type tag 0x02 to the logistic implementation.
func TestUnmarshalModelDispatchesLogistic(t *testing.T) {
	orig := NewLogistic("priority")
	orig.Update(FeatureVector{"corr:1": 1}, 1.0)
	data, err := orig.MarshalBinary()
	if err != nil {
		t.Fatalf("MarshalBinary: %v", err)
	}
	got, err := UnmarshalModel("priority", data)
	if err != nil {
		t.Fatalf("UnmarshalModel: %v", err)
	}
	if _, ok := got.(*logisticModel); !ok {
		t.Fatalf("UnmarshalModel returned %T, want *logisticModel", got)
	}
	if got.Kind() != "priority" {
		t.Errorf("Kind = %q, want priority", got.Kind())
	}
	if got.Score(FeatureVector{"corr:1": 1}) != orig.Score(FeatureVector{"corr:1": 1}) {
		t.Error("dispatched model Scores differ from the original")
	}
}

// UnmarshalBinary rejects a wrong type tag and a truncated blob rather than
// mis-parsing (the property that keeps a future format detectable).
func TestLogisticUnmarshalRejectsBadFraming(t *testing.T) {
	good, _ := NewLogistic("priority").MarshalBinary()

	wrongTag := append([]byte(nil), good...)
	wrongTag[1] = typeTagBaseline
	if err := (&logisticModel{}).UnmarshalBinary(wrongTag); err == nil {
		t.Error("logistic accepted a blob tagged as the baseline")
	}

	if err := (&logisticModel{}).UnmarshalBinary(good[:5]); err == nil {
		t.Error("logistic accepted a truncated blob")
	}
}

// Score must not depend on Go's map iteration order (#222). Two Scores of one
// model over one FeatureVector have to agree BIT FOR BIT, because the promotion
// gate compares AUCs derived from these sums: when a candidate and its incumbent
// are genuinely tied, iteration order decides the promotion and nothing about
// the mail does.
//
// THE WEIGHTS ARE ADVERSARIAL ON PURPOSE — ±1e16 flanking two 1.0s — so this
// assertion is DETERMINISTIC rather than probabilistic. Summed with the large
// terms adjacent they cancel and the ones survive (2); summed with a 1.0 first it
// is swallowed by 1e16 and the total is 0. Realistic weights differ only by ulps,
// so the same test written with them would pass by luck on most runs — a test
// that cannot reliably fail is documentation.
func TestLogisticScoreIsOrderIndependent(t *testing.T) {
	m := NewLogistic("priority").(*logisticModel)
	m.weights = map[string]float64{
		"a:big": 1e16, "b:one": 1.0, "c:one": 1.0, "d:big": -1e16,
	}
	f := FeatureVector{"a:big": 1, "b:one": 1, "c:one": 1, "d:big": 1}

	want := m.Score(f)
	for i := 0; i < 500; i++ {
		if got := m.Score(f); got != want {
			t.Fatalf("Score varies with map iteration order on run %d: %v vs %v", i, got, want)
		}
	}
}

// The serialized blob is a function of the MODEL, not of the runtime. Snapshots
// are stored content-addressed (ADR-004), so a map-ordered payload gives one
// model a different content hash on every save.
func TestLogisticMarshalIsDeterministic(t *testing.T) {
	m := NewLogistic("priority").(*logisticModel)
	for i := 0; i < 200; i++ {
		m.Update(FeatureVector{
			"from:a@x.test": 1, "dom:x.test": 1, "list:1": 1,
			"unsub:1": 1, "subtok:sale": 1, "hour:9": 1,
		}, float64(i%2))
	}

	want, err := m.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for i := 0; i < 100; i++ {
		got, err := m.MarshalBinary()
		if err != nil {
			t.Fatalf("marshal %d: %v", i, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("MarshalBinary is not deterministic: run %d differs from the first", i)
		}
	}
}

// The regression pin behind Update's "deliberately not sorted" comment (#222):
// its per-feature steps each read and write one key and touch no accumulator, so
// only Score's sum was order-dependent. Two models fed identical update sequences
// must therefore serialize to identical BYTES — the strongest available statement
// of "the same model", covering bias, weights and n at once.
//
// THE MODEL IS PRE-SEEDED WITH ADVERSARIAL WEIGHTS, and that is what gives this
// test the ability to fail. Written with ordinary learned magnitudes it passed
// both with and without Score's sort — the summation differences were too small
// to survive into the weights, so it pinned nothing. Seeding ±1e15 on two keys
// makes Score's order-dependence propagate into every subsequent step, and the
// test goes red the moment that sum stops being order-independent. Probed
// exactly that way.
//
// If it ever goes red for a reason OTHER than Score, the claim in Update's
// comment is wrong and its loop needs the sort Score has.
func TestLogisticUpdateIsBitReproducible(t *testing.T) {
	train := func() []byte {
		m := NewLogistic("priority").(*logisticModel)
		m.weights["a:big"] = 1e15
		m.weights["z:big"] = -1e15
		for i := 0; i < 300; i++ {
			m.Update(FeatureVector{
				"a:big": 1, "z:big": 1,
				"from:a@x.test": 1, "dom:x.test": 1, "alias:me@y.test": 1,
				"list:1": 1, "unsub:1": 1, "subtok:sale": 1, "subtok:now": 1,
				"tocc:2": 1, "hour:9": 1, "sent:0": 1,
			}, float64(i%3)/2.0)
		}
		b, err := m.MarshalBinary()
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return b
	}
	want := train()
	for i := 0; i < 20; i++ {
		if got := train(); !bytes.Equal(got, want) {
			t.Fatalf("run %d trained a different model from identical updates", i)
		}
	}
}
