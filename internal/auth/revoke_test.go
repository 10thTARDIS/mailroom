// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
	"golang.org/x/oauth2"

	"mailroom/internal/xoauth2"
)

// RFC 7009 revocation of a session's refresh token (#937, ADR-048 section 5).

const (
	revokeClientID     = "mailroom-test-client"
	revokeClientSecret = "SENTINEL-CLIENT-SECRET-41d2"
)

// revokeCall is one request the stand-in revocation endpoint received.
type revokeCall struct {
	form       url.Values
	authHeader string
}

// revokeProvider is a stand-in provider with a revocation endpoint. onCall runs
// inside the handler, so a test can inspect the world at the moment the call
// arrives (for instance "was the row already revoked?").
type revokeProvider struct {
	srv    *httptest.Server
	mu     sync.Mutex
	calls  []revokeCall
	status int
	body   string
	onCall func()
}

func newRevokeProvider(t *testing.T) *revokeProvider {
	t.Helper()
	p := &revokeProvider{status: http.StatusOK, body: "{}"}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		p.mu.Lock()
		p.calls = append(p.calls, revokeCall{form: r.PostForm, authHeader: r.Header.Get("Authorization")})
		status, body, onCall := p.status, p.body, p.onCall
		p.mu.Unlock()
		if onCall != nil {
			onCall()
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *revokeProvider) callList() []revokeCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]revokeCall(nil), p.calls...)
}

// revokeAuthenticator is an Authenticator whose provider publishes endpoint as
// its revocation endpoint ("" for none).
func revokeAuthenticator(t *testing.T, endpoint string, post bool) *Authenticator {
	t.Helper()
	cfg := Config{
		SigningKey:   testSigningKey(),
		ClientID:     revokeClientID,
		SessionStore: newFakeSessions(),
	}
	return &Authenticator{
		cfg: cfg.withDefaults(),
		oauth2Config: oauth2.Config{
			ClientID: revokeClientID, ClientSecret: revokeClientSecret,
			Endpoint: oauth2.Endpoint{TokenURL: "https://idp.example.com/token"},
		},
		revocationEndpoint: endpoint,
		tokenAuthPost:      post,
		postLogoutRedirect: "/signin?notice=signed_out",
	}
}

func TestRevokeRefreshToken_PostsTheFormWithBasicClientAuth(t *testing.T) {
	p := newRevokeProvider(t)
	a := revokeAuthenticator(t, p.srv.URL+"/revoke", false)

	if err := a.RevokeRefreshToken(context.Background(), xoauth2.Secret(sentinelRefresh)); err != nil {
		t.Fatalf("RevokeRefreshToken: %v", err)
	}
	calls := p.callList()
	if len(calls) != 1 {
		t.Fatalf("provider saw %d calls, want 1", len(calls))
	}
	c := calls[0]
	if c.form.Get("token") != sentinelRefresh || c.form.Get("token_type_hint") != "refresh_token" {
		t.Fatalf("form = %v, want token and token_type_hint=refresh_token", c.form)
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte(url.QueryEscape(revokeClientID)+":"+url.QueryEscape(revokeClientSecret)))
	if c.authHeader != want {
		t.Fatalf("Authorization = %q, want HTTP Basic client authentication", c.authHeader)
	}
	if c.form.Get("client_secret") != "" || c.form.Get("client_id") != "" {
		t.Fatalf("the client credentials were also put in the body: %v", c.form)
	}
}

func TestRevokeRefreshToken_ClientSecretPostWhenOnlyThatIsAdvertised(t *testing.T) {
	p := newRevokeProvider(t)
	a := revokeAuthenticator(t, p.srv.URL+"/revoke", true)

	if err := a.RevokeRefreshToken(context.Background(), xoauth2.Secret(sentinelRefresh)); err != nil {
		t.Fatalf("RevokeRefreshToken: %v", err)
	}
	c := p.callList()[0]
	if c.authHeader != "" {
		t.Fatalf("Authorization = %q, want none with client_secret_post", c.authHeader)
	}
	if c.form.Get("client_id") != revokeClientID || c.form.Get("client_secret") != revokeClientSecret {
		t.Fatalf("form = %v, want the client credentials in the body", c.form)
	}
}

// The same choice as renewal makes, from the same code: the request builder is
// shared, so this pins that revocation cannot drift from it.
func TestTokenEndpointRequest_IsTheOneClientAuthChoice(t *testing.T) {
	for _, post := range []bool{false, true} {
		a := revokeAuthenticator(t, "", post)
		req, err := a.tokenEndpointRequest(context.Background(), "https://idp.example.com/x", url.Values{"a": {"b"}})
		if err != nil {
			t.Fatal(err)
		}
		_, _, hasBasic := req.BasicAuth()
		if hasBasic == post {
			t.Errorf("tokenAuthPost=%v: Basic auth present = %v", post, hasBasic)
		}
	}
}

func TestRevokeRefreshToken_NoEndpointIsANoOp(t *testing.T) {
	p := newRevokeProvider(t)
	a := revokeAuthenticator(t, "", false)
	if err := a.RevokeRefreshToken(context.Background(), xoauth2.Secret(sentinelRefresh)); err != nil {
		t.Fatalf("RevokeRefreshToken without an endpoint: %v", err)
	}
	if n := len(p.callList()); n != 0 {
		t.Fatalf("provider saw %d calls with no revocation_endpoint configured", n)
	}
	// An empty token is nothing to revoke either, and must not be sent.
	a = revokeAuthenticator(t, p.srv.URL+"/revoke", false)
	if err := a.RevokeRefreshToken(context.Background(), ""); err != nil || len(p.callList()) != 0 {
		t.Fatalf("an empty token was sent: err=%v calls=%d", err, len(p.callList()))
	}
}

// A redirect would carry the refresh token and the client secret to wherever it
// points. It is not followed.
func TestRevokeRefreshToken_DoesNotFollowRedirects(t *testing.T) {
	var landed bool
	var mu sync.Mutex
	elsewhere := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		mu.Lock()
		landed = true
		mu.Unlock()
	}))
	defer elsewhere.Close()
	redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/steal", http.StatusTemporaryRedirect)
	}))
	defer redirecting.Close()

	a := revokeAuthenticator(t, redirecting.URL+"/revoke", false)
	err := a.RevokeRefreshToken(context.Background(), xoauth2.Secret(sentinelRefresh))
	if err == nil {
		t.Fatal("a redirect answer was treated as success")
	}
	mu.Lock()
	defer mu.Unlock()
	if landed {
		t.Fatal("the redirect was followed: the token and client secret reached another host")
	}
	assertNoSentinel(t, "the redirect error", err.Error(), sentinelRefresh, revokeClientSecret)
}

func TestRevokeRefreshToken_IsBounded(t *testing.T) {
	old := revokeTimeout
	revokeTimeout = 150 * time.Millisecond
	defer func() { revokeTimeout = old }()

	release := make(chan struct{})
	hang := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	defer hang.Close()
	defer close(release)

	a := revokeAuthenticator(t, hang.URL+"/revoke", false)
	start := time.Now()
	err := a.RevokeRefreshToken(context.Background(), xoauth2.Secret(sentinelRefresh))
	if err == nil {
		t.Fatal("a hanging endpoint reported success")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("took %v against a hung endpoint, want about the bound", d)
	}
	assertNoSentinel(t, "the timeout error", err.Error(), sentinelRefresh, revokeClientSecret)
}

func TestRevokeRefreshToken_ErrorsCarryOnlyAStatusAndAValidatedCode(t *testing.T) {
	p := newRevokeProvider(t)
	p.mu.Lock()
	p.status = http.StatusBadRequest
	p.body = `{"error":"unsupported_token_type","error_description":"echo ` + sentinelRefresh + ` and ` + revokeClientSecret + `"}`
	p.mu.Unlock()
	a := revokeAuthenticator(t, p.srv.URL+"/revoke", false)

	err := a.RevokeRefreshToken(context.Background(), xoauth2.Secret(sentinelRefresh))
	if err == nil {
		t.Fatal("a 400 was treated as success")
	}
	if !strings.Contains(err.Error(), "400") || !strings.Contains(err.Error(), "unsupported_token_type") {
		t.Fatalf("error = %q, want the status and the OAuth error code", err)
	}
	assertNoSentinel(t, "the error", err.Error(), sentinelRefresh, revokeClientSecret)
}

// revokeLogoutRig is a signed-in session with a refresh token, on an
// Authenticator whose provider publishes endpoint as its revocation endpoint.
func revokeLogoutRig(t *testing.T, endpoint string) (*Authenticator, *http.Cookie) {
	t.Helper()
	a := revokeAuthenticator(t, endpoint, false)
	secret, err := mintSessionSecret()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.cfg.SessionStore.Create(context.Background(), NewSessionInput{
		UserID: ulid.Make(), TokenHash: hashSessionCookie(secret),
		RefreshToken: sentinelRefresh, IDToken: "header.payload.sig",
	}); err != nil {
		t.Fatal(err)
	}
	return a, a.newSessionCookie(secret)
}

func TestLogout_RevokesTheRefreshTokenAfterTheRowIsRevoked(t *testing.T) {
	p := newRevokeProvider(t)
	a, c := revokeLogoutRig(t, p.srv.URL+"/revoke")
	row := fakeOf(a).rowFor(c.Value)

	var revokedWhenCalled string
	p.mu.Lock()
	p.onCall = func() { revokedWhenCalled = fakeOf(a).revokedAsOf(row.ref.ID) }
	p.mu.Unlock()

	doLogout(t, a, c)

	calls := p.callList()
	if len(calls) != 1 || calls[0].form.Get("token") != sentinelRefresh {
		t.Fatalf("provider calls = %+v, want exactly one revoking the session's refresh token", calls)
	}
	if revokedWhenCalled != revokeSignedOut {
		t.Fatalf("at the moment the provider was called the row was %q, want %q: the call must come AFTER the revoke",
			revokedWhenCalled, revokeSignedOut)
	}
}

func TestLogout_NoRevocationEndpointMakesNoCall(t *testing.T) {
	p := newRevokeProvider(t)
	a, c := revokeLogoutRig(t, "")
	doLogout(t, a, c)
	if n := len(p.callList()); n != 0 {
		t.Fatalf("provider saw %d calls although discovery published no revocation_endpoint", n)
	}
	if got := fakeOf(a).rowFor(c.Value).revokedAs; got != revokeSignedOut {
		t.Fatalf("session revoked as %q, want %q", got, revokeSignedOut)
	}
}

func TestLogout_AFailingRevocationEndpointDoesNotBlockSignOut(t *testing.T) {
	p := newRevokeProvider(t)
	p.mu.Lock()
	p.status, p.body = http.StatusInternalServerError, `{"error":"server_error"}`
	p.mu.Unlock()
	a, c := revokeLogoutRig(t, p.srv.URL+"/revoke")
	logs := captureLogs(a)

	loc, cookies := doLogout(t, a, c)

	if len(p.callList()) != 1 {
		t.Fatalf("the failing endpoint was called %d times, want 1", len(p.callList()))
	}
	if loc != "/signin?notice=signed_out" {
		t.Fatalf("Location = %q: the sign-out did not complete", loc)
	}
	cleared := false
	for _, ck := range cookies {
		if ck.Name == a.cfg.SessionCookieName && ck.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatal("the session cookie was not cleared")
	}
	if got := fakeOf(a).rowFor(c.Value).revokedAs; got != revokeSignedOut {
		t.Fatalf("session revoked as %q, want %q", got, revokeSignedOut)
	}
	// It was logged (so an operator can see it), and without the token.
	if !strings.Contains(logs.String(), "revoking the refresh token at the provider failed") {
		t.Fatalf("the failure was not logged:\n%s", logs)
	}
	assertNoSentinel(t, "the logs", logs.String(), sentinelRefresh, revokeClientSecret)
}

// A row that could not be revoked here must not have its provider grant revoked:
// the session would stay usable with a dead token.
func TestLogout_NoProviderCallWhenTheRowCouldNotBeRevoked(t *testing.T) {
	p := newRevokeProvider(t)
	a, c := revokeLogoutRig(t, p.srv.URL+"/revoke")
	fakeOf(a).revokeErr = context.DeadlineExceeded
	doLogout(t, a, c)
	if n := len(p.callList()); n != 0 {
		t.Fatalf("provider saw %d calls although the row was not revoked", n)
	}
}

func TestLogout_UnreadableRefreshTokenStillSignsOut(t *testing.T) {
	p := newRevokeProvider(t)
	a, c := revokeLogoutRig(t, p.srv.URL+"/revoke")
	fakeOf(a).refreshErr = context.DeadlineExceeded
	doLogout(t, a, c)
	if n := len(p.callList()); n != 0 {
		t.Fatalf("provider saw %d calls although the token could not be read", n)
	}
	if got := fakeOf(a).rowFor(c.Value).revokedAs; got != revokeSignedOut {
		t.Fatalf("session revoked as %q, want %q", got, revokeSignedOut)
	}
}
