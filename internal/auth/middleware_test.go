// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"
)

func TestMiddleware_ValidSession_InjectsUserID(t *testing.T) {
	a := testAuthenticator(t, true)
	want := ulid.Make()
	sessionCookie := signIn(t, a, want, "")

	var gotUserID ulid.ULID
	var gotOK bool
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUserID, gotOK = UserIDFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	req.AddCookie(sessionCookie)
	rr := httptest.NewRecorder()

	a.Middleware(next).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if !gotOK {
		t.Fatal("UserIDFromContext: ok = false inside wrapped handler")
	}
	if gotUserID != want {
		t.Fatalf("UserIDFromContext = %v, want %v", gotUserID, want)
	}
}

func TestMiddleware_MissingSession_Returns401ByDefault(t *testing.T) {
	a := testAuthenticator(t, true)
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	})

	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	rr := httptest.NewRecorder()

	a.Middleware(next).ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
	if called {
		t.Fatal("next handler was called for an unauthenticated request")
	}
}

func TestMiddleware_InvalidSession_Returns401(t *testing.T) {
	a := testAuthenticator(t, true)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not run")
	})

	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	req.AddCookie(&http.Cookie{Name: a.cfg.SessionCookieName, Value: "garbage"})
	rr := httptest.NewRecorder()

	a.Middleware(next).ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
}

func TestMiddleware_WithUnauthenticatedRedirect(t *testing.T) {
	a := testAuthenticator(t, true)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not run")
	})

	req := httptest.NewRequest(http.MethodGet, "/inbox?x=1", nil)
	rr := httptest.NewRecorder()

	a.Middleware(next, WithUnauthenticatedRedirect("/auth/login")).ServeHTTP(rr, req)

	if rr.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rr.Code)
	}
	loc := rr.Header().Get("Location")
	if !strings.HasPrefix(loc, "/auth/login?return_to=") {
		t.Fatalf("Location = %q, want prefix /auth/login?return_to=", loc)
	}
	if !strings.Contains(loc, "%2Finbox") {
		t.Fatalf("Location = %q, want the original path percent-encoded in return_to", loc)
	}
}

// The OAuth authorization endpoint's query (its state included) survives the
// sign-in round trip; every other path's code/state is still dropped, and a
// code is dropped everywhere.
func TestMiddleware_RedirectKeepsAuthorizeQuery(t *testing.T) {
	a := testAuthenticator(t, true)
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("next handler should not run") })
	rt := func(target string) string {
		rr := httptest.NewRecorder()
		a.Middleware(next, WithUnauthenticatedRedirect("/auth/login")).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, target, nil))
		u, err := url.Parse(rr.Header().Get("Location"))
		if err != nil {
			t.Fatal(err)
		}
		return u.Query().Get("return_to")
	}
	if got := rt("/oauth/authorize?client_id=c&state=s1&code_challenge=x"); got != "/oauth/authorize?client_id=c&state=s1&code_challenge=x" {
		t.Errorf("authorize query not kept: %q", got)
	}
	if got := rt("/oauth/authorize?code=secret&state=s1"); got != "/oauth/authorize" {
		t.Errorf("a code on authorize was carried: %q", got)
	}
	if got := rt("/oauth/callback/google?state=s1"); got != "/oauth/callback/google" {
		t.Errorf("state on another path was carried: %q", got)
	}
}

// An authorize request too long to survive in the login-state cookie gets
// Mailroom's own page, not a redirect that would lose it.
func TestMiddleware_RedirectRefusesOverlongAuthorize(t *testing.T) {
	a := testAuthenticator(t, true)
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("next handler should not run") })
	serve := func(target string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		a.Middleware(next, WithUnauthenticatedRedirect("/auth/login")).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, target, nil))
		return rr
	}
	long := serve("/oauth/authorize?state=" + strings.Repeat("a", 3000))
	if long.Code != http.StatusBadRequest || long.Header().Get("Location") != "" || !strings.Contains(long.Body.String(), "too long") {
		t.Errorf("overlong authorize: %d %q", long.Code, long.Header().Get("Location"))
	}
	if ok := serve("/oauth/authorize?state=" + strings.Repeat("a", 1500)); ok.Code != http.StatusFound {
		t.Errorf("a 1.5 KiB authorize was refused: %d", ok.Code)
	}
	// Other paths are not capped by this rule.
	if other := serve("/inbox?x=" + strings.Repeat("a", 3000)); other.Code != http.StatusFound {
		t.Errorf("other path: %d", other.Code)
	}
}
