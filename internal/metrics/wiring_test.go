// SPDX-License-Identifier: AGPL-3.0-only

package metrics

import (
	"reflect"
	"testing"
)

// wiredBy names, for every metric this package declares, the code that SETS it.
// It exists because the failure this guards against is not a crash — it is a
// metric that is declared, registered, served, and never written, so /metrics
// emits a constant 0 that reads as "healthy" (#524). Eight of ten were in that
// state for most of this file's life.
//
// ⚠ WHAT THIS TEST DOES AND DOES NOT PROVE. It proves that every field of
// Metrics has an entry here, so adding a metric without saying where it is set
// is a red build. It does NOT verify the call site still exists — that is prose,
// checked by review. A machine check was considered and rejected: grepping the
// tree for a field name is exactly how ModelVersion was twice counted as
// "wired", when all four of its hits were a struct field of the same name in an
// unrelated package.
//
// The per-metric behavioural tests are where the wiring is actually proven:
// cmd/mailroom's dispatcher_metrics_test.go for the job counters, and
// internal/jobs' dispatcher_test.go for depth-not-batch.
var wiredBy = map[string]string{
	"SyncLagSeconds":         "cmd/mailroom, via syncrunner.WithSyncLagSink — sampled at pass START, where the lag is non-zero",
	"IdleConnState":          "cmd/mailroom, via syncrunner.WithIdleStateSink",
	"ActionQueueDepth":       "cmd/mailroom's writeback drain, via store.CountOutstandingIntents",
	"WritebackFailuresTotal": "cmd/mailroom's writeback drain, from writeback.Result.Retried + DeadLettered",
	"IndexCursorLagSeconds":  "cmd/mailroom's indexLagTracker, via search.WithSyncSink + store.OldestUnconsumedEmailChangeAt",
	"JobQueueDepth":          "cmd/mailroom's dispatcherMetrics.ObserveQueueDepth, via app.count_due_jobs",
	"JobDeadLettersTotal":    "cmd/mailroom's dispatcherMetrics.DeadLettered",
	"OutboundLedgerLevel":    "cmd/mailroom's writeback drain, via store.OutboundSentToday",
	"SyncDisabledAccounts":   "cmd/mailroom, via syncrunner.WithDisabledAccountsSink",
	"ScannerSignaturePublishedTimestamp": "internal/rescan.Worker.Handle, every time it reads the scanner's " +
		"VERSION (per-user re-scan pass: immediately at boot, then daily when idle or every 30s while " +
		"a backlog drains)",
	"ScannerSignatureVersion":    "internal/rescan.Worker.Handle, same call site as ScannerSignaturePublishedTimestamp",
	"JMAPMethodDuration":         "cmd/mailroom's jmapTimingObserver, via api.WithRequestTiming — one observation per method call",
	"JMAPRequestDuration":        "cmd/mailroom's jmapTimingObserver, via api.WithRequestTiming — one observation per request",
	"ProxyRefusalsTotal":         "cmd/mailroom's proxyObserver, via api.WithProxyObserver — one increment per refused proxy request",
	"ProxyFetchesTotal":          "cmd/mailroom's proxyObserver, via api.WithProxyObserver — one increment per served or cached image",
	"ProxyPrefetchTotal":         "cmd/mailroom's proxyObserver, via api.WithProxyObserver — one increment per image the arrival prefetch handled (ProxyOutcome.Prefetch)",
	"ProxyCacheBytes":            "cmd/mailroom's proxyCacheObserver, via api.WithProxyCacheObserver — after each eviction pass, each prefetch run, and once per user at start",
	"ProxyCacheEntries":          "cmd/mailroom's proxyCacheObserver, same call sites as ProxyCacheBytes",
	"LocalDKIMTotal":             "cmd/mailroom's localDKIMObserver, via ingest.WithLocalDKIM — one increment per newly ingested message that reached the check",
	"OnDemandScanTotal":          "cmd/mailroom's onDemandScanObserver, via api.WithOnDemandObserver — one increment per judged attachment of a decrypted message",
	"SearchDuration":             "cmd/mailroom's searchTimingObserver, via search.WithSearchTiming — one observation per index read",
	"LearnedPlacementTotal":      "cmd/mailroom's learnedPlacementObserver, via ingest.WithLearnedPlacement — one increment per learned-placement arrival decision",
	"FlagRefreshAbortedTotal":    "cmd/mailroom's syncObserver, via mailsync.WithObserver (Observer.FlagRefreshAborted)",
	"FetchFlagsAbsentTotal":      "cmd/mailroom's syncObserver, via mailsync.WithObserver (Observer.FlagsAbsent)",
	"SyncRecoveredArrivalsTotal": "cmd/mailroom's syncObserver, via mailsync.WithObserver (Observer.RecoveredArrival)",
	"FlagSweepMessagesTotal":     "cmd/mailroom's syncObserver, via mailsync.WithObserver (Observer.FlagSweep)",
}

func TestEveryDeclaredMetricIsSetSomewhere(t *testing.T) {
	typ := reflect.TypeOf(Metrics{})

	declared := make(map[string]bool)
	for i := range typ.NumField() {
		f := typ.Field(i)
		if f.Name == "registry" {
			continue // not a metric
		}
		declared[f.Name] = true
		if _, ok := wiredBy[f.Name]; !ok {
			t.Errorf("metric %s is declared but has no entry in wiredBy. A metric nobody "+
				"sets serves a constant 0, which reads as healthy rather than unwired — "+
				"either wire it and name the call site here, or delete it. Feeding it a "+
				"plausible-looking number from somewhere else is the worst of the three.",
				f.Name)
		}
	}

	// The other direction: a stale entry means a metric was deleted and its note
	// left behind, which would make the list lie about what is served.
	for name := range wiredBy {
		if !declared[name] {
			t.Errorf("wiredBy names %s, which Metrics no longer declares — remove the entry", name)
		}
	}

	// ⚠ A COUNT ASSERTION, so this cannot pass vacuously. Reflection over a
	// renamed or emptied struct would otherwise iterate nothing and report
	// success, which is the failure mode this whole issue is about.
	if len(declared) != 26 {
		t.Errorf("Metrics declares %d metrics, want 26. If one was added or removed "+
			"deliberately, bump this number in the same commit", len(declared))
	}
}
