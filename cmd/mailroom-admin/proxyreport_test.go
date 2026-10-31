// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/api"
	"mailroom/internal/egress"
	"mailroom/internal/mime"
)

// TestSchemeLabelKeepsProtocolRelativeSeparate is the row ADR-026 decision A
// turns on. A protocol-relative URL inherits the page's scheme, so on an
// https-served client it is already https and a proxy buys it nothing. Folding
// it into the http row would inflate "images only the proxy can render" with
// images that already render.
func TestSchemeLabelKeepsProtocolRelativeSeparate(t *testing.T) {
	https := schemeLabel("https")
	http := schemeLabel("http")
	rel := schemeLabel("")
	if https == http || http == rel || https == rel {
		t.Fatalf("labels collide: https=%q http=%q protocol-relative=%q", https, http, rel)
	}
	if !strings.Contains(http, "mixed content") {
		t.Errorf("the http label should say why it matters, got %q", http)
	}
	if strings.Contains(rel, "http (") {
		t.Errorf("protocol-relative must not read as http, got %q", rel)
	}
	if got := schemeLabel("ftp"); !strings.Contains(got, "ftp") {
		t.Errorf("an unexpected scheme must stay visible, got %q", got)
	}
}

// TestAddImageCountsSchemesAndParams pins the aggregation, including the part
// that is a privacy rule rather than a counting rule.
func TestAddImageCountsSchemesAndParams(t *testing.T) {
	a := newProxyAggregate()
	id := ulid.MustParse("01ARZ3NDEKTSV4RRFFQ69G5FAV")

	a.addImage(id, api.RemoteImage{
		Host: "cdn.example", Scheme: "https",
		URL: "https://cdn.example/a.png?utm_source=news&sig=SECRETTOKEN",
	})
	a.addImage(id, api.RemoteImage{
		Host: "old.example", Scheme: "http",
		URL: "http://old.example/b.gif",
	})
	a.addImage(id, api.RemoteImage{
		Host: "rel.example", Scheme: "",
		URL: "//rel.example/c.jpg",
	})

	if got := a.bySchemeImgs[schemeLabel("http")]; got != 1 {
		t.Errorf("http images = %d, want 1", got)
	}
	if got := a.bySchemeImgs[schemeLabel("")]; got != 1 {
		t.Errorf("protocol-relative images = %d, want 1", got)
	}
	if a.proxied != 3 {
		t.Errorf("proxied = %d, want 3", a.proxied)
	}
	if len(a.hostImgs) != 3 {
		t.Errorf("hosts = %d, want 3", len(a.hostImgs))
	}
	if a.imgsAnyTP != 1 {
		t.Errorf("images with a known tracking param = %d, want 1", a.imgsAnyTP)
	}
	if a.imgsAnyParam != 1 {
		t.Errorf("images with any param = %d, want 1", a.imgsAnyParam)
	}
	if a.paramImgs["utm_source"] != 1 || a.paramImgs["sig"] != 1 {
		t.Errorf("parameter names not counted: %v", a.paramImgs)
	}
}

// TestAggregateNeverHoldsAQueryValue is the privacy rule, asserted rather than
// commented.
//
// ⚠ The token below is the shape of a real recipient identifier. Parameter
// NAMES are safe to print; a VALUE is the thing that identifies the reader, so
// it must never reach a map that gets printed. The one place a full URL is
// allowed to live is fetchURLs, which exists only to be fetched.
func TestAggregateNeverHoldsAQueryValue(t *testing.T) {
	const secret = "RECIPIENT-TOKEN-9d4f2a"
	a := newProxyAggregate()
	a.addImage(ulid.MustParse("01ARZ3NDEKTSV4RRFFQ69G5FAV"), api.RemoteImage{
		Host: "t.example", Scheme: "https",
		URL: "https://t.example/open.png?uid=" + secret + "&utm_source=n",
	})

	for k := range a.paramImgs {
		if strings.Contains(k, secret) {
			t.Errorf("a query VALUE reached paramImgs as key %q", k)
		}
	}
	for k := range a.hostImgs {
		if strings.Contains(k, secret) {
			t.Errorf("a query VALUE reached hostImgs as key %q", k)
		}
	}
	for _, label := range []string{schemeLabel("https"), schemeLabel("http"), schemeLabel("")} {
		for h := range a.bySchemeHosts[label] {
			if strings.Contains(h, secret) {
				t.Errorf("a query VALUE reached bySchemeHosts as %q", h)
			}
		}
	}
	// The name beside it must still have been counted, or this test would pass
	// on an aggregate that recorded nothing at all.
	if a.paramImgs["uid"] != 1 {
		t.Fatalf("the parameter NAME was not counted (%v) — this test would then "+
			"pass for an aggregate that simply dropped the URL", a.paramImgs)
	}
}

// TestClassifyFetchErrSeparatesRefusalsFromFailures keeps the guard's own
// effect visible. Folding "the guard refused this" into "network error" would
// make the most interesting rows in the fetch table disappear into noise —
// and would report a refusal as an ordinary failure, which is the shape
// ADR-026 §3a forbids.
func TestClassifyFetchErrSeparatesRefusalsFromFailures(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{fmt.Errorf("get: %w", egress.ErrBlockedAddress), "REFUSED by the guard: non-public address"},
		{fmt.Errorf("get: %w", egress.ErrSchemeNotPermitted), "REFUSED by the guard: scheme"},
		{fmt.Errorf("get: %w", egress.ErrTooManyRedirects), "REFUSED by the guard: redirects"},
		{fmt.Errorf("get: %w", egress.ErrTooLarge), "REFUSED by the guard: over the byte cap"},
		{errors.New("dial tcp: i/o timeout"), "network or server error"},
	}
	for _, tc := range cases {
		if got := classifyFetchErr(tc.err); got != tc.want {
			t.Errorf("classifyFetchErr(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
	// Every guard refusal must be distinguishable from every other.
	seen := map[string]bool{}
	for _, tc := range cases {
		if seen[tc.want] {
			t.Errorf("two outcomes share the label %q", tc.want)
		}
		seen[tc.want] = true
	}
}

func TestPercentileAndBytes(t *testing.T) {
	s := []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	if got := pct(s, 50); got != 5 {
		t.Errorf("p50 = %d, want 5", got)
	}
	if got := pct(s, 100); got != 10 {
		t.Errorf("p100 = %d, want 10", got)
	}
	if got := pct(nil, 50); got != 0 {
		t.Errorf("p50 of nothing = %d, want 0", got)
	}
	for _, tc := range []struct {
		n    int64
		want string
	}{{512, "512 B"}, {2048, "2.0 KiB"}, {3 << 20, "3.0 MiB"}} {
		if got := humanBytes(tc.n); got != tc.want {
			t.Errorf("humanBytes(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

func TestSortedByCountIsStable(t *testing.T) {
	m := map[string]int{"b": 2, "a": 2, "c": 5}
	got := sortedByCount(m)
	want := []string{"c", "a", "b"} // count desc, then name asc
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sortedByCount = %v, want %v — a run must be comparable to the last", got, want)
		}
	}
}

// TestAddPrefetchCountsWhatTheJobWouldWalk: the prefetch section (#922) counts
// the job's own list — distinct images per message, beacons never included —
// so its "would be fetched" is a claim about the code that will run.
func TestAddPrefetchCountsWhatTheJobWouldWalk(t *testing.T) {
	a := newProxyAggregate()
	html := `<img src="https://cdn.example/a.png?sig=SECRETTOKEN">` +
		`<img src="https://cdn.example/a.png?sig=SECRETTOKEN">` +
		`<img src="https://cdn.example/b.png">` +
		`<img src="https://pixel.example/o.gif" width="1" height="1">`
	msg, err := mime.ParseBytes([]byte("From: x@news.example\r\nContent-Type: text/html\r\n\r\n" + html))
	if err != nil {
		t.Fatal(err)
	}
	a.addPrefetch(msg)
	a.addPrefetch(msg) // a second message showing the same two images

	if a.pfImages != 4 {
		t.Errorf("images = %d, want 4 (two distinct per message, twice)", a.pfImages)
	}
	if len(a.pfDistinct) != 2 {
		t.Errorf("distinct images = %d, want 2 — the beacon must not be counted", len(a.pfDistinct))
	}
	if a.pfMsgsWithImages != 2 || a.pfMsgsCapped != 0 {
		t.Errorf("messages with images = %d, capped = %d; want 2, 0", a.pfMsgsWithImages, a.pfMsgsCapped)
	}
	for k := range a.pfDistinct {
		if strings.Contains(k, "SECRETTOKEN") {
			t.Error("the aggregate keyed an image by its URL rather than its hash")
		}
	}
}
