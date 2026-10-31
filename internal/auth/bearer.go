// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
)

// API keys: the token format and the bearer gate (#49, spec §13a, ADR-042).
//
// # The token
//
// "mr_" followed by 52 characters of lowercase RFC 4648 base32 (no padding):
// 32 bytes, 256 bits, from crypto/rand. The fixed prefix is what lets a secret
// scanner recognise a leaked key; base32 keeps the token free of characters a
// shell, a URL or a config file would need to escape. Only SHA-256 of the
// whole token is stored. A slow password hash would buy nothing here: the
// input is uniformly random, so there is no dictionary to slow down.
//
// # The gate
//
// An `Authorization: Bearer <token>` header — the standard scheme every HTTP
// client and every MCP client can send, with nothing client-specific about it.
// Every failure gets the same 401, body and headers; see BearerGate.

// APITokenPrefix starts every API key token.
const APITokenPrefix = "mr_"

const (
	apiTokenBytes   = 32
	apiTokenBodyLen = 52 // base32 of 32 bytes, unpadded
	// apiTokenHintLen is how much of the token body the store keeps so a list
	// can tell keys apart: 7 characters, 35 of the 256 bits.
	apiTokenHintLen = 7
)

var tokenEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewAPIToken mints a token. It returns the token (shown to the user once and
// never stored), its SHA-256 (what the store keeps) and its hint.
func NewAPIToken() (token string, hash [32]byte, hint string, err error) {
	buf := make([]byte, apiTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", [32]byte{}, "", fmt.Errorf("auth: read random bytes: %w", err)
	}
	body := strings.ToLower(tokenEncoding.EncodeToString(buf))
	token = APITokenPrefix + body
	return token, HashAPIToken(token), body[:apiTokenHintLen], nil
}

// HashAPIToken is the stored form of a token.
func HashAPIToken(token string) [32]byte { return sha256.Sum256([]byte(token)) }

// WellFormedAPIToken reports whether s has the shape NewAPIToken produces.
func WellFormedAPIToken(s string) bool {
	body, ok := strings.CutPrefix(s, APITokenPrefix)
	if !ok || len(body) != apiTokenBodyLen {
		return false
	}
	for _, c := range body {
		if (c < 'a' || c > 'z') && (c < '2' || c > '7') {
			return false
		}
	}
	return true
}

// ResolveAPIKey maps the hash of a presented token to its user and key. ok is
// false for anything that must be refused (unknown, revoked, suspended, a user
// who cannot sign in). Like ResolveUser it is injected: the store implements it.
type ResolveAPIKey func(ctx context.Context, hash [32]byte) (userID, keyID ulid.ULID, ok bool, err error)

const apiKeyIDContextKey contextKey = iota + 100

// APIKeyIDFromContext returns the API key a request authenticated with, if it
// did. ok is false for a session-authenticated request. Only BearerGate sets
// it, so a handler can rely on it meaning "this request is an agent's".
func APIKeyIDFromContext(ctx context.Context) (ulid.ULID, bool) {
	id, ok := ctx.Value(apiKeyIDContextKey).(ulid.ULID)
	return id, ok
}

// Failed-attempt limiting. Per client address, consulted and charged ONLY when
// an attempt has failed, so it slows a guesser and never refuses a live key.
// Per client address, as #49 asks — with the caveat
// internal/keypub states in full: nothing in this server knows of a trusted
// proxy, so behind one every request has the proxy's address and this becomes
// ONE bucket for every caller. That is acceptable for what it protects. A
// 256-bit token cannot be guessed, so the limit is not what keeps keys secret;
// it slows an anonymous flood of bad tokens (each answered 429 without the
// usual 401 once its bucket is spent). It does NOT bound database lookups: every
// presented token is looked up before the limiter is asked, which is the price
// of never refusing a good key, and a single indexed lookup per request is the
// same order of cost as the cookie path's.
const (
	// DefaultBearerFailureBurst failures are allowed at once per address...
	DefaultBearerFailureBurst = 20
	// ...refilling at one every DefaultBearerFailureRefill (10 a minute).
	DefaultBearerFailureRefill = 6 * time.Second
	// maxFailureBuckets bounds the limiter's memory under a spray of addresses.
	maxFailureBuckets = 4096
)

// FailureLimiter is a per-address token bucket consumed only by FAILED bearer
// attempts. The zero value is not usable; build one with NewFailureLimiter.
type FailureLimiter struct {
	burst  float64
	refill time.Duration
	now    func() time.Time

	mu      sync.Mutex
	buckets map[string]*failBucket
}

type failBucket struct {
	tokens float64
	last   time.Time
}

// NewFailureLimiter builds a limiter. A non-positive burst or refill takes the
// default — there is deliberately no "off".
func NewFailureLimiter(burst int, refill time.Duration) *FailureLimiter {
	if burst <= 0 {
		burst = DefaultBearerFailureBurst
	}
	if refill <= 0 {
		refill = DefaultBearerFailureRefill
	}
	return &FailureLimiter{burst: float64(burst), refill: refill, now: time.Now,
		buckets: map[string]*failBucket{}}
}

func (l *FailureLimiter) bucket(addr string, now time.Time) *failBucket {
	b := l.buckets[addr]
	if b == nil {
		if len(l.buckets) >= maxFailureBuckets {
			// Forget every address whose bucket has refilled; if that frees
			// nothing, forget them all rather than grow without bound.
			for k, v := range l.buckets {
				if v.tokens+now.Sub(v.last).Seconds()/l.refill.Seconds() >= l.burst {
					delete(l.buckets, k)
				}
			}
			if len(l.buckets) >= maxFailureBuckets {
				l.buckets = map[string]*failBucket{}
			}
		}
		b = &failBucket{tokens: l.burst, last: now}
		l.buckets[addr] = b
	}
	if elapsed := now.Sub(b.last); elapsed > 0 {
		b.tokens = min(l.burst, b.tokens+elapsed.Seconds()/l.refill.Seconds())
		b.last = now
	}
	return b
}

// Blocked reports whether addr has spent its failures, and if so how long
// until one more attempt is allowed.
func (l *FailureLimiter) Blocked(addr string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.bucket(addr, l.now())
	if b.tokens >= 1 {
		return false, 0
	}
	return true, time.Duration((1 - b.tokens) * float64(l.refill))
}

// Fail records one failed attempt from addr.
func (l *FailureLimiter) Fail(addr string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.bucket(addr, l.now())
	b.tokens = max(0, b.tokens-1)
}

// BearerGate authenticates API-key requests in front of the session gate.
//
// A request with no Authorization header is not its business: it goes to the
// session-gated handler unchanged, so cookie sessions work exactly as before.
// A request WITH one is decided here and never reaches the session gate:
//
//   - Anything but a well-formed, live key is the SAME 401: status, body and
//     headers do not depend on whether the token was malformed, unknown,
//     revoked or suspended, and the lookup runs for a malformed token too, so
//     the database round trip does not tell them apart either.
//   - A request that carries a session cookie AND a bearer token is refused
//     with that same 401 (ADR-042 §2). Either reading of such a request could
//     be the wrong one: taking the cookie would let a key's caller borrow a
//     browser's full authority, and taking the bearer would attribute to an
//     agent what a person's browser sent. Refusing both is the only choice that
//     cannot be wrong, and no legitimate client needs to send both.
//   - CSRF does not arise: a browser never attaches an Authorization header on
//     its own, and a cross-origin page cannot add one without a CORS preflight
//     this server does not grant.
type BearerGate struct {
	Resolve ResolveAPIKey
	// SessionCookie is the session cookie's name; its presence alongside a
	// bearer token is refused. Empty means only the default name is checked.
	SessionCookie string
	Limiter       *FailureLimiter
	Log           *slog.Logger
}

// Wrap returns the handler for a gated route: bearer requests are
// authenticated here and handed to next with the user and the key in the
// context; everything else goes to sessionGated.
func (g *BearerGate) Wrap(next, sessionGated http.Handler) http.Handler {
	limiter := g.Limiter
	if limiter == nil {
		limiter = NewFailureLimiter(0, 0)
	}
	log := g.Log
	if log == nil {
		log = slog.Default()
	}
	cookie := g.SessionCookie
	if cookie == "" {
		cookie = DefaultSessionCookieName
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header, present := r.Header["Authorization"]
		if !present {
			sessionGated.ServeHTTP(w, r)
			return
		}
		addr := clientAddr(r)
		token := ""
		if len(header) == 1 {
			if scheme, value, ok := strings.Cut(header[0], " "); ok && strings.EqualFold(scheme, "Bearer") {
				token = strings.TrimSpace(value)
			}
		}
		_, cookieErr := r.Cookie(cookie)
		both := cookieErr == nil

		// The lookup runs for EVERY presented credential, well-formed or not:
		// a request refused on shape before the database would answer faster
		// than one refused after it, and that difference is a signal.
		userID, keyID, ok, err := g.Resolve(r.Context(), HashAPIToken(token))
		if err != nil {
			// Never the token: only that a lookup failed, and from where.
			log.ErrorContext(r.Context(), "API key lookup failed", "client", addr, "err", err)
			http.Error(w, "service unavailable", http.StatusServiceUnavailable)
			return
		}
		if !ok || both || !WellFormedAPIToken(token) {
			// THE LIMITER IS CONSULTED ONLY ON THE FAILURE PATH (#49 review).
			// Checked first, it would refuse a VALID key from an address that
			// strangers had flooded — and behind a reverse proxy every request
			// has the proxy's address, so twenty bogus requests would lock every
			// agent out. A live key is never refused here.
			if blocked, retry := limiter.Blocked(addr); blocked {
				w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
				http.Error(w, "too many failed authentication attempts", http.StatusTooManyRequests)
				return
			}
			limiter.Fail(addr)
			log.InfoContext(r.Context(), "API key authentication refused", "client", addr)
			unauthorizedBearer(w)
			return
		}
		ctx := context.WithValue(contextWithUserID(r.Context(), userID), apiKeyIDContextKey, keyID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// unauthorizedBearer is the one refusal. It names the scheme (RFC 6750 §3) and
// nothing else: no error code, no description, nothing that differs by cause.
func unauthorizedBearer(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="mailroom"`)
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}

// clientAddr is the address the request came from, without the port.
func clientAddr(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
