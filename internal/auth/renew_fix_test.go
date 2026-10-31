// SPDX-License-Identifier: AGPL-3.0-only

package auth

// The fix round for the security review of renewal (#937): the consumed refresh
// token is kept, identity is never half-checked, only a deliberate refusal
// revokes, and renewals in flight are bounded. See renew.go phases A to C.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
	"golang.org/x/oauth2"
)

// The reviewer's repro: the grant succeeds and rotates the token, then UserInfo
// fails. The rotated token must be stored (the old one is dead at a single-use
// provider), the session must not be stamped, and the next attempt must present
// the ROTATED token and succeed.
func TestRenew_RotatedTokenIsKeptWhenALaterStepFails(t *testing.T) {
	h := newRenewHarness(t)
	h.singleUse = true
	h.noIDToken = true
	var userInfoDown atomic.Bool
	userInfoDown.Store(true)
	h.a.userInfoFn = func(context.Context, *oauth2.Token) (userInfoResult, error) {
		if userInfoDown.Load() {
			return userInfoResult{}, errors.New("userinfo is down")
		}
		return userInfoResult{Subject: renewSubject, Claims: idTokenClaims{Email: "person@example.com"}}, nil
	}
	h.age(2 * time.Hour)
	before := h.lastChecked()

	rr, called := h.request()
	if rr.Code != http.StatusOK || !called {
		t.Fatalf("status %d, ran %v; want served within the allowance", rr.Code, called)
	}
	if got := h.storedRefresh(); got != "rt-issued-1" {
		t.Fatalf("stored refresh token = %q; want the rotated rt-issued-1", got)
	}
	if !h.lastChecked().Equal(before) {
		t.Fatal("last_checked_at advanced although identity was not re-checked")
	}

	userInfoDown.Store(false)
	h.skew.Store(int64(61 * time.Second))
	if rr, called := h.request(); rr.Code != http.StatusOK || !called {
		t.Fatalf("second attempt: status %d, ran %v", rr.Code, called)
	}
	if h.tokenHits() != 2 || h.presents[1] != "rt-issued-1" {
		t.Fatalf("hits=%d presented=%v; want the second attempt to present the rotated token", h.tokenHits(), h.presents)
	}
	if h.row().revokedAs != "" {
		t.Fatalf("revoked as %q; a token the provider accepted must not be refused", h.row().revokedAs)
	}
	if !h.lastChecked().After(before) {
		t.Error("the successful second attempt did not confirm the session")
	}
	if n := len(h.resolved()); n != 1 {
		t.Errorf("ResolveUser ran %d times, want once (only the attempt that re-checked identity)", n)
	}
}

// The attempt belongs to every waiting request, not the one that started it: a
// client that disconnects mid-grant must not cost the rotated token. Touch must
// not run for the departed client either (it would only log an error).
func TestRenew_RotationIsStoredEvenIfTheRequestIsCancelled(t *testing.T) {
	h := newRenewHarness(t)
	h.rotate = true
	ctx, cancel := context.WithCancel(context.Background())
	h.answer = func(w http.ResponseWriter, _ *http.Request, h *renewHarness) {
		cancel() // the client leaves while the provider is answering
		time.Sleep(50 * time.Millisecond)
		h.success(w, renewNewRT, false, "", nil)
	}
	h.age(time.Hour)
	req := httptest.NewRequest(http.MethodGet, "/protected", nil).WithContext(ctx)
	req.AddCookie(h.cookie)
	h.a.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(httptest.NewRecorder(), req)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && h.storedRefresh() != renewNewRT {
		time.Sleep(20 * time.Millisecond)
	}
	if got := h.storedRefresh(); got != renewNewRT {
		t.Fatalf("stored refresh token = %q; the rotation was lost when the client disconnected", got)
	}
	fs := fakeOf(h.a)
	fs.mu.Lock()
	touches := fs.touchCalls
	fs.mu.Unlock()
	if touches != 0 || strings.Contains(h.logs.String(), "recording session use failed") {
		t.Errorf("Touch ran for a request whose context was already done (calls=%d)", touches)
	}
}

// A half-checked identity must not be confirmed or handed to the resolver: a
// missing email refuses the user and missing groups demote an admin. When
// UserInfo is required and fails, the renewal is transient.
func TestRenew_IncompleteIdentityNeedsUserInfo(t *testing.T) {
	down := func(context.Context, *oauth2.Token) (userInfoResult, error) {
		return userInfoResult{}, errors.New("userinfo is down")
	}
	for name, setup := range map[string]func(h *renewHarness){
		"ID token without an email": func(h *renewHarness) { h.noEmail = true; h.groups = []string{"g"} },
		"ID token without groups":   func(h *renewHarness) {},
	} {
		t.Run(name, func(t *testing.T) {
			h := newRenewHarness(t)
			setup(h)
			h.a.userInfoFn = down
			h.age(2 * time.Hour)
			before := h.lastChecked()
			rr, called := h.request()
			if rr.Code != http.StatusOK || !called || h.row().revokedAs != "" {
				t.Fatalf("status %d, ran %v, revoked %q; want served and not revoked", rr.Code, called, h.row().revokedAs)
			}
			if !h.lastChecked().Equal(before) {
				t.Error("a half-checked identity was stamped as a confirmation")
			}
			if n := len(h.resolved()); n != 0 {
				t.Errorf("ResolveUser ran %d times on a half-checked identity", n)
			}
		})
	}

	t.Run("UserInfo answers without groups: genuinely none", func(t *testing.T) {
		h := newRenewHarness(t)
		h.a.userInfoFn = func(context.Context, *oauth2.Token) (userInfoResult, error) {
			return userInfoResult{Subject: renewSubject, Claims: idTokenClaims{Email: "person@example.com"}}, nil
		}
		h.age(time.Hour)
		before := h.lastChecked()
		if rr, called := h.request(); rr.Code != http.StatusOK || !called {
			t.Fatalf("status %d, ran %v", rr.Code, called)
		}
		got := h.resolved()
		if len(got) != 1 || len(got[0].Groups) != 0 {
			t.Fatalf("ResolveUser got %+v; want one call with no groups", got)
		}
		if !h.lastChecked().After(before) {
			t.Error("a complete re-check did not confirm the session")
		}
	})

	t.Run("UserInfo answers and there is still no email: refused", func(t *testing.T) {
		h := newRenewHarness(t)
		h.noEmail = true
		h.a.userInfoFn = func(context.Context, *oauth2.Token) (userInfoResult, error) {
			return userInfoResult{Subject: renewSubject}, nil
		}
		h.age(time.Hour)
		rr, called := h.request()
		if rr.Code != http.StatusUnauthorized || called || h.row().revokedAs != "refused" {
			t.Fatalf("status %d, ran %v, revoked %q; want a refusal", rr.Code, called, h.row().revokedAs)
		}
	})

	t.Run("UserInfo email fills the gap", func(t *testing.T) {
		h := newRenewHarness(t)
		h.noEmail = true
		h.a.userInfoFn = func(context.Context, *oauth2.Token) (userInfoResult, error) {
			return userInfoResult{Subject: renewSubject, Claims: idTokenClaims{Email: "ui@example.com"}}, nil
		}
		h.age(time.Hour)
		if rr, called := h.request(); rr.Code != http.StatusOK || !called {
			t.Fatalf("status %d, ran %v", rr.Code, called)
		}
		if got := h.resolved(); len(got) != 1 || got[0].Email != "ui@example.com" {
			t.Fatalf("ResolveUser got %+v; want the UserInfo email", got)
		}
	})
}

// OIDC Core 5.3.2: a UserInfo answer for another subject is a refusal, on the
// backfill path (a verified ID token) as well as the UserInfo-only path.
func TestRenew_UserInfoSubjectMismatchOnBackfillIsRefused(t *testing.T) {
	h := newRenewHarness(t) // a verified ID token with the right subject, no groups
	h.a.userInfoFn = func(context.Context, *oauth2.Token) (userInfoResult, error) {
		return userInfoResult{Subject: "someone-else", Claims: idTokenClaims{Groups: []string{"mailroom-admin"}}}, nil
	}
	h.age(time.Hour)
	rr, called := h.request()
	if rr.Code != http.StatusUnauthorized || called || h.row().revokedAs != "refused" {
		t.Fatalf("status %d, ran %v, revoked %q; want a refusal", rr.Code, called, h.row().revokedAs)
	}
	if n := len(h.resolved()); n != 0 {
		t.Fatalf("ResolveUser ran %d times with a mismatched UserInfo subject", n)
	}
}

// Renewals in flight are bounded per process; the overflow is served without a
// provider call and is NOT remembered as a failure, so it renews on its next
// request.
func TestRenew_ConcurrencyIsBounded(t *testing.T) {
	h := newRenewHarness(t)
	h.a.cfg.RenewConcurrency = 1
	h.answer = func(w http.ResponseWriter, _ *http.Request, h *renewHarness) {
		time.Sleep(300 * time.Millisecond)
		h.success(w, "", false, "", nil)
	}
	secret, err := mintSessionSecret()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.a.cfg.SessionStore.Create(context.Background(), NewSessionInput{
		UserID: h.userID, TokenHash: hashSessionCookie(secret), RefreshToken: renewOldRT,
	}); err != nil {
		t.Fatal(err)
	}
	second := h.a.newSessionCookie(secret)
	row2 := fakeOf(h.a).rowFor(secret)
	row2.subject = renewSubject
	row2.ref.LastCheckedAt = time.Now().Add(-time.Hour)
	h.age(time.Hour)

	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i, c := range []*http.Cookie{h.cookie, second} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rr, _ := runMiddleware(h.a, c)
			codes[i] = rr.Code
		}()
	}
	wg.Wait()
	if codes[0] != http.StatusOK || codes[1] != http.StatusOK {
		t.Fatalf("statuses %v; both must be served", codes)
	}
	if h.tokenHits() != 1 {
		t.Fatalf("token endpoint hit %d times with concurrency 1, want 1", h.tokenHits())
	}
	// The one that was turned away is not backing off: its next request renews.
	runMiddleware(h.a, h.cookie)
	runMiddleware(h.a, second)
	if h.tokenHits() != 2 {
		t.Fatalf("token endpoint hit %d times after both sessions asked again, want 2", h.tokenHits())
	}
}

// An ID token on the refresh response that does not verify (a JWKS refetch
// failing during key rotation, say) is transient: the rotation is still stored,
// identity is not re-checked from UserInfo alone, and the session is not marked
// confirmed. Falling back to UserInfo could demote an admin whose groups only
// the ID token carries.
func TestRenew_UnverifiableIDTokenIsTransientButKeepsTheRotation(t *testing.T) {
	h := newRenewHarness(t)
	h.answer = func(w http.ResponseWriter, _ *http.Request, _ *renewHarness) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": renewAccessTok, "refresh_token": renewNewRT, "id_token": "not-a-jwt",
		})
	}
	// UserInfo WORKS, and answers without groups: falling back to it is exactly
	// what would demote an admin, so this is the case the rule must refuse.
	h.a.userInfoFn = func(context.Context, *oauth2.Token) (userInfoResult, error) {
		return userInfoResult{Subject: renewSubject, Claims: idTokenClaims{Email: "user@example.com"}}, nil
	}
	h.age(time.Hour)
	before := h.lastChecked()
	rr, called := h.request()
	if rr.Code != http.StatusOK || !called {
		t.Fatalf("status %d, ran %v; want served within the allowance", rr.Code, called)
	}
	if got := h.storedRefresh(); got != renewNewRT {
		t.Fatalf("stored refresh token = %q; the rotation was lost", got)
	}
	if !h.lastChecked().Equal(before) {
		t.Error("an unverifiable ID token marked the session confirmed")
	}
	if n := len(h.resolved()); n != 0 {
		t.Errorf("the resolver ran %d time(s) on an identity that was not verified", n)
	}
}

// A sign-out that lands while a renewal is in phase B must win: phase C sees
// the row revoked and the request is refused, not confirmed.
func TestRenew_SignOutRacingTheRenewalIsRefused(t *testing.T) {
	h := newRenewHarness(t)
	h.rotate = true
	h.age(time.Hour)
	orig := h.a.resolveUser
	h.a.resolveUser = func(ctx context.Context, c Claims) (ulid.ULID, error) {
		// The user signs out on another tab while identity is being re-checked.
		if err := fakeOf(h.a).Revoke(ctx, h.row().ref, revokeSignedOut); err != nil {
			t.Fatal(err)
		}
		return orig(ctx, c)
	}
	rr, called := h.request()
	if rr.Code != http.StatusUnauthorized || called {
		t.Fatalf("status %d, ran %v; want 401: the sign-out raced the renewal and must win", rr.Code, called)
	}
}

// A renewal slot is only ever released by the acquire that took it, and a
// configured concurrency below 1 still admits one renewal at a time.
func TestRenewSlots(t *testing.T) {
	var s renewState
	r1 := s.tryAcquire(0)
	if r1 == nil {
		t.Fatal("concurrency 0 admitted nobody: renewal would silently never run")
	}
	if s.tryAcquire(0) != nil {
		t.Fatal("concurrency 0 admitted two renewals at once")
	}
	r1()
	r1() // a second release of the same slot must not free another
	r2 := s.tryAcquire(0)
	if r2 == nil {
		t.Fatal("the released slot was not reusable")
	}
	if s.tryAcquire(0) != nil {
		t.Fatal("a double release freed a slot it never held")
	}
	r2()
}
