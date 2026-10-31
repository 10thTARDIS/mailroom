// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"mailroom/internal/metrics"
	"mailroom/internal/store"
)

// Each reason moves only its own series; a reason outside the closed set is
// counted as "other" and mints no series of its own (#840).
func TestLearnedPlacementObserverCountsUnderOnlyItsOwnLabel(t *testing.T) {
	read := func(m *metrics.Metrics) map[string]float64 {
		got := map[string]float64{}
		for _, r := range store.LearnedPlacementLabels() {
			got[r] = testutil.ToFloat64(m.LearnedPlacementTotal.WithLabelValues(r))
		}
		return got
	}
	for _, want := range store.LearnedPlacementLabels() {
		m := metrics.New()
		learnedPlacementObserver(m)(want)
		for series, v := range read(m) {
			exp := 0.0
			if series == want {
				exp = 1
			}
			if v != exp {
				t.Errorf("after %s: %s = %v, want %v", want, series, v, exp)
			}
		}
	}

	m := metrics.New()
	obs := learnedPlacementObserver(m)
	before := testutil.CollectAndCount(m.LearnedPlacementTotal)
	obs("a reason nobody declared")
	if got := testutil.ToFloat64(m.LearnedPlacementTotal.WithLabelValues(store.LearnedLabelOther)); got != 1 {
		t.Errorf("unknown reason counted under other = %v, want 1", got)
	}
	if after := testutil.CollectAndCount(m.LearnedPlacementTotal); after != before {
		t.Errorf("series went %d -> %d for an unknown reason; the label set must stay closed", before, after)
	}
	learnedPlacementObserver(nil)("placed") // a nil registry must not panic
}

// Every series is exported at zero from boot, so "nothing was held back" cannot
// be mistaken for "the counter is not wired".
func TestLearnedPlacementObserverExportsEverySeriesAtZeroFromBoot(t *testing.T) {
	m := metrics.New()
	_ = learnedPlacementObserver(m)
	if got, want := testutil.CollectAndCount(m.LearnedPlacementTotal), len(store.LearnedPlacementLabels()); got != want {
		t.Errorf("series at boot = %d, want %d", got, want)
	}
}
