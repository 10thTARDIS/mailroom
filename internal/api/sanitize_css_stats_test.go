// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"strings"
	"testing"
)

// cssSamples are inputs that between them reach every drop reason the collector
// counts. Reused by the tests below so the identical-output check and the
// counter checks are run over the same material.
//
// ⚠ THIS LIST IS NOT ENOUGH ON ITS OWN, AND THE PROBE IS WHAT PROVED IT.
// TestMeasuredHTMLIsIdenticalToSanitizeHTML originally ran over this list alone,
// and swapping the measured path to the OUTBOUND policy — a real divergence —
// left it green. Nothing here contains a remote <img>, which is the main thing
// the two policies disagree about, so the test could not observe the failure it
// exists to exclude. It now runs over hostileCorpus() as well, which does.
//
// Kept as a named list anyway because the counter tests below need inputs whose
// expected counts are known by hand.
var cssSamples = []string{
	// Ordinary newsletter styling: survives.
	`<style>.wrap{color:#333;font-size:14px;padding:0 8px}</style><p style="color:red">hi</p>`,
	// A property that is NOT on the allowlist, alongside one that is.
	`<div style="animation:spin 2s;color:blue">x</div>`,
	// An allowed property whose VALUE is refused — url() is the #297 case.
	`<div style="background:url(https://tracker.example/p.gif);margin:0">x</div>`,
	// A Sass parent selector: costs its rule, not the stylesheet.
	`<style>&:hover{color:red}p{color:blue}</style>`,
	// A bare declaration in a <style> body is not a rule at all.
	`<style>color:red</style>`,
	// A rule whose every declaration is dropped, so the rule goes too.
	`<style>p{animation:spin 2s}</style>`,
	// At-rules: one allowed, one not.
	`<style>@media screen{p{color:red}}@import url(x.css);</style>`,
	// Malformed declarations.
	`<div style=";;;:red;color">x</div>`,
	// Nothing CSS at all.
	`<p>plain</p>`,
	// A <style> element that loses everything, so the element is dropped.
	`<style>@import url(x.css);</style>`,
}

// ⚠ THIS IS THE TEST THAT MAKES THE WHOLE REPORT WORTH ANYTHING (#305).
//
// The point of threading counters through the real filter — rather than writing
// a second extractor beside it — is that the numbers describe the code that
// actually renders mail. That is a CLAIM until something checks it. If
// MeasureHTMLCSS and SanitizeHTML could ever disagree, the report would be
// describing a system that does not exist, which is precisely the failure this
// project has already paid a month for (#169, #220).
//
// So: same input, byte-identical output — over the CSS samples AND over
// hostileCorpus(), the suite's existing adversarial set. The second half is not
// belt-and-braces: with only the first, a deliberate swap of the measured path
// to the outbound policy went undetected, because these samples contain no
// remote <img> for the two policies to disagree about.
func TestMeasuredHTMLIsIdenticalToSanitizeHTML(t *testing.T) {
	inputs := append(append([]string{}, cssSamples...), hostileCorpus()...)
	for _, in := range inputs {
		want := SanitizeHTML(in)
		got, _ := MeasureHTMLCSS(in)
		if got != want {
			t.Errorf("the instrumented path diverged from the shipped one — every number the "+
				"corpus report prints would be about different code than the renderer runs\n"+
				"  in:   %s\n  ship: %q\n  meas: %q", in, want, got)
		}
	}
}

// The render path passes a nil *cssStats. Every counter helper therefore has to
// tolerate a nil receiver, and the tempting refactor (a shared `bump(m, k)`
// helper) does NOT: evaluating the map argument dereferences the nil pointer
// before the guard inside runs.
//
// The whole suite would catch that as a panic, but only once something reached a
// drop path — so this asserts it directly, on input chosen to hit every counter.
func TestNilStatsNeverPanics(t *testing.T) {
	for _, in := range cssSamples {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("the render path panicked with nil stats on %.60q: %v", in, r)
				}
			}()
			_ = SanitizeHTML(in)
			_ = SanitizeOutboundHTML(in)
		}()
	}
}

// TestMeasureSeparatesPropertyFromValueRejections is the distinction #305 turns
// on. A property missing from the allowlist is a one-line fix; an ALLOWED
// property whose value is always refused means the value rule is wrong. Summed
// into one "dropped" total they are indistinguishable, and the report would not
// be able to tell the operator which of two very different things to do.
func TestMeasureSeparatesPropertyFromValueRejections(t *testing.T) {
	_, m := MeasureHTMLCSS(`<div style="animation:spin 2s;background:url(https://t.example/p.gif);color:red">x</div>`)

	if m.PropNotAllowed["animation"] != 1 {
		t.Errorf("PropNotAllowed[animation] = %d, want 1 — `animation` is not on the allowlist",
			m.PropNotAllowed["animation"])
	}
	if m.ValueRejected["animation"] != 0 {
		t.Errorf("ValueRejected[animation] = %d, want 0 — a property that never reached its "+
			"value must not be reported as a value rejection", m.ValueRejected["animation"])
	}
	if m.ValueRejected["background"] != 1 {
		t.Errorf("ValueRejected[background] = %d, want 1 — `background` IS allowed and its "+
			"url() value is what was refused (#297)", m.ValueRejected["background"])
	}
	if m.PropNotAllowed["background"] != 0 {
		t.Errorf("PropNotAllowed[background] = %d, want 0", m.PropNotAllowed["background"])
	}
	// The denominator. Without it, "dropped 1" cannot be read against anything.
	if m.DeclKept["color"] != 1 {
		t.Errorf("DeclKept[color] = %d, want 1 — a report with no kept count cannot "+
			"distinguish a rare property from a broken one", m.DeclKept["color"])
	}
}

// TestMeasureAttributesALostStylesheet covers the other question the issue asks:
// when a <style> element vanishes, was it the SELECTOR grammar or the property
// allowlist? Those have different fixes and the report has to say which.
func TestMeasureAttributesALostStylesheet(t *testing.T) {
	t.Run("lost to the selector", func(t *testing.T) {
		_, m := MeasureHTMLCSS(`<style>&:hover{color:red}</style>`)
		if m.RulePreludeToken["delim &"] != 1 {
			t.Errorf("RulePreludeToken[delim &] = %v, want 1 — the Sass parent selector is a "+
				"real refused token now that the child combinator is admitted (#324)",
				m.RulePreludeToken)
		}
		if m.RuleNoDeclsLeft != 0 {
			t.Errorf("RuleNoDeclsLeft = %d, want 0 — the declarations were never reached, so "+
				"blaming the allowlist would send someone to fix the wrong list", m.RuleNoDeclsLeft)
		}
		if m.StyleElements != 1 || m.StyleElementsEmptied != 1 {
			t.Errorf("style element accounting = %d seen / %d emptied, want 1/1",
				m.StyleElements, m.StyleElementsEmptied)
		}
	})

	t.Run("lost to the property allowlist", func(t *testing.T) {
		_, m := MeasureHTMLCSS(`<style>p{animation:spin 2s}</style>`)
		if m.RuleNoDeclsLeft != 1 {
			t.Errorf("RuleNoDeclsLeft = %d, want 1", m.RuleNoDeclsLeft)
		}
		if len(m.RulePreludeToken) != 0 {
			t.Errorf("RulePreludeToken = %v, want empty — the selector `p` is fine", m.RulePreludeToken)
		}
		if m.PropNotAllowed["animation"] != 1 {
			t.Errorf("PropNotAllowed[animation] = %d, want 1", m.PropNotAllowed["animation"])
		}
	})
}

// TestMeasureCountsAtRules keeps the at-rule population from being the one with
// no number — @import is the tracking-relevant one and it must be visible.
func TestMeasureCountsAtRules(t *testing.T) {
	_, m := MeasureHTMLCSS(`<style>@media screen{p{color:red}}@import url(x.css);</style>`)
	if m.AtRuleKept["media"] != 1 {
		t.Errorf("AtRuleKept[media] = %d, want 1", m.AtRuleKept["media"])
	}
	if m.AtRuleNotAllowed["import"] != 1 {
		t.Errorf("AtRuleNotAllowed[import] = %d, want 1 — @import fetches, so a corpus with "+
			"many of them is a finding about senders, not noise", m.AtRuleNotAllowed["import"])
	}
}

// The markup guard in writeStyleElement is documented as unreachable. Counting
// it is only useful if it stays at zero on ordinary mail, so this pins that: a
// non-zero in the corpus report means one of the escaping claims is false, not
// that the mail was unusual.
func TestMarkupGuardStaysAtZeroOnOrdinaryMail(t *testing.T) {
	for _, in := range cssSamples {
		if _, m := MeasureHTMLCSS(in); m.StyleElementsMarkupGuard != 0 {
			t.Errorf("StyleElementsMarkupGuard = %d on %.60q — writeStyleElement's own comment "+
				"says this is unreachable because cssWriteIdent escapes and cssStringContentOK "+
				"refuses those characters. A hit here is a finding, not a statistic",
				m.StyleElementsMarkupGuard, in)
		}
	}
}

// TestStyleAttrsAreCountedSeparatelyFromStylesheets: a style="" attribute and a
// <style> element are different populations with different fixes, and the two
// were deliberately not merged.
func TestStyleAttrsAreCountedSeparatelyFromStylesheets(t *testing.T) {
	_, m := MeasureHTMLCSS(`<style>p{color:red}</style><div style="animation:x">a</div><span style="color:blue">b</span>`)
	if m.StyleAttrs != 2 {
		t.Errorf("StyleAttrs = %d, want 2", m.StyleAttrs)
	}
	if m.StyleAttrsEmptied != 1 {
		t.Errorf("StyleAttrsEmptied = %d, want 1 (the `animation` one)", m.StyleAttrsEmptied)
	}
	if m.StyleElements != 1 || m.StyleElementsEmptied != 0 {
		t.Errorf("style elements = %d seen / %d emptied, want 1/0",
			m.StyleElements, m.StyleElementsEmptied)
	}
}

// SortCSSDrops decides what an operator reads first. Sorting by declarations
// would let one newsletter template received 200 times outrank a property that
// 200 different messages lost.
func TestSortCSSDropsLeadsWithMessagesAffected(t *testing.T) {
	d := []CSSDrop{
		{Name: "one-template", Declarations: 400, Messages: 2},
		{Name: "widespread", Declarations: 60, Messages: 55},
		{Name: "tie-b", Declarations: 10, Messages: 55},
		{Name: "tie-a", Declarations: 10, Messages: 55},
	}
	SortCSSDrops(d)
	got := make([]string, len(d))
	for i := range d {
		got[i] = d[i].Name
	}
	want := "widespread,tie-a,tie-b,one-template"
	if strings.Join(got, ",") != want {
		t.Errorf("order = %s, want %s — messages affected first, then declarations, then name",
			strings.Join(got, ","), want)
	}
}

// ⚠ THIS TEST EXISTS BECAUSE THE FIRST CORPUS RUN PRODUCED A NONSENSE NUMBER.
//
// css-report against a corpus of real messages reported hundreds of thousands of "malformed
// declarations" — more than every real drop combined, and the largest figure on
// the page. It was an artefact of the counter, not of the mail:
// cssFilterDeclarationList splits on every top-level semicolon, so an ordinary
// `style="color:red;"` yields a second, EMPTY span, and the empty span was being
// counted as a malformed declaration.
//
// Nothing rendered differently; the report was simply lying about the corpus in
// the row a reader looks at first. An empty span is not a declaration.
func TestTrailingSemicolonsAreNotMalformedDeclarations(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want int
	}{
		{"trailing semicolon", `<div style="color:red;">x</div>`, 0},
		{"several empty spans", `<div style=";;color:red;;;">x</div>`, 0},
		{"only semicolons", `<div style=";;;">x</div>`, 0},
		// A genuinely malformed declaration still counts — otherwise the fix
		// would have traded a false number for a blind one.
		{"no colon", `<div style="color red">x</div>`, 1},
		{"value missing", `<div style="color:">x</div>`, 1},
		{"not an ident first", `<div style="1color:red">x</div>`, 1},
	} {
		if _, m := MeasureHTMLCSS(tc.in); m.DeclMalformed != tc.want {
			t.Errorf("%s: DeclMalformed = %d, want %d (in: %s)",
				tc.name, m.DeclMalformed, tc.want, tc.in)
		}
	}
}

// TestEmptiedStyleElementsAreAttributed is the question #305 asks that a corpus
// total cannot answer: when a <style> element vanishes, was it the selector
// grammar or the property allowlist? "N rules lost to the allowlist and M
// to selectors" says nothing about which emptied any particular element, so the
// counters are snapshotted per element and diffed.
func TestEmptiedStyleElementsAreAttributed(t *testing.T) {
	for _, tc := range []struct {
		name                               string
		in                                 string
		allowlist, selector, both, neither int
	}{
		{"allowlist only", `<style>p{animation:spin 2s}</style>`, 1, 0, 0, 0},
		{"selector only", `<style>&:hover{color:red}</style>`, 0, 1, 0, 0},
		{"both", `<style>&:hover{color:red}div{animation:spin 2s}</style>`, 0, 0, 1, 0},
		{"neither — an at-rule-only sheet", `<style>@import url(x.css);</style>`, 0, 0, 0, 1},
		// A surviving element is attributed to nothing at all.
		{"survives", `<style>p{color:red}</style>`, 0, 0, 0, 0},
	} {
		_, m := MeasureHTMLCSS(tc.in)
		if m.EmptiedByAllowlist != tc.allowlist || m.EmptiedBySelector != tc.selector ||
			m.EmptiedByBoth != tc.both || m.EmptiedByNeither != tc.neither {
			t.Errorf("%s: attribution = allowlist %d / selector %d / both %d / neither %d, "+
				"want %d / %d / %d / %d (in: %s)", tc.name,
				m.EmptiedByAllowlist, m.EmptiedBySelector, m.EmptiedByBoth, m.EmptiedByNeither,
				tc.allowlist, tc.selector, tc.both, tc.neither, tc.in)
		}
	}
}

// The attribution is a diff of counters shared with the whole document, so a
// SECOND element must not inherit the first one's losses. This is the bug that
// snapshot-and-diff exists to avoid, asserted rather than assumed.
func TestAttributionDoesNotLeakBetweenStyleElements(t *testing.T) {
	// First element dies of the selector; second dies of the allowlist.
	_, m := MeasureHTMLCSS(`<style>&:hover{color:red}</style><style>div{animation:spin 2s}</style>`)
	if m.EmptiedBySelector != 1 || m.EmptiedByAllowlist != 1 || m.EmptiedByBoth != 0 {
		t.Errorf("attribution = allowlist %d / selector %d / both %d, want 1 / 1 / 0 — a "+
			"`both` here means the second element inherited the first one's counters",
			m.EmptiedByAllowlist, m.EmptiedBySelector, m.EmptiedByBoth)
	}
}

// Every token kind that can actually be REFUSED must have a name. The first
// corpus run printed "token kind 10" 60 times, which tells an operator nothing
// about what their mail contains.
//
// The set is derived from cssPreludeTokenOK rather than listed here: a kind it
// accepts can never reach the label, and hard-coding the refusable ones would be
// a second copy of that predicate — free to drift, and drifting silently back
// into bare integers is the whole failure being fixed.
func TestEveryRefusablePreludeTokenKindIsNamed(t *testing.T) {
	named := 0
	for k := cssEOF; k <= cssBad; k++ {
		tok := cssToken{kind: k}
		if cssPreludeTokenOK(tok) {
			continue // accepted, so it never reaches the label
		}
		named++
		if label := cssPreludeTokenLabel(tok); strings.HasPrefix(label, "unnamed token kind ") {
			t.Errorf("token kind %d can be refused but has no name — it would reach the "+
				"report as %q", k, label)
		}
	}
	// The bound, reported: a loop that skipped everything would pass silently.
	if named < 5 {
		t.Fatalf("only %d refusable token kinds were examined — cssPreludeTokenOK now accepts "+
			"almost everything, so this test is no longer checking anything", named)
	}
}
