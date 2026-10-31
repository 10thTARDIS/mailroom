// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"mailroom/internal/api"
	"mailroom/internal/metrics"
)

// A flagged on-demand verdict moves mailroom_ondemand_scan_total{verdict="flagged"}
// by exactly 1 and no other series (#47).
func TestOnDemandScanObserverCountsUnderOnlyItsOwnLabel(t *testing.T) {
	for _, v := range api.OnDemandVerdicts() {
		m := metrics.New()
		onDemandScanObserver(m)(v)
		for _, other := range api.OnDemandVerdicts() {
			want := 0.0
			if other == v {
				want = 1
			}
			if got := testutil.ToFloat64(m.OnDemandScanTotal.WithLabelValues(other)); got != want {
				t.Errorf("after %s: %s = %v, want %v", v, other, got, want)
			}
		}
	}
	onDemandScanObserver(nil)("flagged") // a nil registry must not panic
}

// Every series exists at zero from boot, so "no scans yet" is not mistaken for
// "not wired".
func TestOnDemandScanObserverExportsEverySeriesAtZeroFromBoot(t *testing.T) {
	m := metrics.New()
	_ = onDemandScanObserver(m)
	if got, want := testutil.CollectAndCount(m.OnDemandScanTotal), len(api.OnDemandVerdicts()); got != want {
		t.Errorf("series at boot = %d, want %d", got, want)
	}
}
