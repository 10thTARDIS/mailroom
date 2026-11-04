// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"mailroom/internal/api"
	"mailroom/internal/auth"
	"mailroom/internal/config"
	"mailroom/internal/oauthserver"
	"mailroom/internal/store"
)

// gateOAuthServer builds the OAuth server the gate tests mount: on when the
// config has a usable public origin, nil (every route 404s) otherwise. Its
// store is never reached: every probe is anonymous, and the handlers that need
// the database are behind the session gate.
func gateOAuthServer(t *testing.T, cfg *config.Config) *oauthserver.Server {
	t.Helper()
	issuer := cfg.OAuthIssuer()
	if issuer == "" {
		return nil
	}
	s, err := oauthserver.New(oauthserver.Config{Issuer: issuer, Store: new(store.Store), Policy: api.OAuthPolicy(),
		Approve: api.ApproveOAuthGrant})
	if err != nil {
		t.Fatalf("oauthserver.New: %v", err)
	}
	return s
}

// With no usable public origin the whole OAuth surface answers 404 and /mcp's
// challenge is the plain one; with one it is on.
func TestOAuthSurfaceOffWithoutPublicURL(t *testing.T) {
	off := devAuthTestConfig()
	if off.OAuthIssuer() != "" {
		t.Fatal("dev-auth test config unexpectedly has an issuer")
	}
	h, _ := gateHandler(t, off)
	for _, p := range []probe{get("/.well-known/oauth-authorization-server"), get("/.well-known/oauth-protected-resource/mcp"),
		get("/.well-known/oauth-protected-resource"), post("/oauth/token"), post("/oauth/revoke"), get("/oauth/authorize?client_id=x"), post("/oauth/consent")} {
		if rec := do(t, h, p); rec.Code != 404 {
			t.Errorf("OAuth off: %s %s = %d, want 404", p.method, p.url, rec.Code)
		}
	}
	on := oidcTestConfig(t)
	if on.OAuthIssuer() != "http://localhost:8080" {
		t.Fatalf("issuer = %q", on.OAuthIssuer())
	}
	h, _ = gateHandler(t, on)
	rec := do(t, h, get("/.well-known/oauth-authorization-server"))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"issuer":"http://localhost:8080"`) {
		t.Errorf("OAuth on: discovery = %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(t, h, post("/oauth/token")); rec.Code != 400 {
		t.Errorf("OAuth on: empty token request = %d, want 400", rec.Code)
	}
	if rec := do(t, h, get("/oauth/authorize?client_id=x")); rec.Code != 302 || !strings.HasPrefix(rec.Header().Get("Location"), loginPath) {
		t.Errorf("OAuth on: anonymous authorize = %d %q", rec.Code, rec.Header().Get("Location"))
	}
	// /mcp with no credential: the resource-metadata challenge when on...
	mcp := do(t, h, post("/mcp"))
	if mcp.Code != 401 || !strings.Contains(mcp.Header().Get("WWW-Authenticate"), `resource_metadata="http://localhost:8080/.well-known/oauth-protected-resource/mcp"`) {
		t.Errorf("OAuth on: /mcp challenge = %d %q", mcp.Code, mcp.Header().Get("WWW-Authenticate"))
	}
	// ...and exactly the old one when the surface is off.
	bare := *on
	bare.OIDCRedirectURL = "https://sso-unused.example.com/auth/callback"
	bare.PublicURL = "http://not-https.example.com"
	if bare.OAuthIssuer() != "" || bare.OAuthIssuerError() == "" {
		t.Fatalf("a non-https public URL must switch OAuth off with a reason: %q %q", bare.OAuthIssuer(), bare.OAuthIssuerError())
	}
	h, _ = gateHandler(t, &bare)
	mcp = do(t, h, post("/mcp"))
	if mcp.Code != 401 || mcp.Header().Get("WWW-Authenticate") != `Bearer realm="mailroom"` {
		t.Errorf("OAuth off: /mcp challenge = %d %q", mcp.Code, mcp.Header().Get("WWW-Authenticate"))
	}
}

// MAILROOM_OAUTH=off turns the whole surface off even with a usable public URL:
// every OAuth route 404s, discovery is gone, /mcp's refusal is the plain one, and
// an OAuth access token is refused with the same uniform 401 as any unknown
// token. The same config with the switch on is the working surface, so the
// off-state assertions cannot be passing because the surface never existed.
func TestOAuthOffSwitch(t *testing.T) {
	// Dev-auth with a public URL: no sign-in redirect stands between an
	// anonymous probe and the OAuth handlers, so a 404 is the OAuth surface's.
	on := devAuthTestConfig()
	on.PublicURL = "https://mail.example.com"
	off := *on
	off.OAuthDisabled = true
	if off.OAuthIssuer() != "" || on.OAuthIssuer() == "" {
		t.Fatalf("issuer: on=%q off=%q", on.OAuthIssuer(), off.OAuthIssuer())
	}

	// Wiring: no server is built when off, one is built when on, and the
	// reason is logged.
	var logs strings.Builder
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	if s, err := buildOAuthServer(new(store.Store), &off, nil, logger); s != nil || err != nil {
		t.Fatalf("off: buildOAuthServer = %v, %v", s, err)
	}
	if got := logs.String(); !strings.Contains(got, config.EnvOAuth) || !strings.Contains(got, "off") {
		t.Errorf("off: the startup log does not give the reason: %q", got)
	}
	logs.Reset()
	if s, err := buildOAuthServer(new(store.Store), on, new(api.API), logger); s == nil || err != nil {
		t.Fatalf("on: buildOAuthServer = %v, %v", s, err)
	}

	// Behaviour, through the real gate.
	token, _, _, err := auth.NewAPIToken()
	if err != nil {
		t.Fatal(err)
	}
	access := strings.Replace(token, auth.APITokenPrefix, auth.OAuthAccessPrefix, 1)
	probeAll := []probe{get("/.well-known/oauth-authorization-server"), get("/.well-known/oauth-protected-resource/mcp"),
		get("/.well-known/oauth-protected-resource"), post("/oauth/token"), post("/oauth/revoke"),
		get("/oauth/authorize?client_id=x"), post("/oauth/consent")}
	hOff, _ := gateHandler(t, &off)
	hOn, _ := gateHandler(t, on)
	for _, p := range probeAll {
		if rec := do(t, hOff, p); rec.Code != 404 {
			t.Errorf("off: %s %s = %d, want 404", p.method, p.url, rec.Code)
		}
		if rec := do(t, hOn, p); rec.Code == 404 {
			t.Errorf("on: %s %s = 404, the control surface is missing", p.method, p.url)
		}
	}
	req := httptest.NewRequest("POST", "/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+access)
	rec := httptest.NewRecorder()
	hOff.ServeHTTP(rec, req)
	if rec.Code != 401 || rec.Header().Get("WWW-Authenticate") != `Bearer realm="mailroom"` {
		t.Errorf("off: /mcp with an OAuth access token = %d %q, want the plain 401", rec.Code, rec.Header().Get("WWW-Authenticate"))
	}
	// The same refusal for an unknown key: resource metadata only when on.
	req = httptest.NewRequest("POST", "/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec = httptest.NewRecorder()
	hOn.ServeHTTP(rec, req)
	if rec.Code != 401 || !strings.Contains(rec.Header().Get("WWW-Authenticate"), "resource_metadata=") {
		t.Errorf("on: /mcp refusal = %d %q, want the resource metadata", rec.Code, rec.Header().Get("WWW-Authenticate"))
	}
}
