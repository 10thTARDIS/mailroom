// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"net/http"
	"net/http/httptest"
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
