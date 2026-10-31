// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
)

func TestNewAPITokenShape(t *testing.T) {
	seen := map[string]bool{}
	for range 200 {
		tok, hash, hint, err := NewAPIToken()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(tok, "mr_") || len(tok) != 3+52 {
			t.Fatalf("token %q: want mr_ plus 52 characters", tok)
		}
		if !WellFormedAPIToken(tok) {
			t.Fatalf("a minted token is not well-formed: %q", tok)
		}
		if hash != sha256.Sum256([]byte(tok)) {
			t.Fatal("the hash is not SHA-256 of the token")
		}
		if hint != tok[3:10] {
			t.Fatalf("hint %q is not the first 7 characters of the body of %q", hint, tok)
		}
		if seen[tok] {
			t.Fatal("two minted tokens collided")
		}
		seen[tok] = true
	}
	for _, bad := range []string{"", "mr_", "mr_" + strings.Repeat("a", 51), "mr_" + strings.Repeat("a", 53),
		"xx_" + strings.Repeat("a", 52), "mr_" + strings.Repeat("A", 52), "mr_" + strings.Repeat("1", 52),
		"mr_" + strings.Repeat("a", 51) + "="} {
		if WellFormedAPIToken(bad) {
			t.Errorf("%q is reported well-formed", bad)
		}
	}
}

// gateHarness drives BearerGate over a fake resolver that knows one live token.
type gateHarness struct {
	gate      *BearerGate
	handler   http.Handler
	live      string
	userID    ulid.ULID
	keyID     ulid.ULID
	lookups   int
	nextHits  int
	sessHits  int
	gotUser   ulid.ULID
	gotKey    ulid.ULID
	gotKeyOK  bool
	logs      *bytes.Buffer
	failError error
}

func newGateHarness(t *testing.T) *gateHarness {
	t.Helper()
	live, liveHash, _, err := NewAPIToken()
	if err != nil {
		t.Fatal(err)
	}
	h := &gateHarness{live: live, userID: ulid.Make(), keyID: ulid.Make(), logs: &bytes.Buffer{}}
	h.gate = &BearerGate{
		Resolve: func(_ context.Context, hash [32]byte) (ulid.ULID, ulid.ULID, bool, error) {
			h.lookups++
			if h.failError != nil {
				return ulid.ULID{}, ulid.ULID{}, false, h.failError
			}
			if hash == liveHash {
				return h.userID, h.keyID, true, nil
			}
			return ulid.ULID{}, ulid.ULID{}, false, nil
		},
		Limiter: NewFailureLimiter(3, time.Hour),
		Log:     slog.New(slog.NewTextHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.nextHits++
		h.gotUser, _ = UserIDFromContext(r.Context())
		h.gotKey, h.gotKeyOK = APIKeyIDFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	session := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.sessHits++
		if _, ok := APIKeyIDFromContext(r.Context()); ok {
			t.Error("the session path saw an API key in its context")
		}
		w.WriteHeader(http.StatusTeapot)
	})
	h.handler = h.gate.Wrap(next, session)
	return h
}

func (h *gateHarness) do(addr string, mutate func(*http.Request)) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/jmap", strings.NewReader("{}"))
	r.RemoteAddr = addr + ":5555"
	if mutate != nil {
		mutate(r)
	}
	w := httptest.NewRecorder()
	h.handler.ServeHTTP(w, r)
	return w
}

func bearer(tok string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+tok) }
}

func TestBearerGateLiveKeyReachesTheHandlerWithItsIdentity(t *testing.T) {
	h := newGateHarness(t)
	w := h.do("192.0.2.1", bearer(h.live))
	if w.Code != http.StatusOK || h.nextHits != 1 || h.sessHits != 0 {
		t.Fatalf("live key: status %d, next %d, session %d", w.Code, h.nextHits, h.sessHits)
	}
	if h.gotUser != h.userID || !h.gotKeyOK || h.gotKey != h.keyID {
		t.Fatalf("context carried user %s key %s (%v), want %s / %s", h.gotUser, h.gotKey, h.gotKeyOK, h.userID, h.keyID)
	}
	// Scheme names are case-insensitive (RFC 7235 §2.1).
	if w := h.do("192.0.2.1", func(r *http.Request) { r.Header.Set("Authorization", "bearer "+h.live) }); w.Code != http.StatusOK {
		t.Fatalf("lowercase scheme: status %d", w.Code)
	}
}

func TestBearerGateWithoutAHeaderIsTheSessionPath(t *testing.T) {
	h := newGateHarness(t)
	w := h.do("192.0.2.1", func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: DefaultSessionCookieName, Value: "x"})
	})
	if w.Code != http.StatusTeapot || h.sessHits != 1 || h.nextHits != 0 || h.lookups != 0 {
		t.Fatalf("no Authorization header: status %d, session %d, next %d, lookups %d",
			w.Code, h.sessHits, h.nextHits, h.lookups)
	}
}

func TestBearerGateEveryRefusalIsTheSameResponse(t *testing.T) {
	unknown, _, _, _ := NewAPIToken()
	cases := map[string]func(*http.Request){
		"unknown well-formed token": bearer(unknown),
		"malformed token":           bearer("mr_short"),
		"another vendor's token":    bearer("sk-" + strings.Repeat("a", 48)),
		"empty bearer":              func(r *http.Request) { r.Header.Set("Authorization", "Bearer ") },
		"basic auth":                func(r *http.Request) { r.Header.Set("Authorization", "Basic dXNlcjpwYXNz") },
		"two headers": func(r *http.Request) {
			r.Header.Add("Authorization", "Bearer "+unknown)
			r.Header.Add("Authorization", "Bearer "+unknown)
		},
	}
	var first *httptest.ResponseRecorder
	var firstName string
	for name, mutate := range cases {
		h := newGateHarness(t)
		w := h.do("192.0.2.7", mutate)
		if w.Code != http.StatusUnauthorized || h.nextHits != 0 || h.sessHits != 0 {
			t.Fatalf("%s: status %d, next %d, session %d — want 401 and no handler", name, w.Code, h.nextHits, h.sessHits)
		}
		// The lookup runs whatever the shape, so timing does not separate them.
		if h.lookups != 1 {
			t.Errorf("%s: %d lookups, want 1", name, h.lookups)
		}
		if first == nil {
			first, firstName = w, name
			continue
		}
		if w.Body.String() != first.Body.String() ||
			w.Header().Get("WWW-Authenticate") != first.Header().Get("WWW-Authenticate") ||
			w.Header().Get("Content-Type") != first.Header().Get("Content-Type") {
			t.Errorf("%s answered differently from %s: %q %v vs %q %v", name, firstName,
				w.Body.String(), w.Header(), first.Body.String(), first.Header())
		}
	}
	if got := first.Header().Get("WWW-Authenticate"); got != `Bearer realm="mailroom"` {
		t.Errorf("WWW-Authenticate = %q", got)
	}
}

// A revoked or suspended key is what the resolver refuses: the gate must give
// it the unknown-token answer, which the case above pins.
func TestBearerGateRefusesSessionCookiePlusBearer(t *testing.T) {
	h := newGateHarness(t)
	w := h.do("192.0.2.1", func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+h.live)
		r.AddCookie(&http.Cookie{Name: DefaultSessionCookieName, Value: "a-session"})
	})
	if w.Code != http.StatusUnauthorized || h.nextHits != 0 || h.sessHits != 0 {
		t.Fatalf("cookie plus a LIVE bearer: status %d, next %d, session %d — want refused outright",
			w.Code, h.nextHits, h.sessHits)
	}
	// A cookie with another name is not a session and does not trip it.
	if w := h.do("192.0.2.1", func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+h.live)
		r.AddCookie(&http.Cookie{Name: "unrelated", Value: "x"})
	}); w.Code != http.StatusOK {
		t.Fatalf("an unrelated cookie refused a live key: %d", w.Code)
	}
}

func TestBearerGateLimitsFailuresPerAddress(t *testing.T) {
	h := newGateHarness(t) // burst 3, no refill within the test
	unknown, _, _, _ := NewAPIToken()
	for i := range 3 {
		if w := h.do("198.51.100.9", bearer(unknown)); w.Code != http.StatusUnauthorized {
			t.Fatalf("failure %d: status %d, want 401", i+1, w.Code)
		}
	}
	w := h.do("198.51.100.9", bearer(unknown))
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" {
		t.Fatalf("after the burst: status %d (Retry-After %q), want 429", w.Code, w.Header().Get("Retry-After"))
	}
	// A LIVE key from that same, exhausted address still gets in (#49 review):
	// behind a reverse proxy every caller shares one address, so a limiter that
	// refused before resolving would let strangers lock every agent out.
	if w := h.do("198.51.100.9", bearer(h.live)); w.Code != http.StatusOK {
		t.Fatalf("live key from the exhausted address: %d, want 200", w.Code)
	}
	// ...and the bucket is per address.
	if w := h.do("198.51.100.10", bearer(unknown)); w.Code != http.StatusUnauthorized {
		t.Fatalf("a bad key from another address: %d, want 401", w.Code)
	}
	// Successes do not consume the budget.
	h2 := newGateHarness(t)
	for range 10 {
		if w := h2.do("203.0.113.5", bearer(h2.live)); w.Code != http.StatusOK {
			t.Fatalf("live key was limited: %d", w.Code)
		}
	}
}

func TestBearerGateNeverLogsTheToken(t *testing.T) {
	h := newGateHarness(t)
	unknown, _, _, _ := NewAPIToken()
	h.do("192.0.2.1", bearer(unknown))
	h.do("192.0.2.1", bearer(h.live))
	h.failError = errors.New("database down")
	if w := h.do("192.0.2.1", bearer(h.live)); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("lookup error: status %d, want 503", w.Code)
	}
	logs := h.logs.String()
	if !strings.Contains(logs, "refused") || !strings.Contains(logs, "lookup failed") {
		t.Fatalf("the refusal and the lookup error were not logged at all, so this test proves nothing: %s", logs)
	}
	for _, tok := range []string{unknown, h.live, unknown[3:], h.live[3:]} {
		if strings.Contains(logs, tok) {
			t.Fatalf("a token reached the log: %s", logs)
		}
	}
}
