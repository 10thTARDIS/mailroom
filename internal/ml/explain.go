// SPDX-License-Identifier: AGPL-3.0-only

package ml

import (
	"math"
	"sort"
)

// This file is triage EXPLAINABILITY (spec §8, #34): the per-feature breakdown
// of a priority score, so a client can show WHY a message ranks where it does.
// It adds NO new score — it only decomposes the logit a Model already computes
// in Score — and it is a DISPLAY concern: the correction / model-control surface
// is #34 PR2, not here.

// FeatureContribution is one feature key's signed contribution to a score's
// logit: positive pushed the message TOWARD important, negative pushed it away.
// It is the stored, MACHINE-READABLE shape (keys, not labels) — the human phrase
// is applied at render time via FeatureLabel so stored data stays stable while
// wording is free to change.
type FeatureContribution struct {
	Key    string  `json:"key"`
	Weight float64 `json:"weight"`
	// Text is a LITERAL phrase that OVERRIDES FeatureLabel(Key) at render time.
	// When it is set the renderer must use it verbatim and must not consult Key.
	//
	// ⚠ IT EXISTS BECAUSE NOT EVERY EXPLANATION IS A MODEL FEATURE (#242). A
	// rule-sourced label suggestion explains itself with its own conditions —
	// "alias is sub@example.com", "has a List-Unsubscribe header" — rendered
	// by rules.DescribeCondition. Those are not feature keys and never will be:
	// the value in them is the user-supplied literal (which address, which
	// header), so there is no finite vocabulary FeatureLabel could hold, and
	// minting a key like `alias:sub@example.com` to look it up again would be
	// an encode/decode round trip whose only job is to arrive back where it
	// started.
	//
	// The alternative was a second reasons payload with its own column, its own
	// decoder and its own renderer. #176's migration is explicit that both "why?"
	// panels share ONE shape precisely so they cannot drift ("SHAPE IS
	// DELIBERATELY IDENTICAL to app.triage_scores.top_features"); a second shape
	// would undo that to carry a string.
	//
	// omitempty, so every payload written before this field existed — and every
	// model explanation written after it — is byte-identical to what it was. A
	// contribution with no Text renders exactly as before, through FeatureLabel.
	Text string `json:"text,omitempty"`
}

// Explainer is an OPTIONAL Model capability: a model that implements it can
// decompose a score into the signed per-feature contributions that produced it.
// A Model that does NOT implement it (the feature-blind baseline) has no
// explanation, and TopContributions returns nil for it — rendered as "no
// reasons", never a fabricated one.
//
// Contributions returns, per PRESENT feature key, that key's signed contribution
// to the score's LOGIT (the pre-sigmoid sum). By construction the returned values
// sum to Score's logit minus the model's constant bias, so a client reading the
// contributions sees exactly the terms that moved the score.
type Explainer interface {
	Contributions(f FeatureVector) map[string]float64
}

// TopContributions returns the n feature contributions with the largest
// magnitude (|weight|) from m's explanation of f, strongest first. It is nil
// when m is not an Explainer (e.g. the baseline), which the render path treats as
// "no explanation available".
//
// The sort is by |weight| descending with a deterministic key tie-break, so the
// output is STABLE for a given model+features — two messages with the same
// contributions always serialize identically, and a test can assert an exact
// order. n <= 0 returns every contribution (still sorted).
func TopContributions(m Model, f FeatureVector, n int) []FeatureContribution {
	ex, ok := m.(Explainer)
	if !ok {
		return nil
	}
	contribs := ex.Contributions(f)
	out := make([]FeatureContribution, 0, len(contribs))
	for k, w := range contribs {
		out = append(out, FeatureContribution{Key: k, Weight: w})
	}
	sort.Slice(out, func(i, j int) bool {
		ai, aj := math.Abs(out[i].Weight), math.Abs(out[j].Weight)
		if ai != aj {
			return ai > aj
		}
		return out[i].Key < out[j].Key
	})
	if n > 0 && len(out) > n {
		out = out[:n]
	}
	return out
}
