// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"fmt"
	"strings"
	"testing"
)

// ⚠ THE CHECK THE #324 CHANGE RESTS ON.
//
// writeStyleElement now drops a <style> element only when its filtered CSS
// contains '<', because '</style' is the sole terminator of a raw-text element
// and a bare '>' is literal text there. That narrowing is what lets the child
// combinator through — and it is safe only if '<' genuinely cannot reach the
// output.
//
// cssWriteTokens emits exactly ONE token kind as a raw rune: cssDelim. Every
// other kind is escaped (cssWriteIdent, cssWriteHashName, cssWriteString) or
// drawn from our own tables (function names). So the delim allowlists are the
// whole gate, and this test holds all of it:
//
//  1. neither delim allowlist contains '<' — read from the constants, so a future
//     edit to either is caught here rather than in a browser;
//  2. no input, however spelled, produces a '<' in the output.
//
// It is deliberately NOT a restatement of the guard: the guard is the backstop
// for this being wrong. hostile-content-e2e.sh is the third layer, in a real
// browser, with an actual breakout attempt.
func TestOutputCannotContainALessThan(t *testing.T) {
	t.Run("the delim allowlists exclude it, by construction", func(t *testing.T) {
		for name, set := range map[string]string{
			"cssValueDelims":   cssValueDelims,
			"cssPreludeDelims": cssPreludeDelims,
		} {
			if strings.ContainsRune(set, '<') {
				t.Errorf("%s = %q contains '<'. cssDelim is the only token kind written as a "+
					"raw rune, so this single character would make '</style' constructible and "+
					"writeStyleElement's guard the ONLY thing standing between a message body "+
					"and an origin", name, set)
			}
		}
		// The positive half: '>' IS in the prelude set, which is the #324 change.
		// Without this, the test above would pass just as well against a revert.
		if !strings.ContainsRune(cssPreludeDelims, '>') {
			t.Error("cssPreludeDelims no longer contains '>' — the child combinator is refused " +
				"again, and #324's 3,223-rule finding is back")
		}
		if strings.ContainsRune(cssValueDelims, '>') {
			t.Error("cssValueDelims gained '>' — declaration VALUES have no use for it, and " +
				"admitting it there widens the surface #324 deliberately did not touch")
		}
	})

	t.Run("no input produces one", func(t *testing.T) {
		// ⚠ THE FIRST VERSION OF THIS LIST COULD NOT OBSERVE THE FAILURE.
		//
		// Probing #324 by putting '<' into cssPreludeDelims reddened the
		// constant check above and left this subtest GREEN — none of the inputs
		// here had the one shape that actually escapes: a '</style>' sitting in a
		// PRELUDE, followed by a block, so it survives as a selector. Without
		// that shape this is a list of things that were never going to break,
		// which is the most comfortable kind of test and the least useful.
		//
		// The breakout shapes are first, and they are the reason this test exists.
		attempts := []string{
			// ── shapes that DO escape if the delim allowlist admits '<' ──────
			`p{color:red}</style>x{color:blue}`,
			`</style>{color:red}`,
			`</STYLE>x{color:red}`,
			`p{color:red}</style >x{color:blue}`,
			`@media screen{p{color:red}</style>x{color:blue}}`,

			// ── shapes stopped by a DIFFERENT mechanism, kept as regression ───
			// These die at cssNoMarkupChars: '\3c' starts an ident sequence, so
			// the decoded identifier contains '<' and the ident is refused before
			// any delim question arises. They are here so that defence is pinned
			// too — it is independent of the delim allowlists and must stay so.
			`p{color:red}</style><img src=x onerror=alert(1)>`,
			`p{content:"</style>"}`,
			`p{font-family:"</style>"}`,
			`p{font-family:\3c /style}`,
			`p{font-family:a\3c b}`,
			`p{color:\3c }`,
			`p{width:1\3c px}`,
			`\3c style{color:red}`,
			`a\3c b{color:red}`,
			`#\3c x{color:red}`,
			`[a\3c b="c"]{color:red}`,
			`p[title="</style>"]{color:red}`,
			`p{color:rgb(1,2,3)\3c }`,
			`@media \3c {p{color:red}}`,
			`@media (max-width:600px){p{font-family:"</style>"}}`,
			`p{color:red;}</STYLE>`,
			`p{color:red}</ style>`,
			`p{color:red}<\2f style>`,
			`p{color:\00003c}`,
			`p > q{color:red}`, // the newly-allowed case, for contrast
		}
		for _, in := range attempts {
			for _, f := range []struct {
				name string
				fn   func(string) string
			}{
				{"sanitizeStylesheet", sanitizeStylesheet},
				{"sanitizeStyleAttr", sanitizeStyleAttr},
			} {
				out := f.fn(in)
				if strings.ContainsRune(out, '<') {
					t.Errorf("%s emitted a '<'\n  in:  %s\n  out: %q", f.name, in, out)
				}
			}
			// And through the whole HTML path, which is what actually ships.
			doc := fmt.Sprintf("<style>%s</style>", in)
			if got := SanitizeHTML(doc); strings.Contains(got, "</style>") &&
				strings.Count(got, "</style>") > 1 {
				t.Errorf("SanitizeHTML emitted more than one </style> — the element was escaped\n"+
					"  in:  %s\n  out: %q", doc, got)
			}
		}
	})
}

// TestTheLessThanProbeCanObserveAFailure: the assertions above are absence
// claims, and an absence claim over a filter that drops everything is worthless.
// This is the #131 rule applied to them.
func TestTheLessThanProbeCanObserveAFailure(t *testing.T) {
	// The pipeline is alive: ordinary CSS survives, so "no '<' in the output" is
	// not merely "no output".
	if got := sanitizeStylesheet(`p{color:red}`); got != `p{color:red}` {
		t.Fatalf("sanitizeStylesheet(`p{color:red}`) = %q — the filter is dropping ordinary "+
			"stylesheets, so every absence assertion above is vacuous", got)
	}
	// The matcher fires on a string that really does contain one.
	if !strings.ContainsRune(`p{}</style>`, '<') {
		t.Fatal("the matcher used above cannot detect '<'")
	}
	// And the child combinator really does survive — otherwise "no '<'" would be
	// trivially true because the whole rule was dropped, which is exactly the
	// pre-#324 behaviour this change replaced.
	if got := sanitizeStylesheet(`td > p{color:red}`); got != `td > p{color:red}` {
		t.Fatalf("the child combinator did not survive: %q — with the rule dropped, the "+
			"absence of '<' says nothing about the new path", got)
	}
}
