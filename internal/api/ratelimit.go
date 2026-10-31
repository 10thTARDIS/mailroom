// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/auth"
)

// Per-user API rate limiting (spec §10, matrix §14).
//
// The governing rule for every limit in Mailroom is the same one that governs
// quotas: a rejected request gets a CLEAN, explicit answer — here an HTTP 429
// with Retry-After — and NEVER a silently dropped mutation. A throttled Email/set
// must fail loudly so the client retries; it must never look like it succeeded.
//
// Identity is the auth-context user id, never a request parameter (spec §10): a
// client cannot spend, or dodge, another user's budget by naming an account.
//
// The bucket is per user and lives in-process. At ≤ 24 users the map is tiny and
// needs no eviction; a request for a user not seen before starts with a full
// bucket, so the limiter adds no cold-start penalty to normal use.

const (
	// DefaultRateLimitPerSecond / DefaultRateLimitBurst are the enforced defaults
	// (spec §10: "enforced defaults in v1.0; admin UI may lag"). They are generous
	// for an interactive JMAP client — which batches method calls into few HTTP
	// requests — and still cap a runaway or hostile caller.
	DefaultRateLimitPerSecond = 50.0
	DefaultRateLimitBurst     = 100
)

// RateLimitConfig configures the per-user limiter. A non-positive rate or burst
// disables limiting entirely (RateLimiter is then nil and the middleware is a
// pass-through), which is the explicit opt-out an operator or a test can choose.
type RateLimitConfig struct {
	PerSecond float64
	Burst     int
}

// Enabled reports whether the configuration actually limits anything.
func (c RateLimitConfig) Enabled() bool { return c.PerSecond > 0 && c.Burst > 0 }

// RateLimiter is a per-user token bucket.
type RateLimiter struct {
	perSecond float64
	burst     float64
	now       func() time.Time // injectable for deterministic tests

	mu      sync.Mutex
	buckets map[bucketKey]*tokenBucket
}

// bucketKey is whose bucket a request spends: the user's own (a session), or
// one of the user's API keys (#49 review). A key has a bucket of its OWN, so a
// looping agent exhausts itself and never 429s the person's browser — whose
// next request may be the one revoking that key.
//
// Every key request ALSO spends the user's keys-aggregate bucket (allKeys),
// so N keys cannot get N times the throughput; the session bucket stays
// separate, so all the keys together still cannot starve the browser.
type bucketKey struct {
	user    ulid.ULID
	key     ulid.ULID // zero for a session and for the aggregate
	allKeys bool      // the user's shared bucket for all key traffic
}

type tokenBucket struct {
	tokens float64
	last   time.Time
}

// NewRateLimiter builds a limiter from cfg, or returns nil if cfg disables
// limiting (so callers can treat "no limiter" and "limiting off" identically).
func NewRateLimiter(cfg RateLimitConfig) *RateLimiter {
	if !cfg.Enabled() {
		return nil
	}
	return &RateLimiter{
		perSecond: cfg.PerSecond,
		burst:     float64(cfg.Burst),
		now:       time.Now,
		buckets:   make(map[bucketKey]*tokenBucket),
	}
}

// Allow consumes one token for user and reports whether the request may proceed.
// When it may not, retryAfter is how long until a token is available. A nil
// limiter allows everything (limiting is off).
func (rl *RateLimiter) Allow(user ulid.ULID) (ok bool, retryAfter time.Duration) {
	return rl.allow(bucketKey{user: user})
}

// AllowKey is Allow for a request authenticated with API key keyID of user:
// it spends the key's own bucket, never the user's session bucket.
// It takes a token from the key's own bucket AND from the user's
// keys-aggregate bucket, or from neither: a refusal by one bucket consumes
// nothing from the other.
func (rl *RateLimiter) AllowKey(user, keyID ulid.ULID) (ok bool, retryAfter time.Duration) {
	return rl.allow(bucketKey{user: user, key: keyID}, bucketKey{user: user, allKeys: true})
}

// ForgetKey drops a key's own bucket (it is revoked; nothing will spend it
// again). The user's aggregate bucket is kept: it belongs to the other keys.
func (rl *RateLimiter) ForgetKey(user, keyID ulid.ULID) {
	if rl == nil {
		return
	}
	rl.mu.Lock()
	defer rl.mu.Unlock()
	delete(rl.buckets, bucketKey{user: user, key: keyID})
}

// allow takes one token from every bucket in ks, or from none of them.
func (rl *RateLimiter) allow(ks ...bucketKey) (ok bool, retryAfter time.Duration) {
	if rl == nil {
		return true, 0
	}
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := rl.now()
	bs := make([]*tokenBucket, 0, len(ks))
	var wait float64
	for _, k := range ks {
		b := rl.buckets[k]
		if b == nil {
			b = &tokenBucket{tokens: rl.burst, last: now}
			rl.buckets[k] = b
		}
		if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
			b.tokens = min(rl.burst, b.tokens+elapsed*rl.perSecond)
			b.last = now
		}
		if b.tokens < 1 {
			wait = max(wait, (1-b.tokens)/rl.perSecond)
		}
		bs = append(bs, b)
	}
	if wait > 0 {
		return false, time.Duration(wait * float64(time.Second))
	}
	for _, b := range bs {
		b.tokens--
	}
	return true, 0
}

// middleware enforces the per-user limit in front of next. It runs BEHIND the
// auth middleware (it reads the user id the auth layer injected), so an
// unauthenticated request carries no identity and is passed straight through —
// throttling it per user is impossible and it is the auth layer's 401 to give,
// not the limiter's 429.
func (rl *RateLimiter) middleware(next http.Handler) http.Handler {
	if rl == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userID, ok := auth.UserIDFromContext(r.Context())
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		var allowed bool
		var retry time.Duration
		if keyID, isKey := auth.APIKeyIDFromContext(r.Context()); isKey {
			allowed, retry = rl.AllowKey(userID, keyID)
		} else {
			allowed, retry = rl.Allow(userID)
		}
		if !allowed {
			secs := int(retry.Seconds()) + 1
			w.Header().Set("Retry-After", strconv.Itoa(secs))
			// A clean, explicit 429 — the whole point is that the client learns its
			// request did NOT happen, rather than a mutation being silently dropped.
			writeJSON(w, http.StatusTooManyRequests,
				newRequestError("rateLimited", http.StatusTooManyRequests,
					"too many requests; retry after "+strconv.Itoa(secs)+"s"))
			return
		}
		next.ServeHTTP(w, r)
	})
}
