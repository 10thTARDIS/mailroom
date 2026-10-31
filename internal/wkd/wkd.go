// SPDX-License-Identifier: AGPL-3.0-only

// Package wkd implements the address-to-path mapping of the Web Key Directory
// (spec §9, Forgejo #48).
//
// # The standard, and why the revision is pinned here
//
// WKD is an INTERNET-DRAFT, not an RFC: draft-koch-openpgp-webkey-service.
// This package implements REVISION 21, which spec §9 requires be pinned and
// recorded rather than tracked silently. A draft can change under us; a
// published key that a correspondent's client can no longer find is a failure
// nobody gets told about, so the revision we conform to is a fact about this
// code and belongs next to it.
//
// # The two methods, and the operator trap between them
//
// A client looking up user@example.com tries, IN ORDER:
//
//	advanced: https://openpgpkey.example.com/.well-known/openpgpkey/example.com/hu/<hash>?l=<localpart>
//	direct:   https://example.com/.well-known/openpgpkey/hu/<hash>?l=<localpart>
//
// ⚠ Clients MUST try advanced FIRST. So a half-configured `openpgpkey`
// subdomain — one that resolves but does not serve the right thing — breaks
// ALL lookups, including ones the direct method would have answered. The
// subdomain must be absent or fully correct; there is no working middle. That
// is an operator-side DNS fact this code cannot enforce, which is exactly why
// it is written down here and in the deployment docs.
//
// # What is deliberately not here
//
// Fetching OTHER people's keys over WKD is a different job with a different
// threat model (SSRF: an attacker-chosen address becomes an outbound request).
// It is not in this package and needs the allowlist/no-internal-ranges/
// DNS-rebind guard spec §9 requires.
package wkd

import (
	"crypto/sha1" //nolint:gosec // REQUIRED by the WKD draft; not a security choice
	"fmt"
	"strings"
)

// DraftRevision is the revision of draft-koch-openpgp-webkey-service this
// package implements. Recorded per spec §9 so an upgrade is a deliberate act.
const DraftRevision = 21

// zBase32Alphabet is the encoding the draft mandates for the hashed local
// part. It is NOT RFC 4648 base32 — the alphabet is permuted so that the
// characters easiest to confuse by eye are least likely to appear. Using the
// stdlib's base32 here would produce paths that look right and resolve to
// nothing.
const zBase32Alphabet = "ybndrfg8ejkmcpqxot1uwisza345h769"

// HashLocalPart maps a local part to its WKD directory name.
//
// The mapping is: lowercase the local part with plain ASCII rules, take the
// SHA-1, and encode the 20 bytes as z-base-32 — yielding 32 characters.
//
// SHA-1 is not a security decision here and its collision weakness is not
// relevant: this is a directory-name mapping specified by the draft, and using
// anything else would simply produce a path no client looks at.
//
// ⚠ The lowercasing is ASCII-only ON PURPOSE. The draft specifies plain
// down-casing, and Unicode-aware casing differs for some inputs (the Turkish
// dotless i is the classic one), which would put a key at a path the
// correspondent's client never asks for.
func HashLocalPart(localPart string) string {
	lowered := asciiLower(localPart)
	sum := sha1.Sum([]byte(lowered)) //nolint:gosec // see above
	return zBase32(sum[:])
}

func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

// zBase32 encodes bytes with the WKD alphabet, without padding.
func zBase32(src []byte) string {
	var sb strings.Builder
	sb.Grow((len(src)*8 + 4) / 5)

	var buf uint16
	var bits uint
	for _, b := range src {
		buf = buf<<8 | uint16(b)
		bits += 8
		for bits >= 5 {
			bits -= 5
			sb.WriteByte(zBase32Alphabet[(buf>>bits)&0x1f])
		}
	}
	if bits > 0 {
		sb.WriteByte(zBase32Alphabet[(buf<<(5-bits))&0x1f])
	}
	return sb.String()
}

// SplitAddress separates an address into its local part and domain.
func SplitAddress(addr string) (localPart, domain string, err error) {
	at := strings.LastIndex(addr, "@")
	if at <= 0 || at == len(addr)-1 {
		return "", "", fmt.Errorf("wkd: %q is not an address", addr)
	}
	return addr[:at], asciiLower(addr[at+1:]), nil
}

// DirectPath is the request path a client uses for the DIRECT method, i.e.
// what this server answers. It does not include the ?l= query parameter, which
// clients send and servers are not required to use.
//
//	/.well-known/openpgpkey/hu/<hash>
func DirectPath(localPart string) string {
	return "/.well-known/openpgpkey/hu/" + HashLocalPart(localPart)
}

// AdvancedPath is the path under the openpgpkey subdomain. Mailroom serves the
// direct form; this exists so the operator documentation and the key page can
// show the correspondent-side URL that a client will actually try FIRST.
func AdvancedPath(localPart, domain string) string {
	return "/.well-known/openpgpkey/" + asciiLower(domain) + "/hu/" + HashLocalPart(localPart)
}

// PolicyPath is the WKD policy file. Its mere existence tells a client the
// domain supports WKD; an empty body is valid and is what we serve. A domain
// serving keys but no policy file is not a WKD domain as far as some clients
// are concerned.
const PolicyPath = "/.well-known/openpgpkey/policy"
