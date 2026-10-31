// SPDX-License-Identifier: AGPL-3.0-only

package ml

import "testing"

// AuthResultsFail is CONSERVATIVE: it flags a clear failure (DMARC=fail, or
// SPF=fail AND DKIM=fail) and NOTHING else — absent headers, none/neutral/
// softfail, or any pass never count as a failure. Tested against real-shaped
// Authentication-Results strings.
func TestAuthResultsFail(t *testing.T) {
	cases := []struct {
		name   string
		values []string
		want   bool
	}{
		{"empty", nil, false},
		{"no auth tokens", []string{"mx.example.com; none"}, false},
		{"all pass", []string{"mx.example.com; spf=pass smtp.mailfrom=a@example.com; dkim=pass header.i=@example.com; dmarc=pass (p=none) header.from=example.com"}, false},
		{"dmarc fail", []string{"mx.example.com; spf=pass smtp.mailfrom=a@x.com; dkim=pass; dmarc=fail (p=reject) header.from=x.com"}, true},
		{"spf fail only", []string{"mx.example.com; spf=fail smtp.mailfrom=a@x.com; dkim=pass header.i=@x.com"}, false},
		{"dkim fail only", []string{"mx.example.com; spf=pass smtp.mailfrom=a@x.com; dkim=fail header.i=@x.com"}, false},
		{"spf and dkim fail", []string{"mx.example.com; spf=fail smtp.mailfrom=a@x.com; dkim=fail header.i=@x.com"}, true},
		{"softfail is not fail", []string{"mx.example.com; spf=softfail; dkim=fail"}, false},
		{"neutral/none not fail", []string{"mx.example.com; spf=neutral; dkim=none; dmarc=none"}, false},
		{"missing entirely", []string{"mx.example.com"}, false},
		{"case-insensitive dmarc fail", []string{"MX.EXAMPLE.COM; DMARC=FAIL header.from=x.com"}, true},
		{"cfws around equals", []string{"mx.example.com; dmarc = fail (p=none)"}, true},
		{"multi-header: dmarc fail in second", []string{"mx1; spf=pass", "mx2; dmarc=fail"}, true},
		{"multi dkim, one pass one fail -> not failed", []string{"mx; spf=fail; dkim=pass header.i=@x.com; dkim=fail header.i=@evil.com"}, false},
		{"permerror not fail", []string{"mx; spf=permerror; dkim=temperror"}, false},
		{"mailfrom does not trip spf token", []string{"mx; spf=pass smtp.mailfrom=fail@example.com"}, false},
	}
	for _, c := range cases {
		if got := AuthResultsFail(c.values); got != c.want {
			t.Errorf("%s: AuthResultsFail(%q) = %v, want %v", c.name, c.values, got, c.want)
		}
	}
}

// ⚠ THE POSITIVE CONTROLS THE CORPUS REPORT'S ZEROS DEPEND ON (#55 increment 0).
//
// On a real mailbox `auth_fail` can be false for every message.
// That number is worth nothing on its own: "this corpus has no authentication
// failures" and "the detector never fires" produce exactly the same zero, and
// this project has already shipped a report whose expected output was zero and
// which could not tell those apart (#242's "0 of 361", from a filter that made
// zero the only possible answer).
//
// So before any zero from auth-report is read as a finding, the parser has to be
// shown to FIRE — on every method, on every result keyword the report counts,
// and on the shapes that a single boolean cannot distinguish.
func TestAuthResultsOfFiresOnEveryMethodAndResult(t *testing.T) {
	// Every method × the result keywords RFC 8601 actually puts on the wire.
	for _, method := range AuthMethods {
		for _, result := range []string{
			"pass", "fail", "none", "neutral", "softfail",
			"permerror", "temperror", "policy",
		} {
			hdr := "mx.example.com; " + method + "=" + result
			a := AuthResultsOf([]string{hdr})
			if !a.Present {
				t.Errorf("%s: Present = false — the parser did not fire at all", hdr)
			}
			if !a.Has(method, result) {
				t.Errorf("%s: Has(%q, %q) = false — the report would count this message as "+
					"having no result for that method", hdr, method, result)
			}
		}
	}
}

// The three populations the stored boolean cannot show, each asserted to be
// distinguishable — because the report's whole purpose is to separate them.
func TestAuthResultsSeparatesAbsentDisagreeingAndFailed(t *testing.T) {
	t.Run("absent is not failure and not pass", func(t *testing.T) {
		for _, values := range [][]string{nil, {}, {"mx.example.com"}, {"mx.example.com; none"}} {
			a := AuthResultsOf(values)
			if a.Present {
				t.Errorf("%q: Present = true, want false — a message with no method token must "+
					"be countable as ABSENT, which is the population a badge most risks "+
					"showing as verified", values)
			}
			if a.ClearlyFailed() {
				t.Errorf("%q: ClearlyFailed = true, want false", values)
			}
		}
	})

	t.Run("disagreement is its own population", func(t *testing.T) {
		// Multi-signature DKIM: one passes, one fails. Ordinary, and invisible
		// in auth_fail.
		a := AuthResultsOf([]string{"mx; spf=pass; dkim=pass header.i=@x.com; dkim=fail header.i=@evil.com"})
		if !a.Disagrees("dkim") {
			t.Error("Disagrees(dkim) = false on a pass+fail pair — the report cannot show the " +
				"population where methods contradict each other")
		}
		if a.Failed("dkim") {
			t.Error("Failed(dkim) = true despite a passing signature — that would overstate the " +
				"failure count and contradict AuthResultsFail's documented rule")
		}
		if a.ClearlyFailed() {
			t.Error("ClearlyFailed = true — a disagreement is not a clear failure")
		}
	})

	t.Run("a real failure is still a failure", func(t *testing.T) {
		a := AuthResultsOf([]string{"mx; dmarc=fail (p=reject) header.from=x.com"})
		if !a.Failed("dmarc") || !a.ClearlyFailed() {
			t.Error("a bare DMARC=fail is not being reported as a failure — the report's zero " +
				"would then mean nothing at all")
		}
	})
}

// ⚠ THE REFACTOR MUST NOT HAVE MOVED THE SCORER'S SIGNAL.
//
// AuthResultsFail feeds the cold-start scorer (spec §8). Re-expressing it over
// AuthResults was supposed to be behaviour-preserving; "supposed to" is not a
// check. This asserts the documented rule holds over the extracted results for
// every combination of the three methods and the keywords that matter, which is
// a stronger statement than the sixteen hand-written cases above.
func TestClearlyFailedIsExactlyTheDocumentedRule(t *testing.T) {
	keywords := []string{"", "pass", "fail", "softfail", "none"}
	build := func(method, kw string) string {
		if kw == "" {
			return ""
		}
		return method + "=" + kw + "; "
	}
	checked := 0
	for _, spf := range keywords {
		for _, dkim := range keywords {
			for _, dmarc := range keywords {
				hdr := "mx; " + build("spf", spf) + build("dkim", dkim) + build("dmarc", dmarc)
				a := AuthResultsOf([]string{hdr})
				// The rule, spelled out independently of the implementation.
				want := (dmarc == "fail") || (spf == "fail" && dkim == "fail")
				if got := a.ClearlyFailed(); got != want {
					t.Errorf("%q: ClearlyFailed = %v, want %v", hdr, got, want)
				}
				if got := AuthResultsFail([]string{hdr}); got != want {
					t.Errorf("%q: AuthResultsFail = %v, want %v — the exported entry point and "+
						"the method have diverged", hdr, got, want)
				}
				checked++
			}
		}
	}
	if checked != len(keywords)*len(keywords)*len(keywords) {
		t.Fatalf("checked %d combinations — the loop is not covering what it claims", checked)
	}
}

// ── #414: whose verdict is this? ────────────────────────────────────────────
//
// ⚠ THE OLD TEST FOR selfSpoof TOOK AuthPassed AS A BOOLEAN INPUT, so it tested
// the rule given an answer and could not observe that the answer was
// attacker-supplied. The seam between "what the header says" and "who said it"
// is exactly where the defect lived, and no test crossed it. These do.

const forged = "x.example; dkim=pass"
const genuine = "mx.provider.example; dkim=fail; spf=fail"

func TestAForgedAuthservIDIsNotBelieved(t *testing.T) {
	// The shape an attacker sends: their own header claiming a pass, followed by
	// the real MX's genuine verdict. RFC 8601 §5 makes an MTA strip only headers
	// bearing its OWN id, so the first one travels untouched.
	values := []string{forged, genuine}

	// ── CONTROL: the permissive parser DOES see the forged pass. ────────────
	// This is the defect, asserted rather than described — and without it, the
	// assertion below is also what a parser that reads nothing would produce.
	if !AuthResultsOf(values).Has("dkim", "pass") {
		t.Fatalf("CONTROL FAILED: the display parser does not see the forged pass, so this fixture " +
			"does not reproduce the defect and nothing below is meaningful")
	}

	got := TrustedAuthResults(values, []string{"mx.provider.example"})
	if got.Has("dkim", "pass") {
		t.Errorf("a DKIM pass claimed by an untrusted authserv-id was believed. That header is one " +
			"line the SENDER writes, and believing it switches off the selfSpoof warning about " +
			"the sender's own spoofing")
	}
	// ⚠ AND THE TRUSTED HEADER IS STILL READ. A filter that discarded everything
	// would pass the assertion above and be useless.
	if !got.Has("dkim", "fail") {
		t.Errorf("the TRUSTED header's own verdict was discarded too (%+v) — the filter is rejecting "+
			"everything rather than selecting by identity", got)
	}
}

// TWO TRUSTED IDS AT ONCE (#766). While a provider ran its old id (trusted
// until a date) and its new one, mail carries the new id on top. A header the
// SENDER wrote with the old id travels below it unstripped (RFC 8601 §5 strips
// only the server's current id), and must not add its pass to the genuine
// verdict.
func TestOnlyTheTopmostTrustedIDSpeaks(t *testing.T) {
	const current = "mx.new.example; dkim=fail; spf=fail"
	const currentSplit = "mx.new.example; dmarc=fail header.from=bank.example"
	const oldForged = "mx.old.example; dkim=pass; dmarc=pass header.from=bank.example"
	trusted := []string{"mx.new.example", "mx.old.example"}

	// CONTROL: the old id IS trusted on its own, so the filter below is what
	// drops it, not a misconfigured fixture.
	if !TrustedAuthResults([]string{oldForged}, trusted).Has("dkim", "pass") {
		t.Fatalf("CONTROL FAILED: a lone old-id header is not believed, so this fixture cannot " +
			"show the topmost rule doing anything")
	}

	got := TrustedAuthResults([]string{current, currentSplit, oldForged}, trusted)
	if got.Has("dkim", "pass") || got.Has("dmarc", "pass") {
		t.Errorf("a pass under a second trusted id BELOW the topmost one was believed (%+v); on mail "+
			"the new id stamped, that header is one the sender could have written", got)
	}
	// The provider's own split verdicts are all still read.
	if !got.Has("dkim", "fail") || !got.Has("dmarc", "fail") {
		t.Errorf("headers bearing the topmost id were dropped (%+v): a provider may split its "+
			"verdicts over several headers with its own id", got)
	}
}

func TestAnUnconfiguredDeploymentBelievesNothing(t *testing.T) {
	got := TrustedAuthResults([]string{genuine, forged}, nil)
	if got.Present {
		t.Errorf("with no trusted authserv-id configured, %+v was believed — there is no way to tell "+
			"a genuine pass from one the sender wrote, so the honest answer is that nothing has "+
			"been authenticated", got)
	}
	// ⚠ THE DIRECTION MATTERS AND IS THE WHOLE DEFAULT. Nothing believed means
	// AuthPassed is false, which makes selfSpoof FIRE rather than be suppressed.
	// A misconfiguration therefore produces noise, never silence.
	if got.Has("dkim", "pass") {
		t.Errorf("an unconfigured deployment reported a pass: %+v", got)
	}
}

func TestTheAuthservIDIsReadFromTheStartOfTheHeader(t *testing.T) {
	for _, tc := range []struct{ value, want, why string }{
		{"mx.provider.example; dkim=pass", "mx.provider.example", "the plain form"},
		{"  MX.Provider.Example ; dkim=pass", "mx.provider.example", "case and space are not identity"},
		{"mx.provider.example 1; dkim=pass", "mx.provider.example", "RFC 8601 allows a version after the id"},
		{"dkim=pass", "dkim=pass", "no semicolon: the whole value is taken as the id, which matches nothing real"},
	} {
		if got := authservIDOf(tc.value); got != tc.want {
			t.Errorf("authservIDOf(%q) = %q, want %q (%s)", tc.value, got, tc.want, tc.why)
		}
	}
}

// ⚠ THE DISPLAY PARSER AND THE CLASSIFIER SIGNAL ARE DELIBERATELY UNCHANGED.
// They predate the security use, they read every header, and changing them would
// move classifier features across the whole corpus for no security gain — a
// forged `pass` makes ClearlyFailed LESS likely to fire, which is noise in a
// feature rather than an exploit. Pinned so the split is a decision rather than
// an oversight someone later "fixes".
func TestTheDisplayParserStillReadsEveryHeader(t *testing.T) {
	a := AuthResultsOf([]string{forged, genuine})
	if !a.Has("dkim", "pass") || !a.Has("dkim", "fail") {
		t.Errorf("AuthResultsOf stopped reading every header (%+v) — the reader's authResults view "+
			"and the cold-start scorer both depend on it being permissive", a)
	}
}
