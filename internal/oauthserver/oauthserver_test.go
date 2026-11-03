// SPDX-License-Identifier: AGPL-3.0-only

package oauthserver

import (
	"testing"
	"time"
)

func TestRedirectURIRules(t *testing.T) {
	for _, ok := range []string{"https://app.example.com/cb", "http://127.0.0.1/cb", "http://127.0.0.1:8080/cb",
		"http://[::1]:9/cb", "http://localhost:3000/cb", "https://app.example.com/cb?x=1"} {
		if err := ValidateRedirectURI(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "com.example.app:/cb", "http://app.example.com/cb", "https://app.example.com/cb#f",
		"https://user@app.example.com/cb", "javascript:alert(1)", "https:///cb", "https://app.example.com/c\\b",
		"https://app.example.com/ cb", "ftp://app.example.com/cb", "//app.example.com/cb"} {
		if ValidateRedirectURI(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestMatchRedirectURI(t *testing.T) {
	reg := []string{"https://app.example.com/cb", "http://127.0.0.1/cb", "http://localhost/lb?x=1"}
	for _, ok := range []string{"https://app.example.com/cb", "http://127.0.0.1/cb", "http://127.0.0.1:53124/cb",
		"http://localhost:1/lb?x=1"} {
		if !MatchRedirectURI(reg, ok) {
			t.Errorf("%q did not match", ok)
		}
	}
	for _, bad := range []string{"https://App.example.com/cb", "https://app.example.com/cb/", "https://app.example.com/cb?x=1",
		"https://app.example.com:444/cb", "http://app.example.com/cb", "http://127.0.0.1:80/other", "http://127.0.0.1/cb?x=1",
		"http://localhost/lb", "http://[::1]/cb", "https://127.0.0.1:99/cb", "com.example.app:/cb", ""} {
		if MatchRedirectURI(reg, bad) {
			t.Errorf("%q matched", bad)
		}
	}
	// A registered URI that is itself unacceptable never matches.
	if MatchRedirectURI([]string{"com.example.app:/cb"}, "com.example.app:/cb") {
		t.Error("a private-use scheme matched itself")
	}
	if !AllLoopback([]string{"http://127.0.0.1/cb", "http://localhost/x"}) || AllLoopback([]string{"http://127.0.0.1/cb", "https://app.example.com/cb"}) {
		t.Error("AllLoopback")
	}
}

func TestCIMDDocumentValidation(t *testing.T) {
	const id = "https://app.example.com/client.json"
	good := `{"client_id":"` + id + `","client_name":"App","redirect_uris":["https://app.example.com/cb"],"token_endpoint_auth_method":"none"}`
	if _, err := parseCIMD(id, []byte(good)); err != nil {
		t.Fatalf("good document: %v", err)
	}
	for name, doc := range map[string]string{
		"mismatch":     `{"client_id":"https://app.example.com/other.json","client_name":"App","redirect_uris":["https://app.example.com/cb"]}`,
		"no name":      `{"client_id":"` + id + `","redirect_uris":["https://app.example.com/cb"]}`,
		"no redirects": `{"client_id":"` + id + `","client_name":"App"}`,
		"bad redirect": `{"client_id":"` + id + `","client_name":"App","redirect_uris":["http://app.example.com/cb"]}`,
		"secret auth":  `{"client_id":"` + id + `","client_name":"App","redirect_uris":["https://app.example.com/cb"],"token_endpoint_auth_method":"client_secret_post"}`,
		"not json":     `<html>`,
	} {
		if _, err := parseCIMD(id, []byte(doc)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	for _, bad := range []string{"http://app.example.com/c.json", "https://app.example.com", "https://app.example.com/",
		"https://app.example.com/a/../b", "https://u@app.example.com/c", "https://app.example.com/c#f"} {
		if _, err := validCIMDURL(bad); err == nil {
			t.Errorf("client_id %q accepted", bad)
		}
	}
	if IsCIMDClientID("mc_0123") || !IsCIMDClientID("https://x") {
		t.Error("IsCIMDClientID")
	}
}

func TestCacheTTLIsClamped(t *testing.T) {
	for in, want := range map[string]time.Duration{"": time.Hour, "max-age=1": 5 * time.Minute, "no-store": 5 * time.Minute,
		"max-age=600": 10 * time.Minute, "public, max-age=999999999": 24 * time.Hour, "max-age=junk": time.Hour} {
		if got := cacheTTL(in); got != want {
			t.Errorf("cacheTTL(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestResourceMatching(t *testing.T) {
	s := &Server{resource: "https://mail.example.com/mcp"}
	for _, ok := range []string{"", "https://mail.example.com/mcp", "https://MAIL.example.com/mcp", "HTTPS://mail.example.com/mcp/"} {
		if !s.resourceMatches(ok) {
			t.Errorf("%q refused", ok)
		}
	}
	for _, bad := range []string{"https://mail.example.com/", "https://mail.example.com/jmap", "https://other.example.com/mcp",
		"http://mail.example.com/mcp", "https://mail.example.com/mcp?x=1", "https://mail.example.com/mcp/x"} {
		if s.resourceMatches(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestPKCEHelpers(t *testing.T) {
	// RFC 7636 appendix B.
	if got := s256("dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"); got != "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM" {
		t.Errorf("s256 = %s", got)
	}
	if !validChallenge("E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM") || validChallenge("short") {
		t.Error("validChallenge")
	}
	if validVerifier("short") || !validVerifier("dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk") {
		t.Error("validVerifier")
	}
}
