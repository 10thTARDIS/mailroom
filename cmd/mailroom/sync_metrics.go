// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"mailroom/internal/mailsync"
	"mailroom/internal/metrics"
)

// syncObserver turns the sync engine's correctness counters (#908, #909 and
// the CONDSTORE-less flag sweep, ADR-046 §12) into Prometheus counters. The
// labels are the engine's closed sets (paths and outcomes it names itself),
// never mailbox or account names.
type syncObserver struct{ m *metrics.Metrics }

var _ mailsync.Observer = syncObserver{}

func (o syncObserver) RecoveredArrival(path, outcome string, n int) {
	o.m.SyncRecoveredArrivalsTotal.WithLabelValues(path, outcome).Add(float64(n))
}

func (o syncObserver) FlagsAbsent(path string, n int) {
	o.m.FetchFlagsAbsentTotal.WithLabelValues(path).Add(float64(n))
}

func (o syncObserver) FlagRefreshAborted(path string, n int) {
	o.m.FlagRefreshAbortedTotal.WithLabelValues(path).Add(float64(n))
}

func (o syncObserver) FlagSweep(read, applied int) {
	o.m.FlagSweepMessagesTotal.WithLabelValues("read").Add(float64(read))
	o.m.FlagSweepMessagesTotal.WithLabelValues("applied").Add(float64(applied))
}

// newSyncObserver also creates every series at zero, so "nothing happened"
// reads as 0 on /metrics rather than as a missing series.
func newSyncObserver(m *metrics.Metrics) syncObserver {
	for _, p := range []string{"refresh", "gmail", "ingest_reread"} {
		m.FlagRefreshAbortedTotal.WithLabelValues(p)
	}
	for _, p := range []string{"ingest", "refresh", "gmail", "sweep", "ingest_reread"} {
		m.FetchFlagsAbsentTotal.WithLabelValues(p)
	}
	for _, p := range []string{"gap", "flag_fallback"} {
		for _, o := range []string{"notify", "muted", "blocked"} {
			m.SyncRecoveredArrivalsTotal.WithLabelValues(p, o)
		}
	}
	m.FlagSweepMessagesTotal.WithLabelValues("read")
	m.FlagSweepMessagesTotal.WithLabelValues("applied")
	return syncObserver{m: m}
}
