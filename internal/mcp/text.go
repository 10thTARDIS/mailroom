// SPDX-License-Identifier: AGPL-3.0-only

package mcp

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// TEXT RETURNED TO A MODEL IS CLEANED (spec §13a, #49 design pass §5).
//
// Everything a tool returns — a sender's name, a subject, a body, an
// attachment name, a label, a contact's notes, an approval summary — passes
// through cleanValue before it leaves this package. Mail is third-party text,
// and a model reads code points a person never sees: a bidi override can make
// "invoice.exe" read as "exe.eciovni", and a zero-width character can split a
// word a filter or a reviewer was looking for, or hide text between visible
// letters. cleanText removes:
//
//   - C0/C1 control characters, except \n and \t (line breaks are normalised
//     to \n, and U+2028/U+2029 become \n);
//   - the explicit directional formatting characters (embeddings, overrides,
//     isolates, LRM/RLM/ALM) — the same set store.isBidiControl names;
//   - every other Unicode format (Cf) character: zero-width space, joiner and
//     non-joiner, word joiner, BOM, soft hyphen, tag characters;
//   - the rest of Default_Ignorable_Code_Point (isIgnorable): Hangul fillers,
//     variation selectors, the combining grapheme joiner;
//   - U+FFFD, the decoder's mark for bytes that were not UTF-8.
//
// It mirrors store.cleanContactName's predicate (the #831 sanitiser for
// learned names) but keeps line breaks, which a body needs and a name must
// not have. The store's version is unexported and single-line, so it is
// restated here rather than imported; the predicates are the same.

// cleanText is s with the characters above removed.
func cleanText(s string) string {
	if s == "" {
		return s
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return r
		case r == '\r' || r == '\u2028' || r == '\u2029':
			return '\n'
		case r == utf8.RuneError, unicode.IsControl(r), isBidiControl(r), unicode.Is(unicode.Cf, r), isIgnorable(r):
			return -1
		}
		return r
	}, s)
}

// isIgnorable reports the Default_Ignorable_Code_Point characters that are
// NOT Cf (#853 review): the Hangul fillers (U+115F, U+1160, U+3164, U+FFA0),
// the combining grapheme joiner U+034F, U+17B4/U+17B5, U+2065, U+FFF0–FFF8,
// the E0000 block outside the tag characters, and every variation selector
// (U+180B–180D, U+180F, U+FE00–FE0F, U+E0100–E01EF). A renderer draws none of
// them, so they can hide text from a person exactly as a zero-width space
// does. DerivedCoreProperties defines Default_Ignorable as
// Other_Default_Ignorable_Code_Point + Cf + Variation_Selector (minus a few
// exceptions that are Cf, which the Cf rule removes anyway), and the standard
// library carries both tables, so this is the complete set.
func isIgnorable(r rune) bool {
	return unicode.Is(unicode.Other_Default_Ignorable_Code_Point, r) || unicode.Is(unicode.Variation_Selector, r)
}

// isBidiControl reports the explicit directional formatting characters
// (store.isBidiControl's set). They are Cf, so cleanText's Cf rule already
// removes them; naming them keeps the intent readable and the set testable.
func isBidiControl(r rune) bool {
	return (r >= 0x202A && r <= 0x202E) || (r >= 0x2066 && r <= 0x2069) || r == 0x200E || r == 0x200F || r == 0x061C
}

// cleanValue cleans every string in a decoded JSON value, map keys included,
// so no field of any tool's output can carry the characters cleanText removes.
func cleanValue(v any) any {
	switch t := v.(type) {
	case string:
		return cleanText(t)
	case []any:
		for i := range t {
			t[i] = cleanValue(t[i])
		}
		return t
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[cleanText(k)] = cleanValue(val)
		}
		return out
	}
	return v
}

var (
	blankLines = regexp.MustCompile(`\n[ \t]*\n(?:[ \t]*\n)+`)
	spaceRuns  = regexp.MustCompile(`[ \t\r\n\f\v\x{00A0}]+`)
)

// htmlText is the readable text of an HTML body: the visible text with block
// boundaries as line breaks, list items marked, and the subtrees a browser
// never renders as text (script, style, head, title, noscript) dropped
// entirely. Quoted replies are KEPT: this is "read the message", and the quote
// is part of what the message says.
//
// The input is the server's SANITISED HTML (Email/get sanitises every
// text/html body value). Mailroom has two other HTML-to-text helpers, and
// neither fits: ingest's htmlToText (unexported) makes a one-line preview, and
// nudge.HTMLText drops the quoted reply on purpose (it is looking for the new
// questions in a message). This one keeps both the lines and the quote.
func htmlText(s string) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	doc, err := html.Parse(strings.NewReader(s))
	if err != nil {
		return s
	}
	var b strings.Builder
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			if hiddenElement(n) {
				return
			}
			switch n.DataAtom {
			case atom.Script, atom.Style, atom.Head, atom.Title, atom.Noscript, atom.Template:
				return
			case atom.Br:
				b.WriteByte('\n')
				return
			case atom.Li:
				b.WriteString("\n- ")
			case atom.Td, atom.Th:
				b.WriteByte(' ')
			default:
				if isBlock(n.DataAtom) {
					b.WriteByte('\n')
				}
			}
		}
		if n.Type == html.TextNode {
			// Source whitespace (indentation, wrapped lines) is not layout:
			// every run of it is one space; runs across nodes are collapsed
			// below.
			b.WriteString(spaceRuns.ReplaceAllString(n.Data, " "))
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
		if n.Type == html.ElementNode && isBlock(n.DataAtom) {
			b.WriteByte('\n')
		}
	}
	walk(doc)
	lines := strings.Split(b.String(), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSpace(spaceRuns.ReplaceAllString(l, " "))
	}
	out := blankLines.ReplaceAllString(strings.Join(lines, "\n"), "\n\n")
	return strings.TrimSpace(out)
}

// HIDDEN TEXT IS DROPPED (#853 review). Text a person never sees but a model
// reads is a prompt-injection channel: a sender writes instructions in a
// display:none span. The server's sanitiser keeps the `hidden` attribute,
// `aria-hidden` and these CSS properties (they are harmless to a browser), so
// they reach here, and the whole subtree of an element hidden by any of them
// is dropped:
//
//   - the hidden attribute, or aria-hidden="true";
//   - inline display:none, visibility:hidden|collapse, opacity:0, font-size:0
//     (any unit), color:transparent (or an rgba/hsla colour with alpha 0);
//   - a zero height, max-height, width or max-width together with
//     overflow:hidden.
//
// What it cannot see (ADR-044 §5): rules in a <style> block or a class, text
// coloured like its background, text positioned off-screen, and a font too
// small to read but not zero. Those stay a residual risk, bounded by scopes,
// approvals and soft deletes.
func hiddenElement(n *html.Node) bool {
	var style string
	for _, a := range n.Attr {
		switch strings.ToLower(a.Key) {
		case "hidden":
			return true
		case "aria-hidden":
			if strings.EqualFold(strings.TrimSpace(a.Val), "true") {
				return true
			}
		case "style":
			style = a.Val
		}
	}
	if style == "" {
		return false
	}
	decl := map[string]string{}
	for _, d := range strings.Split(strings.ToLower(style), ";") {
		prop, val, ok := strings.Cut(d, ":")
		if !ok {
			continue
		}
		val = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(val), "!important"))
		decl[strings.TrimSpace(prop)] = strings.Join(strings.Fields(val), " ")
	}
	switch {
	case decl["display"] == "none",
		decl["visibility"] == "hidden", decl["visibility"] == "collapse",
		isZero(decl["opacity"]), isZero(decl["font-size"]),
		transparentColor(decl["color"]):
		return true
	}
	if decl["overflow"] == "hidden" || decl["overflow-y"] == "hidden" || decl["overflow-x"] == "hidden" {
		for _, p := range []string{"height", "max-height", "width", "max-width"} {
			if isZero(decl[p]) {
				return true
			}
		}
	}
	return false
}

// isZero reports a CSS length or number whose value is zero, whatever its
// unit: "0", "0.0", ".0", "0px", "0em", "0%".
func isZero(v string) bool {
	v = strings.TrimSpace(v)
	end := 0
	for end < len(v) && (v[end] >= '0' && v[end] <= '9' || v[end] == '.' || v[end] == '+' || v[end] == '-') {
		end++
	}
	if end == 0 {
		return false
	}
	f, err := strconv.ParseFloat(v[:end], 64)
	return err == nil && f == 0
}

// transparentColor reports color:transparent and an rgba()/hsla() (or
// four-argument rgb()/hsl()) colour whose alpha is zero.
func transparentColor(v string) bool {
	if v == "transparent" {
		return true
	}
	open := strings.IndexByte(v, '(')
	if open < 0 || !strings.HasSuffix(v, ")") {
		return false
	}
	switch strings.TrimSpace(v[:open]) {
	case "rgba", "hsla", "rgb", "hsl":
	default:
		return false
	}
	args := strings.FieldsFunc(v[open+1:len(v)-1], func(r rune) bool { return r == ',' || r == '/' || r == ' ' })
	return len(args) == 4 && isZero(args[3])
}

func isBlock(a atom.Atom) bool {
	switch a {
	case atom.P, atom.Div, atom.Hr, atom.Ul, atom.Ol, atom.Tr, atom.Table,
		atom.H1, atom.H2, atom.H3, atom.H4, atom.H5, atom.H6,
		atom.Blockquote, atom.Pre, atom.Section, atom.Article,
		atom.Header, atom.Footer, atom.Aside, atom.Nav, atom.Dd, atom.Dt, atom.Dl:
		return true
	}
	return false
}

// capUTF8 cuts s to at most n bytes without splitting a character, and says
// whether it cut.
func capUTF8(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}
