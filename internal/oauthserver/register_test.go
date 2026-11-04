// SPDX-License-Identifier: AGPL-3.0-only

package oauthserver

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestValidateRegistrationBounds(t *testing.T) {
	name := func(s string) registrationRequest {
		return registrationRequest{ClientName: &s, RedirectURIs: []string{"https://app.example.com/cb"}}
	}
	if _, _, got, code, _ := validateRegistration(name("  A\u200bpp\u202e ")); code != "" || got != "App" {
		t.Errorf("cleaned name = %q (%s)", got, code)
	}
	if _, _, _, code, _ := validateRegistration(name(strings.Repeat("é", 100))); code != "" {
		t.Errorf("100 characters refused: %s", code)
	}
	for _, bad := range []string{"", " ", "\u200b", strings.Repeat("é", 101), strings.Repeat("a", 5000)} {
		if _, _, _, code, _ := validateRegistration(name(bad)); code != "invalid_client_metadata" {
			t.Errorf("name %.12q accepted (code %q)", bad, code)
		}
	}
	// grant_types and response_types default, and are subsets.
	g, r, _, code, _ := validateRegistration(name("App"))
	if code != "" || strings.Join(g, ",") != "authorization_code" || strings.Join(r, ",") != "code" {
		t.Errorf("defaults = %v %v %q", g, r, code)
	}
	for _, grants := range [][]string{{"password"}, {"authorization_code", "implicit"}, {"client_credentials"}} {
		req := name("App")
		req.GrantTypes = grants
		if _, _, _, code, _ := validateRegistration(req); code != "invalid_client_metadata" {
			t.Errorf("grant_types %v accepted", grants)
		}
	}
	none, basic := "none", "client_secret_post"
	req := name("App")
	req.AuthMethod = &none
	if _, _, _, code, _ := validateRegistration(req); code != "" {
		t.Errorf("auth method none refused: %s", code)
	}
	req.AuthMethod = &basic
	if _, _, _, code, _ := validateRegistration(req); code != "invalid_client_metadata" {
		t.Errorf("auth method %s accepted", basic)
	}
	// Redirect URI count and shape.
	req = name("App")
	req.RedirectURIs = nil
	if _, _, _, code, _ := validateRegistration(req); code != "invalid_redirect_uri" {
		t.Errorf("no URIs: %q", code)
	}
	for i := 0; i < 11; i++ {
		req.RedirectURIs = append(req.RedirectURIs, "https://app.example.com/cb"+strconv.Itoa(i))
	}
	if _, _, _, code, _ := validateRegistration(req); code != "invalid_redirect_uri" {
		t.Errorf("11 URIs: %q", code)
	}
	req.RedirectURIs = req.RedirectURIs[:10]
	if _, _, _, code, _ := validateRegistration(req); code != "" {
		t.Errorf("10 URIs: %q", code)
	}
}

func TestRegistrationLimiter(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	l := newRegLimiter()
	l.now = func() time.Time { return now }

	// Burst: five at one instant.
	for i := 0; i < registrationBurst; i++ {
		if ok, _ := l.admit("a"); !ok {
			t.Fatalf("burst %d refused", i)
		}
	}
	if ok, retry := l.admit("a"); ok || retry <= 0 {
		t.Errorf("sixth in one instant: ok=%v retry=%v", ok, retry)
	}
	// Another address has its own budget.
	if ok, _ := l.admit("b"); !ok {
		t.Error("a second address shares the first's burst")
	}
	// The daily count is of STORED registrations, per address, over 24 hours.
	for i := 0; i < RegistrationsPerAddressPerDay; i++ {
		now = now.Add(registrationRefill)
		if ok, _ := l.admit("a"); !ok {
			t.Fatalf("registration %d refused early", i)
		}
		l.registered("a")
	}
	now = now.Add(registrationRefill)
	ok, retry := l.admit("a")
	if ok || retry < time.Hour {
		t.Errorf("51st in a day: ok=%v retry=%v", ok, retry)
	}
	if ok, _ := l.admit("b"); !ok {
		t.Error("address b blocked by a's day")
	}
	now = now.Add(24 * time.Hour)
	if ok, _ := l.admit("a"); !ok {
		t.Error("still blocked a day later")
	}
}

func TestRegistrationLimiterIsBounded(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	l := newRegLimiter()
	l.now = func() time.Time { return now }
	for i := 0; i < 3*maxRegistrationAddrs; i++ {
		l.admit("addr-" + strconv.Itoa(i))
		l.registered("addr-" + strconv.Itoa(i))
	}
	l.mu.Lock()
	n := len(l.m)
	l.mu.Unlock()
	if n > maxRegistrationAddrs {
		t.Errorf("the limiter holds %d addresses, want at most %d", n, maxRegistrationAddrs)
	}
}
