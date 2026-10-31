// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"github.com/oklog/ulid/v2"

	"mailroom/internal/api"
	"mailroom/internal/ingest"
	"mailroom/internal/metrics"
	"mailroom/internal/store"
)

// proxyObserver is the api.WithProxyObserver sink (#674): it counts each image
// proxy request as an image delivered (served or cached) or refused (by reason).
// A nil m counts nothing. The outcome carries only closed labels, so no URL,
// host or message id can reach a metric label.
//
// Every label is created at zero here, at boot: a labelled counter is otherwise
// absent until its first increment, and an absent series cannot be told from a
// proxy that was never wired.
func proxyObserver(m *metrics.Metrics) func(api.ProxyOutcome) {
	if m != nil {
		for _, reason := range api.ProxyRefusalReasons() {
			m.ProxyRefusalsTotal.WithLabelValues(reason)
		}
		m.ProxyFetchesTotal.WithLabelValues(api.ProxyServed)
		m.ProxyFetchesTotal.WithLabelValues(api.ProxyCached)
		for _, r := range prefetchResults {
			m.ProxyPrefetchTotal.WithLabelValues(r)
		}
	}
	return func(o api.ProxyOutcome) {
		if m == nil {
			return
		}
		// The arrival prefetch (#922) is counted apart, so the request-level
		// counters below stay a statement about reads.
		if o.Prefetch {
			switch o.Result {
			case api.ProxyServed:
				m.ProxyPrefetchTotal.WithLabelValues(prefetchFetched).Inc()
			case api.ProxyCached:
				m.ProxyPrefetchTotal.WithLabelValues(prefetchAlreadyCached).Inc()
			case api.ProxyRefused:
				m.ProxyPrefetchTotal.WithLabelValues(prefetchRefused).Inc()
			case api.ProxyEmpty:
				m.ProxyPrefetchTotal.WithLabelValues(prefetchEmpty).Inc()
			case api.ProxyCapReached:
				m.ProxyPrefetchTotal.WithLabelValues(prefetchCapReached).Inc()
			}
			return
		}
		switch o.Result {
		case api.ProxyServed, api.ProxyCached:
			m.ProxyFetchesTotal.WithLabelValues(o.Result).Inc()
		case api.ProxyRefused:
			m.ProxyRefusalsTotal.WithLabelValues(o.Reason).Inc()
		}
	}
}

// The arrival prefetch's result labels (#922): a closed set.
//
// fetched, already_cached, refused and empty count IMAGES; cap_reached counts
// prefetch RUNS that fetched nothing because the user's kept images reached
// MAILROOM_IMAGE_PROXY_PREFETCH_MAX_BYTES.
const (
	prefetchFetched       = "fetched"
	prefetchAlreadyCached = "already_cached"
	prefetchRefused       = "refused"
	prefetchEmpty         = "empty"
	prefetchCapReached    = "cap_reached"
)

var prefetchResults = []string{prefetchFetched, prefetchAlreadyCached, prefetchRefused, prefetchEmpty, prefetchCapReached}

// proxyCacheObserver is the api.WithProxyCacheObserver sink (#922): it sets one
// user's cache gauges, split by whether a live message references each entry.
// A nil m sets nothing. The user label is the user's ULID, as for
// SyncDisabledAccounts; the kind label is a closed pair.
func proxyCacheObserver(m *metrics.Metrics) func(ulid.ULID, store.ProxyCacheSplit) {
	return func(userID ulid.ULID, s store.ProxyCacheSplit) {
		if m == nil {
			return
		}
		u := userID.String()
		m.ProxyCacheBytes.WithLabelValues(u, "referenced").Set(float64(s.ReferencedBytes))
		m.ProxyCacheBytes.WithLabelValues(u, "unreferenced").Set(float64(s.UnreferencedBytes))
		m.ProxyCacheEntries.WithLabelValues(u, "referenced").Set(float64(s.ReferencedEntries))
		m.ProxyCacheEntries.WithLabelValues(u, "unreferenced").Set(float64(s.UnreferencedEntries))
	}
}

// localDKIMObserver is the ingest.WithLocalDKIM sink (#776): it counts each
// newly ingested message that reached the local DKIM check by its closed result
// label. A nil m counts nothing. Every label is created at zero at boot, for
// the reason proxyObserver gives.
func localDKIMObserver(m *metrics.Metrics) func(result string) {
	if m != nil {
		for _, r := range ingest.LocalDKIMResults() {
			m.LocalDKIMTotal.WithLabelValues(r)
		}
	}
	return func(result string) {
		if m != nil {
			m.LocalDKIMTotal.WithLabelValues(result).Inc()
		}
	}
}

// learnedPlacementObserver is the ingest.WithLearnedPlacement sink (#840): it
// counts each learned-placement arrival decision by its reason. The label set
// is closed (store.LearnedPlacementLabels); a value outside it is counted as
// "other" so a future reason can never mint a new series. A nil m counts
// nothing. Every label is created at zero at boot, for the reason
// proxyObserver gives.
func learnedPlacementObserver(m *metrics.Metrics) func(reason string) {
	known := map[string]bool{}
	for _, r := range store.LearnedPlacementLabels() {
		known[r] = true
		if m != nil {
			m.LearnedPlacementTotal.WithLabelValues(r)
		}
	}
	return func(reason string) {
		if m == nil {
			return
		}
		if !known[reason] {
			reason = store.LearnedLabelOther
		}
		m.LearnedPlacementTotal.WithLabelValues(reason).Inc()
	}
}

// onDemandScanObserver is the api.WithOnDemandObserver sink (#47): it counts each
// attachment of a decrypted message judged at download time, by its closed
// verdict label. A nil m counts nothing. Every label is created at zero at boot,
// for the reason proxyObserver gives.
func onDemandScanObserver(m *metrics.Metrics) func(verdict string) {
	if m != nil {
		for _, v := range api.OnDemandVerdicts() {
			m.OnDemandScanTotal.WithLabelValues(v)
		}
	}
	return func(verdict string) {
		if m != nil {
			m.OnDemandScanTotal.WithLabelValues(verdict).Inc()
		}
	}
}
