// SPDX-License-Identifier: AGPL-3.0-only

package keypub

import (
	"sync"
	"time"
)

// Rate limiting for the ANONYMOUS key surface (Forgejo #264, refs #47/#48).
//
// # Why this is global and not per-IP
//
// The obvious design is a per-IP token bucket. It is the wrong one here, and the
// reason is worth writing down because the wrong one LOOKS correct.
//
// ⚠ Nothing in this server has any notion of a trusted proxy: nothing reads
// RemoteAddr and nothing parses X-Forwarded-For. Behind a reverse proxy — which
// is exactly how this surface reaches the internet — every request arrives from
// the proxy's address, so a per-IP bucket collapses into ONE shared bucket and
// the first hostile client starves everybody. That failure is silent: it works
// perfectly against a directly-exposed port in testing, and is ineffective in
// the deployment that motivated writing it.
//
// So this bounds the thing actually at risk — DATABASE WORK — and does not care
// who is asking. It is immune to the proxy problem and to IP spoofing because it
// never looks at an address.
//
// The trade, stated rather than glossed: a hostile client CAN consume the budget
// and degrade lookups for everyone. That is accepted. A starved lookup means a
// correspondent cannot find a key and sends unencrypted mail; unbounded database
// load degrades the whole instance, including the user's own mail. Per-IP
// fairness needs infrastructure that can see real client addresses, and that
// belongs in front of this process, not inside it.
//
// # Why it defaults to ON
//
// ⚠ A limiter that is off unless something remembers to configure it is a
// limiter that ships INERT — this project's most-named failure mode, and exactly
// how key publication itself shipped (#48). So New builds one by default and
// disabling is an explicit act. There is a test asserting a DEFAULT-constructed
// Service throttles, so "nobody wired it up" fails loudly.

const (
	// DefaultRatePerSecond / DefaultRateBurst bound the anonymous surface.
	//
	// Generous for real use and still a bound: a correspondent's mail client
	// looks up one key per recipient, so a burst of 20 covers any plausible
	// single message, and 5/s sustained is far more WKD traffic than a personal
	// domain will ever see. The numbers exist to stop unbounded load, not to
	// ration legitimate lookups.
	DefaultRatePerSecond = 5.0
	DefaultRateBurst     = 20
)

// RateLimitConfig configures the anonymous-surface limiter. A non-positive rate
// or burst disables limiting entirely, which is the explicit opt-out an operator
// or a test can choose — mirroring internal/api's RateLimitConfig so the two
// read the same way.
type RateLimitConfig struct {
	PerSecond float64
	Burst     int
}

// Enabled reports whether the configuration actually limits anything.
func (c RateLimitConfig) Enabled() bool { return c.PerSecond > 0 && c.Burst > 0 }

// DefaultRateLimit is what New installs when the caller says nothing.
func DefaultRateLimit() RateLimitConfig {
	return RateLimitConfig{PerSecond: DefaultRatePerSecond, Burst: DefaultRateBurst}
}

// rateLimiter is a single token bucket shared by the whole anonymous surface.
type rateLimiter struct {
	perSecond float64
	burst     float64
	now       func() time.Time // injectable for deterministic tests

	mu     sync.Mutex
	tokens float64
	last   time.Time
}

// newRateLimiter builds a limiter, or returns nil if cfg disables limiting (so
// callers can treat "no limiter" and "limiting off" identically).
func newRateLimiter(cfg RateLimitConfig, now func() time.Time) *rateLimiter {
	if !cfg.Enabled() {
		return nil
	}
	if now == nil {
		now = time.Now
	}
	return &rateLimiter{
		perSecond: cfg.PerSecond,
		burst:     float64(cfg.Burst),
		now:       now,
		tokens:    float64(cfg.Burst),
		last:      now(),
	}
}

// allow consumes one token and reports whether the request may proceed. When it
// may not, retryAfter is how long until a token is available. A nil limiter
// allows everything (limiting is off).
func (rl *rateLimiter) allow() (ok bool, retryAfter time.Duration) {
	if rl == nil {
		return true, 0
	}
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := rl.now()
	if elapsed := now.Sub(rl.last).Seconds(); elapsed > 0 {
		rl.tokens = min(rl.burst, rl.tokens+elapsed*rl.perSecond)
		rl.last = now
	}
	if rl.tokens >= 1 {
		rl.tokens--
		return true, 0
	}
	need := (1 - rl.tokens) / rl.perSecond
	return false, time.Duration(need * float64(time.Second))
}
