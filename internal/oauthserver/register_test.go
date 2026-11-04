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

func newTestLimiter() (*regLimiter, *time.Time) {
	now := time.Unix(1_800_000_000, 0)
	l := newRegLimiter()
	l.now = func() time.Time { return now }
	return l, &now
}

// The table is bounded and fails closed: strangers arriving in their thousands
// neither erase an address's daily count nor get in once the table is full.
func TestRegistrationLimiterFailsClosedWhenFull(t *testing.T) {
	l, now := newTestLimiter()
	// "victim" uses its whole day.
	for i := 0; i < RegistrationsPerAddressPerDay; i++ {
		*now = now.Add(registrationRefill)
		if ok, _ := l.admit("203.0.113.1"); !ok {
			t.Fatalf("registration %d refused early", i)
		}
		l.registered("203.0.113.1")
	}
	if ok, _ := l.admit("203.0.113.1"); ok {
		t.Fatal("the address is not at its daily limit")
	}
	// maxRegistrationAddrs strangers arrive, each registering once, so none is idle.
	admitted := 0
	for i := 0; i < maxRegistrationAddrs; i++ {
		a := "198.51." + strconv.Itoa(i/256) + "." + strconv.Itoa(i%256)
		if ok, _ := l.admit(a); ok {
			admitted++
			l.registered(a)
		}
	}
	l.mu.Lock()
	n := len(l.m)
	l.mu.Unlock()
	if n > maxRegistrationAddrs {
		t.Errorf("the limiter holds %d addresses, want at most %d", n, maxRegistrationAddrs)
	}
	if admitted != maxRegistrationAddrs-1 {
		t.Errorf("%d strangers admitted, want %d (the table had one place taken)", admitted, maxRegistrationAddrs-1)
	}
	// The address at its limit is STILL refused: nothing was forgotten to make room.
	if ok, _ := l.admit("203.0.113.1"); ok {
		t.Error("an address at its daily limit was admitted after 4096 strangers arrived (its count was forgotten)")
	}
	// A new address is refused while the table is full of busy ones...
	if ok, retry := l.admit("192.0.2.77"); ok || retry <= 0 {
		t.Errorf("a new address while the table is full: ok=%v retry=%v, want refused", ok, retry)
	}
	// ...a known one keeps its own state...
	// ...and a day later everything is idle and the table takes new addresses.
	*now = now.Add(25 * time.Hour)
	if ok, _ := l.admit("192.0.2.77"); !ok {
		t.Error("a new address a day later is still refused")
	}
}

func TestRegistrationLimiterKeysIPv6ByItsSlash64(t *testing.T) {
	l, _ := newTestLimiter()
	// Burst is 5: three from one address and three from ANOTHER in the same /64
	// share it, so the sixth is refused.
	for i, a := range []string{"2001:db8:1:2::1", "2001:db8:1:2::2", "2001:db8:1:2:ffff:ffff:ffff:ffff",
		"2001:db8:1:2:1::", "2001:db8:1:2:2::"} {
		if ok, _ := l.admit(a); !ok {
			t.Fatalf("address %d (%s) refused inside the burst", i, a)
		}
	}
	if ok, _ := l.admit("2001:db8:1:2:abcd::9"); ok {
		t.Error("a sixth address of one /64 got its own budget")
	}
	// A neighbouring /64 is a different subscriber.
	if ok, _ := l.admit("2001:db8:1:3::1"); !ok {
		t.Error("a different /64 shares the first one's budget")
	}
	// IPv4 is keyed by address, and an IPv4-mapped IPv6 address is that address.
	for i := 0; i < registrationBurst; i++ {
		if ok, _ := l.admit("192.0.2.1"); !ok {
			t.Fatalf("v4 burst %d refused", i)
		}
	}
	if ok, _ := l.admit("::ffff:192.0.2.1"); ok {
		t.Error("an IPv4-mapped address got a budget apart from the IPv4 address")
	}
	if ok, _ := l.admit("192.0.2.2"); !ok {
		t.Error("another IPv4 address shares the first's budget")
	}
	if limitKey("2001:db8:1:2::1") != "2001:db8:1:2::/64" || limitKey("192.0.2.9") != "192.0.2.9" || limitKey("not-an-ip") != "not-an-ip" {
		t.Errorf("limitKey: %q %q %q", limitKey("2001:db8:1:2::1"), limitKey("192.0.2.9"), limitKey("not-an-ip"))
	}
}

// The process-wide rate holds however many addresses ask.
func TestRegistrationGlobalRate(t *testing.T) {
	l, now := newTestLimiter()
	for i := 0; i < registrationGlobalBurst; i++ {
		if ok, _ := l.admitGlobal(); !ok {
			t.Fatalf("global %d refused inside the burst", i)
		}
	}
	if ok, retry := l.admitGlobal(); ok || retry <= 0 {
		t.Errorf("the 31st in one instant: ok=%v retry=%v", ok, retry)
	}
	*now = now.Add(registrationGlobalRefill)
	if ok, _ := l.admitGlobal(); !ok {
		t.Error("no place after one refill interval")
	}
	if ok, _ := l.admitGlobal(); ok {
		t.Error("two places after one refill interval")
	}
	*now = now.Add(time.Minute)
	for i := 0; i < registrationGlobalBurst; i++ {
		if ok, _ := l.admitGlobal(); !ok {
			t.Fatalf("after a minute, %d refused (30 a minute)", i)
		}
	}
}
