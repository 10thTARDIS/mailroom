// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"fmt"
	"strings"
	"testing"
)

// Two guarantees about cssAllowedProperties, and an honest statement of what
// neither of them can check (#322).
//
// ⚠ THE FIRST VERSION OF THIS FILE CLAIMED TO ENFORCE THE LIST'S SELECTION RULE
// AND DID NOT. It iterated the allowlist, fed every property a url() value, and
// asserted nothing fetching survived — then PASSED with `background-image` and
// `cursor` deliberately added to the map. Of course it did: the value filter
// refuses a url-token in EVERY property, allowed or not, so the assertion was
// about layer 2 and was completely blind to what layer 1 contains. A test that
// cannot fail when the thing it guards is broken is documentation (CLAUDE.md),
// and that one was mislabelled documentation.
//
// What can actually be checked is split below, and what cannot is said out loud.

// TestEveryAllowedPropertyRefusesAFetchingValue verifies LAYER 2 across the whole
// allowlist: whatever is on the list, none of it can emit a value that names an
// external resource.
//
// This is the guarantee that makes the three <image>-admitting shorthands
// (background, font, list-style) defensible, and it is what the property list is
// defence-in-depth ON TOP OF — not a restatement of it. It drives off the map, so
// a property added tomorrow is covered the moment it is added.
//
// ⚠ IT SAYS NOTHING ABOUT WHETHER A PROPERTY BELONGS ON THE LIST. See
// TestDocumentedlyAbsentPropertiesStayAbsent for the half that does, and the note
// there about the half that cannot be automated at all.
func TestEveryAllowedPropertyRefusesAFetchingValue(t *testing.T) {
	fetching := []struct{ name, value string }{
		{"a plain url-token", `url(https://tracker.example/p.gif)`},
		{"a quoted url-token", `url("https://tracker.example/p.gif")`},
		{"the #297 escaped url", `\75 rl(https://tracker.example/p.gif)`},
		{"an escape mid-ident", `u\72 l(https://tracker.example/p.gif)`},
		{"image-set", `image-set("https://tracker.example/p.gif" 1x)`},
		{"-webkit-image-set", `-webkit-image-set("https://tracker.example/p.gif" 1x)`},
		{"the image() function", `image("https://tracker.example/p.gif")`},
		{"cross-fade", `cross-fade(url(https://tracker.example/p.gif), red)`},
		{"element()", `element(#x)`},
		{"var() indirection", `var(--leak)`},
		{"attr() indirection", `attr(href)`},
		{"an expression", `expression(alert(1))`},
		{"a data: binding", `url(data:text/xml,<x/>)`},
	}
	if len(cssAllowedProperties) < 50 {
		t.Fatalf("the allowlist has only %d entries — this test iterates it, so a shrunken "+
			"map would silently check almost nothing", len(cssAllowedProperties))
	}

	leaked := func(out string) bool {
		l := strings.ToLower(out)
		return strings.Contains(l, "tracker.example") || strings.Contains(l, "url(") ||
			strings.Contains(l, "image-set") || strings.Contains(l, "expression(") ||
			strings.Contains(l, "var(") || strings.Contains(l, "attr(") ||
			strings.Contains(l, "element(") || strings.Contains(l, "cross-fade")
	}

	pairs := 0
	for prop := range cssAllowedProperties {
		for _, f := range fetching {
			pairs++
			decl := prop + ":" + f.value
			if got := sanitizeStyleAttr(decl); leaked(got) {
				t.Errorf("style attr KEPT a fetching value (%s)\n  in:  %s\n  out: %q", f.name, decl, got)
			}
			sheet := fmt.Sprintf("p{%s}", decl)
			if got := sanitizeStylesheet(sheet); leaked(got) {
				t.Errorf("stylesheet KEPT a fetching value (%s)\n  in:  %s\n  out: %q", f.name, sheet, got)
			}
		}
	}
	t.Logf("checked %d (property, fetching-value) pairs across %d allowed properties",
		pairs, len(cssAllowedProperties))
}

// cssMustStayAbsent is the list cssAllowedProperties' own doc names as having to
// stay off it: properties whose value is a <url>, an <image>, or a script hook,
// plus the positioning set excluded for the spoofing reason.
//
// ⚠ THIS IS A SECOND COPY OF A LIST, WHICH IS NORMALLY THE THING TO AVOID — the
// justification is that drift here makes the test INCOMPLETE, never wrong. It
// cannot start passing something it should fail; it can only stop covering a name
// someone added to the doc and not to this. That is an acceptable failure
// direction for a denylist, and it is not acceptable for the allowlist itself,
// which is why the test above iterates the real map instead.
var cssMustStayAbsent = []string{
	// Named in the doc's absent-list: these take a <url>, an <image>, or a hook.
	"background-image", "border-image", "border-image-source", "list-style-image",
	"mask", "mask-image", "-webkit-mask", "-webkit-mask-image",
	"shape-outside", "offset-path", "clip-path", "cursor", "content",
	"filter", "-ms-filter", "src", "behavior", "-moz-binding",
	// SVG paint: takes url(#gradient). The same class; #322 adds it to the doc.
	"fill", "stroke",
	// Absent for the SPOOFING reason, not the fetching one (rule 7 in
	// docs/hostile-content.md). Widening these is a policy decision, and this test
	// is what makes it a deliberate one rather than a quiet one.
	"position", "top", "right", "bottom", "left", "z-index",
	// transform reaches the same overlay outcome as absolute positioning.
	"transform", "transform-origin",
}

// TestDocumentedlyAbsentPropertiesStayAbsent enforces the half of the selection
// rule that CAN be mechanised: the specific names the doc says must never be on
// the list are not on it.
//
// ⚠ WHAT NEITHER TEST CAN DO, stated so nobody mistakes green for safety: decide
// whether a NEW property is URL-capable. "Can this property name an external
// resource" is a fact about CSS, not about this codebase, so it cannot be derived
// from the map. That judgement stays human, it is what the doc's selection rule
// is for, and it is the actual risk when this list is widened.
func TestDocumentedlyAbsentPropertiesStayAbsent(t *testing.T) {
	for _, prop := range cssMustStayAbsent {
		if cssAllowedProperties[prop] {
			t.Errorf("%q is on cssAllowedProperties and must not be — it is named in the "+
				"list's own doc as having to stay absent, either because it can name an "+
				"external resource or because of the positioning/spoofing exclusion "+
				"(rule 7, docs/hostile-content.md)", prop)
		}
	}
	// Custom properties are absent as a CLASS, not by name.
	for _, prop := range []string{"--body-bg", "--x", "--tracking"} {
		if cssAllowedProperties[prop] {
			t.Errorf("the custom property %q is allowed; --* must be absent as a class", prop)
		}
	}
}

// The negative control for both tests above: they must be able to observe a
// failure. Without it, a filter that dropped everything, or a map that was empty,
// would satisfy every assertion here forever — the shape this project has shipped
// three times (#131, #300, and v1.34.6's 1 <= 0).
func TestTheAllowlistProbesCanObserveAFailure(t *testing.T) {
	// The pipeline keeps ordinary declarations, so the url() assertions are not
	// passing merely because nothing survives.
	if got := sanitizeStyleAttr(`color:red`); got != "color:red" {
		t.Fatalf("sanitizeStyleAttr(`color:red`) = %q — the filter is dropping ordinary "+
			"declarations, so every assertion in this file is vacuous", got)
	}
	// The absent-list is non-empty and its membership test really reads the map.
	if len(cssMustStayAbsent) < 10 {
		t.Fatalf("cssMustStayAbsent has %d entries — too few to be checking anything",
			len(cssMustStayAbsent))
	}
	if !cssAllowedProperties["color"] {
		t.Fatal("`color` is not allowed — cssAllowedProperties lookups are not working, so " +
			"TestDocumentedlyAbsentPropertiesStayAbsent would pass against any map at all")
	}
}

// TestTheWidenedPropertiesActuallySurvive is the positive control for #322.
//
// The corpus report proves the added properties stopped being DROPPED. That is
// not the same as their being USABLE: a property admitted by the allowlist whose
// value the value-filter then refuses would vanish just as completely, and the
// report's not-allowed table would still have gone quiet. This asserts the thing
// the widening was for — real declarations, written the way mail writes them,
// coming out the other side intact.
func TestTheWidenedPropertiesActuallySurvive(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`display:flex;align-items:center`, `display:flex;align-items:center`},
		{`justify-content:space-between`, `justify-content:space-between`},
		{`gap:12px`, `gap:12px`},
		{`row-gap:8px;column-gap:16px`, `row-gap:8px;column-gap:16px`},
		{`flex:1 1 auto`, `flex:1 1 auto`},
		{`flex-direction:column;flex-wrap:wrap`, `flex-direction:column;flex-wrap:wrap`},
		{`align-self:flex-end;order:2`, `align-self:flex-end;order:2`},
		{`grid-column:1 / 3`, `grid-column:1 / 3`},
		{`object-fit:cover;object-position:center`, `object-fit:cover;object-position:center`},
		{`mix-blend-mode:multiply`, `mix-blend-mode:multiply`},
		{`color-scheme:light dark`, `color-scheme:light dark`},
		{`margin-block-start:8px`, `margin-block-start:8px`},
		{`zoom:1`, `zoom:1`},
		{`pointer-events:none;user-select:none`, `pointer-events:none;user-select:none`},
		// Still refused, from the same value filter, on a NEWLY allowed property:
		// admitting the property must not admit a URL through it.
		{`object-position:url(https://tracker.example/p.gif)`, ``},
		{`mix-blend-mode:\75 rl(https://tracker.example/p.gif)`, ``},
	} {
		if got := sanitizeStyleAttr(tc.in); got != tc.want {
			t.Errorf("sanitizeStyleAttr(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
