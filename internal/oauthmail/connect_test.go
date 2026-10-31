// SPDX-License-Identifier: AGPL-3.0-only

package oauthmail

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"mailroom/internal/xoauth2"
)

// Unit tests for the connect flow's pure parts: the ID-token checks, scopes,
// the public origin, the code exchange and revocation requests, the reject
// hook and Forget on the Manager. The flow end to end (state, replay,
// cross-user, mix-up, re-auth, disconnect) runs against a real database in
// connect_integration_test.go.

const (
	testClientID  = "client-id"
	googleSubject = "112233445566778899000"
	msTenant      = "72f988bf-86f1-41af-91ab-2d7cd011db47"
)

// makeJWT builds an UNSIGNED-in-effect JWS (a junk signature): the connect
// flow does not verify signatures (idtoken.go explains why), so the tests
// must not pretend it does.
func makeJWT(t *testing.T, claims map[string]any) xoauth2.Secret {
	t.Helper()
	hdr := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	b, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return xoauth2.Secret(hdr + "." + base64.RawURLEncoding.EncodeToString(b) + ".c2lnbmF0dXJl")
}

func googleClaims(now time.Time, nonce string) map[string]any {
	return map[string]any{
		"iss": "https://accounts.google.com", "aud": testClientID, "azp": testClientID,
		"exp": now.Add(time.Hour).Unix(), "iat": now.Unix(), "nonce": nonce,
		"sub": googleSubject, "email": "You@Example.com", "email_verified": true,
	}
}

func msClaims(now time.Time, nonce, tid string) map[string]any {
	return map[string]any{
		"iss": "https://login.microsoftonline.com/" + tid + "/v2.0", "aud": testClientID,
		"exp": now.Add(time.Hour).Unix(), "iat": now.Unix(), "nonce": nonce,
		"sub": "AAAAAAAAAAAAAAAAAAAAAIkzqFVrSaSaFHy782bbtaQ", "tid": tid,
		"preferred_username": "you@example.com",
	}
}

func TestVerifyIDToken(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	g := Google(Client{ID: testClientID, Secret: "s"})
	ms, _ := Microsoft(Client{ID: testClientID, Secret: "s"}, "common")

	id, err := g.verifyIDToken(makeJWT(t, googleClaims(now, "n1")), "n1", now)
	if err != nil || id.Subject != googleSubject || id.Address != "you@example.com" {
		t.Fatalf("google happy path = %+v, %v", id, err)
	}
	id, err = ms.verifyIDToken(makeJWT(t, msClaims(now, "n1", msTenant)), "n1", now)
	if err != nil || id.Address != "you@example.com" {
		t.Fatalf("microsoft happy path = %+v, %v", id, err)
	}

	mut := func(base map[string]any, k string, v any) map[string]any {
		out := map[string]any{}
		for kk, vv := range base {
			out[kk] = vv
		}
		if v == nil {
			delete(out, k)
		} else {
			out[k] = v
		}
		return out
	}
	gc := googleClaims(now, "n1")
	mc := msClaims(now, "n1", msTenant)
	for _, tc := range []struct {
		name   string
		p      Provider
		claims map[string]any
		nonce  string
	}{
		{"google: wrong issuer", g, mut(gc, "iss", "https://evil.example"), "n1"},
		{"google: a Microsoft token at the Google callback", g, mc, "n1"},
		{"microsoft: a Google token at the Microsoft callback", ms, gc, "n1"},
		{"google: another client's token", g, mut(gc, "aud", "someone-else"), "n1"},
		{"google: several audiences without azp", g, mut(mut(gc, "aud", []string{testClientID, "x"}), "azp", nil), "n1"},
		{"google: expired", g, mut(gc, "exp", now.Add(-time.Hour).Unix()), "n1"},
		{"google: no expiry", g, mut(gc, "exp", nil), "n1"},
		{"google: issued in the future", g, mut(gc, "iat", now.Add(time.Hour).Unix()), "n1"},
		{"google: nonce mismatch (a replayed id_token)", g, gc, "n2"},
		{"google: no nonce", g, mut(gc, "nonce", nil), "n1"},
		{"google: unverified email", g, mut(gc, "email_verified", false), "n1"},
		{"google: no subject", g, mut(gc, "sub", nil), "n1"},
		{"google: subject with spaces", g, mut(gc, "sub", "a b"), "n1"},
		{"google: no address", g, mut(gc, "email", nil), "n1"},
		{"google: address with a control separator", g, mut(gc, "email", "a\x01b@example.com"), "n1"},
		{"microsoft: tid disagrees with issuer", ms, mut(mc, "tid", "11111111-1111-1111-1111-111111111111"), "n1"},
		{"microsoft: v1 issuer", ms, mut(mc, "iss", "https://sts.windows.net/"+msTenant+"/"), "n1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tc.p.verifyIDToken(makeJWT(t, tc.claims), tc.nonce, now); !errors.Is(err, errIdentity) {
				t.Fatalf("accepted (err=%v)", err)
			}
		})
	}
	if _, err := g.verifyIDToken("not-a-jwt", "n1", now); !errors.Is(err, errIdentity) {
		t.Error("a non-JWT was accepted")
	}
	if _, err := g.verifyIDToken("", "n1", now); !errors.Is(err, errIdentity) {
		t.Error("an absent id_token was accepted")
	}

	// Tenant narrowing.
	consumers, _ := Microsoft(Client{ID: testClientID, Secret: "s"}, "consumers")
	orgs, _ := Microsoft(Client{ID: testClientID, Secret: "s"}, "organizations")
	one, _ := Microsoft(Client{ID: testClientID, Secret: "s"}, msTenant)
	personal := makeJWT(t, msClaims(now, "n1", msaTenantID))
	work := makeJWT(t, msClaims(now, "n1", msTenant))
	for _, tc := range []struct {
		name string
		p    Provider
		tok  xoauth2.Secret
		ok   bool
	}{
		{"consumers accepts personal", consumers, personal, true},
		{"consumers refuses work", consumers, work, false},
		{"organizations accepts work", orgs, work, true},
		{"organizations refuses personal", orgs, personal, false},
		{"tenant id accepts itself", one, work, true},
		{"tenant id refuses personal", one, personal, false},
		{"common accepts both", ms, personal, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.p.verifyIDToken(tc.tok, "n1", now)
			if (err == nil) != tc.ok {
				t.Fatalf("err = %v, want ok=%v", err, tc.ok)
			}
		})
	}
}

// A partial Google grant (the user unticked the mail scope) is refused; an
// omitted scope list means "as requested" (RFC 6749 §5.1); Microsoft's are
// compared case-insensitively.
func TestMissingScopes(t *testing.T) {
	g := Google(Client{ID: "c", Secret: "s"})
	if m := g.missingScopes([]string{"openid", "email"}); len(m) != 1 || m[0] != "https://mail.google.com/" {
		t.Errorf("partial grant: missing = %v", m)
	}
	if m := g.missingScopes([]string{"openid", "https://mail.google.com/", "email"}); len(m) != 0 {
		t.Errorf("full grant: missing = %v", m)
	}
	if m := g.missingScopes(nil); len(m) != 0 {
		t.Errorf("omitted scope: missing = %v", m)
	}
	ms, _ := Microsoft(Client{ID: "c", Secret: "s"}, "")
	if m := ms.missingScopes([]string{"https://outlook.office.com/imap.accessasuser.all", "https://outlook.office.com/SMTP.Send"}); len(m) != 0 {
		t.Errorf("case-insensitive: missing = %v", m)
	}
	if m := ms.missingScopes([]string{"https://outlook.office.com/IMAP.AccessAsUser.All"}); len(m) != 1 {
		t.Errorf("no SMTP.Send: missing = %v", m)
	}
}

func TestPublicOrigin(t *testing.T) {
	for in, want := range map[string]string{
		"https://mail.example.com":                    "https://mail.example.com",
		"https://mail.example.com/auth/callback?x=1":  "https://mail.example.com",
		"https://mail.example.com:8443/":              "https://mail.example.com:8443",
		"http://localhost:8080/auth/callback":         "http://localhost:8080",
		"http://127.0.0.1:8080":                       "http://127.0.0.1:8080",
		"http://mail.example.com":                     "",
		"mail.example.com":                            "",
		"ftp://mail.example.com":                      "",
		"https://user:pw@mail.example.com":            "",
		"":                                            "",
		"https://mail.example.com/oauth/callback/../": "https://mail.example.com",
	} {
		got, err := PublicOrigin(in)
		if want == "" {
			if err == nil {
				t.Errorf("PublicOrigin(%q) = %q, want an error", in, got)
			}
			continue
		}
		if err != nil || got != want {
			t.Errorf("PublicOrigin(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestResultLocationIsFixed(t *testing.T) {
	if got := ResultLocation(ResultConnected, ProviderGoogle); got != "/?oauth=connected&provider=google#/settings" {
		t.Errorf("ResultLocation = %q", got)
	}
}

func TestAuthorizationIssuer(t *testing.T) {
	g := Google(Client{})
	ms, _ := Microsoft(Client{}, "")
	if !g.authorizationIssuer("https://accounts.google.com") || g.authorizationIssuer("https://login.microsoftonline.com/x/v2.0") {
		t.Error("google iss check wrong")
	}
	if !ms.authorizationIssuer("https://login.microsoftonline.com/"+msTenant+"/v2.0") || ms.authorizationIssuer("https://accounts.google.com") {
		t.Error("microsoft iss check wrong")
	}
}

func TestRandomGenerationRange(t *testing.T) {
	for range 200 {
		g, err := randomGeneration()
		if err != nil || g < 1<<32 || g >= 1<<62 {
			t.Fatalf("randomGeneration = %d, %v", g, err)
		}
	}
}

// The code exchange: the right form (code, verifier, redirect_uri, client
// credentials), the ID token surfaced, and no secret — code, verifier, client
// secret, tokens — in an error even when the provider echoes them.
func TestExchangeRequestShapeAndScrubbing(t *testing.T) {
	const (
		sentCode     = "4/0CODE-sentinel-7f7f"
		sentVerifier = "VERIFIER-sentinel-abababababababababababababababab"
	)
	lc := newLeakCheck(t)
	te, srv := newTokenEndpoint(t, func(n int32, f url.Values, w http.ResponseWriter) {
		if n == 1 {
			writeJSON(w, 200, map[string]any{"access_token": sentAccess1, "refresh_token": sentRefresh,
				"id_token": "h.e.s", "expires_in": 3599, "scope": "openid https://mail.google.com/ email"})
			return
		}
		oauthError(400, "invalid_grant", "code "+sentCode+" verifier "+sentVerifier+" secret "+sentClientSec)(n, f, w)
	})
	p := testProvider(srv.URL, ProviderGoogle)
	resp, err := p.Exchange(context.Background(), srv.Client(), sentCode, sentVerifier, "https://mail.example.com/oauth/callback/google")
	if lc.err(err) != nil {
		t.Fatal(err)
	}
	if resp.IDToken.Reveal() != "h.e.s" || resp.RefreshToken.Reveal() != sentRefresh || len(resp.Scopes) != 3 {
		t.Errorf("exchange response not surfaced: id=%v refresh=%v scopes=%v",
			resp.IDToken.Reveal() == "h.e.s", resp.RefreshToken.Reveal() == sentRefresh, resp.Scopes)
	}
	f := te.forms[0]
	if f.Get("grant_type") != "authorization_code" || f.Get("code") != sentCode || f.Get("code_verifier") != sentVerifier ||
		f.Get("redirect_uri") != "https://mail.example.com/oauth/callback/google" ||
		f.Get("client_id") != "client-id" || f.Get("client_secret") != sentClientSec {
		t.Errorf("exchange form is wrong (keys %v)", keys(f))
	}
	_, err = p.Exchange(context.Background(), srv.Client(), sentCode, sentVerifier, "https://mail.example.com/oauth/callback/google")
	if lc.err(err) == nil {
		t.Fatal("a refused exchange succeeded")
	}
	for _, s := range []string{sentCode, sentVerifier, sentClientSec} {
		if strings.Contains(err.Error(), s) {
			t.Errorf("the exchange error carries a secret the provider echoed: %v", err)
		}
	}
}

func TestRevoke(t *testing.T) {
	var got []url.Values
	_, srv := newTokenEndpoint(t, func(n int32, f url.Values, w http.ResponseWriter) {
		got = append(got, f)
		switch n {
		case 1:
			w.WriteHeader(http.StatusOK)
		case 2:
			oauthError(400, "invalid_token", "Token expired or revoked")(n, f, w)
		default:
			http.Error(w, "nope "+sentRefresh, http.StatusServiceUnavailable)
		}
	})
	p := Google(Client{ID: "c", Secret: sentClientSec})
	p.RevokeURL = srv.URL
	if err := p.Revoke(context.Background(), srv.Client(), sentRefresh); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if got[0].Get("token") != sentRefresh {
		t.Error("revocation did not send the refresh token")
	}
	if err := p.Revoke(context.Background(), srv.Client(), sentRefresh); err != nil {
		t.Errorf("an already-invalid token is not revoked: %v", err)
	}
	err := p.Revoke(context.Background(), srv.Client(), sentRefresh)
	if err == nil || strings.Contains(err.Error(), sentRefresh) {
		t.Errorf("5xx revoke = %v (must fail, without the token)", err)
	}
	if err := (Provider{Name: ProviderMicrosoft}).Revoke(context.Background(), nil, sentRefresh); err == nil {
		t.Error("revoke with no endpoint succeeded")
	}
}

// OBLIGATION (a) at the Manager: the TokenSource's Reject drops the cached
// token, so the protocol layer's one retry gets a REFRESHED token; a Reject of
// a token that has already been replaced changes nothing.
func TestTokenSourceRejectRefreshes(t *testing.T) {
	lc := newLeakCheck(t)
	te, srv := newTokenEndpoint(t, func(n int32, f url.Values, w http.ResponseWriter) {
		okToken([]string{sentAccess1, sentAccess2}[min(int(n)-1, 1)], "")(n, f, w)
	})
	m := newManager(t, lc, srv, newFakeGrants(ProviderGoogle), ProviderGoogle, nil)
	src := m.TokenSource(userA, acctA)
	tok, err := src.Token(context.Background())
	if lc.err(err) != nil || tok.Reveal() != sentAccess1 {
		t.Fatalf("Token = %v", err)
	}
	if tok, _ = src.Token(context.Background()); tok.Reveal() != sentAccess1 || te.calls.Load() != 1 {
		t.Fatal("second Token was not served from the cache")
	}
	src.Reject(sentAccess1)
	tok, err = src.Token(context.Background())
	if lc.err(err) != nil || tok.Reveal() != sentAccess2 || te.calls.Load() != 2 {
		t.Fatalf("after Reject: token is the new one=%v, calls=%d — the refused token was not dropped",
			tok.Reveal() == sentAccess2, te.calls.Load())
	}
	src.Reject(sentAccess1) // stale: already replaced
	if _, _ = src.Token(context.Background()); te.calls.Load() != 2 {
		t.Error("a stale Reject caused a refresh")
	}
}

// OBLIGATION (b): Forget drops the cached token, and a refresh that was in
// flight when Forget ran does not put its token back in the cache.
func TestManagerForget(t *testing.T) {
	lc := newLeakCheck(t)
	release := make(chan struct{})
	te, srv := newTokenEndpoint(t, func(n int32, f url.Values, w http.ResponseWriter) {
		if n == 2 {
			<-release // the second refresh is in flight across a Forget
		}
		okToken([]string{sentAccess1, sentAccess2, sentAccess1}[min(int(n)-1, 2)], "")(n, f, w)
	})
	m := newManager(t, lc, srv, newFakeGrants(ProviderGoogle), ProviderGoogle, nil)
	if _, err := m.AccessToken(context.Background(), userA, acctA); lc.err(err) != nil {
		t.Fatal(err)
	}
	m.Forget(userA, acctA)
	if _, ok := m.cached(cacheKey{userA, acctA}); ok {
		t.Fatal("Forget left the token in the cache")
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = m.AccessToken(context.Background(), userA, acctA) // refresh #2, blocked
	}()
	for te.calls.Load() < 2 {
		time.Sleep(time.Millisecond)
	}
	m.Forget(userA, acctA) // the grant ends while #2 is in flight
	close(release)
	<-done
	if _, ok := m.cached(cacheKey{userA, acctA}); ok {
		t.Fatal("a refresh in flight across Forget re-filled the cache")
	}
	if _, _ = m.AccessToken(context.Background(), userA, acctA); te.calls.Load() != 3 {
		t.Errorf("after Forget the next AccessToken did not go back to the grant (calls=%d)", te.calls.Load())
	}
}

// SECURITY REVIEW M2: the default token client owns its transport — not
// http.DefaultTransport, which any package may mutate — with a TLS 1.2 floor,
// the environment's proxy, and redirects refused. Both the Manager and the
// Connector fall back to it when given no client.
func TestDefaultTokenClientOwnsItsTransport(t *testing.T) {
	tr, ok := defaultTokenClient.Transport.(*http.Transport)
	if !ok || tr == nil || defaultTokenClient.Transport == http.DefaultTransport {
		t.Fatalf("default token client transport = %T (shared default: %v)", defaultTokenClient.Transport,
			defaultTokenClient.Transport == http.DefaultTransport)
	}
	if tr.TLSClientConfig == nil || tr.TLSClientConfig.MinVersion != tls.VersionTLS12 || tr.Proxy == nil {
		t.Errorf("transport TLS/proxy not set: %+v", tr.TLSClientConfig)
	}
	if c := noRedirects(nil); c.Transport != defaultTokenClient.Transport || c.CheckRedirect == nil {
		t.Error("noRedirects(nil) does not use the owned transport with redirects refused")
	}
	if m := NewManager(nil, nil); m.http != defaultTokenClient {
		t.Error("the Manager does not default to the owned client")
	}
}

// The PKCE challenge the flow sends is S256 of the verifier it sealed: the
// helper the integration test's fake provider uses to check it.
func s256(v string) string {
	sum := sha256.Sum256([]byte(v))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

var _ = s256
