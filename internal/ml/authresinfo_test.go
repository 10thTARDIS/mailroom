// SPDX-License-Identifier: AGPL-3.0-only

package ml

import (
	"reflect"
	"testing"
	"time"
)

// DMARCPassFor binds a pass to the From domain learned placement acts on.
// Every row is a header a real MTA or an attacker could produce; the
// adversarial ones are the reason the tokenizer exists.
func TestDMARCPassFor(t *testing.T) {
	cases := []struct {
		name   string
		values []string
		domain string
		want   bool
	}{
		{"pass bound to the domain", []string{"mx; dmarc=pass (p=reject) header.from=bank.example"}, "bank.example", true},
		{"case-insensitive", []string{"mx; DMARC=Pass header.from=Bank.EXAMPLE"}, "bank.example", true},
		{"trailing root dot", []string{"mx; dmarc=pass header.from=bank.example."}, "bank.example", true},
		{"quoted value", []string{`mx; dmarc=pass header.from="bank.example"`}, "bank.example", true},
		{"IDNA: U-label header, A-label key", []string{"mx; dmarc=pass header.from=bänk.example"}, "xn--bnk-qla.example", true},
		{"cfws around equals", []string{"mx; dmarc = pass header.from = bank.example"}, "bank.example", true},
		{"pass for ANOTHER domain", []string{"mx; dmarc=pass header.from=attacker.example"}, "bank.example", false},
		{"pass without header.from", []string{"mx; dmarc=pass (p=none)"}, "bank.example", false},
		{"two header.from on one result", []string{"mx; dmarc=pass header.from=bank.example header.from=attacker.example"}, "bank.example", false},
		{"another method's property for the domain", []string{"mx; spf=pass smtp.mailfrom=bank.example; dmarc=fail header.from=bank.example"}, "bank.example", false},
		{"spf header.from does not lend dmarc a binding", []string{"mx; spf=pass header.from=bank.example; dmarc=pass header.from=attacker.example"}, "bank.example", false},
		{"any fail vetoes, same header", []string{"mx; dmarc=pass header.from=bank.example; dmarc=fail header.from=bank.example"}, "bank.example", false},
		{"any fail vetoes, other header", []string{"mx; dmarc=pass header.from=bank.example", "mx; dmarc=fail"}, "bank.example", false},
		{"fail for another domain still vetoes", []string{"mx; dmarc=pass header.from=bank.example; dmarc=fail header.from=other.example"}, "bank.example", false},
		{"pass inside a comment", []string{"mx; dmarc=none (was: dmarc=pass header.from=bank.example)"}, "bank.example", false},
		{"pass inside a nested comment", []string{"mx; dmarc=none (a (dmarc=pass header.from=bank.example) b)"}, "bank.example", false},
		{"header.from inside a comment", []string{"mx; dmarc=pass (header.from=bank.example) header.from=attacker.example"}, "bank.example", false},
		{"comment chars inside a quoted value are text", []string{`mx; dmarc=pass header.from="bank.example" policy.x="(a)"`}, "bank.example", true},
		{"pass smuggled in a property value", []string{"mx; spf=none smtp.mailfrom=dmarc=pass@x.example; dmarc=none header.from=bank.example"}, "bank.example", false},
		{"no semicolon", []string{"mx dmarc=pass header.from=bank.example"}, "bank.example", false},
		{"unterminated comment fails closed", []string{"mx; dmarc=none (oops dmarc=pass header.from=bank.example"}, "bank.example", false},
		{"temperror is not a pass", []string{"mx; dmarc=temperror header.from=bank.example"}, "bank.example", false},
		{"empty domain", []string{"mx; dmarc=pass header.from=bank.example"}, "", false},
	}
	// Every row names the From domain itself or an unrelated one, so relaxed
	// alignment (#846) must answer exactly as strict does on all of them.
	for _, mode := range []DMARCAlignment{DMARCStrict, DMARCRelaxed} {
		for _, c := range cases {
			if got := AuthResultsOf(c.values).DMARCPassFor(c.domain, mode); got != c.want {
				t.Errorf("%s (mode %d): DMARCPassFor(%q) on %q = %v, want %v", c.name, mode, c.domain, c.values, got, c.want)
			}
		}
	}
}

// The regex this tokenizer replaced read `dkim=pass` out of a property value
// the SENDER controls (an envelope local part), inside the provider's own,
// trusted header. That switched off the selfSpoof warning.
func TestAPropertyValueCannotStartAResult(t *testing.T) {
	a := AuthResultsOf([]string{"mx.provider.example; spf=fail smtp.mailfrom=dkim=pass@attacker.example; dkim=none"})
	if a.Has("dkim", "pass") || a.AffirmativelyPassed() {
		t.Errorf("a dkim=pass inside smtp.mailfrom was read as a result: %+v", a.Results)
	}
	if !a.Has("spf", "fail") || !a.Has("dkim", "none") {
		t.Errorf("the real clauses were lost: %+v", a.Results)
	}
	if b := AuthResultsOf([]string{"mx; spf=fail (dkim=pass)"}); b.Has("dkim", "pass") {
		t.Errorf("a result inside a comment was read: %+v", b.Results)
	}
}

// The authserv-id match is exact, and nothing but a version may follow it.
func TestAuthservIDMatchIsExact(t *testing.T) {
	trusted := []string{"mx.provider.example"}
	for _, c := range []struct {
		value string
		want  bool
	}{
		{"mx.provider.example; dmarc=pass header.from=bank.example", true},
		{"MX.Provider.Example 1; dmarc=pass header.from=bank.example", true},
		{"mx.provider.example (provider comment); dmarc=pass header.from=bank.example", true},
		{"mx.provider.example.attacker.example; dmarc=pass header.from=bank.example", false},
		{"attacker-mx.provider.example; dmarc=pass header.from=bank.example", false},
		{"mx.provider.exampl; dmarc=pass header.from=bank.example", false},
		{"mx.provider.example evil; dmarc=pass header.from=bank.example", false},
		{"mx.provider.example 1 x; dmarc=pass header.from=bank.example", false},
		{"(mx.provider.example) attacker.example; dmarc=pass header.from=bank.example", false},
		{`"mx.provider.example"; dmarc=pass header.from=bank.example`, false},
	} {
		got := TopmostTrustedAuthResults([]string{c.value}, trusted).DMARCPassFor("bank.example", DMARCStrict)
		if got != c.want {
			t.Errorf("%q: trusted pass = %v, want %v", c.value, got, c.want)
		}
	}
}

// Only the topmost trusted header counts for learned placement: the
// provider prepends its own above anything the sender wrote.
func TestTopmostTrustedAuthResultsReadsOneHeader(t *testing.T) {
	genuine := "mx.provider.example; dmarc=fail header.from=bank.example"
	forgedBelow := "mx.provider.example; dmarc=pass header.from=bank.example"
	if TopmostTrustedAuthResults([]string{genuine, forgedBelow}, []string{"mx.provider.example"}).DMARCPassFor("bank.example", DMARCStrict) {
		t.Error("a trusted-id pass BELOW the provider's own fail was believed")
	}
	// Untrusted headers above the provider's are skipped, not "topmost".
	if !TopmostTrustedAuthResults([]string{"attacker.example; dmarc=fail", forgedBelow},
		[]string{"mx.provider.example"}).DMARCPassFor("bank.example", DMARCStrict) {
		t.Error("an untrusted header above hid the topmost trusted one")
	}
	if TopmostTrustedAuthResults([]string{forgedBelow}, nil).Present {
		t.Error("with nothing trusted, nothing may be read")
	}
}

// ── #766: date-bounded trust ────────────────────────────────────────────────

func TestTrustedAt(t *testing.T) {
	day := func(s string) time.Time { v, _ := time.Parse("2006-01-02", s); return v.UTC() }
	entries := []string{"mx.new.example", "Old.Example<2026-07-01", "bad.example<2026-99-99", "<2026-07-01", "twice.example<2026-07-01<2026-08-01"}
	cases := []struct {
		name     string
		received time.Time
		want     []string
	}{
		{"before the bound", day("2026-06-30").Add(23*time.Hour + 59*time.Minute), []string{"mx.new.example", "old.example"}},
		{"long before the bound", day("2025-01-01"), []string{"mx.new.example", "old.example"}},
		{"exactly at the bound: not strictly before", day("2026-07-01"), []string{"mx.new.example"}},
		{"after the bound", day("2026-07-02"), []string{"mx.new.example"}},
		{"zero time trusts no bounded entry", time.Time{}, []string{"mx.new.example"}},
		{"bound is read in UTC, not the message's zone",
			time.Date(2026, 6, 30, 23, 0, 0, 0, time.FixedZone("x", -3*3600)), []string{"mx.new.example"}},
	}
	for _, c := range cases {
		if got := TrustedAt(entries, c.received); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: TrustedAt = %v, want %v", c.name, got, c.want)
		}
	}
	if got := TrustedAt(nil, day("2026-01-01")); len(got) != 0 {
		t.Errorf("no entries must resolve to no ids, got %v", got)
	}
}

// THE FAIL-CLOSED PROPERTY. A raw bounded entry that reaches a trust check
// WITHOUT going through TrustedAt must never match a header, whatever the
// header's authserv-id — including the bare id and the literal `id<date`.
//
// Negative control (recorded in the PR): making trustedSet strip the bound so
// the raw entry normalises to the bare id reddens this test.
func TestRawBoundedEntryNeverMatchesAHeader(t *testing.T) {
	raw := []string{"aspmx1.example<2026-07-01"}
	headers := []string{
		"aspmx1.example; dmarc=pass header.from=bank.example",
		"aspmx1.example<2026-07-01; dmarc=pass header.from=bank.example",
		"ASPMX1.example; dkim=pass",
	}
	if got := TrustedAuthResults(headers, raw); got.Present {
		t.Errorf("TrustedAuthResults trusted a raw bounded entry: %+v", got)
	}
	if got := TopmostTrustedAuthResults(headers, raw); got.Present {
		t.Errorf("TopmostTrustedAuthResults trusted a raw bounded entry: %+v", got)
	}
	if IsTrustedAuthservID("aspmx1.example", raw) || IsTrustedAuthservID("aspmx1.example<2026-07-01", raw) {
		t.Error("IsTrustedAuthservID trusted a raw bounded entry")
	}
	// CONTROL: the same header IS trusted once resolved at a time inside the bound, so
	// the assertions above exclude something this setup can produce.
	in := TrustedAt(raw, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	if got := TrustedAuthResults(headers[:1], in); !got.Present {
		t.Fatal("CONTROL FAILED: the resolved entry does not trust its own header")
	}
	if got := TrustedAuthResults(headers[:1], TrustedAt(raw, time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC))); got.Present {
		t.Fatal("a header past the bound was trusted")
	}
}

func TestAuthNoticeFor(t *testing.T) {
	cases := []struct {
		name                  string
		configured, trusted   bool
		locallyAuthored, self bool
		localPass             bool
		want                  AuthNotice
	}{
		{"no ids configured", false, true, false, false, false, NoticeUnconfigured},
		{"no ids configured, local pass", false, false, false, false, true, NoticeUnconfigured},
		{"trusted header", true, true, false, false, false, NoticeTrusted},
		{"trusted header wins over a local pass", true, true, false, false, true, NoticeTrusted},
		{"trusted header on own mail still shown", true, true, true, true, false, NoticeTrusted},
		// Untrusted headers are not an input: this is the same case as no header.
		{"no trusted header, inbound", true, false, false, false, false, NoticeUnauthenticated},
		{"no trusted header, local aligned pass", true, false, false, false, true, NoticeLocalDKIM},
		{"no trusted header, locally authored", true, false, true, false, true, NoticeSelf},
		{"no trusted header, Sent/Drafts", true, false, false, true, true, NoticeSelf},
	}
	for _, c := range cases {
		if got := AuthNoticeFor(c.configured, c.trusted, c.locallyAuthored, c.self, c.localPass); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}
