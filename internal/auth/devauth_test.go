// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/oklog/ulid/v2"
)

func TestDevAuth_InjectsUserID(t *testing.T) {
	want := ulid.Make()

	var gotUserID ulid.ULID
	var gotOK bool
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUserID, gotOK = UserIDFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/jmap", nil) // no cookie, no token
	rr := httptest.NewRecorder()

	DevAuth(next, want).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if !gotOK {
		t.Fatal("UserIDFromContext: ok = false — dev-auth must use the SAME context key as Middleware")
	}
	if gotUserID != want {
		t.Fatalf("UserIDFromContext = %v, want %v", gotUserID, want)
	}
}

// The whole (deliberate) point of dev-auth: an unauthenticated request is
// served, not rejected. This is the test that documents the danger.
func TestDevAuth_AuthenticatesNobody(t *testing.T) {
	id := ulid.Make()
	calls := 0
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++ })

	h := DevAuth(next, id)
	for _, req := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "/", nil),
		httptest.NewRequest(http.MethodPost, "/jmap", nil),
	} {
		// A forged/garbage session cookie changes nothing either way.
		req.AddCookie(&http.Cookie{Name: DefaultSessionCookieName, Value: "garbage"})
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
	if calls != 2 {
		t.Fatalf("next called %d times, want 2 (dev-auth never rejects a request)", calls)
	}
}

// A zero user id is a wiring bug; dev-auth must fail closed rather than inject a
// zero-value identity that downstream code could mistake for a real user.
func TestDevAuth_ZeroUserIDFailsClosed(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler must not run without a user id")
	})

	req := httptest.NewRequest(http.MethodGet, "/jmap", nil)
	rr := httptest.NewRecorder()

	DevAuth(next, ulid.ULID{}).ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rr.Code)
	}
}
