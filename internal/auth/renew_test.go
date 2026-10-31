// SPDX-License-Identifier: AGPL-3.0-only

package auth

// Silent session renewal and identity refresh (#937, ADR-048 sections 4 and
// 4a), driven through Middleware against the fake session store and an httptest
// token endpoint. The cross-process half (the advisory lock) is proven against
// PostgreSQL in renew_integration_test.go.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

const (
	renewSubject   = "user-sub-1"
	renewOldRT     = "refresh-token-OLD"
	renewNewRT     = "refresh-token-NEW"
	renewAccessTok = "SENTINEL-ACCESS-TOKEN-5b1d"
)

// renewHarness is a callback harness plus a token endpoint that answers
// refresh_token grants as the test dictates.
type renewHarness struct {
	*callbackHarness
	srv *httptest.Server

	mu       sync.Mutex
	hits     int
	presents []string // refresh tokens presented, in order
	// answer writes the token endpoint's reply; nil means a normal success.
	answer func(w http.ResponseWriter, r *http.Request, h *renewHarness)
	// rotate: the success reply carries renewNewRT.
	rotate bool
	// noIDToken: the success reply carries no id_token.
	noIDToken bool
	// idTokenSub overrides the ID token's subject ("" = renewSubject).
	idTokenSub string
	groups     []string
	// noEmail: the ID token carries no email claim.
	noEmail bool
	// singleUse makes the endpoint behave like Authentik: every success issues
	// a fresh refresh token, and a token presented twice (or never issued) is
	// invalid_grant.
	singleUse bool
	valid     map[string]bool
	issued    int

	userID ulid.ULID
	cookie *http.Cookie
	skew   atomic.Int64 // nanoseconds added to the Authenticator's clock
	logs   interface{ String() string }
}

func newRenewHarness(t *testing.T) *renewHarness {
	t.Helper()
	h := &renewHarness{callbackHarness: newCallbackHarness(t)}
	h.srv = httptest.NewServer(http.HandlerFunc(h.handle))
	t.Cleanup(h.srv.Close)
	h.a.oauth2Config.Endpoint.TokenURL = h.srv.URL + "/token"
	h.a.now = func() time.Time { return time.Now().Add(time.Duration(h.skew.Load())) }
	h.logs = captureLogs(h.a)
	h.userID = h.resolveID

	secret, err := mintSessionSecret()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.a.cfg.SessionStore.Create(context.Background(), NewSessionInput{
		UserID: h.userID, TokenHash: hashSessionCookie(secret), RefreshToken: renewOldRT, IDToken: "old-id-token",
	}); err != nil {
		t.Fatal(err)
	}
	h.cookie = h.a.newSessionCookie(secret)
	h.row().subject = renewSubject
	return h
}

func (h *renewHarness) row() *fakeSessionRow { return fakeOf(h.a).rowFor(h.cookie.Value) }

// age makes the session's last confirmation d old.
func (h *renewHarness) age(d time.Duration) {
	row := h.row()
	fs := fakeOf(h.a)
	fs.mu.Lock()
	defer fs.mu.Unlock()
	row.ref.LastCheckedAt = time.Now().Add(-d)
}

// storedRefresh reads the session's stored refresh token under the fake's lock
// (a renewal may be writing it concurrently).
func (h *renewHarness) storedRefresh() string {
	row := h.row()
	fs := fakeOf(h.a)
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return row.in.RefreshToken
}

func (h *renewHarness) lastChecked() time.Time {
	row := h.row()
	fs := fakeOf(h.a)
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return row.ref.LastCheckedAt
}

func (h *renewHarness) tokenHits() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.hits
}

func (h *renewHarness) handle(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	h.mu.Lock()
	h.hits++
	h.presents = append(h.presents, r.PostFormValue("refresh_token"))
	answer, rotate, noID, sub, groups := h.answer, h.rotate, h.noIDToken, h.idTokenSub, h.groups
	rt := ""
	if rotate {
		rt = renewNewRT
	}
	reused := false
	if h.singleUse {
		if h.valid == nil {
			h.valid = map[string]bool{renewOldRT: true}
		}
		presented := r.PostFormValue("refresh_token")
		reused = !h.valid[presented]
		delete(h.valid, presented)
		if !reused && answer == nil {
			h.issued++
			rt = fmt.Sprintf("rt-issued-%d", h.issued)
			h.valid[rt] = true
		}
	}
	h.mu.Unlock()
	if reused {
		oauthError(http.StatusBadRequest, "invalid_grant", "token already used")(w, r, h)
		return
	}

	if r.PostFormValue("grant_type") != "refresh_token" {
		h.t.Errorf("grant_type = %q, want refresh_token", r.PostFormValue("grant_type"))
	}
	if u, p, ok := r.BasicAuth(); !ok || u != testClientID || p != "test-secret" {
		h.t.Errorf("client credentials not sent as HTTP Basic: %v %q %q", ok, u, p)
	}
	if answer != nil {
		answer(w, r, h)
		return
	}
	h.success(w, rt, noID, sub, groups)
}

// success writes the normal reply; rt "" means no rotation.
func (h *renewHarness) success(w http.ResponseWriter, rt string, noID bool, sub string, groups []string) {
	body := map[string]any{"access_token": renewAccessTok, "token_type": "Bearer", "expires_in": 3600}
	if rt != "" {
		body["refresh_token"] = rt
	}
	if !noID {
		if sub == "" {
			sub = renewSubject
		}
		claims := h.baseClaims(sub, "")
		delete(claims, "nonce")
		if h.noEmail {
			delete(claims, "email")
		}
		if groups != nil {
			claims["groups"] = groups
		}
		body["id_token"] = h.signIDToken(claims)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

// oauthError answers with an RFC 6749 error.
func oauthError(status int, code, desc string) func(http.ResponseWriter, *http.Request, *renewHarness) {
	return func(w http.ResponseWriter, _ *http.Request, _ *renewHarness) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "error_description": desc})
	}
}

func (h *renewHarness) request() (*httptest.ResponseRecorder, bool) {
	return runMiddleware(h.a, h.cookie)
}

func (h *renewHarness) resolved() []Claims {
	return h.resolveArgs
}

func TestRenew_FreshSessionMakesNoProviderCall(t *testing.T) {
	h := newRenewHarness(t)
	h.age(14 * time.Minute)
	rr, called := h.request()
	if rr.Code != http.StatusOK || !called {
		t.Fatalf("status %d, ran %v; want served", rr.Code, called)
	}
	if h.tokenHits() != 0 || fakeOf(h.a).renewCalls != 0 {
		t.Fatalf("a fresh session renewed: %d token hits, %d store renewals", h.tokenHits(), fakeOf(h.a).renewCalls)
	}
	if len(rr.Result().Cookies()) != 0 {
		t.Error("a request that did not renew re-sent the cookie")
	}
}

func TestRenew_StaleSessionRenewsOnceAndResendsTheCookie(t *testing.T) {
	h := newRenewHarness(t)
	h.rotate = true
	h.age(20 * time.Minute)
	before := h.lastChecked()

	rr, called := h.request()
	if rr.Code != http.StatusOK || !called {
		t.Fatalf("status %d, ran %v; want served", rr.Code, called)
	}
	if h.tokenHits() != 1 {
		t.Fatalf("token endpoint hit %d times, want exactly 1", h.tokenHits())
	}
	if h.presents[0] != renewOldRT {
		t.Errorf("presented %q, want the stored refresh token", h.presents[0])
	}
	if !h.lastChecked().After(before) {
		t.Error("last_checked_at was not advanced")
	}
	if got := h.row().in.RefreshToken; got != renewNewRT {
		t.Errorf("stored refresh token = %q, want the rotated one", got)
	}
	var c *http.Cookie
	for _, sc := range rr.Result().Cookies() {
		if sc.Name == DefaultSessionCookieName {
			c = sc
		}
	}
	if c == nil {
		t.Fatal("the cookie was not re-sent after a successful renewal")
	}
	if c.Value != h.cookie.Value {
		t.Error("the re-sent cookie carries a different secret")
	}
	if d := time.Until(c.Expires) - SessionCookieLifetime; d < -time.Minute || d > time.Minute {
		t.Errorf("re-sent cookie Expires = %v, want about 30 days out", c.Expires)
	}

	// The session is fresh now: the next request makes no call.
	h.request()
	if h.tokenHits() != 1 {
		t.Fatalf("a freshly renewed session was renewed again (%d hits)", h.tokenHits())
	}
}

func TestRenew_ProviderThatDoesNotRotateKeepsTheStoredToken(t *testing.T) {
	h := newRenewHarness(t)
	h.age(time.Hour)
	if rr, called := h.request(); rr.Code != http.StatusOK || !called {
		t.Fatalf("status %d, ran %v", rr.Code, called)
	}
	if got := h.row().in.RefreshToken; got != renewOldRT {
		t.Fatalf("stored refresh token = %q, want the old one kept", got)
	}
	if got := h.row().in.IDToken; got == "old-id-token" {
		t.Error("the new ID token was not stored")
	}
}

// 20 concurrent stale requests make one attempt: one store renewal, one
// provider call, all served. Counting store renewals (not only provider calls)
// is what lets this test fail without the singleflight, because the store's own
// lock and re-read would also keep the provider call to one.
func TestRenew_ConcurrentRequestsShareOneAttempt(t *testing.T) {
	h := newRenewHarness(t)
	h.answer = func(w http.ResponseWriter, _ *http.Request, h *renewHarness) {
		time.Sleep(150 * time.Millisecond) // let the burst pile up behind the first
		h.success(w, "", false, "", nil)
	}
	h.age(time.Hour)

	const n = 20
	var wg sync.WaitGroup
	codes := make([]int, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			rr, _ := h.request()
			codes[i] = rr.Code
		}()
	}
	close(start)
	wg.Wait()

	for i, c := range codes {
		if c != http.StatusOK {
			t.Errorf("request %d: status %d, want 200", i, c)
		}
	}
	if got := h.tokenHits(); got != 1 {
		t.Errorf("token endpoint hit %d times, want exactly 1", got)
	}
	if got := fakeOf(h.a).renewCalls; got != 1 {
		t.Errorf("%d store renewals for one burst, want exactly 1 (singleflight)", got)
	}
}

func TestRenew_ProviderRefusalRevokesAndIs401(t *testing.T) {
	for _, code := range []string{"invalid_grant", "interaction_required", "login_required", "consent_required"} {
		t.Run(code, func(t *testing.T) {
			h := newRenewHarness(t)
			h.answer = oauthError(http.StatusBadRequest, code, "the user is gone")
			h.age(time.Hour)
			rr, called := h.request()
			if rr.Code != http.StatusUnauthorized || called {
				t.Fatalf("status %d, ran %v; want 401", rr.Code, called)
			}
			if h.row().revokedAs != "refused" {
				t.Fatalf("revoked as %q, want refused", h.row().revokedAs)
			}
			// And it stays ended.
			if rr, called := h.request(); rr.Code != http.StatusUnauthorized || called {
				t.Fatalf("a refused session was served again: %d, %v", rr.Code, called)
			}
			if h.tokenHits() != 1 {
				t.Errorf("token endpoint hit %d times, want 1", h.tokenHits())
			}
		})
	}
}

// The provider refusing THIS SERVER is not the user's fault: served within the
// allowance, logged at ERROR, refused after it.
func TestRenew_ServerRefusalIsServedWithinTheAllowanceOnly(t *testing.T) {
	for _, code := range []string{"invalid_client", "unauthorized_client", "some_unknown_error"} {
		t.Run(code, func(t *testing.T) {
			h := newRenewHarness(t)
			h.answer = oauthError(http.StatusUnauthorized, code, "bad client")
			h.age(2 * time.Hour)
			rr, called := h.request()
			if rr.Code != http.StatusOK || !called {
				t.Fatalf("within the allowance: status %d, ran %v; want served", rr.Code, called)
			}
			if h.row().revokedAs != "" {
				t.Fatalf("a server-side refusal revoked the session as %q", h.row().revokedAs)
			}
			if !strings.Contains(h.logs.String(), `"level":"ERROR"`) || !strings.Contains(h.logs.String(), code) {
				t.Errorf("a server refusal was not logged at ERROR with its code:\n%s", h.logs.String())
			}

			// Past the allowance the existing expiry path applies.
			h.age(h.a.cfg.OutageAllowance + time.Hour)
			h.skew.Store(int64(2 * time.Minute)) // outside the retry spacing
			rr, called = h.request()
			if rr.Code != http.StatusUnauthorized || called {
				t.Fatalf("past the allowance: status %d, ran %v; want 401", rr.Code, called)
			}
			if h.row().revokedAs != revokeExpired {
				t.Fatalf("revoked as %q, want %q", h.row().revokedAs, revokeExpired)
			}
		})
	}
}

func TestRenew_TransientFailuresAreServedAndNotRevoked(t *testing.T) {
	cases := map[string]func(w http.ResponseWriter, r *http.Request, h *renewHarness){
		"5xx": func(w http.ResponseWriter, _ *http.Request, _ *renewHarness) {
			http.Error(w, "boom", http.StatusBadGateway)
		},
		"429":                            oauthError(http.StatusTooManyRequests, "slow_down", "later"),
		"5xx with an invalid_grant body": oauthError(http.StatusServiceUnavailable, "invalid_grant", "an intermediary"),
		"a redirect": func(w http.ResponseWriter, r *http.Request, _ *renewHarness) {
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
		},
		"a non-OAuth 4xx body": func(w http.ResponseWriter, _ *http.Request, _ *renewHarness) {
			http.Error(w, "<html>nope</html>", http.StatusForbidden)
		},
		"a 200 without an access token": func(w http.ResponseWriter, _ *http.Request, _ *renewHarness) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"token_type":"Bearer"}`))
		},
		"temporarily_unavailable": oauthError(http.StatusBadRequest, "temporarily_unavailable", "later"),
	}
	for name, answer := range cases {
		t.Run(name, func(t *testing.T) {
			h := newRenewHarness(t)
			h.answer = answer
			h.age(2 * time.Hour)
			before := h.lastChecked()
			rr, called := h.request()
			if rr.Code != http.StatusOK || !called {
				t.Fatalf("status %d, ran %v; want served within the allowance", rr.Code, called)
			}
			if h.row().revokedAs != "" {
				t.Fatalf("a transient failure revoked the session as %q", h.row().revokedAs)
			}
			if !h.lastChecked().Equal(before) {
				t.Error("a failed renewal advanced last_checked_at")
			}
			if len(rr.Result().Cookies()) != 0 {
				t.Error("the cookie was re-sent without a confirmation")
			}
			if !strings.Contains(h.logs.String(), `"level":"WARN"`) {
				t.Errorf("a transient failure was not logged at WARN:\n%s", h.logs.String())
			}
		})
	}
}

// A redirect is never followed: the target would receive the refresh token and
// the client secret.
func TestRenew_RedirectIsNotFollowed(t *testing.T) {
	var followed atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { followed.Add(1) }))
	defer other.Close()

	h := newRenewHarness(t)
	h.answer = func(w http.ResponseWriter, r *http.Request, _ *renewHarness) {
		http.Redirect(w, r, other.URL+"/steal", http.StatusTemporaryRedirect)
	}
	h.age(time.Hour)
	if rr, called := h.request(); rr.Code != http.StatusOK || !called {
		t.Fatalf("status %d, ran %v", rr.Code, called)
	}
	if followed.Load() != 0 {
		t.Fatal("the token request followed a redirect and carried its credentials to another host")
	}
}

func TestRenew_TimeoutIsTransient(t *testing.T) {
	h := newRenewHarness(t)
	h.a.cfg.RenewTimeout = 100 * time.Millisecond
	release := make(chan struct{})
	h.answer = func(w http.ResponseWriter, r *http.Request, _ *renewHarness) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}
	defer close(release)
	h.age(time.Hour)
	start := time.Now()
	rr, called := h.request()
	if rr.Code != http.StatusOK || !called {
		t.Fatalf("status %d, ran %v; want served", rr.Code, called)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("the request waited %v; the renewal budget was not enforced", time.Since(start))
	}
	if h.row().revokedAs != "" {
		t.Fatal("a timeout revoked the session")
	}
}

// After a failed attempt there is no new attempt for a minute; after it, there is.
func TestRenew_RetrySpacing(t *testing.T) {
	h := newRenewHarness(t)
	h.answer = func(w http.ResponseWriter, _ *http.Request, _ *renewHarness) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}
	h.age(time.Hour)

	h.request()
	h.request()
	h.request()
	if h.tokenHits() != 1 {
		t.Fatalf("token endpoint hit %d times across requests inside the spacing, want 1", h.tokenHits())
	}
	h.skew.Store(int64(61 * time.Second))
	if rr, called := h.request(); rr.Code != http.StatusOK || !called {
		t.Fatalf("status %d, ran %v", rr.Code, called)
	}
	if h.tokenHits() != 2 {
		t.Fatalf("token endpoint hit %d times after the spacing, want 2", h.tokenHits())
	}
}

func TestRenew_SubjectMismatchIsRefusedBeforeTheResolverRuns(t *testing.T) {
	t.Run("in the ID token", func(t *testing.T) {
		h := newRenewHarness(t)
		h.idTokenSub = "someone-else"
		h.age(time.Hour)
		rr, called := h.request()
		if rr.Code != http.StatusUnauthorized || called {
			t.Fatalf("status %d, ran %v; want 401", rr.Code, called)
		}
		if h.row().revokedAs != "refused" {
			t.Fatalf("revoked as %q, want refused", h.row().revokedAs)
		}
		if n := len(h.resolved()); n != 0 {
			t.Fatalf("ResolveUser was called %d times for a mismatched subject; it must not be", n)
		}
	})
	t.Run("in UserInfo", func(t *testing.T) {
		h := newRenewHarness(t)
		h.noIDToken = true
		h.a.userInfoFn = func(context.Context, *oauth2.Token) (userInfoResult, error) {
			return userInfoResult{Subject: "someone-else", Claims: idTokenClaims{Email: "x@example.com"}}, nil
		}
		h.age(time.Hour)
		rr, called := h.request()
		if rr.Code != http.StatusUnauthorized || called {
			t.Fatalf("status %d, ran %v; want 401", rr.Code, called)
		}
		if h.row().revokedAs != "refused" {
			t.Fatalf("revoked as %q, want refused", h.row().revokedAs)
		}
		if n := len(h.resolved()); n != 0 {
			t.Fatalf("ResolveUser was called %d times for a mismatched subject", n)
		}
	})
}

// The resolver gets the claims the renewal produced, which is how a removed
// admin group takes effect without a new login.
func TestRenew_ResolverReceivesTheRefreshedClaims(t *testing.T) {
	h := newRenewHarness(t)
	h.groups = []string{"mailroom-users"}
	h.age(time.Hour)
	if rr, called := h.request(); rr.Code != http.StatusOK || !called {
		t.Fatalf("status %d, ran %v", rr.Code, called)
	}
	got := h.resolved()
	if len(got) != 1 {
		t.Fatalf("ResolveUser called %d times, want 1", len(got))
	}
	c := got[0]
	if c.Subject != renewSubject || c.Email != "person@example.com" || len(c.Groups) != 1 || c.Groups[0] != "mailroom-users" {
		t.Fatalf("ResolveUser got %+v; want the renewal's subject, email and groups", c)
	}
}

func TestRenew_UserInfoOnlyIdentity(t *testing.T) {
	h := newRenewHarness(t)
	h.noIDToken = true
	h.a.userInfoFn = func(_ context.Context, tok *oauth2.Token) (userInfoResult, error) {
		if tok.AccessToken != renewAccessTok {
			t.Errorf("UserInfo was called with %q, want the new access token", tok.AccessToken)
		}
		return userInfoResult{Subject: renewSubject, Claims: idTokenClaims{
			Email: "ui@example.com", Name: "Ui Person", Groups: []string{"g"},
		}}, nil
	}
	h.age(time.Hour)
	if rr, called := h.request(); rr.Code != http.StatusOK || !called {
		t.Fatalf("status %d, ran %v", rr.Code, called)
	}
	got := h.resolved()
	if len(got) != 1 || got[0].Email != "ui@example.com" || got[0].Name != "Ui Person" {
		t.Fatalf("ResolveUser got %+v; want the UserInfo claims", got)
	}
}

// Neither a verified ID token nor UserInfo: no usable identity. Transient,
// nothing stored, the session NOT marked fresh.
func TestRenew_NoUsableIdentityIsTransientAndNotStamped(t *testing.T) {
	h := newRenewHarness(t)
	h.noIDToken = true
	h.rotate = true
	h.a.userInfoFn = func(context.Context, *oauth2.Token) (userInfoResult, error) {
		return userInfoResult{}, errors.New("userinfo is down")
	}
	h.age(2 * time.Hour)
	before := h.lastChecked()

	rr, called := h.request()
	if rr.Code != http.StatusOK || !called {
		t.Fatalf("status %d, ran %v; want served within the allowance", rr.Code, called)
	}
	if !h.lastChecked().Equal(before) {
		t.Fatal("a renewal that could not re-check identity was stamped as a confirmation")
	}
	// The grant consumed the old token (single-use at Authentik): the rotated
	// one must be stored even though identity could not be re-checked.
	if got := h.row().in.RefreshToken; got != renewNewRT {
		t.Errorf("stored refresh token = %q; the rotated token must be kept (the old one is dead)", got)
	}
	if h.row().revokedAs != "" {
		t.Fatalf("revoked as %q; a missing identity is not a refusal", h.row().revokedAs)
	}
	if n := len(h.resolved()); n != 0 {
		t.Fatalf("ResolveUser ran %d times without an identity", n)
	}
	if len(rr.Result().Cookies()) != 0 {
		t.Error("the cookie was re-sent without a confirmation")
	}
}

func TestRenew_ResolverRefusalsRevoke(t *testing.T) {
	t.Run("error", func(t *testing.T) {
		h := newRenewHarness(t)
		h.resolveErr = fmt.Errorf("%w: account is disabled", ErrRefused)
		h.age(time.Hour)
		rr, called := h.request()
		if rr.Code != http.StatusUnauthorized || called || h.row().revokedAs != "refused" {
			t.Fatalf("status %d, ran %v, revoked %q; want a refusal", rr.Code, called, h.row().revokedAs)
		}
	})
	t.Run("a database error is not a refusal", func(t *testing.T) {
		h := newRenewHarness(t)
		h.resolveErr = errors.New("write tcp 10.0.0.1:5432: connection reset by peer")
		h.age(2 * time.Hour)
		before := h.lastChecked()
		rr, called := h.request()
		if rr.Code != http.StatusOK || !called || h.row().revokedAs != "" {
			t.Fatalf("status %d, ran %v, revoked %q; a resolver failure must not revoke", rr.Code, called, h.row().revokedAs)
		}
		if !h.lastChecked().Equal(before) {
			t.Error("a renewal whose resolver failed was stamped as a confirmation")
		}
	})
	t.Run("cannot provision", func(t *testing.T) {
		h := newRenewHarness(t)
		h.resolveErr = ErrCannotProvision
		h.age(time.Hour)
		if rr, _ := h.request(); rr.Code != http.StatusUnauthorized || h.row().revokedAs != "refused" {
			t.Fatalf("status %d, revoked %q; want a refusal", rr.Code, h.row().revokedAs)
		}
	})
	t.Run("a different user", func(t *testing.T) {
		h := newRenewHarness(t)
		h.resolveID = ulid.Make() // the subject now maps elsewhere
		h.age(time.Hour)
		rr, called := h.request()
		if rr.Code != http.StatusUnauthorized || called || h.row().revokedAs != "refused" {
			t.Fatalf("status %d, ran %v, revoked %q; want a refusal", rr.Code, called, h.row().revokedAs)
		}
		if !strings.Contains(h.logs.String(), `"level":"ERROR"`) {
			t.Errorf("a subject that now maps to another user was not logged at ERROR:\n%s", h.logs.String())
		}
	})
}

// No refresh token: no provider call, no per-request log line, and the session
// lives to the allowance exactly as before renewal existed.
func TestRenew_NoRefreshTokenMakesNoCallAndLivesToTheAllowance(t *testing.T) {
	h := newRenewHarness(t)
	h.row().in.RefreshToken = ""
	h.age(2 * time.Hour)
	for i := 0; i < 3; i++ {
		if rr, called := h.request(); rr.Code != http.StatusOK || !called {
			t.Fatalf("request %d: status %d, ran %v; want served", i, rr.Code, called)
		}
	}
	if h.tokenHits() != 0 {
		t.Fatalf("token endpoint hit %d times for a session with no refresh token", h.tokenHits())
	}
	if strings.TrimSpace(h.logs.String()) != "" {
		t.Errorf("a session with no refresh token logged per request:\n%s", h.logs.String())
	}
	h.age(h.a.cfg.OutageAllowance + time.Minute)
	h.skew.Store(int64(2 * time.Minute))
	if rr, called := h.request(); rr.Code != http.StatusUnauthorized || called {
		t.Fatalf("past the allowance: status %d, ran %v; want 401", rr.Code, called)
	}
}

// A failure of the store itself is neither a refusal nor a 503: served within the
// allowance, never revoked.
func TestRenew_StoreErrorIsServedWithinTheAllowance(t *testing.T) {
	h := newRenewHarness(t)
	fakeOf(h.a).renewErr = errors.New("database is down")
	h.age(2 * time.Hour)
	rr, called := h.request()
	if rr.Code != http.StatusOK || !called {
		t.Fatalf("status %d, ran %v; want served", rr.Code, called)
	}
	if h.row().revokedAs != "" {
		t.Fatalf("a store error revoked the session as %q", h.row().revokedAs)
	}
	if h.tokenHits() != 0 {
		t.Error("the provider was called without the store's lock")
	}
}

// A session the locked re-read finds revoked (another device signed it out
// while this request waited) is refused, with no provider call.
func TestRenew_SessionRevokedWhileWaitingIs401(t *testing.T) {
	h := newRenewHarness(t)
	fakeOf(h.a).renewSeesRevoked = true
	h.age(time.Hour)
	rr, called := h.request()
	if rr.Code != http.StatusUnauthorized || called {
		t.Fatalf("status %d, ran %v; want 401", rr.Code, called)
	}
	if h.tokenHits() != 0 {
		t.Error("the provider was called for a revoked session")
	}
}

// ADR-048 section 4: tokens never reach a log line or an error. This drives
// every renewal outcome with sentinel tokens, including a provider that echoes
// the token it was sent in its error_description.
func TestRenew_TokensNeverReachLogsOrResponses(t *testing.T) {
	const newID = "SENTINEL-NEW-ID-TOKEN"
	echo := func(w http.ResponseWriter, r *http.Request, _ *renewHarness) {
		_ = r.ParseForm()
		oauthError(http.StatusBadRequest, "invalid_client", "bad request for "+r.PostFormValue("refresh_token")+" and test-secret")(w, r, nil)
	}
	answers := map[string]func(http.ResponseWriter, *http.Request, *renewHarness){
		"success": nil,
		"echoed":  echo,
		"refused": func(w http.ResponseWriter, r *http.Request, _ *renewHarness) {
			_ = r.ParseForm()
			oauthError(http.StatusBadRequest, "invalid_grant", r.PostFormValue("refresh_token"))(w, r, nil)
		},
		"garbage id token": func(w http.ResponseWriter, _ *http.Request, _ *renewHarness) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": renewAccessTok, "refresh_token": renewNewRT, "id_token": newID,
			})
		},
	}
	for name, answer := range answers {
		t.Run(name, func(t *testing.T) {
			h := newRenewHarness(t)
			h.rotate = true
			h.answer = answer
			h.row().in.RefreshToken = sentinelRefresh
			h.age(time.Hour)
			rr, _ := h.request()
			captured := h.logs.String() + "\n" + rr.Body.String()
			assertNoSentinel(t, name+": logs and response", captured, sentinelRefresh, renewNewRT, renewAccessTok, newID, "test-secret")
			if strings.TrimSpace(h.logs.String()) == "" && name != "success" {
				t.Error("nothing was logged: the capture is not observing the renewal")
			}
		})
	}
}

func TestPrefersClientSecretPost(t *testing.T) {
	for _, tc := range []struct {
		methods []string
		want    bool
	}{
		{nil, false},
		{[]string{"client_secret_basic", "client_secret_post"}, false},
		{[]string{"client_secret_post"}, true},
		{[]string{"private_key_jwt"}, false},
	} {
		if got := prefersClientSecretPost(tc.methods); got != tc.want {
			t.Errorf("prefersClientSecretPost(%v) = %v, want %v", tc.methods, got, tc.want)
		}
	}
}
