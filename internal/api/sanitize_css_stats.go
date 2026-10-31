// SPDX-License-Identifier: AGPL-3.0-only

package api

import "sort"

// Accounting for what the CSS filter drops (#305).
//
// # Why this lives inside the filter rather than beside it
//
// v1.34.4 replaced sanitizeCSS's regex with a property ALLOWLIST (#297). The
// security property does not depend on that list being generous enough — an
// allowlist is safe by construction however short it is. The RENDERING does, and
// the list's contents were a judgement call written from what HTML mail is
// generally understood to need, with no evidence from any actual mailbox.
//
// ⚠ THE OBVIOUS WAY TO MEASURE THAT IS THE WRONG ONE. A report that re-extracted
// CSS from the HTML and re-applied an allowlist check would be measuring a
// SECOND implementation — code written by the same person, at the same time, to
// agree with the same conclusion. This project has already paid for that once:
// every number the classification epic was judged on for a month came from an
// evaluation with four defects, so "structural features don't generalise" had
// never actually been tested (#169, #220). A measurement is a claim about the
// code that produced it.
//
// So the counters are threaded through the REAL filter and the REAL HTML walk.
// The CSS these numbers describe is the CSS the renderer hands the filter,
// extracted by the renderer's own code, filtered by the renderer's own
// predicates. TestMeasuredHTMLIsIdenticalToSanitizeHTML is what turns that from
// a claim into a fact.
//
// # Why a nil *cssStats is the render path
//
// Every method here has a NIL RECEIVER GUARD, so the filter calls them
// unconditionally and the render path passes nil. That keeps the instrumentation
// from growing `if stats != nil` branches at nine call sites inside
// security-critical code, where a mis-nested condition would be a filtering bug
// rather than an accounting one. Counters cannot change what survives: nothing
// here is read back by the filter.

// cssStats counts why declarations, rules and <style> elements were dropped.
//
// The nine outcomes below are the complete set — read out of the filter, not out
// of the issue that asked for two of them. A population with no number is the
// thing this class of report exists to prevent (#155, #176, #177), so a drop
// reason with no counter would be exactly the gap being complained about.
type cssStats struct {
	// --- declarations -------------------------------------------------------

	// PropNotAllowed counts declarations dropped because the property is not on
	// cssAllowedProperties, keyed by the property as the sender spelled it
	// (lowercased). THE headline number: these are the entries a hole in the
	// list would show up as.
	PropNotAllowed map[string]int
	// ValueRejected counts declarations whose property IS allowed but whose
	// value contained a token cssValueTokenOK refused, keyed by property.
	//
	// ⚠ Deliberately separate from PropNotAllowed, and it is the distinction the
	// issue turns on: a property missing from the list is a one-line fix, while
	// an allowed property whose value is always rejected means the VALUE rule is
	// wrong — a different bug with a different fix. Summed together they would be
	// indistinguishable.
	ValueRejected map[string]int
	// DeclMalformed counts declarations that were never well-formed: no leading
	// ident, no colon, an empty value, or unbalanced brackets. Not evidence about
	// the allowlist — counted so it cannot be silently folded into something that
	// is.
	DeclMalformed int
	// DeclKept counts surviving declarations per property: the denominator. A
	// property dropped 40 times and kept 4000 times is a different finding from
	// one dropped 40 times and never kept, and only this column can tell them
	// apart.
	DeclKept map[string]int

	// --- qualified rules ----------------------------------------------------

	// RuleNoBlock counts preludes with no { } after them — a bare `color:red`
	// sitting in a <style> body. Browsers drop these too, so it is expected
	// noise rather than a loss we caused.
	RuleNoBlock int
	// RulePreludeToken counts rules dropped because a token in the SELECTOR was
	// refused by cssPreludeTokenOK, keyed by a short label for the token. This is
	// where the documented `>` child-combinator loss shows up, and separating it
	// from RuleNoDeclsLeft is what lets a vanished <style> element be attributed
	// to the selector grammar rather than to the property allowlist.
	RulePreludeToken map[string]int
	// RulePreludeEmpty counts rules whose selector was empty or unbalanced.
	RulePreludeEmpty int
	// RuleNoDeclsLeft counts rules whose selector was fine and whose every
	// declaration was dropped — the rule died of the allowlist rather than of
	// the selector grammar. Reading this against RulePreludeToken is how a lost
	// <style> element gets attributed to a cause.
	RuleNoDeclsLeft int

	// --- at-rules -----------------------------------------------------------

	// AtRuleNotAllowed counts at-rules refused by name (or refused for being a
	// statement at-rule like `@import …;`), keyed by name.
	AtRuleNotAllowed map[string]int
	// AtRulePreludeBad counts allowed at-rules whose prelude was rejected.
	AtRulePreludeBad map[string]int
	// AtRuleEmptyInner counts allowed at-rules whose entire body was filtered
	// away — a @media block that lost every rule inside it.
	AtRuleEmptyInner map[string]int
	// AtRuleKept counts at-rules that survived, keyed by name.
	AtRuleKept map[string]int

	// --- <style> elements and style="" attributes ---------------------------

	// StyleElements is every <style> element the renderer reached.
	StyleElements int
	// StyleElementsEmptied is those whose filtered text came back empty, so
	// writeStyleElement dropped the element.
	StyleElementsEmptied int
	// StyleElementsMarkupGuard is those dropped by writeStyleElement's
	// ContainsAny(css, "<>") check.
	//
	// ⚠ A NON-ZERO HERE IS A FINDING, NOT A STATISTIC. That guard's own comment
	// says it should be unreachable, because cssWriteIdent escapes and
	// cssStringContentOK refuses those characters in every encoding. If this
	// column is ever anything but 0, one of those two claims is false and the
	// element that reached it is worth reading by hand.
	StyleElementsMarkupGuard int
	// StyleAttrs is every style="" attribute the renderer reached;
	// StyleAttrsEmptied is those that came back empty and were dropped.
	StyleAttrs        int
	StyleAttrsEmptied int

	// EmptiedBy* attributes each LOST <style> element to a cause, which is the
	// question #305 actually asks: when a stylesheet vanishes, was it the
	// selector grammar or the property allowlist? Corpus totals cannot answer
	// it — "N rules lost to the allowlist and M to selectors" says nothing
	// about which emptied any particular element — so the rule counters are
	// snapshotted before each element and diffed after.
	EmptiedByAllowlist int // only declaration-level drops happened inside it
	EmptiedBySelector  int // only selector-token drops happened inside it
	EmptiedByBoth      int // both, so it needed either fix to survive
	EmptiedByNeither   int // no rule drops at all: an empty or at-rule-only sheet

	// RemoteImages is every <img> URL the sanitizer parked for the "show remote
	// content" affordance (#55 increment 2). It is NOT a CSS statistic — see
	// sanitize_image_stats.go for why it lives on this struct anyway, and why
	// this type keeps its narrower name for now.
	RemoteImages []RemoteImage

	// Silent counts the image forms the sanitizer drops outright, before they
	// could be parked and reach the proxy (#674). See SilentImageForms in
	// sanitize_image_stats.go.
	Silent SilentImageForms
}

// ruleDropMark is a snapshot of the rule-level counters, taken so one <style>
// element's losses can be told apart from the rest of the document's.
type ruleDropMark struct {
	noDeclsLeft  int
	preludeToken int
	preludeEmpty int
}

func (s *cssStats) markRuleDrops() ruleDropMark {
	if s == nil {
		return ruleDropMark{}
	}
	total := 0
	for _, n := range s.RulePreludeToken {
		total += n
	}
	return ruleDropMark{
		noDeclsLeft:  s.RuleNoDeclsLeft,
		preludeToken: total,
		preludeEmpty: s.RulePreludeEmpty,
	}
}

// newCSSStats builds a collector with its maps ready. Only the measurement path
// calls this; the render path passes nil.
func newCSSStats() *cssStats {
	return &cssStats{
		PropNotAllowed:   map[string]int{},
		ValueRejected:    map[string]int{},
		DeclKept:         map[string]int{},
		RulePreludeToken: map[string]int{},
		AtRuleNotAllowed: map[string]int{},
		AtRulePreludeBad: map[string]int{},
		AtRuleEmptyInner: map[string]int{},
		AtRuleKept:       map[string]int{},
	}
}

// ⚠ EVERY HELPER TESTS s FIRST AND TOUCHES A FIELD SECOND, and the obvious
// refactor here is a real bug rather than a style preference. Factoring the
// common line out as `s.bump(s.PropNotAllowed, prop)` reads as nil-safe and is
// not: evaluating the ARGUMENT `s.PropNotAllowed` dereferences s before bump is
// ever entered, so the render path — which passes nil — panics on the first
// dropped declaration. Written out per counter so the guard cannot be bypassed
// by an argument.

func (s *cssStats) propNotAllowed(prop string) {
	if s == nil {
		return
	}
	s.PropNotAllowed[prop]++
}

func (s *cssStats) valueRejected(prop string) {
	if s == nil {
		return
	}
	s.ValueRejected[prop]++
}

func (s *cssStats) declKept(prop string) {
	if s == nil {
		return
	}
	s.DeclKept[prop]++
}

func (s *cssStats) atRuleNotAllowed(n string) {
	if s == nil {
		return
	}
	s.AtRuleNotAllowed[n]++
}

func (s *cssStats) atRulePreludeBad(n string) {
	if s == nil {
		return
	}
	s.AtRulePreludeBad[n]++
}

func (s *cssStats) atRuleEmptyInner(n string) {
	if s == nil {
		return
	}
	s.AtRuleEmptyInner[n]++
}

func (s *cssStats) atRuleKept(n string) {
	if s == nil {
		return
	}
	s.AtRuleKept[n]++
}

func (s *cssStats) declMalformed() {
	if s == nil {
		return
	}
	s.DeclMalformed++
}

func (s *cssStats) ruleNoBlock() {
	if s == nil {
		return
	}
	s.RuleNoBlock++
}

func (s *cssStats) rulePreludeEmpty() {
	if s == nil {
		return
	}
	s.RulePreludeEmpty++
}

// rulePreludeToken records a selector token the filter refused, keyed by a
// short human label rather than the raw token — "the `>` child combinator" is
// what an operator can act on; a token-kind integer is not.
func (s *cssStats) rulePreludeToken(label string) {
	if s == nil {
		return
	}
	s.RulePreludeToken[label]++
}

func (s *cssStats) ruleNoDeclsLeft() {
	if s == nil {
		return
	}
	s.RuleNoDeclsLeft++
}

// styleElement records one <style> element's outcome. before is the mark taken
// immediately prior to filtering it, which is what lets an emptied element be
// attributed to a cause rather than merely counted.
func (s *cssStats) styleElement(emptied, markupGuard bool, before ruleDropMark) {
	if s == nil {
		return
	}
	s.StyleElements++
	if markupGuard {
		s.StyleElementsMarkupGuard++
	}
	if !emptied {
		return
	}
	s.StyleElementsEmptied++

	now := s.markRuleDrops()
	byAllowlist := now.noDeclsLeft > before.noDeclsLeft
	bySelector := now.preludeToken > before.preludeToken || now.preludeEmpty > before.preludeEmpty
	switch {
	case byAllowlist && bySelector:
		s.EmptiedByBoth++
	case byAllowlist:
		s.EmptiedByAllowlist++
	case bySelector:
		s.EmptiedBySelector++
	default:
		s.EmptiedByNeither++
	}
}

func (s *cssStats) styleAttr(emptied bool) {
	if s == nil {
		return
	}
	s.StyleAttrs++
	if emptied {
		s.StyleAttrsEmptied++
	}
}

// --- the exported measurement surface ------------------------------------------

// CSSDrop is one property's (or at-rule name's) accounting across a corpus.
type CSSDrop struct {
	// Name is the property or at-rule name, as the filter lowercased it.
	Name string
	// Declarations is how many individual declarations this accounts for.
	Declarations int
	// Messages is how many distinct MESSAGES were affected.
	//
	// ⚠ Kept independent of Declarations on purpose. One newsletter template
	// received 200 times and 200 different senders produce the same declaration
	// count and mean completely different things; only this column distinguishes
	// "a template quirk" from "a hole the whole mailbox falls through".
	Messages int
}

// CSSMeasurement is one message's CSS accounting: the raw counters, exported so
// a report can aggregate them. It is produced by MeasureHTMLCSS.
type CSSMeasurement struct {
	PropNotAllowed map[string]int
	ValueRejected  map[string]int
	DeclKept       map[string]int
	DeclMalformed  int

	RuleNoBlock      int
	RulePreludeEmpty int
	RulePreludeToken map[string]int
	RuleNoDeclsLeft  int

	AtRuleNotAllowed map[string]int
	AtRulePreludeBad map[string]int
	AtRuleEmptyInner map[string]int
	AtRuleKept       map[string]int

	StyleElements            int
	StyleElementsEmptied     int
	StyleElementsMarkupGuard int
	StyleAttrs               int
	StyleAttrsEmptied        int

	EmptiedByAllowlist int
	EmptiedBySelector  int
	EmptiedByBoth      int
	EmptiedByNeither   int
}

// MeasureHTMLCSS sanitizes in EXACTLY as SanitizeHTML does and additionally
// reports what the CSS filter dropped on the way.
//
// It returns the sanitized HTML as well as the measurement, and that return
// value is not decoration: TestMeasuredHTMLIsIdenticalToSanitizeHTML compares it
// against SanitizeHTML for the same input, which is the check that this really
// is the shipped filter and not a parallel one that has drifted. Without it,
// "the report measures what the renderer does" would be an assertion.
func MeasureHTMLCSS(in string) (string, CSSMeasurement) {
	st := newCSSStats()
	out := sanitizeFragment(in, false, st, nil)
	return out, CSSMeasurement{
		PropNotAllowed:           st.PropNotAllowed,
		ValueRejected:            st.ValueRejected,
		DeclKept:                 st.DeclKept,
		DeclMalformed:            st.DeclMalformed,
		RuleNoBlock:              st.RuleNoBlock,
		RulePreludeEmpty:         st.RulePreludeEmpty,
		RulePreludeToken:         st.RulePreludeToken,
		RuleNoDeclsLeft:          st.RuleNoDeclsLeft,
		AtRuleNotAllowed:         st.AtRuleNotAllowed,
		AtRulePreludeBad:         st.AtRulePreludeBad,
		AtRuleEmptyInner:         st.AtRuleEmptyInner,
		AtRuleKept:               st.AtRuleKept,
		StyleElements:            st.StyleElements,
		StyleElementsEmptied:     st.StyleElementsEmptied,
		StyleElementsMarkupGuard: st.StyleElementsMarkupGuard,
		StyleAttrs:               st.StyleAttrs,
		StyleAttrsEmptied:        st.StyleAttrsEmptied,
		EmptiedByAllowlist:       st.EmptiedByAllowlist,
		EmptiedBySelector:        st.EmptiedBySelector,
		EmptiedByBoth:            st.EmptiedByBoth,
		EmptiedByNeither:         st.EmptiedByNeither,
	}
}

// CSSPropertyIsAllowed reports whether a property survives the allowlist. It
// exists so a report can say which of the properties it saw dropped are
// DELIBERATE exclusions rather than oversights, reading the shipped map rather
// than a copy of it.
func CSSPropertyIsAllowed(prop string) bool { return cssAllowedProperties[prop] }

// SortCSSDrops orders drops by messages affected, then declarations, then name
// — most-likely-to-be-noticed first, with a deterministic tie-break so two runs
// over the same corpus print the same report.
func SortCSSDrops(d []CSSDrop) {
	sort.Slice(d, func(i, j int) bool {
		if d[i].Messages != d[j].Messages {
			return d[i].Messages > d[j].Messages
		}
		if d[i].Declarations != d[j].Declarations {
			return d[i].Declarations > d[j].Declarations
		}
		return d[i].Name < d[j].Name
	})
}
