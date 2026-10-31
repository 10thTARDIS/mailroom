// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

func prefixes(t *testing.T, ss ...string) []netip.Prefix {
	t.Helper()
	var out []netip.Prefix
	for _, s := range ss {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			t.Fatalf("prefix %q: %v", s, err)
		}
		out = append(out, p)
	}
	return out
}

// The address shown in the devices list (#937). Table-driven, with the spoofing
// rows first because they are the point: an untrusted peer must not be able to
// choose what it shows.
func TestForwardedClientAddr(t *testing.T) {
	trusted := prefixes(t, "10.0.0.0/8", "172.18.0.5/32", "fd00::/8")
	many := strings.Repeat("203.0.113.9, ", maxForwardedHops) + "203.0.113.1"

	tests := []struct {
		name    string
		trusted []netip.Prefix
		remote  string
		xff     []string
		want    string
	}{
		// --- spoofing: the header is attacker-written unless a trusted proxy sent it
		{"untrusted peer with a header is reported as itself", trusted, "198.51.100.7:4000", []string{"203.0.113.50"}, "198.51.100.7"},
		{"untrusted peer claiming a trusted hop", trusted, "198.51.100.7:4000", []string{"10.1.2.3"}, "198.51.100.7"},
		{"no trusted proxies configured ignores the header", nil, "10.0.0.1:4000", []string{"203.0.113.50"}, "10.0.0.1"},
		{"empty trusted list ignores the header", []netip.Prefix{}, "10.0.0.1:4000", []string{"203.0.113.50"}, "10.0.0.1"},

		// --- the normal cases behind a trusted proxy
		{"one proxy appends the client", trusted, "10.0.0.1:4000", []string{"203.0.113.50"}, "203.0.113.50"},
		{"client-supplied entries to the left are not believed past the first untrusted hop",
			trusted, "10.0.0.1:4000", []string{"1.1.1.1, 203.0.113.50"}, "203.0.113.50"},
		{"two trusted hops are walked through", trusted, "10.0.0.1:4000", []string{"203.0.113.50, 10.9.9.9"}, "203.0.113.50"},
		{"a single-host prefix matches", trusted, "172.18.0.5:4000", []string{"203.0.113.50"}, "203.0.113.50"},
		{"a neighbour of the single-host prefix does not", trusted, "172.18.0.6:4000", []string{"203.0.113.50"}, "172.18.0.6"},
		{"repeated header lines are one list", trusted, "10.0.0.1:4000", []string{"1.1.1.1", "203.0.113.50"}, "203.0.113.50"},
		{"IPv6 client", trusted, "10.0.0.1:4000", []string{"2001:db8::7"}, "2001:db8::7"},
		{"IPv6 proxy", trusted, "[fd00::1]:4000", []string{"203.0.113.50"}, "203.0.113.50"},
		{"an IPv4-mapped client prints as IPv4", trusted, "10.0.0.1:4000", []string{"::ffff:203.0.113.50"}, "203.0.113.50"},
		{"no header: the peer", trusted, "10.0.0.1:4000", nil, "10.0.0.1"},

		// --- malformed: the walk stops, and the trusted hop reached is NOT reported
		{"garbage entry", trusted, "10.0.0.1:4000", []string{"not-an-ip"}, "10.0.0.1"},
		{"empty entry", trusted, "10.0.0.1:4000", []string{"203.0.113.50, "}, "10.0.0.1"},
		{"entry with a port", trusted, "10.0.0.1:4000", []string{"203.0.113.50:5555"}, "10.0.0.1"},
		{"entry with a zone", trusted, "10.0.0.1:4000", []string{"fe80::1%eth0"}, "10.0.0.1"},
		{"malformed entry left of a valid one is never reached", trusted, "10.0.0.1:4000", []string{"junk, 203.0.113.50"}, "203.0.113.50"},
		{"malformed entry right of a trusted hop stops the walk", trusted, "10.0.0.1:4000", []string{"203.0.113.50, junk, 10.9.9.9"}, "10.0.0.1"},
		{"every hop trusted: the peer", trusted, "10.0.0.1:4000", []string{"10.5.5.5, 10.6.6.6"}, "10.0.0.1"},
		{"a chain longer than the bound: the peer", trusted, "10.0.0.1:4000", []string{many}, "10.0.0.1"},

		// --- the peer itself
		{"a peer address that is not host:port", trusted, "10.0.0.1", []string{"203.0.113.50"}, "203.0.113.50"},
		{"an unparseable peer is returned as it came", trusted, "not-an-address", []string{"203.0.113.50"}, "not-an-address"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = tc.remote
			for _, v := range tc.xff {
				r.Header.Add("X-Forwarded-For", v)
			}
			if got := forwardedClientAddr(r, tc.trusted); got != tc.want {
				t.Errorf("forwardedClientAddr(remote=%q, xff=%q) = %q, want %q", tc.remote, tc.xff, got, tc.want)
			}
		})
	}
}

// The authenticator's display address uses its configured proxies, and the
// bearer rate limiter's clientAddr does not (this increment leaves it alone).
func TestDisplayAddrUsesConfigAndClientAddrIgnoresIt(t *testing.T) {
	a := testAuthenticator(t, true)
	a.cfg.TrustedProxies = prefixes(t, "10.0.0.0/8")
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "10.0.0.1:4000"
	r.Header.Set("X-Forwarded-For", "203.0.113.50")
	if got := a.displayAddr(r); got != "203.0.113.50" {
		t.Errorf("displayAddr = %q, want the forwarded client", got)
	}
	if got := clientAddr(r); got != "10.0.0.1" {
		t.Errorf("clientAddr = %q, want the connection's own address", got)
	}
}
