// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Provider tokens are bearer credentials. These tests run the callback and the
// logout with sentinel tokens and assert that neither appears in ANY captured
// log line or in the body of a failure response (ADR-048 section 4, ADR-046's
// sentinel pattern).

const (
	sentinelRefresh = "SENTINEL-REFRESH-TOKEN-7f3a9c"
)

func captureLogs(a *Authenticator) *bytes.Buffer {
	var buf bytes.Buffer
	a.log = slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return &buf
}

// leaked returns the sentinels that appear in captured.
func leaked(captured string, sentinels ...string) []string {
	var out []string
	for _, s := range sentinels {
		if strings.Contains(captured, s) {
			out = append(out, s)
		}
	}
	return out
}

func assertNoSentinel(t *testing.T, where, captured string, sentinels ...string) {
	t.Helper()
	for _, s := range leaked(captured, sentinels...) {
		t.Errorf("%s contains the sentinel token %q:\n%s", where, s, captured)
	}
}

func TestTokensNeverReachLogsOrErrors(t *testing.T) {
	h := newCallbackHarness(t)
	logs := captureLogs(h.a)

	fs := flowState{
		State: "state-123", Nonce: "nonce-123", Verifier: "verifier-123",
		Expiry: time.Now().Add(time.Minute),
	}
	idToken := h.signIDToken(h.baseClaims("user-sub-1", fs.Nonce))
	// Both the happy path (token issued, then logout) and the paths that log or
	// return errors: a Create failure and a hint-less provider.
	currentTokenSpec = tokenResponseSpec{
		wantCode: "auth-code-123", wantVerifier: fs.Verifier,
		idToken: idToken, refreshToken: sentinelRefresh,
	}

	// 1. Successful login.
	rr := h.run(fs, nil)
	if rr.Code != http.StatusFound {
		t.Fatalf("login status = %d", rr.Code)
	}
	c := issuedSession(rr, h.a)
	if c == nil {
		t.Fatal("no session cookie")
	}

	// 2. A second login replaces the first, and a Create failure that carries
	// no token in its own text.
	rr = h.run(fs, c)
	if rr.Code != http.StatusFound {
		t.Fatalf("second login status = %d", rr.Code)
	}
	fakeOf(h.a).createErr = errors.New("database is down")
	rr = h.run(fs, nil)
	failBody := rr.Body.String()
	fakeOf(h.a).createErr = nil

	// 3. Logout through the provider path, then with every store call failing.
	h.a.endSessionEndpoint = "https://idp.example.com/end-session/"
	r := httptest.NewRequest(http.MethodGet, "/auth/logout", nil)
	r.AddCookie(issuedSession(h.run(fs, nil), h.a))
	h.a.LogoutHandler(httptest.NewRecorder(), r)
	fakeOf(h.a).revokeErr = errors.New("database is down")
	fakeOf(h.a).idTokenErr = errors.New("database is down")
	r = httptest.NewRequest(http.MethodGet, "/auth/logout", nil)
	r.AddCookie(c)
	h.a.LogoutHandler(httptest.NewRecorder(), r)

	// 4. No refresh token at all: the Info line must say so without a token.
	currentTokenSpec.refreshToken = ""
	h.run(fs, nil)

	captured := logs.String()
	if captured == "" {
		t.Fatal("nothing was logged: the capture is not observing the package's logger")
	}
	if !strings.Contains(captured, "provider issued no refresh token") {
		t.Errorf("the no-refresh-token line was not logged:\n%s", captured)
	}
	assertNoSentinel(t, "the captured log", captured, sentinelRefresh, idToken)
	assertNoSentinel(t, "the failure response body", failBody, sentinelRefresh, idToken)
}

// The negative control: the same check must go red when a token IS logged,
// otherwise the test above could not fail.
func TestSentinelCheckCanFail(t *testing.T) {
	a := testAuthenticator(t, true)
	logs := captureLogs(a)
	a.logger().Info("oops", "refresh_token", sentinelRefresh)

	if len(leaked(logs.String(), sentinelRefresh)) != 1 {
		t.Fatal("the leak check did not flag a logged token: the sentinel test cannot fail")
	}
	if len(leaked("a line with no token in it", sentinelRefresh)) != 0 {
		t.Fatal("the leak check flagged a clean line")
	}
}
