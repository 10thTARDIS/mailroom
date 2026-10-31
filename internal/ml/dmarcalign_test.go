// SPDX-License-Identifier: AGPL-3.0-only

package ml

import (
	"testing"

	"golang.org/x/net/publicsuffix"
)

// prodShape is an Authentication-Results header in the shape a provider that
// writes the DMARC POLICY domain into header.from stamps on a subdomain
// sender's mail (#840: every refused sender looked like this). Names are
// placeholders; the structure is the measured one.
func prodShape(headerFrom string) string {
	return "mx1.provider.example;\r\n" +
		"\tdkim=pass header.d=bank.example header.s=s1 header.b=AbCdEf12;\r\n" +
		"\tspf=pass (mx1.provider.example: domain of bounce@news.bank.example designates 192.0.2.10 as permitted sender) smtp.mailfrom=bounce@news.bank.example;\r\n" +
		"\tdmarc=pass (policy=reject) header.from=" + headerFrom
}

// TestPSLFixtures pins the Public Suffix List facts the alignment tests
// below rely on, so a list update that changes one turns THIS red with a
// reason, instead of silently turning an alignment case into a test of
// nothing.
func TestPSLFixtures(t *testing.T) {
	for _, c := range []struct {
		domain, suffix string
		icann          bool
	}{
		{"news.example.co.uk", "co.uk", true},                 // multi-level ICANN suffix
		{"news.tenant.github.io", "github.io", false},         // private-section suffix
		{"news.foo.bar.kawasaki.jp", "bar.kawasaki.jp", true}, // wildcard rule *.kawasaki.jp
	} {
		if s, icann := publicsuffix.PublicSuffix(c.domain); s != c.suffix || icann != c.icann {
			t.Errorf("fixture: PublicSuffix(%q) = %q icann=%v, want %q icann=%v", c.domain, s, icann, c.suffix, c.icann)
		}
	}
	// kawasaki.jp is ABOVE the wildcard suffix yet not itself a suffix: the
	// case where "H is not a public suffix" alone would accept a parent
	// beyond the organisational domain.
	if e, err := publicsuffix.EffectiveTLDPlusOne("kawasaki.jp"); err != nil || e != "kawasaki.jp" {
		t.Errorf("fixture: EffectiveTLDPlusOne(kawasaki.jp) = %q, %v; want itself", e, err)
	}
}

// TestOrgDomain: the one PSL helper.
//
// NEGATIVE CONTROLS (run on 2026-10-23): with the `NormalizeAuthDomain(d) != d`
// refusal removed, the uppercase row goes red (the trailing-dot row stays
// refused by the list's own empty-label error); with EffectiveTLDPlusOne
// replaced by the identity, every suffix and subdomain row goes red, and so
// do DKIMAligned's public-suffix cases in internal/ingest.
func TestOrgDomain(t *testing.T) {
	for _, c := range []struct {
		in, want string
		ok       bool
	}{
		{"news.bank.example", "bank.example", true},
		{"bank.example", "bank.example", true},
		{"a.b.news.example.co.uk", "example.co.uk", true},
		{"example.co.uk", "example.co.uk", true},
		{"news.tenant.github.io", "tenant.github.io", true},
		{"xn--bnk-qla.example", "xn--bnk-qla.example", true},
		{"co.uk", "", false},
		{"github.io", "", false},
		{"example", "", false},
		{"", "", false},
		{"News.Bank.Example", "", false}, // not normalised: the caller must normalise
		{"bank.example.", "", false},     // not normalised
	} {
		got, ok := OrgDomain(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("OrgDomain(%q) = %q, %v; want %q, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

// TestDMARCHeaderFromMatch is #846's rule, one row per accepted shape and per
// refusal the issue lists. Each row runs in BOTH modes: strict must match only
// the exact rows, and relaxed must add exactly the org-domain rows.
//
// NEGATIVE CONTROLS (each run on 2026-10-23, one mutation at a time):
//   - `org != h` removed: only "parent above the org domain, itself no
//     suffix" (kawasaki.jp) goes red. The H-is-its-own-org-domain check still
//     refuses every public suffix and the intermediate parent, which is why
//     the kawasaki.jp row exists: it is the input only `org != h` stops;
//   - `org != h` AND the H-is-its-own-org-domain check removed: the co.uk,
//     uk, TLD, github.io, intermediate-parent and kawasaki.jp rows go red;
//   - the H-is-its-own-org-domain check alone removed: nothing goes red. It
//     is implied by `org != h` (the PSL's longest rule makes OrgDomain(D)
//     never a suffix) and kept as defence in depth, not claimed as tested;
//   - the strict-subdomain test (`HasSuffix(d, "."+h)`) removed: nothing goes
//     red, for the same reason: h == OrgDomain(d) and h != d imply it. The
//     reverse and look-alike rows are refused by `org != h`;
//   - `lookupHostname` made always true: the underscore row goes red;
//   - the `mode != DMARCRelaxed` early return removed: every org-domain row
//     goes red in strict mode;
//   - the unknown-mode refusal removed: the zero-mode row goes red.
func TestDMARCHeaderFromMatch(t *testing.T) {
	const (
		no    = DMARCNoMatch
		exact = DMARCExactMatch
		org   = DMARCOrgDomainMatch
	)
	cases := []struct {
		name, headerFrom, domain string
		relaxed                  DMARCMatch // strict expects exact rows only
	}{
		{"equal domain", "bank.example", "bank.example", exact},
		{"equal subdomain (as today)", "news.bank.example", "news.bank.example", exact},
		{"subdomain, org-domain pass", "bank.example", "news.bank.example", org},
		{"deeper subdomain, org-domain pass", "bank.example", "a.news.bank.example", org},
		{"multi-level suffix: org domain is example.co.uk", "example.co.uk", "news.example.co.uk", org},
		{"multi-level suffix: H = co.uk is a public suffix", "co.uk", "news.example.co.uk", no},
		{"multi-level suffix: H = uk", "uk", "news.example.co.uk", no},
		{"single-level: H = the TLD", "example", "news.bank.example", no},
		{"private-section suffix: tenant org domain", "tenant.github.io", "news.tenant.github.io", org},
		{"private-section suffix: H = github.io", "github.io", "news.tenant.github.io", no},
		{"sibling subdomain", "shop.bank.example", "news.bank.example", no},
		{"intermediate parent below the org domain", "news.bank.example", "a.news.bank.example", no},
		{"parent above the org domain, itself no suffix", "kawasaki.jp", "news.foo.bar.kawasaki.jp", no},
		{"wildcard-suffix org domain", "foo.bar.kawasaki.jp", "news.foo.bar.kawasaki.jp", org},
		{"reverse: subdomain pass for the parent", "news.bank.example", "bank.example", no},
		{"look-alike: not on a label boundary", "bank.example", "news.evilbank.example", no},
		{"unrelated domain", "attacker.example", "news.bank.example", no},
		{"uppercase and trailing dots", "BANK.Example.", "News.BANK.example.", org},
		{"IDN: U-label From, A-label header.from", "xn--bnk-qla.example", "news.bänk.example", org},
		{"IDN: A-label From, U-label header.from", "bänk.example", "news.xn--bnk-qla.example", org},
		{"not a hostname: underscore label", "a_b.example", "news.a_b.example", no},
		{"empty header.from", "", "news.bank.example", no},
		{"empty domain", "bank.example", "", no},
	}
	for _, c := range cases {
		strict := no
		if c.relaxed == exact {
			strict = exact
		}
		if got := DMARCHeaderFromMatch(c.headerFrom, c.domain, DMARCStrict); got != strict {
			t.Errorf("%s: strict match(%q for %q) = %q, want %q", c.name, c.headerFrom, c.domain, got, strict)
		}
		if got := DMARCHeaderFromMatch(c.headerFrom, c.domain, DMARCRelaxed); got != c.relaxed {
			t.Errorf("%s: relaxed match(%q for %q) = %q, want %q", c.name, c.headerFrom, c.domain, got, c.relaxed)
		}
	}
	// No mode chosen is no match at all, not a default.
	if got := DMARCHeaderFromMatch("bank.example", "bank.example", 0); got != no {
		t.Errorf("zero mode on an exact pair = %q, want no match", got)
	}
}

// TestDMARCMatchForRelaxed: the rule inside the clause logic, on the
// production header shape. The clause rules (fail vetoes, exactly one
// header.from, comments are not text) still hold in relaxed mode.
//
// NEGATIVE CONTROL (run): with DMARCMatchFor's `len(hf) != 1` refusal
// loosened to `len(hf) == 0`, both two-header.from rows go red (and the
// strict table's); with the fail veto removed, both fail rows go red.
func TestDMARCMatchForRelaxed(t *testing.T) {
	const d = "news.bank.example"
	cases := []struct {
		name    string
		values  []string
		relaxed DMARCMatch
		strict  bool
	}{
		{"production shape, org-domain pass", []string{prodShape("bank.example")}, DMARCOrgDomainMatch, false},
		{"production shape, exact pass", []string{prodShape(d)}, DMARCExactMatch, true},
		{"production shape, pass for an unrelated domain", []string{prodShape("attacker.example")}, DMARCNoMatch, false},
		{"production shape, pass for a sibling", []string{prodShape("shop.bank.example")}, DMARCNoMatch, false},
		{"two header.from, both the org domain", []string{"mx; dmarc=pass header.from=bank.example header.from=bank.example"}, DMARCNoMatch, false},
		{"two header.from, org domain and attacker", []string{"mx; dmarc=pass header.from=bank.example header.from=attacker.example"}, DMARCNoMatch, false},
		{"org-domain pass vetoed by a fail", []string{"mx; dmarc=pass header.from=bank.example; dmarc=fail header.from=other.example"}, DMARCNoMatch, false},
		{"org-domain pass vetoed by a bare fail in another header", []string{"mx; dmarc=pass header.from=bank.example", "mx; dmarc=fail"}, DMARCNoMatch, false},
		{"org-domain pass inside a comment", []string{"mx; dmarc=none (dmarc=pass header.from=bank.example)"}, DMARCNoMatch, false},
		{"org-domain header.from inside a comment", []string{"mx; dmarc=pass (header.from=bank.example) header.from=attacker.example"}, DMARCNoMatch, false},
		{"exact outranks org-domain", []string{"mx; dmarc=pass header.from=bank.example; dmarc=pass header.from=" + d}, DMARCExactMatch, true},
		{"temperror for the org domain", []string{"mx; dmarc=temperror header.from=bank.example"}, DMARCNoMatch, false},
	}
	for _, c := range cases {
		a := AuthResultsOf(c.values)
		if got := a.DMARCMatchFor(d, DMARCRelaxed); got != c.relaxed {
			t.Errorf("%s: relaxed = %q, want %q", c.name, got, c.relaxed)
		}
		if got := a.DMARCPassFor(d, DMARCRelaxed); got != (c.relaxed != DMARCNoMatch) {
			t.Errorf("%s: relaxed DMARCPassFor = %v, disagrees with DMARCMatchFor", c.name, got)
		}
		if got := a.DMARCPassFor(d, DMARCStrict); got != c.strict {
			t.Errorf("%s: strict = %v, want %v", c.name, got, c.strict)
		}
	}
	// Only the topmost trusted header is read, in relaxed mode too: an
	// org-domain pass the sender wrote below the provider's own fail does not
	// count.
	top := TopmostTrustedAuthResults([]string{
		"mx1.provider.example; dmarc=fail header.from=bank.example",
		prodShape("bank.example"),
	}, []string{"mx1.provider.example"})
	if top.DMARCPassFor(d, DMARCRelaxed) {
		t.Error("an org-domain pass below the topmost trusted header was believed")
	}
	if !TopmostTrustedAuthResults([]string{prodShape("bank.example")}, []string{"mx1.provider.example"}).DMARCPassFor(d, DMARCRelaxed) {
		t.Error("positive control: the production shape alone, trusted, must pass relaxed")
	}
}
