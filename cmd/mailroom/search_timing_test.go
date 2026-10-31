// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"testing"

	"mailroom/internal/metrics"
	"mailroom/internal/search"
)

// The wiring wiredBy names for SearchDuration: each kind lands in its own
// series, with its own count, and there are exactly the two kinds.
func TestSearchTimingObserverFeedsTheHistogram(t *testing.T) {
	m := metrics.New()
	obs := searchTimingObserver(m)
	obs(search.TimingSearch, ms(12))
	obs(search.TimingSearch, ms(30))
	obs(search.TimingSnippet, ms(4))

	families, err := m.Registry().Gather()
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]uint64{}
	sums := map[string]float64{}
	for _, f := range families {
		if f.GetName() != "mailroom_search_duration_seconds" {
			continue
		}
		for _, metric := range f.GetMetric() {
			kind := ""
			for _, l := range metric.GetLabel() {
				if l.GetName() == "kind" {
					kind = l.GetValue()
				} else {
					t.Errorf("unexpected label %q: the only label is kind", l.GetName())
				}
			}
			counts[kind] = metric.GetHistogram().GetSampleCount()
			sums[kind] = metric.GetHistogram().GetSampleSum()
		}
	}
	if len(counts) != 2 || counts["search"] != 2 || counts["snippet"] != 1 {
		t.Fatalf("counts = %v, want search=2 snippet=1", counts)
	}
	if s := sums["search"]; s < 0.0419 || s > 0.0421 {
		t.Errorf("search sum = %v s, want 0.042 (seconds, not milliseconds)", s)
	}

	// A nil registry is a no-op, not a panic.
	searchTimingObserver(nil)(search.TimingSearch, ms(1))
}
