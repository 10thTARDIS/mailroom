// SPDX-License-Identifier: AGPL-3.0-only

// Package stats holds the small statistical helpers that more than one
// package gates a decision on. It has no dependencies inside the module, so
// both internal/ml (which imports internal/store) and internal/store itself
// can use it without an import cycle.
package stats

import "math"

// Z95 is the two-sided 95% normal quantile. internal/ml's label promotion gate
// uses 1.96, the rounded form; learned placement (internal/placement) uses
// this exact value. The difference is in the fourth decimal of any bound.
const Z95 = 1.959963984540054

// WilsonLowerBound returns the lower end of the Wilson score interval for a
// binomial proportion: successes out of trials, at the given z.
//
// WHY WILSON AND NOT THE TEXTBOOK NORMAL INTERVAL. The normal approximation
// (p̂ ± z·√(p̂(1-p̂)/n)) is worst exactly where a gate decision lives: small n
// and proportions near 0 or 1. At 5 of 5 it reports an interval of zero
// width, which would let a gate pass on five observations; Wilson returns
// ~0.57 there, which is the honest reading of "five for five tells you
// little". It also never leaves [0,1].
//
// Returns NaN for trials <= 0: no evidence, which must not read as 0.0
// ("measured terrible") the way a zero would.
//
// Moved here from internal/ml (#714) so learned placement's correction gate
// and the label promotion gate are one calculation, not two copies that can
// drift.
func WilsonLowerBound(successes, trials int, z float64) float64 {
	if trials <= 0 {
		return math.NaN()
	}
	n := float64(trials)
	phat := float64(successes) / n
	z2 := z * z
	centre := phat + z2/(2*n)
	margin := z * math.Sqrt(phat*(1-phat)/n+z2/(4*n*n))
	lb := (centre - margin) / (1 + z2/n)
	return math.Max(0, lb)
}
