// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"reflect"
	"testing"

	"mailroom/internal/ml"
	"mailroom/internal/store"
)

// The `authResults` Email property (#55 increment 1).
//
// Every test here asserts a DISTINCTION rather than a value, because the whole
// property is three states that must not collapse into each other: null ("does
// not apply"), present:false ("arrived unauthenticated"), and present:true with
// per-method results. Increment 0 measured what collapsing them costs — a single
// boolean that reads "no failure" while DKIM has failed on 3 messages.
//
// Each test below was probed by breaking the mechanism it covers and confirming
// it goes red; the probes are recorded next to the assertions they justify.

func authOf(t *testing.T, values ...string) ml.AuthResults {
	t.Helper()
	return ml.AuthResultsOf(values)
}

// obj is authResultsObject's map result, or a failure if it returned null.
func obj(t *testing.T, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("expected an object, got %#v", v)
	}
	return m
}

// TestAuthResultsNullOnlyWhenOursAndSilent pins the null case to BOTH its
// conditions. Null means "the question does not apply and there is nothing to
// report" — not merely "we wrote it".
//
// ⚠ Probed in both directions, because each half of the condition can rot
// independently: dropping `locallyAuthored` from the guard reddens the first
// case, and dropping `!a.Present` reddens the third.
func TestAuthResultsNullOnlyWhenOursAndSilent(t *testing.T) {
	// Ours, and nothing was stamped on it — a draft. This is the population a
	// display must never call "unverified": a large share of headerless
	// messages are these.
	if got := authResultsObject(authOf(t), true); got != nil {
		t.Errorf("locally authored with no header: want null, got %#v", got)
	}
	// Arrived from outside with no header. NOT null — nobody vouched for it, and
	// that is a real state a client should be able to act on.
	if got := authResultsObject(authOf(t), false); got == nil {
		t.Error("remote with no header: want an object with present:false, got null")
	}
	// Ours, but our own server stamped a result on the way out. Real information
	// about outbound authentication; suppressing it on authorship alone would
	// throw it away to keep the rule tidy.
	got := authResultsObject(authOf(t, "mx.example.com; spf=pass"), true)
	if got == nil {
		t.Fatal("locally authored WITH a header: want the results, got null")
	}
	if p := obj(t, got)["present"]; p != true {
		t.Errorf("locally authored with a header: present = %#v, want true", p)
	}
}

// TestAuthResultsAbsentIsPresentFalseNotEmptyMethods separates "arrived with no
// authentication" from "authenticated and said nothing". A client that saw empty
// arrays here could not tell those apart.
//
// Probed: making authMethodResults return an empty []string instead of null
// reddens this.
func TestAuthResultsAbsentIsPresentFalseNotEmptyMethods(t *testing.T) {
	m := obj(t, authResultsObject(authOf(t, "mx.example.com; none"), false))
	if m["present"] != false {
		t.Errorf("present = %#v, want false (no method token anywhere)", m["present"])
	}
	for _, method := range []string{"spf", "dkim", "dmarc"} {
		v, ok := m[method]
		if !ok {
			t.Errorf("%s: key missing entirely; it must be present and null", method)
			continue
		}
		if v != nil {
			t.Errorf("%s = %#v, want null — the method did not run, which is not "+
				"the same as running and returning nothing", method, v)
		}
	}
}

// TestAuthResultsKeepsEveryResultForAMethod is the multi-signature case. One
// passing DKIM signature alongside a failing one is ordinary mail, and either
// keyword alone would be a verdict the header does not contain.
//
// Probed: making authMethodResults return only the first key reddens this.
func TestAuthResultsKeepsEveryResultForAMethod(t *testing.T) {
	m := obj(t, authResultsObject(authOf(t,
		"mx.example.com; dkim=pass header.d=a.example",
		"mx.example.com; dkim=fail header.d=b.example"), false))
	got, ok := m["dkim"].([]string)
	if !ok {
		t.Fatalf("dkim = %#v, want a []string", m["dkim"])
	}
	if want := []string{"fail", "pass"}; !reflect.DeepEqual(got, want) {
		t.Errorf("dkim = %v, want %v — both results, sorted", got, want)
	}
}

// TestAuthResultsOrderIsStable guards a defect that would surface only
// intermittently. The parser holds results in a map and Go randomises map
// iteration, so without the sort the SAME message could serialise differently on
// two consecutive Email/get calls — which reads to a client as the message
// having changed, and to a test as a flake rather than a bug.
//
// Probed: removing the sort.Strings call reddens this within a few iterations.
func TestAuthResultsOrderIsStable(t *testing.T) {
	values := []string{"mx.example.com; dkim=pass", "mx.example.com; dkim=fail",
		"mx.example.com; dkim=neutral", "mx.example.com; dkim=temperror"}
	first, _ := obj(t, authResultsObject(authOf(t, values...), false))["dkim"].([]string)
	for i := 0; i < 200; i++ {
		got, _ := obj(t, authResultsObject(authOf(t, values...), false))["dkim"].([]string)
		if !reflect.DeepEqual(got, first) {
			t.Fatalf("iteration %d: %v, first call gave %v — order is not stable", i, got, first)
		}
	}
}

// TestAuthResultsReportsFactsNotAVerdict is the property's reason for existing,
// asserted rather than left to the doc comment.
//
// The three messages that motivated increment 1 have DKIM failing while SPF
// passes, so the stored auth_fail boolean is FALSE for them by its documented
// rule. If this property ever collapsed to that same verdict it would be
// silently useless for exactly the population it was built to show.
func TestAuthResultsReportsFactsNotAVerdict(t *testing.T) {
	a := authOf(t, "mx.example.com; spf=pass; dkim=fail; dmarc=pass")

	if a.ClearlyFailed() {
		t.Fatal("fixture no longer matches the motivating case: the stored " +
			"verdict should be FALSE here (spf passes), which is the whole point")
	}
	m := obj(t, authResultsObject(a, false))
	dkim, _ := m["dkim"].([]string)
	if !reflect.DeepEqual(dkim, []string{"fail"}) {
		t.Errorf("dkim = %v, want [fail] — visible even though the verdict is clean", dkim)
	}
	if spf, _ := m["spf"].([]string); !reflect.DeepEqual(spf, []string{"pass"}) {
		t.Errorf("spf = %v, want [pass]", spf)
	}
}

// TestEmailObjectAuthResultsOptIn holds the line every Mailroom extension
// property holds: it costs nothing until a client names it. A property that
// leaked into the default set would put a blob read behind every list view.
func TestEmailObjectAuthResultsOptIn(t *testing.T) {
	if defaultEmailProperties["authResults"] {
		t.Error("authResults is in defaultEmailProperties — it must be opt-in, " +
			"or an Email/get that omits `properties` starts reading blobs")
	}
	if !rawProperties["authResults"] {
		t.Error("authResults is not in rawProperties — Email/get would then never " +
			"fetch the blob for it and the property would be silently absent")
	}
}

// TestBuildEmailBodyParsesAuthenticationResults closes the gap every other test
// in this file leaves open: they all hand authResultsObject a value built by
// hand, so ALL of them would still pass if buildEmailBody never read the header
// at all and the property were `present:false` on every real message forever.
//
// This is the #155 shape — a read path that compiles, renders, and is silently
// empty — so it is asserted against actual RFC 5322 bytes.
//
// ⚠ READS authTrusted, NOT a permissive parse (#625): buildEmailBody has no
// field for the latter any more (see body.go), so this is also the only test
// left that proves buildEmailBody wires the trusted authserv-id list through
// to the parse at all.
//
// Probed: deleting the `authTrusted:` line from buildEmailBody reddens this
// and nothing else in the package.
func TestBuildEmailBodyParsesAuthenticationResults(t *testing.T) {
	// Two headers from the one receiving server, as a provider that splits its
	// verdicts writes them: the parser must read ALL of them rather than only
	// the first. (A header with a DIFFERENT trusted id below the topmost one is
	// not read at all since #766 — ml.TestOnlyTheTopmostTrustedIDSpeaks.)
	raw := "From: a@example.com\r\n" +
		"Authentication-Results: mx1.example.net; spf=pass smtp.mailfrom=a@example.com\r\n" +
		"Authentication-Results: mx1.example.net; dkim=fail header.d=example.com; dmarc=permerror\r\n" +
		"Subject: hi\r\n\r\nhello there\r\n"

	body := buildEmailBody([]byte(raw), hashOf(raw), bodyArgs{
		trustedAuthservIDs: []string{"mx1.example.net", "mx2.example.net"},
	})
	if !body.authTrusted.Present {
		t.Fatal("Present is false — the header was not read from the message bytes at all")
	}
	m := obj(t, authResultsObject(body.authTrusted, false))
	for method, want := range map[string][]string{
		"spf": {"pass"}, "dkim": {"fail"}, "dmarc": {"permerror"},
	} {
		got, _ := m[method].([]string)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s = %v, want %v", method, got, want)
		}
	}
	// The second hop's results are here, so multi-header parsing works — the
	// single-hop version of this fixture would pass with only the first read.
	if !body.authTrusted.Failed("dkim") {
		t.Error("dkim should read as failed (fail, no pass) from the SECOND header")
	}
}

// TestBuildEmailBodyAbsentHeaderIsNotPresent is the negative control for the
// test above: it proves Present distinguishes the two populations rather than
// being true for every message that parses.
func TestBuildEmailBodyAbsentHeaderIsNotPresent(t *testing.T) {
	raw := "From: a@example.com\r\nSubject: hi\r\n\r\nhello there\r\n"
	body := buildEmailBody([]byte(raw), hashOf(raw), bodyArgs{
		trustedAuthservIDs: []string{"mx1.example.net"},
	})
	if body.authTrusted.Present {
		t.Error("Present is true for a message with no Authentication-Results header")
	}
}

// ── #625: the displayed notice must read the TRUSTED parse ─────────────────
//
// Before the fix, the `authResults` Email property was built in email.go from
// the permissive parse (ml.AuthResultsOf over every Authentication-Results
// header, unfiltered by authserv-id). RFC 8601 §5 only requires a relay to
// strip headers bearing ITS OWN id, so a sender's own forged header survives
// to the reading pane exactly as well as the provider's genuine one.
//
// Every test below goes through emailObject (email.go), the SAME assembly
// path Email/get uses, rather than calling authResultsObject directly with a
// hand-built ml.AuthResults — that would only prove authResultsObject renders
// correctly, and say nothing about WHICH parse email.go feeds it, which is
// the actual line #625 fixed.
func authResultsViaEmailObject(t *testing.T, raw string, trustedIDs []string) any {
	t.Helper()
	body := buildEmailBody([]byte(raw), hashOf(raw), bodyArgs{trustedAuthservIDs: trustedIDs})
	out := emailObject(store.EmailView{}, map[string]bool{"authResults": true}, body)
	return out["authResults"]
}

// TestAuthResultsTrustedParseIgnoresForgedPass is THE ATTACK: a message
// carries the provider's genuine `dmarc=fail` alongside a sender-forged
// `dmarc=pass` from an untrusted authserv-id. The property must still report
// DMARC as failed — a forged header from an id the operator did not name must
// not be able to cancel a real failure.
//
// ⚠ THIS IS THE TEST TO NEGATIVE-CONTROL. On a copy, reverting email.go's
// `authResultsObject(body.authTrusted, ...)` back to reading the permissive
// parse (restore a `body.auth` field computed via `ml.AuthResultsOf` over
// every header, and read that instead) makes dmarc collect BOTH keywords
// across all headers — [fail, pass] rather than [fail] — which is not what
// this test asserts. Confirm that reversion actually reddens this test before
// trusting the fix.
func TestAuthResultsTrustedParseIgnoresForgedPass(t *testing.T) {
	raw := "From: a@example.com\r\n" +
		"Authentication-Results: mx.example.com; dmarc=fail (p=reject) header.from=example.com\r\n" +
		"Authentication-Results: evil.example; dmarc=pass\r\n" +
		"Subject: hi\r\n\r\nhello there\r\n"

	m := obj(t, authResultsViaEmailObject(t, raw, []string{"mx.example.com"}))
	dmarc, _ := m["dmarc"].([]string)
	if !reflect.DeepEqual(dmarc, []string{"fail"}) {
		t.Errorf("dmarc = %v, want [fail] — the forged evil.example pass must not "+
			"survive the trusted-id filter and must not cancel the genuine fail", dmarc)
	}
}

// TestAuthResultsForgedOnlyHeaderIsNoInformation covers the header with NO
// trusted authserv-id at all: everything is forged from the reader's point of
// view, so the property must read as no authentication information — never a
// pass, which is what the sender wrote and hoped would be believed.
func TestAuthResultsForgedOnlyHeaderIsNoInformation(t *testing.T) {
	raw := "From: a@example.com\r\n" +
		"Authentication-Results: evil.example; spf=pass; dkim=pass; dmarc=pass\r\n" +
		"Subject: hi\r\n\r\nhello there\r\n"

	m := obj(t, authResultsViaEmailObject(t, raw, []string{"mx.example.com"}))
	if m["present"] != false {
		t.Errorf("present = %#v, want false", m["present"])
	}
	for _, method := range []string{"spf", "dkim", "dmarc"} {
		if v := m[method]; v != nil {
			t.Errorf("%s = %#v, want null — no trusted header said anything", method, v)
		}
	}
}

// TestAuthResultsUnsetAuthservIDsIsSilentByDesign is the operator-facing
// default: MAILROOM_AUTHSERV_IDS unset, and a genuine-looking fail header.
// Even a real-looking failure must not be shown, because with nothing trusted
// there is no way to tell it apart from a forged one — silent by design
// (#625), not a regression to chase.
func TestAuthResultsUnsetAuthservIDsIsSilentByDesign(t *testing.T) {
	raw := "From: a@example.com\r\n" +
		"Authentication-Results: mx.example.com; dmarc=fail (p=reject) header.from=example.com\r\n" +
		"Subject: hi\r\n\r\nhello there\r\n"

	// trustedAuthservIDs unset. The property must be ABSENT (null), not an
	// object with present:false: the web client renders present:false as the
	// "unauthenticated" notice (web/src/core/senderAuth.ts), so an empty parse
	// would put that notice on every remote message the moment an operator
	// has not configured the setting. Null is the client's "say nothing".
	if got := authResultsViaEmailObject(t, raw, nil); got != nil {
		t.Errorf("authResults = %#v, want nil — with no trusted authserv-id the notice "+
			"must be silent, not \"unauthenticated\" on every message", got)
	}
}

// TestAuthResultsTrustedPassIsReported is the no-regression case: a legitimate
// trusted pass must still be shown as a pass. The fix must not turn every
// notice silent — only the forgeable ones.
func TestAuthResultsTrustedPassIsReported(t *testing.T) {
	raw := "From: a@example.com\r\n" +
		"Authentication-Results: mx.example.com; spf=pass smtp.mailfrom=a@example.com; " +
		"dkim=pass header.d=example.com; dmarc=pass\r\n" +
		"Subject: hi\r\n\r\nhello there\r\n"

	m := obj(t, authResultsViaEmailObject(t, raw, []string{"mx.example.com"}))
	if m["present"] != true {
		t.Fatalf("present = %#v, want true", m["present"])
	}
	for _, method := range []string{"spf", "dkim", "dmarc"} {
		got, _ := m[method].([]string)
		if !reflect.DeepEqual(got, []string{"pass"}) {
			t.Errorf("%s = %v, want [pass] — a genuine trusted pass must still be shown", method, got)
		}
	}
}
