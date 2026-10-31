// SPDX-License-Identifier: AGPL-3.0-only

package ml

import (
	"strings"
	"time"

	"golang.org/x/net/idna"

	"mailroom/internal/authserv"
)

// THE ONE Authentication-Results TOKENIZER (#714 security review).
//
// RFC 8601 §2.2: `authserv-id [CFWS version] ";" resinfo *(";" resinfo)`,
// where each resinfo is `method[/version] "=" result [reasonspec]
// *(ptype "." property "=" value)`. Every caller (the display property, the
// sender warnings, the cold-start feature, learned placement) reads a header
// through this, so a property can only ever be attributed to the clause it
// was written in.
//
// ⚠ WHY IT REPLACED A REGULAR EXPRESSION. The previous parser was
// `\b(spf|dkim|dmarc)\s*=\s*([a-z]+)` over the whole header value. It read
// tokens inside comments, and inside OTHER clauses' property values, where
// the sender controls the text: an envelope sender with the local part
// `dkim=pass` (legal in RFC 5321) put `smtp.mailfrom=dkim=pass@x.example`
// into the provider's own, trusted header, and the regex reported a DKIM
// pass. Here, comments are removed first, and a `method=result` pair is only
// recognised as a TOKEN of its own whose name has no '.', so a property value
// can never start a clause.
//
// Fail closed throughout: anything that does not parse as a clause is
// ignored, so a malformed header authenticates nothing.

// ResultClause is one resinfo: a method's result and the properties written
// in the same clause.
type ResultClause struct {
	// Method is the lowercased method name without any "/version".
	Method string
	// Result is the lowercased result keyword (letters only).
	Result string
	// Props maps a lowercased "ptype.property" to every value it was given in
	// THIS clause, unquoted. A property repeated in one clause keeps every
	// value, so a caller can refuse the ambiguity rather than pick one.
	Props map[string][]string
}

// parseResinfo returns the result clauses of one header value, in order.
func parseResinfo(v string) []ResultClause {
	segs := splitUnquoted(stripComments(v), ';')
	if len(segs) < 2 {
		// No ';': RFC 8601 has no resinfo without one. The whole value is at
		// most an authserv-id.
		return nil
	}
	var out []ResultClause
	for _, seg := range segs[1:] {
		var cur *ResultClause
		for _, tok := range tokens(seg) {
			name, val, ok := strings.Cut(tok, "=")
			if !ok {
				continue // "none", or junk
			}
			name = strings.ToLower(name)
			switch {
			case name == "":
				continue
			case strings.Contains(name, "."):
				if cur != nil {
					cur.Props[name] = append(cur.Props[name], unquote(val))
				}
			case name == "reason":
				// reasonspec: free text about the clause, never a result.
			default:
				method, _, _ := strings.Cut(name, "/")
				out = append(out, ResultClause{Method: method, Result: resultKeyword(val), Props: map[string][]string{}})
				cur = &out[len(out)-1]
			}
		}
	}
	return out
}

// resultKeyword keeps the leading letters of a result, lowercased.
func resultKeyword(v string) string {
	v = strings.ToLower(v)
	for i, r := range v {
		if r < 'a' || r > 'z' {
			return v[:i]
		}
	}
	return v
}

// stripComments removes RFC 5322 comments, nested ones included, honouring
// quoted strings and quoted-pairs (a '(' inside quotes is text). An
// unterminated comment swallows the rest of the value: fail closed.
func stripComments(v string) string {
	var b strings.Builder
	depth, inQuote, escaped := 0, false, false
	for _, r := range v {
		switch {
		case escaped:
			escaped = false
			if depth == 0 {
				b.WriteRune(r)
			}
			continue
		case r == '\\':
			escaped = true
			if depth == 0 {
				b.WriteRune(r)
			}
			continue
		}
		if inQuote {
			b.WriteRune(r)
			if r == '"' {
				inQuote = false
			}
			continue
		}
		switch r {
		case '(':
			depth++
			b.WriteRune(' ')
		case ')':
			if depth > 0 {
				depth--
			}
		case '"':
			if depth == 0 {
				inQuote = true
				b.WriteRune(r)
			}
		default:
			if depth == 0 {
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}

// splitUnquoted splits s on sep outside quoted strings.
func splitUnquoted(s string, sep rune) []string {
	var out []string
	var b strings.Builder
	inQuote, escaped := false, false
	for _, r := range s {
		switch {
		case escaped:
			escaped = false
		case r == '\\' && inQuote:
			escaped = true
		case r == '"':
			inQuote = !inQuote
		case r == sep && !inQuote:
			out = append(out, b.String())
			b.Reset()
			continue
		}
		b.WriteRune(r)
	}
	return append(out, b.String())
}

// tokens splits a comment-free resinfo into whitespace-separated tokens,
// outside quotes, after joining CFWS around '=' ("dmarc = fail" is one token).
func tokens(seg string) []string {
	var out []string
	var b strings.Builder
	inQuote, escaped := false, false
	flush := func() {
		if b.Len() > 0 {
			out = append(out, b.String())
			b.Reset()
		}
	}
	rs := []rune(seg)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case escaped:
			escaped = false
			b.WriteRune(r)
		case inQuote:
			b.WriteRune(r)
			switch r {
			case '\\':
				escaped = true
			case '"':
				inQuote = false
			}
		case r == '"':
			inQuote = true
			b.WriteRune(r)
		case r == ' ' || r == '\t' || r == '\r' || r == '\n':
			// Whitespace before '=' or right after it joins the token.
			j := i
			for j < len(rs) && (rs[j] == ' ' || rs[j] == '\t' || rs[j] == '\r' || rs[j] == '\n') {
				j++
			}
			prevEq := b.Len() > 0 && strings.HasSuffix(b.String(), "=")
			nextEq := j < len(rs) && rs[j] == '='
			if !prevEq && !nextEq {
				flush()
			}
			i = j - 1
		default:
			b.WriteRune(r)
		}
	}
	flush()
	return out
}

// unquote removes one level of quoted-string quoting.
func unquote(v string) string {
	if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		v = v[1 : len(v)-1]
		return strings.NewReplacer(`\"`, `"`, `\\`, `\`).Replace(v)
	}
	return v
}

// NormalizeAuthDomain lowercases a domain, drops a trailing root dot and
// converts it to its ASCII (punycode) form, so a U-label and its A-label
// compare equal. A name IDNA refuses keeps its lowercased form, which then
// matches only itself.
func NormalizeAuthDomain(d string) string {
	d = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(d)), ".")
	if d == "" {
		return ""
	}
	if a, err := idna.Lookup.ToASCII(d); err == nil && a != "" {
		return a
	}
	return d
}

// DMARCAlignment is how a DMARC pass's header.from may relate to the From
// domain a caller acts on. There is no default: every caller names one, so no
// caller changes behaviour by accident. The zero value is not a mode and
// matches nothing (fail closed).
type DMARCAlignment int

const (
	// DMARCStrict: header.from must EQUAL the From domain.
	DMARCStrict DMARCAlignment = iota + 1
	// DMARCRelaxed (#846, RFC 7489 §3.1 relaxed, narrowed): header.from may
	// also be the From domain's ORGANISATIONAL domain. See DMARCHeaderFromMatch.
	DMARCRelaxed
)

// DMARCMatch says how a pass's header.from matched the From domain.
type DMARCMatch string

const (
	// DMARCNoMatch: no pass counts for the domain.
	DMARCNoMatch DMARCMatch = ""
	// DMARCExactMatch: header.from equals the From domain.
	DMARCExactMatch DMARCMatch = "exact"
	// DMARCOrgDomainMatch: header.from is the From domain's organisational
	// domain and the From domain is a strict subdomain of it (DMARCRelaxed
	// only).
	DMARCOrgDomainMatch DMARCMatch = "org-domain"
)

// DMARCHeaderFromMatch is THE rule for whether one header.from value H speaks
// for the From domain D, after NormalizeAuthDomain on both:
//
//   - exact (both modes): H == D, as it always was;
//   - org-domain (DMARCRelaxed only, #846): D is a STRICT subdomain of H, and
//     H == OrgDomain(D), and H is not itself a public suffix.
//
// Everything else is no match. In particular, under DMARCRelaxed:
//
//   - a public suffix never speaks for anything beneath it (`co.uk`,
//     `example`, a private-section suffix): OrgDomain(D) is never a suffix;
//   - a SIBLING (`shop.bank.example` for `news.bank.example`) is not a
//     parent at all;
//   - an INTERMEDIATE parent (`news.bank.example` for `a.news.bank.example`)
//     and a parent ABOVE the organisational domain are not OrgDomain(D);
//   - the REVERSE (a pass for `news.bank.example` covering `bank.example`)
//     is refused: D must be the subdomain;
//   - a look-alike suffix (`evilbank.example` is not under `bank.example`)
//     fails the label-boundary test;
//   - any Public Suffix List error, and any name that is not a valid IDNA
//     lookup hostname in its A-label form, refuses (the exact branch keeps its
//     old, compare-only behaviour).
//
// An unknown mode (the zero value included) matches nothing, exact included.
//
// WHY THIS IS NOT A NEW TRUST. The receiving MTA evaluated DMARC for the
// message's one From domain (a caller that acts on this also refuses any From
// that is not exactly one mailbox in one header, so the MTA's From and D are
// the same). Some MTAs write the policy (organisational) domain into
// header.from rather than the From domain itself; DMARC's own relaxed
// alignment already treats every name under the organisational domain as one
// owner. What this does NOT accept is a pass whose header.from names anything
// but D or exactly D's organisational domain.
func DMARCHeaderFromMatch(headerFrom, domain string, mode DMARCAlignment) DMARCMatch {
	if mode != DMARCStrict && mode != DMARCRelaxed {
		return DMARCNoMatch
	}
	d, h := NormalizeAuthDomain(domain), NormalizeAuthDomain(headerFrom)
	if d == "" || h == "" {
		return DMARCNoMatch
	}
	if h == d {
		return DMARCExactMatch
	}
	if mode != DMARCRelaxed {
		return DMARCNoMatch
	}
	if !lookupHostname(d) || !lookupHostname(h) {
		return DMARCNoMatch
	}
	if !strings.HasSuffix(d, "."+h) {
		return DMARCNoMatch // not a strict subdomain on a label boundary
	}
	org, ok := OrgDomain(d)
	if !ok || org != h {
		return DMARCNoMatch
	}
	// Defence in depth: H must be its own organisational domain, never a
	// public suffix. OrgDomain(D) cannot be one by construction (the PSL's
	// longest matching rule would have been H), so this guards the library,
	// not the input.
	if hOrg, ok := OrgDomain(h); !ok || hOrg != h {
		return DMARCNoMatch
	}
	return DMARCOrgDomainMatch
}

// lookupHostname reports a name that IDNA's lookup profile accepts unchanged
// (an A-label hostname: STD3 letters, digits and hyphens, valid punycode).
// NormalizeAuthDomain keeps a refused name in lowercased form; the relaxed
// branch refuses it instead.
func lookupHostname(d string) bool {
	a, err := idna.Lookup.ToASCII(d)
	return err == nil && a == d
}

// DMARCPassFor reports a DMARC pass BOUND TO domain (the From domain the
// caller acts on) under mode: DMARCMatchFor(domain, mode) != DMARCNoMatch.
//
// ⚠ Meaningful only on a trusted parse (TopmostTrustedAuthResults).
func (a AuthResults) DMARCPassFor(domain string, mode DMARCAlignment) bool {
	return a.DMARCMatchFor(domain, mode) != DMARCNoMatch
}

// DMARCMatchFor reports how a DMARC pass is bound to domain, for learned
// placement (#714) and every other caller of DMARCPassFor:
//
//   - ANY dmarc=fail clause vetoes, whatever domain it names or omits.
//   - A pass counts only when its OWN clause carries exactly one header.from,
//     and that value matches domain under mode (DMARCHeaderFromMatch). A pass
//     with no header.from, or with two, is not a pass: fail closed.
//   - An exact match outranks an org-domain one when both are present.
//
// ⚠ Meaningful only on a trusted parse (TopmostTrustedAuthResults).
func (a AuthResults) DMARCMatchFor(domain string, mode DMARCAlignment) DMARCMatch {
	if NormalizeAuthDomain(domain) == "" {
		return DMARCNoMatch
	}
	best := DMARCNoMatch
	for _, c := range a.Clauses {
		if c.Method != "dmarc" {
			continue
		}
		switch c.Result {
		case "fail":
			return DMARCNoMatch
		case "pass":
			hf := c.Props["header.from"]
			if len(hf) != 1 {
				continue
			}
			switch DMARCHeaderFromMatch(hf[0], domain, mode) {
			case DMARCExactMatch:
				best = DMARCExactMatch
			case DMARCOrgDomainMatch:
				if best == DMARCNoMatch {
					best = DMARCOrgDomainMatch
				}
			}
		}
	}
	return best
}

// TopmostTrustedAuthResults parses only the TOPMOST Authentication-Results
// header whose authserv-id is trusted (values are in message order, topmost
// first, as internal/mime returns them).
//
// WHY ONLY ONE (#714). TrustedAuthResults believes every header bearing a
// trusted id. RFC 8601 §5 asks the receiving MTA to remove headers bearing
// its own id from incoming mail, but nothing here can verify that a given
// provider does. A sender who writes `Authentication-Results:
// mx.provider.example; dmarc=pass header.from=bank.example` into their
// message puts it BELOW the headers the receiving MTA prepends, so the
// topmost trusted one is the provider's own. Learned placement reads only
// that one. A provider that splits its verdicts over several headers would
// lose the lower ones, which fails closed (no placement).
func TopmostTrustedAuthResults(values []string, trusted []string) AuthResults {
	ok := trustedSet(trusted)
	for _, v := range values {
		if ok[authservIDOf(v)] {
			return AuthResultsOf([]string{v})
		}
	}
	return AuthResults{Results: map[string]map[string]bool{}}
}

func trustedSet(trusted []string) map[string]bool {
	ok := make(map[string]bool, len(trusted))
	for _, id := range trusted {
		id = normalizeAuthservID(id)
		// ⚠ A RAW BOUNDED ENTRY IS NEVER A TRUSTED ID (#766). `id<date` has
		// not been through TrustedAt, so nobody has checked it against the
		// message's received time; it must match nothing rather than be read
		// as the bare id. normalizeAuthservID leaves the '<' alone, and this
		// drops the entry outright so not even a header whose authserv-id is
		// literally `id<date` can match it.
		if id != "" && !strings.Contains(id, authserv.BoundSep) {
			ok[id] = true
		}
	}
	return ok
}

// TrustedAt resolves MAILROOM_AUTHSERV_IDS entries to the PLAIN ids trusted
// for ONE message, judged at the message's received time. Every consumer of a
// trust decision calls it immediately before TrustedAuthResults,
// TopmostTrustedAuthResults, IsTrustedAuthservID or ical.Verified.
//
//   - `id` is trusted for every message.
//   - `id<YYYY-MM-DD` is trusted only when received is non-zero and strictly
//     before 00:00 UTC on that date.
//   - A malformed entry is dropped (config.ValidateAuthservIDs refuses to
//     start on one, so this is defence in depth).
//
// ⚠ WHY A DATE BOUND EXISTS AT ALL. RFC 8601 §5 has an MTA strip only the
// headers bearing ITS OWN current authserv-id. After a provider changes the id
// it stamps, a header bearing the OLD id is no longer stripped on arrival: a
// sender can write one and it passes through untouched. Trusting the old id
// forever would therefore re-open the forgery hole for all new mail, so it is
// trusted only for mail that arrived while it was still the provider's id.
//
// ⚠ received IS THE MESSAGE'S INTERNALDATE (messages.received_at), SET BY THE
// PROVIDER AT DELIVERY. NEVER the Date header: the sender writes that, and a
// forger would simply back-date the message to fall inside the bound.
//
// ⚠ FAIL-CLOSED, TWICE. A zero received time trusts NO bounded entry (an
// unknown time is not "before"). And the entries a caller passes WITHOUT
// resolving them are inert: trustedSet drops any entry containing '<', so a
// call site that forgets TrustedAt trusts less, never more.
func TrustedAt(entries []string, received time.Time) []string {
	var out []string
	for _, raw := range entries {
		e, ok, err := authserv.Parse(raw)
		if err != nil || !ok {
			continue
		}
		if e.Bounded && (received.IsZero() || !received.Before(e.Bound)) {
			continue
		}
		out = append(out, e.ID)
	}
	return out
}
