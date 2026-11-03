// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"strings"
	"testing"

	"mailroom/internal/api"
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
