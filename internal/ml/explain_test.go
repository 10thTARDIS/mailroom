// SPDX-License-Identifier: AGPL-3.0-only

package ml

import (
	"math"
	"testing"
)

// A logistic model's Contributions is the signed per-feature breakdown of the
// same logit Score sums: the top contribution is the largest weight·value term,
// zero-weight / zero-value features are omitted, and the contributions sum
// CONSISTENTLY with Score (sigmoid(bias + Σ contributions) == Score).
func TestLogisticContributions(t *testing.T) {
	m := NewLogistic("priority").(*logisticModel)
	m.weights = map[string]float64{
		"corr:1":               1.5,
		"from:vip@example.com": 0.4,
		"list:1":               -0.9,
		"sent:0":               0.0, // zero weight -> no contribution
	}
	f := FeatureVector{
		"corr:1":               1,
		"from:vip@example.com": 1,
		"list:1":               1,
		"sent:0":               1, // zero weight
		"hour:9":               1, // no weight at all
	}

	c := m.Contributions(f)
	if _, ok := c["sent:0"]; ok {
		t.Error("zero-weight feature reported as a contribution")
	}
	if _, ok := c["hour:9"]; ok {
		t.Error("unweighted feature reported as a contribution")
	}
	if math.Abs(c["corr:1"]-1.5) > 1e-12 {
		t.Errorf("corr:1 contribution = %v, want 1.5", c["corr:1"])
	}
	if math.Abs(c["list:1"]-(-0.9)) > 1e-12 {
		t.Errorf("list:1 contribution = %v, want -0.9", c["list:1"])
	}

	// Sum-consistency with Score: sigmoid(bias + Σ contributions) == Score(f).
	var sum float64
	for _, w := range c {
		sum += w
	}
	if got := sigmoid(m.bias + sum); math.Abs(got-m.Score(f)) > 1e-12 {
		t.Errorf("contributions do not sum to Score: sigmoid(bias+Σ)=%v, Score=%v", got, m.Score(f))
	}

	// The top contribution is the highest-magnitude term (corr:1 at 1.5).
	top := TopContributions(m, f, 5)
	if len(top) == 0 || top[0].Key != "corr:1" {
		t.Errorf("top contribution = %+v, want corr:1 first", top)
	}
}

// The cold-start heuristic explains itself over its FIXED weights, mirroring
// Score term for term: a list+authfail message's strongest reasons are the
// list / authfail DOWN signals; a known+direct message's strongest are the
// known / direct UP signals; and the breakdown sums consistently with Score.
func TestColdStartContributions(t *testing.T) {
	m := newColdStart()

	// list + authfail: top two magnitudes are authfail:1 (-2.5) and unsub:1 (-1.6).
	// unsub now outweighs list (-1.4) — deliberate, per #157: List-Unsubscribe is
	// the more reliable bulk marker of the two, since transactional mail omits it
	// on purpose while plenty of bulk carries no List-Id at all.
	listAuth := featuresFromFacts(MessageFacts{
		IsListMail: true, HasListUnsubscribe: true, ToCcCount: 50, AuthFail: true,
	})
	top := TopContributions(m, listAuth, 5)
	if len(top) < 2 {
		t.Fatalf("expected at least 2 contributions, got %+v", top)
	}
	got := map[string]bool{top[0].Key: true, top[1].Key: true}
	if !got["authfail:1"] || !got["unsub:1"] {
		t.Errorf("top-2 negative reasons = %v, want authfail:1 & unsub:1", got)
	}
	for _, c := range top[:2] {
		if c.Weight >= 0 {
			t.Errorf("expected a DOWN reason, got %+v", c)
		}
	}
	assertSumConsistent(t, m, listAuth)

	// corresponded + direct: top two magnitudes are corr:1 (1.8) and direct:1 (0.9).
	knownDirect := featuresFromFacts(MessageFacts{
		HasCorresponded: true, OutboundCount: 8, ToCcCount: 1,
	})
	top2 := TopContributions(m, knownDirect, 5)
	if len(top2) < 2 {
		t.Fatalf("expected at least 2 contributions, got %+v", top2)
	}
	got2 := map[string]bool{top2[0].Key: true, top2[1].Key: true}
	if !got2["corr:1"] || !got2[coldStartDirectKey] {
		t.Errorf("top-2 positive reasons = %v, want corr:1 & %s", got2, coldStartDirectKey)
	}
	for _, c := range top2[:2] {
		if c.Weight <= 0 {
			t.Errorf("expected an UP reason, got %+v", c)
		}
	}
	assertSumConsistent(t, m, knownDirect)
}

// assertSumConsistent checks the cold-start contributions add up to Score's
// logit (minus the constant bias), i.e. sigmoid(bias + Σ) == Score.
func assertSumConsistent(t *testing.T, m coldStartModel, f FeatureVector) {
	t.Helper()
	var sum float64
	for _, w := range m.Contributions(f) {
		sum += w
	}
	if got := sigmoid(coldStartBias + sum); math.Abs(got-m.Score(f)) > 1e-12 {
		t.Errorf("cold-start contributions do not sum to Score: sigmoid(bias+Σ)=%v, Score=%v", got, m.Score(f))
	}
}

// TopContributions sorts by |weight| descending, honors the top-N cap, returns
// every contribution for n<=0, and breaks ties deterministically by key.
func TestTopContributionsOrderingAndTopN(t *testing.T) {
	m := NewLogistic("priority").(*logisticModel)
	m.weights = map[string]float64{"a": 3, "b": -5, "c": 1, "d": 2, "e": -4}
	f := FeatureVector{"a": 1, "b": 1, "c": 1, "d": 1, "e": 1}

	top := TopContributions(m, f, 3)
	want := []string{"b", "e", "a"} // |−5| > |−4| > |3|
	if len(top) != len(want) {
		t.Fatalf("top-3 length = %d, want %d (%+v)", len(top), len(want), top)
	}
	for i, k := range want {
		if top[i].Key != k {
			t.Errorf("top[%d] = %q, want %q", i, top[i].Key, k)
		}
	}

	if all := TopContributions(m, f, 0); len(all) != 5 {
		t.Errorf("n<=0 returned %d, want all 5", len(all))
	}

	// Deterministic tie-break by key when magnitudes are equal.
	m.weights = map[string]float64{"y": -2, "x": 2}
	tie := TopContributions(m, FeatureVector{"x": 1, "y": 1}, 2)
	if len(tie) != 2 || tie[0].Key != "x" || tie[1].Key != "y" {
		t.Errorf("tie-break order = %+v, want x then y", tie)
	}
}

// A feature-blind model (the baseline) is not an Explainer, so it has no
// contributions: TopContributions returns nil, which the render path treats as
// "no reasons" rather than a fabricated one.
func TestTopContributionsBaselineNil(t *testing.T) {
	if got := TopContributions(NewBaseline("priority"), FeatureVector{"corr:1": 1}, 5); got != nil {
		t.Errorf("baseline TopContributions = %+v, want nil", got)
	}
}

// FeatureLabel renders a representative key from each family, and falls back to
// the key itself for an unknown one.
func TestFeatureLabel(t *testing.T) {
	cases := map[string]string{
		"corr:1":               "You have written to this sender",
		"reply:1":              "A reply in a thread you're part of",
		"list:1":               "Mailing-list mail",
		"unsub:1":              "Bulk mail (has unsubscribe)",
		"authfail:1":           "Failed sender authentication",
		"flagged:1":            "Flagged by your mail server",
		"attach:1":             "Has an attachment",
		coldStartDirectKey:     "Addressed directly to you",
		"from:vip@example.com": "From vip@example.com",
		"dom:example.com":      "From the domain example.com",
		"alias:me@example.com": "Sent to me@example.com",
		"subtok:invoice":       "Subject mentions “invoice”",
		"sent:6-10":            "You've written to them before",
		"tocc:26+":             "Sent to many recipients",
		"hour:9":               "Received around 9:00",
	}
	for key, want := range cases {
		if got := FeatureLabel(key); got != want {
			t.Errorf("FeatureLabel(%q) = %q, want %q", key, got, want)
		}
	}
	if got := FeatureLabel("mystery:42"); got != "mystery:42" {
		t.Errorf("unknown key fallback = %q, want the key itself", got)
	}
}
