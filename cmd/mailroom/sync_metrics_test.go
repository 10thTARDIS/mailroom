// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"mailroom/internal/metrics"
)

func TestSyncObserverFeedsTheCounters(t *testing.T) {
	m := metrics.New()
	o := newSyncObserver(m)
	if n := testutil.CollectAndCount(m.SyncRecoveredArrivalsTotal); n != 6 {
		t.Errorf("recovered-arrival series pre-created = %d, want 6", n)
	}
	o.RecoveredArrival("gap", "notify", 2)
	o.FlagsAbsent("refresh", 1)
	o.FlagRefreshAborted("refresh", 3)
	o.FlagSweep(10, 4)
	for name, got := range map[string]float64{
		"recovered": testutil.ToFloat64(m.SyncRecoveredArrivalsTotal.WithLabelValues("gap", "notify")),
		"absent":    testutil.ToFloat64(m.FetchFlagsAbsentTotal.WithLabelValues("refresh")),
		"aborted":   testutil.ToFloat64(m.FlagRefreshAbortedTotal.WithLabelValues("refresh")),
		"read":      testutil.ToFloat64(m.FlagSweepMessagesTotal.WithLabelValues("read")),
		"applied":   testutil.ToFloat64(m.FlagSweepMessagesTotal.WithLabelValues("applied")),
	} {
		want := map[string]float64{"recovered": 2, "absent": 1, "aborted": 3, "read": 10, "applied": 4}[name]
		if got != want {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
}
