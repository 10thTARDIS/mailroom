// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"golang.org/x/oauth2"
)

func testLoginAuthenticator(t *testing.T) *Authenticator {
	t.Helper()
	a := testAuthenticator(t, true)
	a.oauth2Config = oauth2.Config{
		ClientID:     "test-client",
		ClientSecret: "test-secret",
		RedirectURL:  "https://mail.example.com/auth/callback",
		Endpoint: oauth2.Endpoint{
			AuthURL:  "https://auth.example.com/authorize",
			TokenURL: "https://auth.example.com/token",
		},
		Scopes: DefaultScopes,
	}
	return a
}

func TestLoginHandler_SetsStateCookieAndRedirects(t *testing.T) {
	a := testLoginAuthenticator(t)

	req := httptest.NewRequest(http.MethodGet, "/auth/login?return_to=/inbox", nil)
	rr := httptest.NewRecorder()

	a.LoginHandler(rr, req)

	if rr.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rr.Code)
	}

	loc, err := url.Parse(rr.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	if loc.Scheme+"://"+loc.Host+loc.Path != "https://auth.example.com/authorize" {
		t.Fatalf("Location base = %q, want the configured AuthURL", loc.Scheme+"://"+loc.Host+loc.Path)
	}
	q := loc.Query()
	for _, key := range []string{"state", "nonce", "code_challenge", "code_challenge_method", "client_id", "redirect_uri", "response_type"} {
		if q.Get(key) == "" {
			t.Errorf("redirect URL missing query param %q (got %q)", key, loc.RawQuery)
		}
	}
	if q.Get("code_challenge_method") != "S256" {
		t.Errorf("code_challenge_method = %q, want S256", q.Get("code_challenge_method"))
	}
	if q.Get("response_type") != "code" {
		t.Errorf("response_type = %q, want code", q.Get("response_type"))
	}

	// Recover the cookie and confirm its contents are consistent with the
	// redirect URL: state/nonce match exactly, and the PKCE challenge in
	// the URL is the correct S256 derivation of the verifier in the
	// cookie.
	cookies := rr.Result().Cookies()
	var stateCookie *http.Cookie
	for _, c := range cookies {
		if c.Name == a.cfg.StateCookieName {
			stateCookie = c
		}
	}
	if stateCookie == nil {
		t.Fatal("no login-state cookie was set")
	}
	if !stateCookie.HttpOnly {
		t.Error("state cookie: HttpOnly = false, want true")
	}
	if !stateCookie.Secure {
		t.Error("state cookie: Secure = false, want true (CookieSecure: true)")
	}
	if stateCookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("state cookie: SameSite = %v, want SameSiteLaxMode", stateCookie.SameSite)
	}

	var fs flowState
	if err := decodeSigned(a.cfg.SigningKey, statePurpose, stateCookie.Value, &fs); err != nil {
		t.Fatalf("decodeSigned(state cookie): %v", err)
	}
	if fs.State != q.Get("state") {
		t.Errorf("cookie state %q != URL state %q", fs.State, q.Get("state"))
	}
	if fs.Nonce != q.Get("nonce") {
		t.Errorf("cookie nonce %q != URL nonce %q", fs.Nonce, q.Get("nonce"))
	}
	if fs.ReturnTo != "/inbox" {
		t.Errorf("cookie ReturnTo = %q, want /inbox", fs.ReturnTo)
	}
	wantChallenge := oauth2.S256ChallengeFromVerifier(fs.Verifier)
	if q.Get("code_challenge") != wantChallenge {
		t.Errorf("URL code_challenge = %q, want S256(verifier) = %q", q.Get("code_challenge"), wantChallenge)
	}
}

func TestLoginHandler_UniqueStateAndNonceAcrossCalls(t *testing.T) {
	a := testLoginAuthenticator(t)

	states := make(map[string]bool)
	nonces := make(map[string]bool)
	for i := 0; i < 20; i++ {
		req := httptest.NewRequest(http.MethodGet, "/auth/login", nil)
		rr := httptest.NewRecorder()
		a.LoginHandler(rr, req)

		loc, err := url.Parse(rr.Header().Get("Location"))
		if err != nil {
			t.Fatalf("parse Location: %v", err)
		}
		state := loc.Query().Get("state")
		nonce := loc.Query().Get("nonce")
		if states[state] {
			t.Fatalf("duplicate state generated: %q", state)
		}
		if nonces[nonce] {
			t.Fatalf("duplicate nonce generated: %q", nonce)
		}
		states[state] = true
		nonces[nonce] = true
	}
}

func TestSanitizeReturnTo(t *testing.T) {
	cases := map[string]string{
		"":                      "",
		"/inbox":                "/inbox",
		"/inbox?x=1":            "/inbox?x=1",
		"//evil.example.com":    "",
		"http://evil.example":   "",
		"https://evil.example/": "",
		"not-a-path":            "",
		// A browser reads a backslash as a slash (WHATWG URL): each of these
		// would resolve to another origin if it reached Location.
		`/\evil.example.com`:         "",
		`/\\evil.example.com`:        "",
		`/\/evil.example.com`:        "",
		"/%5Cevil.example.com":       "/%5Cevil.example.com", // escaped: a path segment, not a slash
		"/inbox\t//evil.example.com": "",
		"/inbox\n":                   "",
	}
	for in, want := range cases {
		if got := sanitizeReturnTo(in); got != want {
			t.Errorf("sanitizeReturnTo(%q) = %q, want %q", in, got, want)
		}
	}
}
