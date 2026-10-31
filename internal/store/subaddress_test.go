// SPDX-License-Identifier: AGPL-3.0-only

package store

import "testing"

// SubaddressBase is the syntactic half of the subaddress authorization (#62).
// It is a pure function over a string and needs no database, which is the point:
// the authorization it feeds consults no message data at all, so the only way it
// can be wrong is by mis-parsing an address.
func TestSubaddressBase(t *testing.T) {
	for _, tc := range []struct {
		addr string
		want string
		ok   bool
	}{
		{"me+shop@example.com", "me@example.com", true},
		// RFC 5233: the detail is everything after the FIRST delimiter, so this
		// resolves against "me@d" and never walks further. That single strip is
		// also what keeps the fallback from recursing.
		{"me+a+b@example.com", "me@example.com", true},
		{"me+SHOP@example.com", "me@example.com", true},
		{"first.last+tag@example.com", "first.last@example.com", true},

		// Nothing to strip.
		{"me@example.com", "", false},
		{"", "", false},

		// Nonsense that must not produce a lookup. "+tag@d" is the dangerous one:
		// an empty base local-part would make the base "@d", and a careless
		// implementation could match that against something.
		{"+tag@example.com", "", false},
		{"me+@example.com", "", false},
		{"+@example.com", "", false},
		{"me+shop", "", false},
		{"me+shop@", "", false},
		{"@example.com", "", false},

		// The "@" is found from the RIGHT, so a "+" in the DOMAIN is not a
		// delimiter — stripping there would hand back a different domain, which
		// is the one mistake here that could authorize the wrong thing.
		{"me@ex+ample.com", "", false},
		{"me+tag@ex+ample.com", "me@ex+ample.com", true},
		// A stray second "@" leaves the local part with no "+": nothing to strip.
		{"a@b@example.com", "", false},
	} {
		got, ok := SubaddressBase(tc.addr)
		if ok != tc.ok {
			t.Errorf("SubaddressBase(%q) ok = %v, want %v (got %q)", tc.addr, ok, tc.ok, got)
			continue
		}
		if ok && got != tc.want {
			t.Errorf("SubaddressBase(%q) = %q, want %q", tc.addr, got, tc.want)
		}
	}
}

// NormalizeAddress must NEVER learn to do this. Under a catchall, "me+shop@d"
// and "me@d" are distinct addresses that a sender chose between, and collapsing
// them in normalization would silently merge two identities, two contacts and
// two aliases. Stripping belongs only to the authorization fallback, where the
// base is a fact being looked up rather than an identity being compared.
func TestNormalizeAddressKeepsTheSubaddress(t *testing.T) {
	const addr = "Me+Shop@Example.com"
	if got := NormalizeAddress(addr); got != "me+shop@example.com" {
		t.Errorf("NormalizeAddress(%q) = %q — the +tag must survive normalization", addr, got)
	}
}
