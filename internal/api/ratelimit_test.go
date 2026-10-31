// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/auth"
)

// TestRateLimiterBurstThenRefill: a user may spend the burst, is then refused,
// and recovers as tokens refill — all on an injected clock, so the test is
// deterministic rather than timing-dependent.
func TestRateLimiterBurstThenRefill(t *testing.T) {
	now := time.Unix(0, 0)
	rl := NewRateLimiter(RateLimitConfig{PerSecond: 10, Burst: 3})
	rl.now = func() time.Time { return now }
	u := ulid.Make()

	for i := 0; i < 3; i++ {
		if ok, _ := rl.Allow(u); !ok {
			t.Fatalf("request %d within burst was refused", i+1)
		}
	}
	ok, retry := rl.Allow(u)
	if ok {
		t.Fatal("the 4th request over a burst of 3 was allowed")
	}
	if retry <= 0 {
		t.Errorf("retryAfter = %v, want > 0", retry)
	}

	// After 100ms at 10/s exactly one token is back.
	now = now.Add(100 * time.Millisecond)
	if ok, _ := rl.Allow(u); !ok {
		t.Error("a refilled token was not honored")
	}
	if ok, _ := rl.Allow(u); ok {
		t.Error("a second request after a single refilled token was allowed")
	}
}

// TestRateLimiterIsolatesUsers: one user exhausting the budget does not throttle
// another. A shared bucket would be a cross-user denial-of-service.
func TestRateLimiterIsolatesUsers(t *testing.T) {
	rl := NewRateLimiter(RateLimitConfig{PerSecond: 1, Burst: 1})
	a, b := ulid.Make(), ulid.Make()
	if ok, _ := rl.Allow(a); !ok {
		t.Fatal("A's first request refused")
	}
	if ok, _ := rl.Allow(a); ok {
		t.Fatal("A's second request should be refused (burst 1)")
	}
	if ok, _ := rl.Allow(b); !ok {
		t.Error("B was throttled by A's spending — buckets are not isolated")
	}
}

// TestRateLimitConfigDisabled: a non-positive rate or burst turns limiting off,
// and a nil limiter allows everything.
func TestRateLimitConfigDisabled(t *testing.T) {
	if NewRateLimiter(RateLimitConfig{PerSecond: 0, Burst: 10}) != nil {
		t.Error("a zero rate must disable the limiter")
	}
	if NewRateLimiter(RateLimitConfig{PerSecond: 10, Burst: 0}) != nil {
		t.Error("a zero burst must disable the limiter")
	}
	var nilRL *RateLimiter
	if ok, _ := nilRL.Allow(ulid.Make()); !ok {
		t.Error("a nil limiter must allow everything")
	}
}

// TestRateLimitMiddleware429: over the limit, the middleware answers a clean 429
// with Retry-After — never a dropped mutation reaching the handler.
func TestRateLimitMiddleware429(t *testing.T) {
	rl := NewRateLimiter(RateLimitConfig{PerSecond: 1, Burst: 1})
	reached := 0
	handler := auth.DevAuth(rl.middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached++
		w.WriteHeader(http.StatusOK)
	})), ulid.Make())

	first := serve(handler, "POST", "/jmap")
	if first.Code != http.StatusOK {
		t.Fatalf("first request: status %d, want 200", first.Code)
	}
	second := serve(handler, "POST", "/jmap")
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("second request: status %d, want 429", second.Code)
	}
	if second.Header().Get("Retry-After") == "" {
		t.Error("429 response carries no Retry-After")
	}
	if reached != 1 {
		t.Errorf("the throttled request reached the handler %d times, want the mutation dropped BEFORE it (1)", reached)
	}
}

// TestRateLimitPassesUnauthenticated: with no user id in context the limiter is a
// pass-through (throttling per-user is impossible; the 401 is the auth layer's).
func TestRateLimitPassesUnauthenticated(t *testing.T) {
	rl := NewRateLimiter(RateLimitConfig{PerSecond: 1, Burst: 1})
	handler := rl.middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	for i := 0; i < 5; i++ {
		if rec := serve(handler, "GET", "/jmap/session"); rec.Code != http.StatusOK {
			t.Fatalf("unauthenticated request %d was throttled (status %d)", i+1, rec.Code)
		}
	}
}

// serve is a tiny recorder helper (mirrors the one in the integration suite, but
// this file compiles in the unit lane where that one is absent).
func serve(h http.Handler, method, url string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, url, nil))
	return rec
}

// An API key spends its own bucket, never the session's (#49 review, third
// pass): a looping agent must not 429 the person's browser, whose next request
// may be the one revoking that key.
//
// RED PROBE: make the middleware call rl.Allow(userID) for key requests too and
// the session request below is throttled.
func TestRateLimitKeyHasItsOwnBucket(t *testing.T) {
	rl := NewRateLimiter(RateLimitConfig{PerSecond: 0.001, Burst: 2})
	user, key := ulid.Make(), ulid.Make()
	tok, hash, _, err := auth.NewAPIToken()
	if err != nil {
		t.Fatal(err)
	}
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	gate := &auth.BearerGate{Resolve: func(_ context.Context, h [32]byte) (ulid.ULID, ulid.ULID, bool, error) {
		return user, key, h == hash, nil
	}}
	keyHandler := gate.Wrap(rl.middleware(ok), http.NotFoundHandler())
	sessionHandler := auth.DevAuth(rl.middleware(ok), user)

	keyReq := func() int {
		r := httptest.NewRequest("POST", "/jmap", nil)
		r.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		keyHandler.ServeHTTP(w, r)
		return w.Code
	}
	codes := []int{keyReq(), keyReq(), keyReq()}
	if codes[0] != http.StatusOK || codes[1] != http.StatusOK || codes[2] != http.StatusTooManyRequests {
		t.Fatalf("key requests = %v, want 200 200 429 (the key's burst is 2)", codes)
	}
	for i := range 2 {
		if rec := serve(sessionHandler, "POST", "/jmap"); rec.Code != http.StatusOK {
			t.Fatalf("session request %d after the key exhausted its bucket: %d, want 200", i+1, rec.Code)
		}
	}
	// And the session's spending does not refill or drain the key's.
	if c := keyReq(); c != http.StatusTooManyRequests {
		t.Fatalf("key request after session traffic: %d, want still 429", c)
	}
}

// All of a user's keys together are bounded by one aggregate bucket, so N
// keys do not get N times the throughput; a refusal by the aggregate takes
// nothing from the key's own bucket; the session keeps its own bucket.
//
// RED PROBE: make AllowKey spend only the key's own bucket and the second
// key is admitted; make allow consume before checking every bucket and the
// refused key's own bucket is drained.
func TestRateLimitKeysShareAnAggregate(t *testing.T) {
	rl := NewRateLimiter(RateLimitConfig{PerSecond: 0.001, Burst: 2})
	user, k1, k2 := ulid.Make(), ulid.Make(), ulid.Make()
	for i := range 2 {
		if ok, _ := rl.AllowKey(user, k1); !ok {
			t.Fatalf("key 1 request %d refused, want its burst of 2", i+1)
		}
	}
	if ok, retry := rl.AllowKey(user, k2); ok || retry <= 0 {
		t.Fatalf("key 2 admitted (retry %v) after key 1 spent the aggregate, want refused", retry)
	}
	if b := rl.buckets[bucketKey{user: user, key: k2}]; b == nil || b.tokens != 2 {
		t.Fatalf("key 2's own bucket = %+v after an aggregate refusal, want untouched (2 tokens)", b)
	}
	if ok, _ := rl.Allow(user); !ok {
		t.Fatal("the session was throttled by key traffic")
	}
	rl.ForgetKey(user, k1)
	if _, kept := rl.buckets[bucketKey{user: user, key: k1}]; kept {
		t.Error("a revoked key's bucket was kept")
	}
	if _, kept := rl.buckets[bucketKey{user: user, allKeys: true}]; !kept {
		t.Error("forgetting one key dropped the aggregate the other keys share")
	}
}
