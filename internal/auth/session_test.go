// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/xoauth2"
)

func testSigningKey() []byte {
	return []byte("test-signing-key-32-bytes-long!!")
}

// fakeSessions is an in-memory SessionStore that records every call, so tests
// can assert both outcomes and "the store was never asked".
type fakeSessions struct {
	mu   sync.Mutex
	rows map[[32]byte]*fakeSessionRow

	lookupCalls int
	touchCalls  int
	lookupErr   error
	createErr   error
	idTokenErr  error
	refreshErr  error
	revokeErr   error
	touchErr    error

	// renewLock stands in for the per-session advisory lock: one renewal at a
	// time, whatever the number of callers. renewCalls counts Renew calls (not
	// provider calls), so a test can tell a collapsed burst from a serialised one.
	renewLock  sync.Mutex
	renewCalls int
	renewErr   error
	// renewSeesRevoked makes the locked re-read find the row revoked, as a
	// sign-out from another device would while a request waited.
	renewSeesRevoked bool
	confirmCalls     int
	confirmErr       error
}

type fakeSessionRow struct {
	ref        SessionRef
	in         NewSessionInput
	revokedAs  string // "" while live
	subject    string // the user's oidc_subject, for Renew
	renewals   int    // rotations applied
	generation int64
}

func newFakeSessions() *fakeSessions {
	return &fakeSessions{rows: map[[32]byte]*fakeSessionRow{}}
}

func (f *fakeSessions) Lookup(_ context.Context, hash [32]byte) (SessionRef, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lookupCalls++
	if f.lookupErr != nil {
		return SessionRef{}, false, f.lookupErr
	}
	row, ok := f.rows[hash]
	if !ok || row.revokedAs != "" {
		return SessionRef{}, false, nil
	}
	return row.ref, true, nil
}

func (f *fakeSessions) Create(_ context.Context, in NewSessionInput) (ulid.ULID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return ulid.ULID{}, f.createErr
	}
	id := ulid.Make()
	f.rows[in.TokenHash] = &fakeSessionRow{
		ref: SessionRef{ID: id, UserID: in.UserID, LastCheckedAt: time.Now()},
		in:  in,
	}
	return id, nil
}

func (f *fakeSessions) Touch(context.Context, SessionRef, string, time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.touchCalls++
	return f.touchErr
}

func (f *fakeSessions) Revoke(_ context.Context, ref SessionRef, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.revokeErr != nil {
		return f.revokeErr
	}
	for _, row := range f.rows {
		if row.ref.ID == ref.ID && row.revokedAs == "" {
			row.revokedAs = reason
		}
	}
	return nil
}

func (f *fakeSessions) IDToken(_ context.Context, ref SessionRef) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.idTokenErr != nil {
		return "", f.idTokenErr
	}
	for _, row := range f.rows {
		if row.ref.ID == ref.ID {
			return row.in.IDToken, nil
		}
	}
	return "", nil
}

func (f *fakeSessions) RefreshToken(_ context.Context, ref SessionRef) (xoauth2.Secret, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.refreshErr != nil {
		return "", f.refreshErr
	}
	for _, row := range f.rows {
		if row.ref.ID == ref.ID {
			return xoauth2.Secret(row.in.RefreshToken), nil
		}
	}
	return "", nil
}

// revokedAsOf reports why the session was revoked, "" while it is live.
func (f *fakeSessions) revokedAsOf(id ulid.ULID) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, row := range f.rows {
		if row.ref.ID == id {
			return row.revokedAs
		}
	}
	return ""
}

// Renew mirrors the real store's contract: serialised per session, re-reads
// inside the lock, calls do only for a live, still-stale session, and applies the
// decision.
func (f *fakeSessions) Renew(ctx context.Context, ref SessionRef, staleBefore time.Time,
	do func(ctx context.Context, cur RenewCurrent) RenewDecision) (RenewResult, error) {
	f.mu.Lock()
	f.renewCalls++
	renewErr, seesRevoked := f.renewErr, f.renewSeesRevoked
	f.mu.Unlock()
	if renewErr != nil {
		return RenewResult{}, renewErr
	}
	f.renewLock.Lock()
	defer f.renewLock.Unlock()
	if seesRevoked {
		return RenewResult{Revoked: true}, nil
	}

	f.mu.Lock()
	var row *fakeSessionRow
	for _, r := range f.rows {
		if r.ref.ID == ref.ID {
			row = r
		}
	}
	if row == nil || row.revokedAs != "" {
		f.mu.Unlock()
		return RenewResult{Revoked: true}, nil
	}
	if !row.ref.LastCheckedAt.Before(staleBefore) {
		at := row.ref.LastCheckedAt
		f.mu.Unlock()
		return RenewResult{LastCheckedAt: at}, nil
	}
	cur := RenewCurrent{
		RefreshToken: xoauth2.Secret(row.in.RefreshToken), Subject: row.subject, LastCheckedAt: row.ref.LastCheckedAt,
	}
	f.mu.Unlock()

	d := do(ctx, cur)

	f.mu.Lock()
	defer f.mu.Unlock()
	switch d.Kind {
	case RenewRotate:
		if !d.RefreshToken.Empty() {
			row.in.RefreshToken = d.RefreshToken.Reveal()
		}
		if !d.IDToken.Empty() {
			row.in.IDToken = d.IDToken.Reveal()
		}
		row.generation++ // a rotation does not confirm
		row.renewals++
	case RenewRefuse:
		row.revokedAs = "refused"
	}
	return RenewResult{Ran: true, Revoked: row.revokedAs != "", LastCheckedAt: row.ref.LastCheckedAt, Generation: row.generation}, nil
}

// Confirm stamps the session when its generation is still g.
func (f *fakeSessions) Confirm(_ context.Context, ref SessionRef, g int64) (time.Time, bool, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.confirmCalls++
	if f.confirmErr != nil {
		return time.Time{}, false, false, f.confirmErr
	}
	for _, r := range f.rows {
		if r.ref.ID != ref.ID {
			continue
		}
		if r.revokedAs != "" {
			return time.Time{}, false, true, nil
		}
		if r.generation == g {
			r.ref.LastCheckedAt = time.Now()
			return r.ref.LastCheckedAt, true, false, nil
		}
		return time.Time{}, false, false, nil
	}
	return time.Time{}, false, true, nil
}

// count returns the number of rows (live or revoked).
func (f *fakeSessions) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.rows)
}

// rowFor finds the row for a cookie value.
func (f *fakeSessions) rowFor(cookieValue string) *fakeSessionRow {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rows[hashSessionCookie(cookieValue)]
}

// fakeOf returns a's fake session store.
func fakeOf(a *Authenticator) *fakeSessions { return a.cfg.SessionStore.(*fakeSessions) }

func testAuthenticator(t *testing.T, secure bool) *Authenticator {
	t.Helper()
	cfg := Config{
		SigningKey:        testSigningKey(),
		CookieSecure:      secure,
		SessionCookieName: DefaultSessionCookieName,
		StateCookieName:   DefaultStateCookieName,
		OutageAllowance:   DefaultOutageAllowance,
		StateTTL:          DefaultStateTTL,
		SessionStore:      newFakeSessions(),
	}
	return &Authenticator{cfg: cfg.withDefaults()}
}

// signIn creates a live session for userID in a's store and returns the cookie
// that authenticates it.
func signIn(t *testing.T, a *Authenticator, userID ulid.ULID, idToken string) *http.Cookie {
	t.Helper()
	secret, err := mintSessionSecret()
	if err != nil {
		t.Fatalf("mintSessionSecret: %v", err)
	}
	if _, err := a.cfg.SessionStore.Create(context.Background(), NewSessionInput{
		UserID: userID, TokenHash: hashSessionCookie(secret), IDToken: idToken,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	return a.newSessionCookie(secret)
}

func TestSessionCookie_FormatAndFlags(t *testing.T) {
	a := testAuthenticator(t, true)
	secret, err := mintSessionSecret()
	if err != nil {
		t.Fatalf("mintSessionSecret: %v", err)
	}
	if !wellFormedSessionCookie(secret) {
		t.Fatalf("a freshly minted secret %q is not well-formed", secret)
	}
	if !strings.HasPrefix(secret, "s1.") || len(secret) != 3+52 {
		t.Fatalf("secret %q: want \"s1.\" + 52 base32 chars", secret)
	}
	if secret != strings.ToLower(secret) || strings.ContainsAny(secret, "=") {
		t.Errorf("secret %q must be lowercase base32 without padding", secret)
	}

	c := a.newSessionCookie(secret)
	if c.Name != DefaultSessionCookieName {
		t.Errorf("Name = %q, want %q", c.Name, DefaultSessionCookieName)
	}
	if !c.HttpOnly {
		t.Error("HttpOnly = false, want true")
	}
	if !c.Secure {
		t.Error("Secure = false, want true (CookieSecure: true)")
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v, want Lax", c.SameSite)
	}
	if c.Path != "/" {
		t.Errorf("Path = %q, want /", c.Path)
	}
	want := time.Now().Add(SessionCookieLifetime)
	if d := c.Expires.Sub(want); d < -time.Minute || d > time.Minute {
		t.Errorf("Expires = %v, want about %v (30 days)", c.Expires, want)
	}
	if SessionCookieLifetime != 30*24*time.Hour {
		t.Errorf("SessionCookieLifetime = %v, want 30 days", SessionCookieLifetime)
	}

	if insecure := testAuthenticator(t, false).newSessionCookie(secret); insecure.Secure {
		t.Error("Secure = true with CookieSecure: false")
	}
}

func TestMintSessionSecret_Unique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 500; i++ {
		s, err := mintSessionSecret()
		if err != nil {
			t.Fatal(err)
		}
		if seen[s] {
			t.Fatalf("duplicate secret %q", s)
		}
		seen[s] = true
	}
}

func TestWellFormedSessionCookie(t *testing.T) {
	good := "s1." + strings.Repeat("a", 52)
	if !wellFormedSessionCookie(good) {
		t.Fatalf("%q should be well-formed", good)
	}
	for name, v := range map[string]string{
		"empty":         "",
		"legacy signed": "eyJ1aWQiOiIwMUFCQyIsImV4cCI6MX0.c2lnbmF0dXJl",
		"wrong prefix":  "s2." + strings.Repeat("a", 52),
		"too short":     "s1." + strings.Repeat("a", 51),
		"too long":      "s1." + strings.Repeat("a", 53),
		"uppercase":     "s1." + strings.Repeat("A", 52),
		"bad alphabet":  "s1." + strings.Repeat("1", 52),
		"padding":       "s1." + strings.Repeat("a", 51) + "=",
	} {
		if wellFormedSessionCookie(v) {
			t.Errorf("%s: %q accepted", name, v)
		}
	}
}

// run drives Middleware with one request and reports the response and whether
// the wrapped handler ran.
func runMiddleware(a *Authenticator, c *http.Cookie) (*httptest.ResponseRecorder, bool) {
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	})
	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	if c != nil {
		req.AddCookie(c)
	}
	rr := httptest.NewRecorder()
	a.Middleware(next).ServeHTTP(rr, req)
	return rr, called
}

func TestSession_LegacyAndGarbageCookiesRefusedWithoutAStoreCall(t *testing.T) {
	a := testAuthenticator(t, true)
	// A cookie in the retired signed format: base64 payload, dot, base64 MAC.
	legacy, err := encodeSigned(a.cfg.SigningKey, statePurpose, map[string]string{"uid": ulid.Make().String()})
	if err != nil {
		t.Fatal(err)
	}
	for name, v := range map[string]string{"legacy": legacy, "garbage": "garbage", "short": "s1.abc"} {
		rr, called := runMiddleware(a, &http.Cookie{Name: a.cfg.SessionCookieName, Value: v})
		if rr.Code != http.StatusUnauthorized || called {
			t.Errorf("%s: status %d, handler ran %v; want 401 and not run", name, rr.Code, called)
		}
	}
	if n := fakeOf(a).lookupCalls; n != 0 {
		t.Fatalf("the store was asked %d times about cookies that cannot be sessions; want 0", n)
	}
}

func TestSession_UnknownHashIs401(t *testing.T) {
	a := testAuthenticator(t, true)
	secret, _ := mintSessionSecret()
	rr, called := runMiddleware(a, a.newSessionCookie(secret))
	if rr.Code != http.StatusUnauthorized || called {
		t.Fatalf("status %d, ran %v; want 401 and not run", rr.Code, called)
	}
	if fakeOf(a).lookupCalls != 1 {
		t.Fatalf("lookupCalls = %d, want 1", fakeOf(a).lookupCalls)
	}
}

// A database error during the check is 503, NOT 401: the web client reads a 401
// as "session expired" and bounces every open tab to sign-in (ADR-048 section 3).
func TestSession_StoreErrorIs503AndTheCookieIsNotCleared(t *testing.T) {
	a := testAuthenticator(t, true)
	c := signIn(t, a, ulid.Make(), "")
	fakeOf(a).lookupErr = errors.New("connection refused")

	rr, called := runMiddleware(a, c)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 on a store error", rr.Code)
	}
	if called {
		t.Fatal("the wrapped handler ran on a store error")
	}
	if got := rr.Result().Cookies(); len(got) != 0 {
		t.Fatalf("a store error set cookies %v; the session cookie must be left alone", got)
	}

	// The same error on a redirecting (browser) route is also 503, not a redirect
	// to sign-in: a blip must not bounce the browser either.
	req := httptest.NewRequest(http.MethodGet, "/inbox", nil)
	req.AddCookie(c)
	rr2 := httptest.NewRecorder()
	a.Middleware(http.NotFoundHandler(), WithUnauthenticatedRedirect("/auth/login")).ServeHTTP(rr2, req)
	if rr2.Code != http.StatusServiceUnavailable {
		t.Fatalf("browser route status = %d, want 503", rr2.Code)
	}
}

func TestSession_RevokedIsRefused(t *testing.T) {
	a := testAuthenticator(t, true)
	c := signIn(t, a, ulid.Make(), "")
	row := fakeOf(a).rowFor(c.Value)
	if err := a.cfg.SessionStore.Revoke(context.Background(), row.ref, revokeSignedOut); err != nil {
		t.Fatal(err)
	}
	rr, called := runMiddleware(a, c)
	if rr.Code != http.StatusUnauthorized || called {
		t.Fatalf("status %d, ran %v; want 401 for a revoked session", rr.Code, called)
	}
}

// A session whose last confirmation is older than the outage allowance is
// refused AND revoked as expired.
func TestSession_ExpiredIsRefusedAndRevoked(t *testing.T) {
	a := testAuthenticator(t, true)
	c := signIn(t, a, ulid.Make(), "")
	row := fakeOf(a).rowFor(c.Value)
	row.ref.LastCheckedAt = time.Now().Add(-a.cfg.OutageAllowance - time.Minute)

	rr, called := runMiddleware(a, c)
	if rr.Code != http.StatusUnauthorized || called {
		t.Fatalf("status %d, ran %v; want 401 for an expired session", rr.Code, called)
	}
	if row.revokedAs != revokeExpired {
		t.Fatalf("revoked as %q, want %q", row.revokedAs, revokeExpired)
	}
	if fakeOf(a).touchCalls != 0 {
		t.Fatal("an expired session was touched")
	}
}

// Just inside the allowance still works, and is touched.
func TestSession_WithinAllowanceIsServedAndTouched(t *testing.T) {
	a := testAuthenticator(t, true)
	c := signIn(t, a, ulid.Make(), "")
	row := fakeOf(a).rowFor(c.Value)
	row.ref.LastCheckedAt = time.Now().Add(-a.cfg.OutageAllowance + time.Minute)

	rr, called := runMiddleware(a, c)
	if rr.Code != http.StatusOK || !called {
		t.Fatalf("status %d, ran %v; want served", rr.Code, called)
	}
	if fakeOf(a).touchCalls != 1 {
		t.Fatalf("touchCalls = %d, want 1", fakeOf(a).touchCalls)
	}
	if row.revokedAs != "" {
		t.Fatalf("a live session was revoked as %q", row.revokedAs)
	}
}

func TestSession_PutsUserAndSessionInContext(t *testing.T) {
	a := testAuthenticator(t, true)
	want := ulid.Make()
	c := signIn(t, a, want, "")

	var gotUser ulid.ULID
	var gotUserOK bool
	var gotRef SessionRef
	var gotRefOK bool
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser, gotUserOK = UserIDFromContext(r.Context())
		gotRef, gotRefOK = SessionFromContext(r.Context())
	})
	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	req.AddCookie(c)
	a.Middleware(next).ServeHTTP(httptest.NewRecorder(), req)

	if !gotUserOK || gotUser != want {
		t.Fatalf("UserIDFromContext = %v, %v; want %v", gotUser, gotUserOK, want)
	}
	if !gotRefOK || gotRef.UserID != want || gotRef.ID != fakeOf(a).rowFor(c.Value).ref.ID {
		t.Fatalf("SessionFromContext = %+v, %v", gotRef, gotRefOK)
	}
	if _, ok := SessionFromContext(context.Background()); ok {
		t.Error("SessionFromContext reported a session on a bare context")
	}
}

func TestClearedSessionCookie(t *testing.T) {
	c := testAuthenticator(t, true).clearedSessionCookie()
	if c.Value != "" || c.MaxAge >= 0 {
		t.Fatalf("not a clearing cookie: %+v", c)
	}
}

// Recording use is bookkeeping: a Touch failure is logged and the request is
// still served, because the session itself was found live.
func TestSession_TouchFailureStillServes(t *testing.T) {
	a := testAuthenticator(t, true)
	c := signIn(t, a, ulid.Make(), "")
	fakeOf(a).touchErr = errors.New("database is down")
	rr, called := runMiddleware(a, c)
	if rr.Code != http.StatusOK || !called {
		t.Fatalf("a Touch failure refused the request: status %d, handler called %v", rr.Code, called)
	}
	if fakeOf(a).touchCalls == 0 {
		t.Fatal("Touch was never called, so this test proves nothing")
	}
}
