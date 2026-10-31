// SPDX-License-Identifier: AGPL-3.0-only

package search

import (
	"fmt"
	"strings"
	"unicode"
)

// SyntaxError is a query the parser understood well enough to know it can match
// nothing (#712). The API answers it with invalidArguments instead of an empty
// list, because "no results" and "your query is malformed" look identical to a
// user staring at a blank page.
type SyntaxError struct {
	Operator string // "from", "to", ...
	Value    string // the value as typed
	Reason   string
}

func (e *SyntaxError) Error() string {
	return fmt.Sprintf("invalid search %s: filter %q: %s", e.Operator, e.Value, e.Reason)
}

// addressNode compiles a from:/to:/cc: value.
//
//	from:alice@shop.example    exact address (unchanged)
//	from:shop.example          that domain AND its subdomains   (#712)
//	from:*.shop.example        the same
//	from:@shop.example         the same
//	from:alice                 exact address match, as before — no '@' and no dot
//	                           is not a domain, and is left to match whatever it
//	                           matched before (nothing, in practice: addresses
//	                           always contain '@')
//	from:*foo  from:foo*       SyntaxError: '*' is only valid as a leading "*."
//	from:alice@  from:@        SyntaxError: nothing left to match
//
// A literally empty value (`from:` with nothing after it — the state of the
// search box mid-keystroke) is NOT an error: it keeps matching nothing, as it
// always has, so typing an operator does not flash a validation error.
func addressNode(op string, addr, domain Field, raw string) (Node, error) {
	v := normalizeExact(raw)
	if v == "" {
		return ExactTerm(addr, v), nil
	}
	fail := func(reason string) (Node, error) {
		return nil, &SyntaxError{Operator: op, Value: raw, Reason: reason}
	}

	marker := "" // an explicit domain marker: leading "*.", "@" or "."
	for _, m := range []string{"*.", "@", "."} {
		if strings.HasPrefix(v, m) {
			marker, v = m, v[len(m):]
			break
		}
	}
	v = strings.TrimRight(v, ".")
	if v == "" {
		return fail("no domain given; try a domain such as example.com")
	}
	if strings.Contains(v, "*") {
		return fail("'*' is only supported as a leading \"*.\" before a domain, e.g. *.example.com")
	}

	if marker == "" {
		at := strings.IndexByte(v, '@')
		switch {
		case at >= 0:
			if at == len(v)-1 {
				return fail("the address has no domain after the '@'")
			}
			return ExactTerm(addr, v), nil // a full address stays exact
		case !strings.Contains(v, "."):
			return ExactTerm(addr, v), nil // "alice": not a domain, behaves as before
		}
	}

	if !validDomain(v) {
		return fail("not a valid domain; use letters, digits and hyphens separated by dots")
	}
	if !strings.Contains(v, ".") {
		// "*.com" / ".com": a bare TLD is not indexed (it would be every
		// message), so this could only ever match nothing.
		return fail("a top-level domain on its own is not searchable; add the name before it")
	}
	return ExactTerm(domain, v), nil
}

// FromExactAddress reports what `from:<value>` compiles to when that is an
// EXACT ADDRESS term, and false when it is anything else — a domain term
// (#712: "example.com", "@example.com", "*.example.com", ".example.com"), a
// syntax error, or nothing at all. It is the parser's own answer, exported so
// a caller holding a sender key (the clean-up ranking, #53) can refuse one the
// search would read as more than one address.
func FromExactAddress(value string) (string, bool) {
	n, err := addressNode("from", FieldFrom, FieldFromDomain, value)
	if err != nil {
		return "", false
	}
	t, ok := n.(Term)
	if !ok || t.Field != FieldFrom || t.Match != MatchExact || t.Value == "" || !strings.Contains(t.Value, "@") {
		return "", false
	}
	return t.Value, true
}

// validDomain reports whether every dot-separated label is non-empty and made of
// letters, digits and hyphens (any script: internationalized names are indexed
// as typed, lowercased).
func validDomain(d string) bool {
	if d == "" {
		return false
	}
	for _, label := range strings.Split(d, ".") {
		if label == "" {
			return false
		}
		for _, r := range label {
			if r != '-' && !unicode.IsLetter(r) && !unicode.IsDigit(r) {
				return false
			}
		}
	}
	return true
}

// looksLikeDomain reports whether a bare query word reads as a hostname: it has
// a dot and every part is alphanumeric or a hyphen. Such a word is searched as a
// PHRASE (#712) — see parseToken. Anything with other punctuation (an address, a
// URL, a version with a suffix) keeps its previous treatment.
func looksLikeDomain(w string) bool {
	return strings.Contains(w, ".") && validDomain(w)
}

// looksLikeEmail reports whether a bare query word is a complete email address:
// one '@', a local part of ordinary address characters, and a dotted domain.
// Such a word is matched exactly against the address fields (#728); see parseToken.
func looksLikeEmail(w string) bool {
	local, domain, ok := strings.Cut(w, "@")
	if !ok || local == "" || !looksLikeDomain(domain) {
		return false
	}
	for _, r := range local {
		if r != '.' && r != '_' && r != '-' && r != '+' && r != '\'' && !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

// AddressDomainTerm returns the domain-view term for a bare domain-looking value
// ("shop.example", "*.shop.example", "@shop.example"), or false when v is not
// one. It is for callers (the JMAP from/to/cc conditions) that match an address
// condition several ways and want the domain way too. addr is FieldFrom or
// FieldTo.
func AddressDomainTerm(addr Field, v string) (Node, bool) {
	dom := FieldFromDomain
	if addr == FieldTo {
		dom = FieldToDomain
	}
	v = strings.TrimSpace(v)
	if !strings.HasPrefix(v, "*.") && !strings.HasPrefix(v, "@") && !looksLikeDomain(v) {
		return nil, false
	}
	n, err := addressNode("", addr, dom, v)
	if err != nil {
		return nil, false
	}
	if t, ok := n.(Term); ok && t.Field == dom {
		return t, true
	}
	return nil, false
}
