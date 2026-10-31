// SPDX-License-Identifier: AGPL-3.0-only

package stats

import (
	"math"
	"testing"
)

// The values the two gates rest on. 19/20 is the case that makes learned
// placement's first correction in twenty trip its gate (internal/placement),
// and 5/5 is the case Wilson exists to get right.
func TestWilsonLowerBound(t *testing.T) {
	cases := []struct {
		succ, trials int
		want         float64
	}{
		{5, 5, 0.5655},
		{20, 20, 0.8389},
		{19, 20, 0.7639},
		{38, 40, 0.8350},
		{0, 10, 0},
	}
	for _, c := range cases {
		got := WilsonLowerBound(c.succ, c.trials, Z95)
		if math.Abs(got-c.want) > 0.0005 {
			t.Errorf("WilsonLowerBound(%d, %d) = %.4f, want %.4f", c.succ, c.trials, got, c.want)
		}
	}
	if !math.IsNaN(WilsonLowerBound(0, 0, Z95)) {
		t.Error("no trials must be NaN (no evidence), not a number that reads as a measurement")
	}
}
