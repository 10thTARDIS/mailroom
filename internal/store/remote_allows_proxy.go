// SPDX-License-Identifier: AGPL-3.0-only

package store

import "strings"

// The PROXY's reading of a remote-content grant (ADR-026 §3c), which is
// deliberately NOT RemoteAllowsMatch.
//
// # Why there are two of these
//
// RemoteAllowsMatch mirrors the CLIENT, on purpose, because the question a
// report asks is "what would the client do" — and a report that answered a
// slightly different question would describe a mailbox nobody has.
//
// The proxy asks a different question: "should the SERVER fetch this". The two
// only ever diverge on input a sender chose, and on that input the client's
// answer is the wrong one to copy, because the client is describing an existing
// behaviour while the proxy is deciding whether to make a request. So this is
// fail-closed: anything it cannot read as exactly one ordinary address is
// refused rather than interpreted.
//
// ⚠ THE DIVERGENCE THAT MATTERS IS THE MULTIPLE-@ ADDRESS, and it is an attack
// rather than a curiosity. clientAddressDomain takes the text after the LAST
// "@", so a From of
//
//	attacker@evil.example@trusted.example
//
// reads as the domain "trusted.example" and matches a domain grant for it. A
// quoted local part makes that a well-formed RFC 5322 address, so it is not
// even malformed mail. Under the client rule the proxy would fetch on a
// stranger's say-so; under this one it does not.
//
// TestProxyMatchDivergesFromTheClientDeliberately pins every case where the two
// disagree, in both directions, so neither can drift into the other silently.

// ProxyMatchingRemoteAllow returns the grant that permits the SERVER to fetch
// remote content for this sender, or nil. Precedence is the same as the
// client's — exact address before domain — because when both rules agree that a
// sender is allowed, they must agree on WHY.
func ProxyMatchingRemoteAllow(from string, allows []RemoteAllow) *RemoteAllow {
	addr, domain, ok := proxySplitAddress(from)
	if !ok || len(allows) == 0 {
		return nil
	}
	for i := range allows {
		if allows[i].Kind == RemoteAllowAddress &&
			proxyNormalizeAddress(allows[i].Value) == addr {
			return &allows[i]
		}
	}
	for i := range allows {
		// ⚠ The GRANT is normalised as a bare domain, not as an address:
		// proxyNormalizeAddress would refuse "example.com" for having no "@".
		// Reusing the address normaliser here would make every domain grant
		// dead — fail-closed in the direction that silently disables the
		// feature, which is the failure this project keeps paying for.
		if allows[i].Kind == RemoteAllowDomain &&
			proxyNormalizeDomain(allows[i].Value) == domain {
			return &allows[i]
		}
	}
	// An unrecognised kind matches nothing, so a future kind cannot come to mean
	// "everything" to older code.
	return nil
}

// ProxyRemoteAllowsMatch reports whether the server may fetch for this sender.
func ProxyRemoteAllowsMatch(from string, allows []RemoteAllow) bool {
	return ProxyMatchingRemoteAllow(from, allows) != nil
}

// proxyForbiddenInAddress are the characters that mean this string is not a
// bare address: a display name, a header list, or a comment has come through
// with it. Any of them and the value is refused rather than salvaged.
const proxyForbiddenInAddress = " \t\r\n<>,;\"()[]\\"

// proxySplitAddress lowercases, trims, and splits an address into itself and
// its domain, REFUSING anything that is not exactly one ordinary address.
//
// ⚠ IT IS ONE SPLIT, ON PURPOSE, and the address and the domain it returns are
// two halves of the same decision. An earlier version had proxyNormalizeAddress
// and proxyAddressDomain each find the "@" independently, and probing exposed
// what that bought: mutating one of them to LastIndex left every test green,
// because the two then disagreed and the mismatch happened to fail closed. It
// was right by accident, and a test cannot tell "correct" from "accidentally
// not wrong" when two copies of a rule have to agree.
//
// Splitting on the FIRST "@" and requiring everything after it to be a valid
// domain is what makes "exactly one @" true — proxyNormalizeDomain refuses a
// value containing "@", so "a@b@c" fails as the domain "b@c". An explicit
// strings.Count guard used to sit here too and was deleted for the same reason:
// probing showed no test could distinguish which of the two refused the input.
//
// Returns ok=false for every refusal, so a caller that forgets to check gets
// "no match" rather than a permissive one.
func proxySplitAddress(addr string) (normalized, domain string, ok bool) {
	a := strings.ToLower(strings.TrimSpace(addr))
	if a == "" || strings.ContainsAny(a, proxyForbiddenInAddress) {
		return "", "", false
	}
	at := strings.Index(a, "@")
	if at <= 0 { // at == 0 is an empty local part
		return "", "", false
	}
	d := proxyNormalizeDomain(a[at+1:])
	if d == "" {
		return "", "", false
	}
	return a, d, true
}

// proxyNormalizeAddress is proxySplitAddress's first return, for comparing an
// address GRANT's value against a From. Returns "" on refusal.
func proxyNormalizeAddress(addr string) string {
	a, _, ok := proxySplitAddress(addr)
	if !ok {
		return ""
	}
	return a
}

// proxyNormalizeDomain lowercases and trims a bare domain, refusing anything
// that is not a plain dotted name. Returns "" on refusal.
func proxyNormalizeDomain(domain string) string {
	d := strings.ToLower(strings.TrimSpace(domain))
	if d == "" || strings.ContainsAny(d, proxyForbiddenInAddress) {
		return ""
	}
	if strings.Contains(d, "@") || strings.Contains(d, "/") {
		return ""
	}
	// A leading, trailing or doubled dot is a typo or an evasion, never a name
	// that resolves the way it looks like it does.
	if strings.HasPrefix(d, ".") || strings.HasSuffix(d, ".") || strings.Contains(d, "..") {
		return ""
	}
	return d
}
