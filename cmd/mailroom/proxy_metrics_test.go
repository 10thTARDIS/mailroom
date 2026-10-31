// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"testing"

	"github.com/oklog/ulid/v2"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"mailroom/internal/api"
	"mailroom/internal/metrics"
	"mailroom/internal/store"
)

// TestProxyObserverCountsEachOutcomeUnderOnlyItsOwnLabel: one outcome moves its
// own series by exactly 1 and nothing else (#674).
func TestProxyObserverCountsEachOutcomeUnderOnlyItsOwnLabel(t *testing.T) {
	refusals := []string{"egress_refused", "scheme_refused", "upstream_status", "content_type",
		"too_large", "rate_limited", "resolve_not_allowed", "unauthorized"}
	results := []string{api.ProxyServed, api.ProxyCached}

	read := func(m *metrics.Metrics) map[string]float64 {
		got := map[string]float64{}
		for _, r := range refusals {
			got["refusal/"+r] = testutil.ToFloat64(m.ProxyRefusalsTotal.WithLabelValues(r))
		}
		for _, r := range results {
			got["fetch/"+r] = testutil.ToFloat64(m.ProxyFetchesTotal.WithLabelValues(r))
		}
		return got
	}

	cases := map[string]api.ProxyOutcome{}
	for _, r := range refusals {
		cases["refusal/"+r] = api.ProxyOutcome{Result: api.ProxyRefused, Reason: r}
	}
	for _, r := range results {
		cases["fetch/"+r] = api.ProxyOutcome{Result: r}
	}
	for want, outcome := range cases {
		m := metrics.New()
		proxyObserver(m)(outcome)
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
	proxyObserver(nil)(api.ProxyOutcome{Result: api.ProxyServed}) // a nil registry must not panic
}

// TestProxyObserverExportsEverySeriesAtZeroFromBoot: before any image is asked
// for, /metrics already carries every reason and both results at 0, so "no
// refusals yet" cannot be mistaken for "the proxy is not wired" (#674).
func TestProxyObserverExportsEverySeriesAtZeroFromBoot(t *testing.T) {
	m := metrics.New()
	_ = proxyObserver(m)
	if got, want := testutil.CollectAndCount(m.ProxyRefusalsTotal), len(api.ProxyRefusalReasons()); got != want {
		t.Errorf("refusal series at boot = %d, want %d (one per reason)", got, want)
	}
	if got := testutil.CollectAndCount(m.ProxyFetchesTotal); got != 2 {
		t.Errorf("fetch series at boot = %d, want 2 (served, cached)", got)
	}
	if v := testutil.ToFloat64(m.ProxyRefusalsTotal.WithLabelValues("egress_refused")); v != 0 {
		t.Errorf("egress_refused at boot = %v, want 0", v)
	}
}

// TestProxyObserverCountsPrefetchApart: an arrival-prefetch outcome (#922)
// moves only mailroom_proxy_prefetch_total, never the request-level counters,
// so the refusal RATE stays a statement about reads.
func TestProxyObserverCountsPrefetchApart(t *testing.T) {
	cases := map[string]api.ProxyOutcome{
		prefetchFetched:       {Result: api.ProxyServed, Prefetch: true},
		prefetchAlreadyCached: {Result: api.ProxyCached, Prefetch: true},
		prefetchRefused:       {Result: api.ProxyRefused, Reason: "upstream_status", Prefetch: true, Host: "cdn.example"},
		prefetchEmpty:         {Result: api.ProxyEmpty, Prefetch: true},
		prefetchCapReached:    {Result: api.ProxyCapReached, Prefetch: true},
	}
	for want, o := range cases {
		m := metrics.New()
		obs := proxyObserver(m)
		obs(o)
		for _, r := range prefetchResults {
			exp := 0.0
			if r == want {
				exp = 1
			}
			if v := testutil.ToFloat64(m.ProxyPrefetchTotal.WithLabelValues(r)); v != exp {
				t.Errorf("after %s: prefetch %s = %v, want %v", want, r, v, exp)
			}
		}
		if v := testutil.ToFloat64(m.ProxyRefusalsTotal.WithLabelValues("upstream_status")) +
			testutil.ToFloat64(m.ProxyFetchesTotal.WithLabelValues(api.ProxyServed)) +
			testutil.ToFloat64(m.ProxyFetchesTotal.WithLabelValues(api.ProxyCached)); v != 0 {
			t.Errorf("a prefetch outcome (%s) moved a request-level counter by %v", want, v)
		}
	}
	m := metrics.New()
	_ = proxyObserver(m)
	if got := testutil.CollectAndCount(m.ProxyPrefetchTotal); got != len(prefetchResults) {
		t.Errorf("prefetch series at boot = %d, want %d", got, len(prefetchResults))
	}
}

// TestProxyCacheObserverSetsBothKinds: the gauges carry the referenced and
// unreferenced halves separately, because only the second is bounded.
func TestProxyCacheObserverSetsBothKinds(t *testing.T) {
	m := metrics.New()
	user := ulid.MustParse("01ARZ3NDEKTSV4RRFFQ69G5FAV")
	proxyCacheObserver(m)(user, store.ProxyCacheSplit{
		ReferencedEntries: 3, ReferencedBytes: 3000, UnreferencedEntries: 2, UnreferencedBytes: 200})
	for _, c := range []struct {
		g    float64
		want float64
		name string
	}{
		{testutil.ToFloat64(m.ProxyCacheBytes.WithLabelValues(user.String(), "referenced")), 3000, "bytes/referenced"},
		{testutil.ToFloat64(m.ProxyCacheBytes.WithLabelValues(user.String(), "unreferenced")), 200, "bytes/unreferenced"},
		{testutil.ToFloat64(m.ProxyCacheEntries.WithLabelValues(user.String(), "referenced")), 3, "entries/referenced"},
		{testutil.ToFloat64(m.ProxyCacheEntries.WithLabelValues(user.String(), "unreferenced")), 2, "entries/unreferenced"},
	} {
		if c.g != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.g, c.want)
		}
	}
	proxyCacheObserver(nil)(user, store.ProxyCacheSplit{}) // a nil registry must not panic
}
