// SPDX-License-Identifier: AGPL-3.0-only

package keypub

// Rate limiting on the anonymous surface (#264).
//
// The load-bearing test here is TestDefaultServiceThrottles. Everything else
// checks the limiter behaves; that one checks it EXISTS without anyone asking,
// which is the failure this package has actually suffered — key publication
// shipped with no production caller and every test green (#48). A limiter that
// only works when a caller remembers to configure it is the same defect wearing
// a different hat.

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fixedClock lets a test spend the budget and then advance time deterministically
// instead of sleeping.
type fixedClock struct{ t time.Time }

func (c *fixedClock) now() time.Time      { return c.t }
func (c *fixedClock) add(d time.Duration) { c.t = c.t.Add(d) }

func okLookup(t *testing.T) *fakeLookup {
	return &fakeLookup{t: t, found: true, key: []byte{0x99, 0x01}, fingerprint: "FPR"}
}

// TestDefaultServiceThrottles: a Service built the ordinary way must limit.
//
// ⚠ THIS IS THE ONE THAT MATTERS. If the limiter defaulted to off, every other
// test in this file would still pass by explicitly enabling it, and production
// would run unbounded — the exact shape of #48, where publication was built,
// tested and inert because nothing switched it on.
func TestDefaultServiceThrottles(t *testing.T) {
	svc, err := New(okLookup(t), "example.org", testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if svc.limit == nil {
		t.Fatal("a default-constructed Service has NO rate limiter — the anonymous surface " +
			"would be unbounded in production")
	}

	h := svc.HUHandler()
	hash := strings.Repeat("y", hashLen)

	// Spend the whole default burst, then one more.
	var lastCode int
	for i := 0; i < DefaultRateBurst+1; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/.well-known/openpgpkey/hu/"+hash, nil))
		lastCode = rec.Code
	}
	if lastCode != http.StatusTooManyRequests {
		t.Errorf("after %d requests the default service answered %d, want 429 — it is not limiting",
			DefaultRateBurst+1, lastCode)
	}
}

// TestThrottleRefillsOverTime pins that the bucket is a bucket and not a
// permanent ban: a throttled surface must recover, or one burst locks every
// correspondent out until restart.
func TestThrottleRefillsOverTime(t *testing.T) {
	clock := &fixedClock{t: time.Now()}
	svc, err := New(okLookup(t), "example.org", testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	svc.WithRateLimit(RateLimitConfig{PerSecond: 1, Burst: 2}, clock.now)

	h := svc.HUHandler()
	hash := strings.Repeat("y", hashLen)
	get := func() int {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/.well-known/openpgpkey/hu/"+hash, nil))
		return rec.Code
	}

	if got := get(); got == http.StatusTooManyRequests {
		t.Fatalf("first request throttled (%d); the burst should cover it", got)
	}
	get() // spend the second token
	if got := get(); got != http.StatusTooManyRequests {
		t.Fatalf("third request = %d, want 429 with a burst of 2", got)
	}

	clock.add(2 * time.Second) // 1/s refill
	if got := get(); got == http.StatusTooManyRequests {
		t.Error("still throttled after the bucket should have refilled — a burst would lock " +
			"out every correspondent permanently")
	}
}

// TestThrottleSendsRetryAfter: a 429 without Retry-After tells a well-behaved
// client nothing about when to come back, so it either gives up or hammers.
func TestThrottleSendsRetryAfter(t *testing.T) {
	svc, err := New(okLookup(t), "example.org", testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	svc.WithRateLimit(RateLimitConfig{PerSecond: 1, Burst: 1}, nil)

	h := svc.HUHandler()
	hash := strings.Repeat("y", hashLen)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/.well-known/openpgpkey/hu/"+hash, nil))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/.well-known/openpgpkey/hu/"+hash, nil))

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("code = %d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("a 429 with no Retry-After leaves a client guessing")
	}
}

// TestMalformedPathsDoNotSpendTheBudget is the design decision made executable:
// garbage traffic must not be able to starve real lookups. Malformed paths never
// reach the database, so they are free here and bounded upstream.
func TestMalformedPathsDoNotSpendTheBudget(t *testing.T) {
	// failIfCalled proves these never reach the store, which is what makes them
	// safe to leave unbudgeted.
	svc, err := New(&fakeLookup{t: t, failIfCalled: true}, "example.org", testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	svc.WithRateLimit(RateLimitConfig{PerSecond: 1, Burst: 2}, nil)

	h := svc.HUHandler()
	for i := 0; i < 50; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
			fmt.Sprintf("/.well-known/openpgpkey/hu/not-a-valid-hash-%d", i), nil))
		if rec.Code == http.StatusTooManyRequests {
			t.Fatalf("a malformed path was throttled at request %d — garbage traffic can "+
				"starve real lookups", i)
		}
	}

	// And the budget is intact for a real one: this is the positive control,
	// without which the loop above would pass against a limiter that was simply
	// disabled.
	good := okLookup(t)
	svc2, err := New(good, "example.org", testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	svc2.WithRateLimit(RateLimitConfig{PerSecond: 1, Burst: 2}, nil)
	h2 := svc2.HUHandler()
	rec := httptest.NewRecorder()
	h2.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/.well-known/openpgpkey/hu/"+strings.Repeat("y", hashLen), nil))
	if rec.Code == http.StatusTooManyRequests {
		t.Error("a well-formed lookup was throttled with a full budget")
	}
}

// TestThrottleCannotEnumerate: the 429 must be identical for a hash that exists
// and one that does not, or the limiter becomes the address oracle the rest of
// this package is built to prevent.
//
// The limiter runs BEFORE the lookup, so this holds by construction — and this
// test is what stops someone "optimising" it to after.
func TestThrottleCannotEnumerate(t *testing.T) {
	published := strings.Repeat("y", hashLen)
	missing := strings.Repeat("a", hashLen)

	respond := func(found bool, hash string) *httptest.ResponseRecorder {
		svc, err := New(&fakeLookup{t: t, found: found, key: []byte{0x99}, fingerprint: "F"},
			"example.org", testLogger())
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		svc.WithRateLimit(RateLimitConfig{PerSecond: 1, Burst: 1}, nil)
		h := svc.HUHandler()
		// Spend the single token, then ask.
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet,
			"/.well-known/openpgpkey/hu/"+hash, nil))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
			"/.well-known/openpgpkey/hu/"+hash, nil))
		return rec
	}

	hit := respond(true, published)
	miss := respond(false, missing)

	if hit.Code != miss.Code {
		t.Errorf("throttled hit = %d, throttled miss = %d — the limiter distinguishes a "+
			"published address from an unpublished one", hit.Code, miss.Code)
	}
	if hit.Body.String() != miss.Body.String() {
		t.Errorf("throttled bodies differ:\n hit: %q\nmiss: %q", hit.Body.String(), miss.Body.String())
	}
}

// TestRateLimitCanBeDisabledExplicitly: the opt-out has to work, or an operator
// who needs it will reach for something worse.
func TestRateLimitCanBeDisabledExplicitly(t *testing.T) {
	svc, err := New(okLookup(t), "example.org", testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	svc.WithRateLimit(RateLimitConfig{PerSecond: 0, Burst: 0}, nil)
	if svc.limit != nil {
		t.Fatal("a disabled config still built a limiter")
	}

	h := svc.HUHandler()
	hash := strings.Repeat("y", hashLen)
	for i := 0; i < DefaultRateBurst*3; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/.well-known/openpgpkey/hu/"+hash, nil))
		if rec.Code == http.StatusTooManyRequests {
			t.Fatalf("throttled at request %d with limiting disabled", i)
		}
	}
}
