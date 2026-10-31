// SPDX-License-Identifier: AGPL-3.0-only

package ml

import (
	"math"
	"testing"
)

// Baseline scores 0.5 fresh and moves toward the label under Update.
func TestBaselineScoreAndUpdate(t *testing.T) {
	m := NewBaseline("priority")
	if got := m.Score(nil); math.Abs(got-0.5) > 1e-9 {
		t.Fatalf("fresh baseline Score = %v, want 0.5", got)
	}
	// Train toward 1.0: score must rise and stay in (0.5, 1).
	for i := 0; i < 200; i++ {
		m.Update(FeatureVector{"x": 1}, 1.0)
	}
	up := m.Score(nil)
	if up <= 0.5 || up >= 1.0 {
		t.Fatalf("after positive training Score = %v, want in (0.5, 1)", up)
	}

	// Train a fresh model toward 0.0: score must fall into (0, 0.5).
	m2 := NewBaseline("priority")
	for i := 0; i < 200; i++ {
		m2.Update(nil, 0.0)
	}
	down := m2.Score(nil)
	if down <= 0.0 || down >= 0.5 {
		t.Fatalf("after negative training Score = %v, want in (0, 0.5)", down)
	}
}

// MarshalBinary/UnmarshalBinary is an exact round-trip on a single instance.
func TestBaselineMarshalRoundTrip(t *testing.T) {
	m := NewBaseline("labeler").(*baselineModel)
	for i := 0; i < 37; i++ {
		m.Update(FeatureVector{"a": 1}, 1.0)
	}
	data, err := m.MarshalBinary()
	if err != nil {
		t.Fatalf("MarshalBinary: %v", err)
	}

	got := &baselineModel{}
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
	if got.Score(nil) != m.Score(nil) {
		t.Errorf("Score after round-trip = %v, want %v", got.Score(nil), m.Score(nil))
	}
	if got.lr == 0 {
		t.Error("lr not restored after Unmarshal (would freeze online updates)")
	}
}

// UnmarshalModel dispatches on the blob's type tag and reconstructs a baseline.
func TestUnmarshalModelDispatch(t *testing.T) {
	orig := NewBaseline("priority")
	orig.Update(nil, 1.0)
	data, err := orig.MarshalBinary()
	if err != nil {
		t.Fatalf("MarshalBinary: %v", err)
	}
	got, err := UnmarshalModel("priority", data)
	if err != nil {
		t.Fatalf("UnmarshalModel: %v", err)
	}
	if got.Kind() != "priority" {
		t.Errorf("Kind = %q, want priority", got.Kind())
	}
	if got.Score(nil) != orig.Score(nil) {
		t.Errorf("Score = %v, want %v", got.Score(nil), orig.Score(nil))
	}
}

// UnmarshalModel rejects a bad magic and an unknown type tag rather than
// mis-parsing — the property that makes a future format detectable.
func TestUnmarshalModelRejectsBadFraming(t *testing.T) {
	good, _ := NewBaseline("priority").MarshalBinary()

	badMagic := append([]byte(nil), good...)
	badMagic[0] ^= 0xFF
	if _, err := UnmarshalModel("priority", badMagic); err == nil {
		t.Error("UnmarshalModel accepted a blob with a corrupt magic byte")
	}

	unknownTag := append([]byte(nil), good...)
	unknownTag[1] = 0x7F // no factory registered for this tag
	if _, err := UnmarshalModel("priority", unknownTag); err == nil {
		t.Error("UnmarshalModel accepted an unknown model type tag")
	}

	if _, err := UnmarshalModel("priority", []byte{modelMagic}); err == nil {
		t.Error("UnmarshalModel accepted a too-short blob")
	}
}
