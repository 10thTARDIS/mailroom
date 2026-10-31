// SPDX-License-Identifier: AGPL-3.0-only

package search

import "testing"

// FromExactAddress is the parser's own answer to "is from:<v> one address?"
// (#53 security review). Every domain-shaped value — which #712 reads as a
// domain and its subdomains — and every syntax error is refused.
func TestFromExactAddress(t *testing.T) {
	ok := map[string]string{
		"alice@example.com":   "alice@example.com",
		"Alice@Example.com":   "alice@example.com", // normalised; callers compare to their key
		"a.b+tag@mail.ex.org": "a.b+tag@mail.ex.org",
	}
	for in, want := range ok {
		if got, yes := FromExactAddress(in); !yes || got != want {
			t.Errorf("FromExactAddress(%q) = %q, %v; want %q, true", in, got, yes, want)
		}
	}
	for _, bad := range []string{"example.com", "@example.com", "*.example.com", ".example.com",
		"alice@", "@", "", "alice", "*foo", "*.com"} {
		if got, yes := FromExactAddress(bad); yes {
			t.Errorf("FromExactAddress(%q) = %q, true; want refused", bad, got)
		}
	}
}
