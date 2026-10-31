// SPDX-License-Identifier: AGPL-3.0-only

package store

import "testing"

// TestProxyMatchAgreesWithTheClientOnOrdinaryMail is the half that keeps the
// divergence honest. Two rules that disagree everywhere are not one stricter
// rule and one looser one, they are two features — so on the input that
// actually occurs, they must give the same answer AND the same reason.
func TestProxyMatchAgreesWithTheClientOnOrdinaryMail(t *testing.T) {
	allows := allowsFor(
		[2]string{"address", "news@example.com"},
		[2]string{"domain", "list.example.org"},
	)
	for _, from := range []string{
		"news@example.com",
		"News@Example.COM",
		"  news@example.com  ",
		"anyone@list.example.org",
		"other@example.com",     // no grant — both say no
		"a@mail.example.org",    // subdomain — both say no
		"a@notlist.example.org", // suffix — both say no
		"",                      // empty — both say no
		"news",                  // no domain — both say no
		"news@",                 // trailing @ — both say no
	} {
		client := MatchingRemoteAllow(from, allows)
		proxy := ProxyMatchingRemoteAllow(from, allows)
		switch {
		case client == nil && proxy == nil:
			// agreed: no
		case client == nil || proxy == nil:
			t.Errorf("from %q: client=%v proxy=%v — they must agree on ordinary mail",
				from, describeAllow(client), describeAllow(proxy))
		// ⚠ Compared by kind+value, NOT by ID: allowsFor leaves ID at its zero
		// value, so an ID comparison here would be two zeroes and could never
		// fail — a check that cannot observe the thing it names.
		case client.Kind != proxy.Kind || client.Value != proxy.Value:
			t.Errorf("from %q: agreed to allow but for different reasons: client=%s proxy=%s",
				from, describeAllow(client), describeAllow(proxy))
		}
	}
}

// TestProxyMatchDivergesFromTheClientDeliberately pins every input where the
// two rules disagree. ADR-026 §3c requires the divergence to exist and requires
// it to be deliberate; this is what makes "deliberate" checkable.
//
// ⚠ EVERY CASE HERE IS ONE THE CLIENT ALLOWS AND THE PROXY REFUSES, and that
// direction is the whole point. A case where the proxy allowed something the
// client refused would be the proxy fetching content the blocking policy exists
// to prevent — a bypass wearing a privacy feature's costume — so the test
// asserts the direction, not merely the difference.
func TestProxyMatchDivergesFromTheClientDeliberately(t *testing.T) {
	allows := allowsFor(
		[2]string{"address", "news@example.com"},
		[2]string{"domain", "example.com"},
	)
	cases := []struct {
		from string
		why  string
	}{
		{
			"attacker@evil.example@example.com",
			"⚠ THE ATTACK. The client takes the domain after the LAST @, so this " +
				"reads as example.com and matches the domain grant. A quoted local " +
				"part makes it valid RFC 5322, so it is not even malformed.",
		},
		{
			"a@b@example.com",
			"the same shape, minimal",
		},
		{
			"news@example.com@example.com",
			"the granted address used as a local part",
		},
		{
			"@example.com",
			"empty local part: the client's LastIndex still yields example.com",
		},
		// ⚠ These four exist because probing deleted the forbidden-character
		// check and every test stayed green. Every earlier case had its bad
		// character land in the DOMAIN half, where proxyNormalizeDomain catches
		// it independently — so nothing exercised the address-level check alone.
		// A local part with a forbidden character and a perfectly clean domain
		// is the only shape that can.
		{
			"news team@example.com",
			"space in the local part; the client sees domain example.com and allows",
		},
		{
			"news,other@example.com",
			"a comma: two addresses jammed into one From, and the client takes the last",
		},
		{
			"news<x@example.com",
			"an angle bracket in the local part",
		},
		{
			"\"news\"@example.com",
			"a quoted local part. Valid RFC 5322, and refused anyway: the proxy " +
				"would rather not fetch than guess. Fail-closed costs a broken " +
				"image here; guessing costs a fetch on a stranger's say-so.",
		},
	}
	for _, tc := range cases {
		t.Run(tc.from, func(t *testing.T) {
			if MatchingRemoteAllow(tc.from, allows) == nil {
				t.Fatalf("this case no longer diverges: the client now refuses %q too, "+
					"so it proves nothing about the proxy's strictness. %s", tc.from, tc.why)
			}
			if g := ProxyMatchingRemoteAllow(tc.from, allows); g != nil {
				t.Errorf("proxy ALLOWED %q via %s — it must fail closed here.\n%s",
					tc.from, describeAllow(g), tc.why)
			}
		})
	}
}

// TestProxyMatchNeverAllowsWhatTheClientRefuses is the direction that would be a
// security bug rather than a difference of opinion. It sweeps a corpus of
// hostile and malformed senders and asserts the proxy is never the more
// permissive of the two.
func TestProxyMatchNeverAllowsWhatTheClientRefuses(t *testing.T) {
	allows := allowsFor(
		[2]string{"address", "news@example.com"},
		[2]string{"domain", "example.com"},
		[2]string{"domain", "list.example.org"},
		// ⚠ An UNKNOWN kind whose value would match if the kind were ignored.
		// Without this row the corpus contains only valid kinds, so the kind
		// check has nothing to fire on — probing confirmed the whole test stayed
		// green with "unknown kinds match too" spliced in. The same trap is
		// documented on the client's own test, and this file walked into it
		// anyway.
		[2]string{"everything", "attacker.example"},
		// A domain grant carrying an "@", which proxyNormalizeDomain must refuse
		// on the GRANT side — the only place that check is load-bearing alone.
		[2]string{"domain", "evil.example@example.com"},
	)
	corpus := []string{
		"anyone@attacker.example", "news@attacker.example",
		"a@evil.example@example.com", "x@example.com",
		"", " ", "\t", "\n",
		"news", "news@", "@", "@@", "a@@b",
		"news@example.com", "NEWS@EXAMPLE.COM", " news@example.com ",
		"<news@example.com>", "News <news@example.com>",
		"news@example.com>", "\"news@example.com\"",
		"news@example.com, other@example.com",
		"news@example.com; other@example.com",
		"news@example.com (Newsletter)",
		"a@b@example.com", "attacker@evil.example@example.com",
		"@example.com", "news@.example.com", "news@example.com.",
		"news@exa..mple.com", "news@sub.example.com",
		"news@example.com\n", "news@example.com\r\nBcc: x@y",
		"news@EXAMPLE.com", "anyone@list.example.org",
		"news@example.com/../", "news@[127.0.0.1]",
	}
	for _, from := range corpus {
		clientOK := RemoteAllowsMatch(from, allows)
		proxyOK := ProxyRemoteAllowsMatch(from, allows)
		if proxyOK && !clientOK {
			t.Errorf("from %q: proxy ALLOWS but client REFUSES — the proxy must never "+
				"be the more permissive of the two; that is a fetch the blocking "+
				"policy exists to prevent", from)
		}
	}
}

// TestProxyMatchRefusesUnusableGrants asserts the GRANT side directly, rather
// than hoping the sweep above happens to cover it.
//
// ⚠ Both cases exist because probing found the sweep could not see them. The
// sweep only fires when the proxy is MORE permissive than the client, and for a
// grant that the client also refuses for its own reasons, that comparison stays
// quiet however wrong the proxy is. A test whose assertion is structurally
// unable to observe the mutation is not covering it.
func TestProxyMatchRefusesUnusableGrants(t *testing.T) {
	// ⚠ ONE CASE PER LOOP, because the kind is checked in two places and a value
	// that cannot match a given loop's rule proves nothing about that loop. The
	// first version used one address-shaped value for both; probing spliced
	// "unknown kinds match too" into the DOMAIN loop and the test stayed green,
	// because "news@example.com" can never normalise as a domain and so could
	// not have matched there however broken the kind check was.
	t.Run("unknown kind whose value is a valid ADDRESS", func(t *testing.T) {
		allows := allowsFor([2]string{"everything", "news@example.com"})
		if g := ProxyMatchingRemoteAllow("news@example.com", allows); g != nil {
			t.Errorf("an unknown grant kind allowed a fetch via %s — a future kind "+
				"must not come to mean \"everything\" to this code", describeAllow(g))
		}
	})
	t.Run("unknown kind whose value is a valid DOMAIN", func(t *testing.T) {
		allows := allowsFor([2]string{"everything", "example.com"})
		if g := ProxyMatchingRemoteAllow("news@example.com", allows); g != nil {
			t.Errorf("an unknown grant kind allowed a fetch via %s in the domain "+
				"loop — this is the case the address-shaped value could not see",
				describeAllow(g))
		}
	})
	t.Run("domain grant containing an @", func(t *testing.T) {
		// This is the only place proxyNormalizeDomain's "@" refusal is the sole
		// mechanism: nothing else inspects a grant value.
		allows := allowsFor([2]string{"domain", "evil.example@example.com"})
		for _, from := range []string{
			"a@evil.example@example.com",
			"a@example.com",
			"a@evil.example",
		} {
			if g := ProxyMatchingRemoteAllow(from, allows); g != nil {
				t.Errorf("from %q matched the unusable domain grant %s",
					from, describeAllow(g))
			}
		}
	})
}

// TestProxyMatchStillAllowsDomainGrants is the guard against fail-closed
// becoming fail-useless. proxyNormalizeAddress refuses a value with no "@", so
// running a domain GRANT through it would kill every domain grant while every
// refusal test above stayed green.
func TestProxyMatchStillAllowsDomainGrants(t *testing.T) {
	allows := allowsFor([2]string{"domain", "example.com"})
	for _, from := range []string{
		"anyone@example.com",
		"Someone@Example.com",
		"a.b+tag@example.com",
	} {
		if !ProxyRemoteAllowsMatch(from, allows) {
			t.Errorf("domain grant did not match %q — the proxy is refusing everything, "+
				"which is an outage rather than a guard", from)
		}
	}
}

func describeAllow(g *RemoteAllow) string {
	if g == nil {
		return "<none>"
	}
	return string(g.Kind) + " " + g.Value
}
