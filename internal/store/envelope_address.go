// SPDX-License-Identifier: AGPL-3.0-only

package store

import "strings"

// CanonicalEnvelopeAddress validates an SMTP envelope address (an
// EmailSubmission envelope's mailFrom or rcptTo email) as a STRICT ASCII
// addr-spec and returns its canonical, lowercased form (#852 review).
//
// Strict, because the envelope is handed to SMTP and a key's envelope
// recipients lead the approval summary: anything looser lets free text
// through. net/mail.ParseAddress was not enough on its own, because it
// accepts quoted local parts and non-ASCII runes, so a quoted local part
// with no-break spaces could still read as summary prose. The rule:
//
//   - only printable ASCII (no rune above 0x7E, no space or control);
//   - local part: an RFC 5322 dot-atom of atext (A-Z a-z 0-9 and
//     !#$%&'*+/=?^_`{|}~-). No quoted string, no leading, trailing or
//     doubled dot, at most 64 octets;
//   - domain: LDH labels (A-labels such as xn--... included), each 1-63
//     characters, not starting or ending with '-', at least two labels, at
//     most 253 octets. IP literals are refused;
//   - at most 254 octets in all.
//
// ASCII only matches the send path: provider.ErrEAIUnsupported refuses every
// non-ASCII envelope address, because the SMTP client cannot emit SMTPUTF8.
// No surrounding whitespace or angle brackets are accepted: an envelope
// email is a bare address.
func CanonicalEnvelopeAddress(s string) (string, bool) {
	if s == "" || len(s) > 254 {
		return "", false
	}
	for i := 0; i < len(s); i++ {
		if s[i] <= 0x20 || s[i] > 0x7E {
			return "", false
		}
	}
	at := strings.LastIndexByte(s, '@')
	if at <= 0 || at == len(s)-1 {
		return "", false
	}
	local, domain := s[:at], s[at+1:]
	if !validDotAtom(local) || !validLDHDomain(domain) {
		return "", false
	}
	return strings.ToLower(s), true
}

func validDotAtom(local string) bool {
	if len(local) == 0 || len(local) > 64 {
		return false
	}
	for _, part := range strings.Split(local, ".") {
		if part == "" { // leading, trailing or doubled dot
			return false
		}
		for i := 0; i < len(part); i++ {
			if !isAtext(part[i]) {
				return false
			}
		}
	}
	return true
}

func isAtext(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	}
	return strings.IndexByte("!#$%&'*+/=?^_`{|}~-", c) >= 0
}

func validLDHDomain(domain string) bool {
	if len(domain) > 253 {
		return false
	}
	labels := strings.Split(domain, ".")
	if len(labels) < 2 {
		return false
	}
	for _, l := range labels {
		if len(l) == 0 || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return false
		}
		for i := 0; i < len(l); i++ {
			if !isLDH(l[i]) {
				return false
			}
		}
	}
	return true
}

func isLDH(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-'
}
