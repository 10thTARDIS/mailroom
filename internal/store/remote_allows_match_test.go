// SPDX-License-Identifier: AGPL-3.0-only

package store

import "testing"

// RemoteAllowsMatch is a Go copy of a rule whose authority is TypeScript
// (web/src/core/remoteAllows.ts). These cases mirror that file's own suite, so a
// change on either side that is not made on both shows up here rather than as a
// quietly wrong number in a corpus report.

func allowsFor(pairs ...[2]string) []RemoteAllow {
	out := make([]RemoteAllow, 0, len(pairs))
	for _, p := range pairs {
		out = append(out, RemoteAllow{Kind: RemoteAllowKind(p[0]), Value: p[1]})
	}
	return out
}

func TestRemoteAllowsMatchMirrorsTheClient(t *testing.T) {
	addressGrant := allowsFor([2]string{"address", "news@example.com"})
	domainGrant := allowsFor([2]string{"domain", "example.com"})

	cases := []struct {
		name   string
		from   string
		allows []RemoteAllow
		want   bool
	}{
		{"exact address matches", "news@example.com", addressGrant, true},
		{"address grant is case-insensitive", "News@Example.COM", addressGrant, true},
		{"address grant does not match a sibling", "other@example.com", addressGrant, false},
		{"domain grant matches anyone there", "anyone@example.com", domainGrant, true},
		{"domain grant does not match a subdomain", "a@mail.example.com", domainGrant, false},
		{"domain grant does not match a suffix", "a@notexample.com", domainGrant, false},
		// ⚠ Fail-closed cases. Every one of these is an input a tracker can
		// choose, so "uncertain" must never resolve to "load".
		{"no grants", "news@example.com", nil, false},
		{"empty from", "", domainGrant, false},
		{"from with no domain", "news", domainGrant, false},
		{"from ending in @", "news@", domainGrant, false},
		{"grant with an empty value", "news@example.com", allowsFor([2]string{"domain", ""}), false},
		// ⚠ The value here is deliberately one that WOULD match if the kind were
		// not checked. An earlier version of this case used "*", which could not
		// have matched under any rule — so it passed whether or not the kind was
		// examined at all, and probing found it green with the check removed. A
		// future `kind` must not come to mean "everything" to older code, and
		// this is the only case that can observe that.
		{"grant with an unknown kind", "news@example.com", allowsFor([2]string{"everything", "example.com"}), false},
		{"unknown kind matching the address exactly", "news@example.com",
			allowsFor([2]string{"everything", "news@example.com"}), false},
		{"a domain grant is not an address grant", "example.com", domainGrant, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := RemoteAllowsMatch(c.from, c.allows); got != c.want {
				t.Errorf("RemoteAllowsMatch(%q) = %v, want %v", c.from, got, c.want)
			}
		})
	}
}

// TestRemoteAllowsMatchPrefersTheAddressGrant pins WHICH grant is reported when
// both apply, because the report names it and "why did images load" is the
// question a person actually asks.
func TestRemoteAllowsMatchPrefersTheAddressGrant(t *testing.T) {
	both := allowsFor(
		[2]string{"domain", "example.com"},
		[2]string{"address", "news@example.com"},
	)
	got := MatchingRemoteAllow("news@example.com", both)
	if got == nil {
		t.Fatal("no grant matched, want the address grant")
	}
	if got.Kind != RemoteAllowAddress {
		t.Errorf("matched the %q grant, want the more specific address grant", got.Kind)
	}
}

// ⚠ TestRemoteAllowsMatchDivergesFromTheStoreHelpers records, rather than hides,
// the two places where the client's rule and this package's own address helpers
// disagree. Both are cases a tracker can construct deliberately.
//
// This test exists so the divergence is a decision with a reason attached
// instead of something the next person discovers from a wrong report. If the
// client is ever changed to match NormalizeAddress/AddressDomain, this test is
// the one that should fail.
func TestRemoteAllowsMatchDivergesFromTheStoreHelpers(t *testing.T) {
	// 1. Angle brackets. NormalizeAddress strips them; the client does not, so a
	//    grant does NOT match a bracketed From.
	if NormalizeAddress("<news@example.com>") != "news@example.com" {
		t.Fatal("precondition changed: NormalizeAddress no longer strips <>")
	}
	if RemoteAllowsMatch("<news@example.com>", allowsFor([2]string{"address", "news@example.com"})) {
		t.Error("a bracketed From matched — this mirrors the client, which does not strip <>; " +
			"if that changed, change both sides together")
	}

	// 2. Several "@". AddressDomain refuses the address outright; the client
	//    takes the text after the LAST one, so a domain grant DOES match.
	if AddressDomain("a@b@example.com") != "" {
		t.Fatal("precondition changed: AddressDomain no longer refuses a multi-@ address")
	}
	if !RemoteAllowsMatch("a@b@example.com", allowsFor([2]string{"domain", "example.com"})) {
		t.Error("a multi-@ From did not match a domain grant — the client takes the text " +
			"after the LAST @, and this must agree with the client, not with AddressDomain")
	}
}
