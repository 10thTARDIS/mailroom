// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"time"

	"mailroom/internal/metrics"
	"mailroom/internal/search"
)

// searchTimingObserver is the search.WithSearchTiming observer (#724): one
// mailroom_search_duration_seconds observation per index read, labelled with
// the read's kind. A nil m observes nothing (tests build the service without a
// registry). The label is search.TimingKind's closed set, never query text.
func searchTimingObserver(m *metrics.Metrics) func(search.TimingKind, time.Duration) {
	return func(kind search.TimingKind, d time.Duration) {
		if m == nil {
			return
		}
		m.SearchDuration.WithLabelValues(string(kind)).Observe(d.Seconds())
	}
}
