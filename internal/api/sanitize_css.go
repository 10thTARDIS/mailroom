// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"strconv"
	"strings"
)

// SERVER-SIDE CSS SANITIZATION (spec §11, Forgejo #39, rebuilt for Forgejo #297).
//
// ── WHY THIS FILE EXISTS AT ALL, WHICH IS THE INTERESTING PART ───────────────
//
// sanitize.go opens with an argument (see its ── WHY THIS IS A PARSE-FILTER-
// RESERIALIZE, NOT A REGEX ── section) that the classic way to lose is to
// pattern-match on a string, because the browser's parser and the sanitizer's
// parser then disagree about what the bytes MEAN. The HTML half took that
// seriously: it parses with the browser's own tree-construction algorithm,
// filters the TREE against an allowlist, and re-serializes.
//
// The CSS half did not. Until #297 it was one regular expression:
//
//	var cssDanger = regexp.MustCompile(`(?i)…|url\s*\(|expression\s*\(|…`)
//
// CSS Syntax Level 3 §4.3.1 unescapes an identifier BEFORE anything compares it,
// so `\75 rl(https://tracker.example/x.png)` is a perfectly ordinary <url-token>
// that spells `url(` to every browser on earth and contains none of those bytes.
// `image-set('https://tracker.example/x.png' 1x)` needed no trick at all — it was
// simply not on the list. Both were MEASURED fetching in the pinned Chromium
// image, inside a srcdoc frame carrying the real sandbox token list and the exact
// messageCSP(), and the harm was worse than a stylesheet: a body whose only
// remote reference lives in escaped CSS carries no data-mailroom-blocked marker,
// so the web client's hasBlockedRemoteContent() is false and NEITHER banner
// renders. Combined with a remembered per-sender allow (#202) that is a silent,
// per-recipient tracking beacon through both layers.
//
// The test could not have found it: assertInertCSS asserted the absence of the
// literal substrings "url(", "@import", "expression(" — THE SAME STRING MODEL AS
// THE CODE. A test built on the code's model cannot discover the code's model
// error. (It is now structural; see sanitize_test.go.)
//
// ── WHAT REPLACED IT: AN ALLOWLIST OF PROPERTIES ─────────────────────────────
//
// This file is the CSS half of the same shape the HTML half already had:
//
//  1. TOKENIZE with a CSS Syntax Level 3 tokenizer (§4). Escapes are decoded
//     HERE, once, by the same rules a browser decodes them — so `\75 rl(`,
//     `u\72 l(`, `@\69 mport` and `e\78 pression(` arrive at the filter already
//     spelled `url(`, `url(`, `@import` and `expression(`. There is no string to
//     out-clever, because the comparison no longer happens on bytes.
//  2. FILTER the token stream STRUCTURALLY: a declaration survives only if its
//     PROPERTY is on an allowlist, and only if every token in its VALUE is on a
//     value allowlist. A rule survives only if its prelude is inert and its block
//     kept at least one declaration.
//  3. RE-SERIALIZE CANONICALLY. Nothing is copied out of the input. Every name we
//     emit comes from OUR OWN tables (the allowlist's spelling, not the sender's),
//     every value identifier is re-escaped into a form that can only be read back
//     as that identifier, and every string is re-quoted. What we emit is a
//     rendering of a token stream we fully inspected — the same claim the HTML
//     half makes about its tree.
//
// An allowlist of PROPERTIES is what makes this immune to the whole class of bug
// #297 was: it does not matter how cleverly a value spells `url(`, because a
// property that is only meaningful as a URL — background-image, list-style-image,
// border-image-source, cursor, content, filter, mask, src, behavior,
// -moz-binding — is not on the list and its declaration never survives to have
// its value read. See cssAllowedProperties for the deliberate exceptions
// (`background`, `font`, `list-style`) and the argument for them.
//
// ── FAIL CLOSED, ALWAYS ──────────────────────────────────────────────────────
//
// Every "I do not understand this" path in this file drops the declaration, the
// rule, or the whole stylesheet — never keeps it. A message body whose CSS is
// destroyed is a message that renders unstyled; a message body whose CSS is
// misunderstood is an origin.

// --- the tokenizer (CSS Syntax Level 3 §4) ------------------------------------

type cssTokenKind uint8

const (
	cssEOF cssTokenKind = iota
	cssWhitespace
	cssIdent
	cssFunction // an identifier immediately followed by '('
	cssAtKeyword
	cssHash
	cssString
	cssURL // a <url-token>: url( with an UNQUOTED argument. Always fatal here.
	cssNumber
	cssColon
	cssSemicolon
	cssComma
	cssLBrace
	cssRBrace
	cssLParen
	cssRParen
	cssLBracket
	cssRBracket
	cssCDO // "<!--"
	cssCDC // "-->"
	cssDelim
	// cssBad is a bad-string, a bad-url, a stray backslash, or anything else the
	// tokenizer refuses to model. It is never serialized and it always kills its
	// enclosing declaration or rule.
	cssBad
)

// cssToken is one token. `value` carries the DECODED text — an identifier's
// escapes are already resolved, a string's contents are already unquoted — which
// is the single fact that closes #297: the filter never sees the sender's
// spelling of a name, only its meaning.
type cssToken struct {
	kind  cssTokenKind
	value string // ident/function/at-keyword/hash: the decoded name. string: the decoded contents. number: the numeric text.
	unit  string // number: the decoded dimension unit, or "%" for a percentage.
	delim rune   // delim: the character.
}

// cssMaxTokens bounds a pathological stylesheet. A message body is hostile input
// and this runs on the read path of every HTML mail; this is already far past any
// real newsletter. Over the bound we return a single cssBad, so the whole
// stylesheet is dropped rather than half-filtered.
const cssMaxTokens = 200000

type cssScanner struct {
	src []rune
	pos int
}

// eofRune stands for "past the end". It is not a valid CSS code point, so no
// input can forge it.
const eofRune rune = -1

func newCSSScanner(s string) *cssScanner {
	// §3.3 input preprocessing: normalize newlines and replace NUL. (The caller
	// has already stripped the other C0 controls — see sanitizeCSSInput.)
	s = strings.NewReplacer("\r\n", "\n", "\r", "\n", "\f", "\n", "\x00", "�").Replace(s)
	return &cssScanner{src: []rune(s)}
}

func (z *cssScanner) at(i int) rune {
	if i < 0 || i >= len(z.src) {
		return eofRune
	}
	return z.src[i]
}

func (z *cssScanner) cur() rune { return z.at(z.pos) }

func cssIsWhitespace(r rune) bool { return r == ' ' || r == '\t' || r == '\n' }
func cssIsDigit(r rune) bool      { return r >= '0' && r <= '9' }

func cssIsHexDigit(r rune) bool {
	return cssIsDigit(r) || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
}

// cssIsNameStart / cssIsName are §4.2's ident-start and ident code points. Every
// code point >= 0x80 is an ident code point, which is why a non-ASCII font name
// tokenizes as one identifier rather than as garbage.
func cssIsNameStart(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '_' || r >= 0x80
}

func cssIsName(r rune) bool { return cssIsNameStart(r) || cssIsDigit(r) || r == '-' }

// validEscapeAt is §4.3.8: a backslash that is not at the end of input and is not
// followed by a newline.
func (z *cssScanner) validEscapeAt(i int) bool {
	if z.at(i) != '\\' {
		return false
	}
	n := z.at(i + 1)
	return n != eofRune && n != '\n'
}

// startsIdentAt is §4.3.9 ("would start an ident sequence").
func (z *cssScanner) startsIdentAt(i int) bool {
	switch r := z.at(i); {
	case r == '-':
		n := z.at(i + 1)
		return cssIsNameStart(n) || n == '-' || z.validEscapeAt(i+1)
	case cssIsNameStart(r):
		return true
	case r == '\\':
		return z.validEscapeAt(i)
	}
	return false
}

// startsNumberAt is §4.3.10 ("would start a number").
func (z *cssScanner) startsNumberAt(i int) bool {
	switch r := z.at(i); r {
	case '+', '-':
		return cssIsDigit(z.at(i+1)) || (z.at(i+1) == '.' && cssIsDigit(z.at(i+2)))
	case '.':
		return cssIsDigit(z.at(i + 1))
	default:
		return cssIsDigit(r)
	}
}

// consumeEscape is §4.3.7. z.pos is positioned just AFTER the backslash.
//
// This is the function #297 was about. A browser runs it before it decides what
// an identifier says; the old regex did not, so `\75 rl(` and `url(` were
// different strings to us and the same token to Chromium.
func (z *cssScanner) consumeEscape() rune {
	r := z.cur()
	if r == eofRune {
		return '�'
	}
	if !cssIsHexDigit(r) {
		z.pos++
		return r
	}
	cp := 0
	for n := 0; n < 6 && cssIsHexDigit(z.cur()); n++ {
		cp = cp*16 + cssHexValue(z.cur())
		z.pos++
	}
	// A single whitespace character after the hex digits TERMINATES the escape and
	// is consumed. This is why `\75 rl(` is `url(` and not `u rl(`.
	if cssIsWhitespace(z.cur()) {
		z.pos++
	}
	if cp == 0 || cp > 0x10FFFF || (cp >= 0xD800 && cp <= 0xDFFF) {
		return '�'
	}
	return rune(cp)
}

func cssHexValue(r rune) int {
	switch {
	case r >= '0' && r <= '9':
		return int(r - '0')
	case r >= 'a' && r <= 'f':
		return int(r-'a') + 10
	default:
		return int(r-'A') + 10
	}
}

// consumeName is §4.3.11, and it returns the DECODED name.
func (z *cssScanner) consumeName() string {
	var b strings.Builder
	for {
		switch r := z.cur(); {
		case cssIsName(r):
			b.WriteRune(r)
			z.pos++
		case z.validEscapeAt(z.pos):
			z.pos++ // the backslash
			b.WriteRune(z.consumeEscape())
		default:
			return b.String()
		}
	}
}

// consumeNumber is §4.3.12. The returned text contains only signs, digits, '.'
// and an exponent, so it is safe to emit verbatim.
func (z *cssScanner) consumeNumber() string {
	start := z.pos
	if r := z.cur(); r == '+' || r == '-' {
		z.pos++
	}
	for cssIsDigit(z.cur()) {
		z.pos++
	}
	if z.cur() == '.' && cssIsDigit(z.at(z.pos+1)) {
		z.pos += 2
		for cssIsDigit(z.cur()) {
			z.pos++
		}
	}
	if r := z.cur(); r == 'e' || r == 'E' {
		i := z.pos + 1
		if s := z.at(i); s == '+' || s == '-' {
			i++
		}
		if cssIsDigit(z.at(i)) {
			z.pos = i
			for cssIsDigit(z.cur()) {
				z.pos++
			}
		}
	}
	return string(z.src[start:z.pos])
}

// consumeNumeric is §4.3.3: number, percentage or dimension.
func (z *cssScanner) consumeNumeric() cssToken {
	num := z.consumeNumber()
	if z.startsIdentAt(z.pos) {
		return cssToken{kind: cssNumber, value: num, unit: z.consumeName()}
	}
	if z.cur() == '%' {
		z.pos++
		return cssToken{kind: cssNumber, value: num, unit: "%"}
	}
	return cssToken{kind: cssNumber, value: num}
}

// consumeString is §4.3.5. A raw newline inside a string is a bad-string, which
// is fatal to whatever contains it.
func (z *cssScanner) consumeString(quote rune) cssToken {
	z.pos++ // the opening quote
	var b strings.Builder
	for {
		switch r := z.cur(); r {
		case eofRune, quote:
			if r == quote {
				z.pos++
			}
			return cssToken{kind: cssString, value: b.String()}
		case '\n':
			// §4.3.5 does NOT consume the newline: it belongs to the next token.
			return cssToken{kind: cssBad}
		case '\\':
			if z.at(z.pos+1) == eofRune {
				z.pos++
				continue
			}
			if z.at(z.pos+1) == '\n' { // an escaped newline is a line continuation
				z.pos += 2
				continue
			}
			z.pos++
			b.WriteRune(z.consumeEscape())
		default:
			b.WriteRune(r)
			z.pos++
		}
	}
}

// consumeURL is §4.3.6, the UNQUOTED url( … ) form. Its result is always
// rejected by the filter — it is modelled faithfully only so the scanner stays in
// step with the browser about where the token ENDS.
func (z *cssScanner) consumeURL() cssToken {
	for cssIsWhitespace(z.cur()) {
		z.pos++
	}
	var b strings.Builder
	for {
		switch r := z.cur(); {
		case r == ')':
			z.pos++
			return cssToken{kind: cssURL, value: b.String()}
		case r == eofRune:
			return cssToken{kind: cssURL, value: b.String()}
		case cssIsWhitespace(r):
			for cssIsWhitespace(z.cur()) {
				z.pos++
			}
			if z.cur() == ')' {
				z.pos++
			}
			return cssToken{kind: cssURL, value: b.String()}
		case r == '"' || r == '\'' || r == '(':
			z.consumeBadURLRemnants()
			return cssToken{kind: cssBad}
		case r == '\\':
			if !z.validEscapeAt(z.pos) {
				z.consumeBadURLRemnants()
				return cssToken{kind: cssBad}
			}
			z.pos++
			b.WriteRune(z.consumeEscape())
		default:
			b.WriteRune(r)
			z.pos++
		}
	}
}

// consumeBadURLRemnants is §4.3.14: swallow to the closing paren so the scanner
// resynchronizes where the browser's does.
func (z *cssScanner) consumeBadURLRemnants() {
	for {
		switch r := z.cur(); {
		case r == eofRune:
			return
		case r == ')':
			z.pos++
			return
		case z.validEscapeAt(z.pos):
			z.pos++
			z.consumeEscape()
		default:
			z.pos++
		}
	}
}

// consumeIdentLike is §4.3.4: ident, function or url token.
func (z *cssScanner) consumeIdentLike() cssToken {
	name := z.consumeName()
	if z.cur() != '(' {
		return cssToken{kind: cssIdent, value: name}
	}
	if !strings.EqualFold(name, "url") {
		z.pos++
		return cssToken{kind: cssFunction, value: name}
	}
	// `url(` — a quoted argument means a FUNCTION token, an unquoted one means a
	// <url-token>. Both are refused by the filter; the distinction only decides
	// where the token stops.
	z.pos++
	i := z.pos
	for cssIsWhitespace(z.at(i)) {
		i++
	}
	if r := z.at(i); r == '"' || r == '\'' {
		return cssToken{kind: cssFunction, value: name}
	}
	return z.consumeURL()
}

// next returns the next token (§4.3.1).
func (z *cssScanner) next() cssToken {
	r := z.cur()
	switch {
	case r == eofRune:
		return cssToken{kind: cssEOF}

	case cssIsWhitespace(r):
		for cssIsWhitespace(z.cur()) {
			z.pos++
		}
		return cssToken{kind: cssWhitespace}

	case r == '/' && z.at(z.pos+1) == '*':
		// A comment is consumed as a token BOUNDARY, not as nothing — which is what
		// makes "@im/**/port" two tokens (`@im`, `port`) rather than the reassembled
		// "@import" a naive comment-stripping pass would produce. Either way it does
		// not survive; this way it does not survive for the RIGHT reason.
		z.pos += 2
		for z.cur() != eofRune {
			if z.cur() == '*' && z.at(z.pos+1) == '/' {
				z.pos += 2
				break
			}
			z.pos++
		}
		return cssToken{kind: cssWhitespace}

	case r == '"' || r == '\'':
		return z.consumeString(r)

	case r == '#':
		if cssIsName(z.at(z.pos+1)) || z.validEscapeAt(z.pos+1) {
			z.pos++
			return cssToken{kind: cssHash, value: z.consumeName()}
		}
		z.pos++
		return cssToken{kind: cssDelim, delim: '#'}

	case r == '@':
		if z.startsIdentAt(z.pos + 1) {
			z.pos++
			return cssToken{kind: cssAtKeyword, value: z.consumeName()}
		}
		z.pos++
		return cssToken{kind: cssDelim, delim: '@'}

	case r == '(':
		z.pos++
		return cssToken{kind: cssLParen}
	case r == ')':
		z.pos++
		return cssToken{kind: cssRParen}
	case r == '[':
		z.pos++
		return cssToken{kind: cssLBracket}
	case r == ']':
		z.pos++
		return cssToken{kind: cssRBracket}
	case r == '{':
		z.pos++
		return cssToken{kind: cssLBrace}
	case r == '}':
		z.pos++
		return cssToken{kind: cssRBrace}
	case r == ',':
		z.pos++
		return cssToken{kind: cssComma}
	case r == ':':
		z.pos++
		return cssToken{kind: cssColon}
	case r == ';':
		z.pos++
		return cssToken{kind: cssSemicolon}

	case r == '<':
		if z.at(z.pos+1) == '!' && z.at(z.pos+2) == '-' && z.at(z.pos+3) == '-' {
			z.pos += 4
			return cssToken{kind: cssCDO}
		}
		z.pos++
		return cssToken{kind: cssDelim, delim: '<'}

	case r == '+' || r == '.':
		if z.startsNumberAt(z.pos) {
			return z.consumeNumeric()
		}
		z.pos++
		return cssToken{kind: cssDelim, delim: r}

	case r == '-':
		if z.startsNumberAt(z.pos) {
			return z.consumeNumeric()
		}
		if z.at(z.pos+1) == '-' && z.at(z.pos+2) == '>' {
			z.pos += 3
			return cssToken{kind: cssCDC}
		}
		if z.startsIdentAt(z.pos) {
			return z.consumeIdentLike()
		}
		z.pos++
		return cssToken{kind: cssDelim, delim: '-'}

	case cssIsDigit(r):
		return z.consumeNumeric()

	case r == '\\':
		if z.validEscapeAt(z.pos) {
			return z.consumeIdentLike()
		}
		// §4.3.1: a stray backslash is a parse error. Fail closed.
		z.pos++
		return cssToken{kind: cssBad}

	case cssIsNameStart(r):
		return z.consumeIdentLike()

	default:
		z.pos++
		return cssToken{kind: cssDelim, delim: r}
	}
}

// cssTokenize runs the scanner to exhaustion. Over cssMaxTokens it returns a
// single cssBad, which drops everything downstream.
func cssTokenize(s string) []cssToken {
	z := newCSSScanner(s)
	out := make([]cssToken, 0, 64)
	for {
		t := z.next()
		if t.kind == cssEOF {
			return out
		}
		out = append(out, t)
		if len(out) > cssMaxTokens {
			return []cssToken{{kind: cssBad}}
		}
	}
}

// --- canonical re-serialization ----------------------------------------------
//
// Nothing here copies a byte out of the input. An identifier is rebuilt from its
// DECODED runes with every character outside [A-Za-z0-9_-] (and non-ASCII)
// re-escaped, so the bytes we emit can be read back as that identifier and as
// nothing else — no `(`, no `;`, no `{`, no `<`, no quote, however the sender
// spelled it. That is the CSS analogue of html.EscapeString in the HTML half.

// cssWriteIdent emits name as an identifier that can only be read back as name.
func cssWriteIdent(b *strings.Builder, name string) {
	runes := []rune(name)
	for i, r := range runes {
		safe := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '_' || r >= 0x80 ||
			cssIsDigit(r) || r == '-'
		switch {
		case !safe:
			cssWriteEscape(b, r)
		case cssIsDigit(r) && i == 0:
			// An identifier may not START with a digit.
			cssWriteEscape(b, r)
		case cssIsDigit(r) && i == 1 && runes[0] == '-':
			// "-3" would tokenize as a number, not an identifier.
			cssWriteEscape(b, r)
		case r == '-' && i == 0 && len(runes) == 1:
			// A lone "-" is a delim, not an identifier.
			cssWriteEscape(b, r)
		default:
			b.WriteRune(r)
		}
	}
}

// cssWriteEscape emits the "\<hex><space>" form. The trailing space terminates
// the escape, so the following character is never absorbed into it.
func cssWriteEscape(b *strings.Builder, r rune) {
	b.WriteByte('\\')
	b.WriteString(strconv.FormatInt(int64(r), 16))
	b.WriteByte(' ')
}

// cssWriteString re-quotes a string.
//
// The escapes for '<' and '>' are UNREACHABLE today — cssStringContentOK already
// refuses a string containing either — and that is on purpose: if someone ever
// widens the content rule, the writer keeps the "no '<' or '>' in emitted CSS"
// promise, and sanitize_test.go's independent literal check goes red so the
// decision gets made deliberately rather than inherited.
func cssWriteString(b *strings.Builder, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"', '\\', '<', '>', '\n':
			cssWriteEscape(b, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
}

// cssWriteTokens serializes a vetted token run. Every token it can be handed has
// already passed cssValueTokenOK or cssPreludeTokenOK; anything else is a
// programming error and is dropped rather than guessed at.
func cssWriteTokens(b *strings.Builder, toks []cssToken) {
	for _, t := range toks {
		switch t.kind {
		case cssWhitespace:
			b.WriteByte(' ')
		case cssIdent:
			cssWriteIdent(b, t.value)
		case cssFunction:
			// The name comes from OUR allowlist table, canonically spelled — the
			// sender's spelling of `rgb` never reaches the output.
			b.WriteString(cssASCIILower(t.value))
			b.WriteByte('(')
		case cssHash:
			b.WriteByte('#')
			cssWriteHashName(b, t.value)
		case cssString:
			cssWriteString(b, t.value)
		case cssNumber:
			b.WriteString(t.value)
			if t.unit == "%" {
				b.WriteByte('%')
			} else {
				cssWriteIdent(b, t.unit)
			}
		case cssColon:
			b.WriteByte(':')
		case cssComma:
			b.WriteByte(',')
		case cssLParen:
			b.WriteByte('(')
		case cssRParen:
			b.WriteByte(')')
		case cssLBracket:
			b.WriteByte('[')
		case cssRBracket:
			b.WriteByte(']')
		case cssDelim:
			b.WriteRune(t.delim)
		}
	}
}

// cssWriteHashName emits a hash's name. Unlike an identifier a hash MAY start
// with a digit (#333), so only genuinely unsafe characters are escaped.
func cssWriteHashName(b *strings.Builder, name string) {
	for _, r := range name {
		if cssIsName(r) {
			b.WriteRune(r)
			continue
		}
		cssWriteEscape(b, r)
	}
}

// cssASCIILower lowercases ASCII only. strings.ToLower is Unicode-aware, and a
// Unicode-aware fold is the wrong tool for a security comparison: it maps
// characters onto ASCII ones that were never written (the Kelvin sign folds to
// "k"), which is a way to spell an allowlist entry that nobody typed.
func cssASCIILower(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		b.WriteByte(c)
	}
	return b.String()
}

// --- the lists ----------------------------------------------------------------

// cssAllowedProperties is what a message body may set. Everything else — every
// vendor hack, every custom property, every property nobody has thought about —
// is dropped.
//
// THE SELECTION RULE, so the next person can extend it correctly:
//
//	A property is on this list only if it CANNOT name an external resource.
//	Anything whose value is a <url>, an <image>, or a script hook is absent and
//	must stay absent:
//
//	  background-image, border-image*, list-style-image, mask*, -webkit-mask*,
//	  shape-outside, offset-path, clip-path, cursor, content, filter,
//	  -ms-filter, src, behavior, -moz-binding, fill, stroke, and every custom
//	  property (--*).
//
//	That is the whole #297 fix in one sentence: it does not matter how a value
//	spells `url(` if no property that would fetch one is ever kept.
//
// `fill` and `stroke` joined that list in #322: SVG paint takes url(#gradient),
// which is the same class as the rest, and the corpus found `fill` in 28
// messages — so it was a real candidate that had to be refused on purpose rather
// than by not having been thought about.
//
// ⚠ THE RULE IS NOW PARTLY ENFORCED, AND ONLY PARTLY. sanitize_css_allowlist_test.go
// checks that every property ON this map refuses a fetching value, and that the
// specific names above are absent from it. Neither can decide whether a NEW
// property is URL-capable — that is a fact about CSS, not about this code — so
// widening this list still requires the judgement the rule describes. Do not read
// a green suite as permission.
//
// THREE SHORTHANDS ARE HERE THAT DO ADMIT AN <image>, and the argument for them
// is deliberate rather than an oversight:
//
//	background, font, list-style
//
// `background:#f4f4f4` is how essentially every HTML newsletter paints a table
// cell, `font:` is how they set a family and size at once, and dropping the three
// would visibly wreck ordinary mail for no gain the value allowlist does not
// already give. An <image> can only enter a value as a <url-token> (a token type
// the filter rejects outright, in every position, in every property) or as an
// image function — image-set(), -webkit-image-set(), image(), cross-fade(),
// element(), paint() — and cssAllowedValueFunctions is an ALLOWLIST, so a
// function is refused unless it is named there. That is the same KIND of
// guarantee as the property list, not a denylist, which is why it is acceptable
// here and why #297's failure cannot recur through it.
//
// Deliberately absent for a different reason: position/top/right/bottom/left/
// z-index. They cannot fetch, but absolute positioning is how a body shows the
// user one thing and a scanner another, and no mail needs it inside a frame that
// contains nothing but the message. See rule 7 in docs/hostile-content.md — if
// real mail is being destroyed, widen this list, do not weaken the policy.
var cssAllowedProperties = map[string]bool{
	// Colour and text.
	"color": true, "opacity": true, "visibility": true,
	"font": true, "font-family": true, "font-size": true, "font-size-adjust": true,
	"font-stretch": true, "font-style": true, "font-variant": true,
	"font-variant-caps": true, "font-variant-numeric": true, "font-weight": true,
	"font-feature-settings": true, "font-kerning": true,
	"line-height": true, "letter-spacing": true, "word-spacing": true,
	"text-align": true, "text-align-last": true, "text-indent": true,
	"text-justify": true, "text-overflow": true, "text-shadow": true,
	"text-transform": true, "text-rendering": true,
	"text-decoration": true, "text-decoration-color": true,
	"text-decoration-line": true, "text-decoration-style": true,
	"text-decoration-thickness": true, "text-underline-offset": true,
	"text-underline-position": true,
	"text-size-adjust":        true, "-webkit-text-size-adjust": true, "-ms-text-size-adjust": true,
	"-webkit-font-smoothing": true, "-moz-osx-font-smoothing": true,
	"white-space": true, "word-break": true, "word-wrap": true, "overflow-wrap": true,
	"hyphens": true, "direction": true, "unicode-bidi": true, "writing-mode": true,
	"tab-size": true, "vertical-align": true, "quotes": true,

	// Box.
	"display": true, "box-sizing": true, "float": true, "clear": true,
	"overflow": true, "overflow-x": true, "overflow-y": true,
	"width": true, "min-width": true, "max-width": true,
	"height": true, "min-height": true, "max-height": true,
	"margin": true, "margin-top": true, "margin-right": true,
	"margin-bottom": true, "margin-left": true,
	"padding": true, "padding-top": true, "padding-right": true,
	"padding-bottom": true, "padding-left": true,

	// Border. border-image* is NOT here: it takes an <image>.
	"border": true, "border-top": true, "border-right": true,
	"border-bottom": true, "border-left": true,
	"border-color": true, "border-style": true, "border-width": true,
	"border-top-color": true, "border-top-style": true, "border-top-width": true,
	"border-right-color": true, "border-right-style": true, "border-right-width": true,
	"border-bottom-color": true, "border-bottom-style": true, "border-bottom-width": true,
	"border-left-color": true, "border-left-style": true, "border-left-width": true,
	"border-radius": true, "border-top-left-radius": true, "border-top-right-radius": true,
	"border-bottom-left-radius": true, "border-bottom-right-radius": true,
	"border-collapse": true, "border-spacing": true,
	"outline": true, "outline-color": true, "outline-style": true,
	"outline-width": true, "outline-offset": true,
	// box-shadow is <color> || <length>{2,4} || inset — no <image>, no <url>. It
	// is here rather than left out because real templates use it heavily and
	// leaving it out costs rendering for nothing. -ms-interpolation-mode is the
	// Outlook image-scaling hint every newsletter boilerplate carries.
	"box-shadow": true, "-ms-interpolation-mode": true,

	// Background. background-image is NOT here; `background` is, under the value
	// allowlist argued for above. The rest are inert on their own and only exist
	// so a template that sets them alongside a colour is not half-mangled.
	"background": true, "background-color": true, "background-position": true,
	"background-repeat": true, "background-size": true, "background-attachment": true,
	"background-clip": true, "background-origin": true,

	// Tables and lists.
	"table-layout": true, "caption-side": true, "empty-cells": true,
	"list-style": true, "list-style-type": true, "list-style-position": true,

	// ── ADDED FROM A CORPUS MEASUREMENT (#322), NOT FROM A GUESS ──────────────
	//
	// #305 ran the shipped filter over a corpus of real messages and counted what it
	// dropped, per property, with messages-affected kept separate from
	// declaration counts. Everything below was losing real mail. Every one of
	// them takes keywords, lengths, numbers or colours: none can name an
	// external resource, so the selection rule above is unchanged and #297's
	// guarantee does not move.
	//
	// The figure after each is MESSAGES AFFECTED out of 1,804 with an HTML part.
	// Entries marked (family) were NOT separately measured and are here because
	// splitting a shorthand from its longhands renders one intent incoherently —
	// allowing `gap` while dropping `row-gap` is arbitrary. They are additions on
	// reasoning rather than evidence, and are labelled so the distinction
	// survives.

	// Flex and alignment — the finding. Mail is laid out with flexbox now.
	//   measured: align-items 149, gap 86, align-self 47, align-content 41,
	//             flex-direction 39, flex 36, justify-content 32, flex-grow 26,
	//             column-gap 5, flex-shrink 5
	"align-items": true, "align-self": true, "align-content": true,
	"justify-content": true, "gap": true, "column-gap": true,
	"flex": true, "flex-direction": true, "flex-grow": true, "flex-shrink": true,
	// (family)
	"flex-basis": true, "flex-flow": true, "flex-wrap": true, "order": true,
	"justify-items": true, "justify-self": true, "row-gap": true,
	"place-content": true, "place-items": true, "place-self": true,

	// Grid placement — measured: grid-column 35. grid-row is (family).
	//
	// grid-template-areas is deliberately NOT here: its value is a string, and
	// strings are admitted only for the four properties in
	// cssStringValueProperties, so it would be dropped anyway. An allowlist entry
	// that can never match is the kind of check this project keeps finding.
	"grid-column": true, "grid-row": true,

	// Inert presentation the corpus showed being dropped.
	//   measured: mix-blend-mode 143, color-scheme 118, object-fit 112,
	//             line-break 85, image-rendering 49, text-underline-color 39,
	//             font-variant-ligatures 36, appearance 32, user-select 25,
	//             orphans 18, widows 18, text-wrap 18, pointer-events 15,
	//             object-position 9, caret-color 7, text-wrap-mode 5,
	//             white-space-collapse 5, font-optical-sizing 5
	"mix-blend-mode": true, "color-scheme": true, "object-fit": true,
	"object-position": true, "line-break": true, "image-rendering": true,
	"text-underline-color": true, "font-variant-ligatures": true,
	"font-optical-sizing": true, "appearance": true, "user-select": true,
	"pointer-events": true, "caret-color": true,
	"orphans": true, "widows": true,
	"text-wrap": true, "text-wrap-mode": true, "white-space-collapse": true,

	// `zoom` (measured: 99) is non-standard but universally implemented, and its
	// value is a number or a percentage. It is here because it cannot fetch and
	// because it grants no hiding power that `font-size` and `display` do not
	// already give — not because non-standard properties are generally welcome.
	"zoom": true,

	// Logical box properties — measured: margin-block-start 54, margin-block-end
	// 54, margin-block 9. The inline axis is (family).
	//
	// The padding logical properties are deliberately NOT here: nothing in the
	// corpus used them, and adding an unmeasured set to look symmetrical is how
	// the original list acquired its gaps.
	"margin-block": true, "margin-block-start": true, "margin-block-end": true,
	"margin-inline": true, "margin-inline-start": true, "margin-inline-end": true,

	// Scroll margin — measured: scroll-margin-top 35. The other three sides and
	// the shorthand are (family).
	"scroll-margin": true, "scroll-margin-top": true, "scroll-margin-right": true,
	"scroll-margin-bottom": true, "scroll-margin-left": true,
}

// cssStringValueProperties are the only properties whose value may contain a
// STRING token. A string is how a font family with a space is written and there
// is no other honest use for one in mail — and a string is the shape a URL takes
// inside image-set() and @import, so refusing it everywhere else removes the
// token type from every place it could ever have meant a resource.
var cssStringValueProperties = map[string]bool{
	"font": true, "font-family": true, "quotes": true,
	"font-feature-settings": true,
}

// cssStringContentOK restricts what a surviving string may CONTAIN, which is a
// second, independent reason the strings we emit cannot be a URL: a font family
// is letters, digits, spaces, dots, hyphens and non-ASCII, and nothing else.
func cssStringContentOK(s string) bool {
	for _, r := range s {
		switch {
		case cssIsName(r): // letters, digits, '-', '_', and every non-ASCII rune
		case r == ' ' || r == '.':
		default:
			return false
		}
	}
	return true
}

// cssNoMarkupChars keeps a promise that is easy to state, cheap to hold, and
// worth having: NO '<' AND NO '>' EVER APPEARS IN THE CSS WE EMIT, IN ANY
// ENCODING.
//
// It is applied to the DECODED text of every identifier, hash and dimension unit
// that survives. `font-family:a\3c \2f style\3e ` decodes to the identifier
// `a</style>`, and cssWriteIdent would faithfully re-escape it — the emitted
// bytes carry no raw '<', the HTML tokenizer never sees one, writeStyleElement's
// markup guard never trips, and it is inert. All of which is true, and all of
// which is a four-step argument someone has to reconstruct.
//
// A font family called "a<b" does not exist. Refusing the character is cheaper
// than the argument, it keeps the emitted-bytes claim to one sentence, and it
// keeps sanitize_test.go's independent literal check — which decodes every escape
// before it looks — free of the false positives that would erode trust in it.
// (This was found by probing that test, not by reasoning: the corpus had no
// identifier carrying an escaped structural character until one was added.)
func cssNoMarkupChars(s string) bool {
	return !strings.ContainsAny(s, "<>")
}

// PROBED: deleting the escape branch in cssWriteIdent (emitting the decoded rune
// raw) makes `font-family:a\3b b`, `a\7b b`, `a\28 b`, `a\22 b`, `a\5c b` and
// `a\'b` stop being fixed points in TestSanitizeCSSIsTotalInertAndAFixedPoint —
// e.g. once="font-family:a{b", twice="". Those six inputs exist for that reason;
// before they were added the escaper was not observable by any test.

// cssAllowedValueFunctions is every function that may appear in a declaration's
// value. It is an ALLOWLIST, which is the point: image-set(), -webkit-image-set(),
// image(), cross-fade(), element(), paint(), expression(), attr() and everything
// nobody has thought of yet are refused by NOT BEING HERE, rather than by being
// enumerated somewhere as dangerous.
//
// var() is deliberately absent. A custom property cannot be declared (--* is not
// on the property list) so var() could only ever resolve to its fallback — but
// var() is also the one construct whose value comes from somewhere else, and
// "somewhere else" is the wrong shape for this file.
var cssAllowedValueFunctions = map[string]bool{
	"rgb": true, "rgba": true, "hsl": true, "hsla": true, "hwb": true,
	"calc": true, "min": true, "max": true, "clamp": true,
	"linear-gradient": true, "radial-gradient": true, "conic-gradient": true,
	"repeating-linear-gradient": true, "repeating-radial-gradient": true,
	"repeating-conic-gradient": true,
}

// cssAllowedSelectorFunctions is every function that may appear in a rule's
// prelude (a selector, or a media query). Same allowlist discipline: a prelude
// that names a function we do not recognize costs its rule.
var cssAllowedSelectorFunctions = map[string]bool{
	"not": true, "is": true, "where": true, "has": true, "matches": true,
	"lang": true, "dir": true,
	"nth-child": true, "nth-last-child": true,
	"nth-of-type": true, "nth-last-of-type": true,
	"-moz-any": true, "-webkit-any": true,
}

// cssAllowedAtRules is every at-rule a message stylesheet may contain. Both are
// pure GROUPING constructs: their contents are filtered by exactly the same
// stylesheet parser, so they add no capability at all.
//
// Absent, and each for its own reason:
//
//	@import      fetches a stylesheet. This is the one #297's regex was written
//	             for, and `@\69 mport` walked straight past it.
//	@font-face   fetches a font (its `src:` descriptor is a url).
//	@charset     re-interprets the bytes of the stylesheet.
//	@namespace   changes what a selector means.
//	@property    can give a custom property a <url> initial value.
//	@keyframes   would need `animation` on the property list, which it is not.
var cssAllowedAtRules = map[string]bool{
	"media": true, "supports": true,
}

// cssValueDelims are the only bare delimiter characters a VALUE may contain:
// "!important", the "12px/1.5" of a font shorthand, and calc()'s arithmetic.
// Everything else — '<', '>', '@', '\\', '&', a stray quote — costs the
// declaration.
const cssValueDelims = "!/+-*"

// cssPreludeDelims are the delimiters a selector or media query may contain.
//
// ⚠ '>' IS HERE AS OF #324, AND '<' IS DELIBERATELY NOT. The distinction is the
// whole change, so it is worth stating exactly.
//
// It used to exclude both, for a reason that was sound when written:
// writeStyleElement dropped the ENTIRE <style> element if any '<' or '>' reached
// its output, so admitting '>' would have traded one child-combinator rule for
// the whole stylesheet. The price of that trade was estimated as "a small
// rendering loss" and was never measured. #305's corpus report measured it:
// thousands of rules and many emptied stylesheets on a corpus of real messages — after #322,
// more stylesheets than the property allowlist destroys.
//
// The trade was avoidable, because the guard was blunter than the hazard.
// A <style> element is RAW TEXT: nothing terminates it but '</style'. A bare '>'
// is literal text with no meaning at all there. So the guard needs to refuse '<',
// not '<' and '>' — and '<' is refused by every path that can reach the output:
//
//	delims        cssValueDelims and this constant, neither of which contains '<'
//	identifiers   cssWriteIdent escapes every non-name rune, so '<' becomes "\3c "
//	hash names    cssWriteHashName, same escape
//	strings       cssWriteString escapes '<' and '>' by name
//	numbers       digits, sign, '.', 'e' — and the unit goes through cssWriteIdent
//	functions     the name is a key of OUR allowlist tables, never the sender's text
//
// cssDelim is the ONLY token kind cssWriteTokens writes as a raw rune. That is
// what makes the delim allowlists the entire gate, and it is why adding one
// character to one of them is a change worth this much comment.
//
// '</style' is therefore unconstructible rather than merely filtered — the same
// kind of claim the property allowlist makes. TestOutputCannotContainALessThan
// holds every one of those paths to it, and hostile-content-e2e.sh proves it in
// a real browser with an actual breakout attempt, because this is exactly the
// shape of reasoning that produced #297: correct about the model it had, wrong
// about the model the browser had.
const cssPreludeDelims = ".*+~|^$=/->"

// --- the filter ---------------------------------------------------------------

// cssValueTokenOK reports whether one token may appear in the value of the named
// property. `prop` is the canonical (lowercased, allowlisted) property name.
func cssValueTokenOK(t cssToken, prop string) bool {
	switch t.kind {
	case cssWhitespace, cssComma:
		return true
	case cssNumber:
		return cssNoMarkupChars(t.unit)
	case cssHash, cssIdent:
		return cssNoMarkupChars(t.value)
	case cssString:
		return cssStringValueProperties[prop] && cssStringContentOK(t.value)
	case cssFunction:
		return cssAllowedValueFunctions[cssASCIILower(t.value)]
	case cssLParen, cssRParen:
		// calc((100% - 20px) / 2). Balance is checked separately.
		return true
	case cssDelim:
		return t.delim < 0x80 && strings.ContainsRune(cssValueDelims, t.delim)
	default:
		// cssURL, cssAtKeyword, cssColon, cssSemicolon, cssLBrace, cssRBrace,
		// cssLBracket, cssRBracket, cssCDO, cssCDC, cssBad — none of them has any
		// business in a declaration value.
		return false
	}
}

// cssPreludeTokenOK reports whether one token may appear in a selector or a media
// query.
func cssPreludeTokenOK(t cssToken) bool {
	switch t.kind {
	case cssWhitespace, cssComma, cssColon, cssLParen, cssRParen, cssLBracket, cssRBracket:
		return true
	case cssNumber:
		return cssNoMarkupChars(t.unit)
	case cssHash, cssIdent:
		return cssNoMarkupChars(t.value)
	case cssString:
		return cssStringContentOK(t.value)
	case cssFunction:
		return cssAllowedSelectorFunctions[cssASCIILower(t.value)]
	case cssDelim:
		return t.delim < 0x80 && strings.ContainsRune(cssPreludeDelims, t.delim)
	default:
		return false
	}
}

// cssPreludeTokenLabel names a refused selector token for the corpus report
// (#305), in terms an operator can act on. A delimiter is reported as itself —
// `>` and `+` and `~` are combinators someone can look for in a real stylesheet
// — and everything else by kind, since a rejected function or string matters as
// a category rather than as its text. It is never used by the filter, only to
// key a counter.
func cssPreludeTokenLabel(t cssToken) string {
	switch t.kind {
	case cssDelim:
		return "delim " + string(t.delim)
	case cssFunction:
		return "function " + cssASCIILower(t.value) + "()"
	case cssString:
		return "string"
	case cssIdent, cssHash:
		return "name with markup characters"
	case cssURL:
		return "url()"
	case cssBad:
		return "unparseable token"
	case cssSemicolon:
		return "semicolon"
	case cssAtKeyword:
		return "at-keyword"
	case cssNumber:
		return "number with markup characters"
	case cssLBrace, cssRBrace:
		return "stray brace"
	case cssCDO, cssCDC:
		return "<!-- or -->"
	case cssEOF:
		return "end of input"
	default:
		// Not reachable for any kind the tokenizer emits — every one is named
		// above. Kept so a NEW token kind cannot silently join the table as a
		// bare integer, which is what the first corpus run printed
		// ("token kind 10", 60 occurrences) before the list was completed.
		return "unnamed token kind " + strconv.Itoa(int(t.kind))
	}
}

// cssTokensBalanced reports whether every opener in toks has its closer. An
// unbalanced run is refused rather than repaired: a value we cannot bracket is a
// value we do not understand.
func cssTokensBalanced(toks []cssToken) bool {
	var stack []cssTokenKind
	for _, t := range toks {
		switch t.kind {
		case cssFunction, cssLParen:
			stack = append(stack, cssRParen)
		case cssLBracket:
			stack = append(stack, cssRBracket)
		case cssLBrace:
			stack = append(stack, cssRBrace)
		case cssRParen, cssRBracket, cssRBrace:
			if len(stack) == 0 || stack[len(stack)-1] != t.kind {
				return false
			}
			stack = stack[:len(stack)-1]
		}
	}
	return len(stack) == 0
}

func cssTrimWhitespace(toks []cssToken) []cssToken {
	for len(toks) > 0 && toks[0].kind == cssWhitespace {
		toks = toks[1:]
	}
	for len(toks) > 0 && toks[len(toks)-1].kind == cssWhitespace {
		toks = toks[:len(toks)-1]
	}
	return toks
}

// cssFilterDeclaration turns one `prop: value` token run into the text to emit,
// or "" if it does not survive.
func cssFilterDeclaration(toks []cssToken, st *cssStats) string {
	toks = cssTrimWhitespace(toks)
	if len(toks) == 0 {
		// ⚠ AN EMPTY SPAN IS NOT A DECLARATION AND MUST NOT BE COUNTED AS ONE
		// (#305). cssFilterDeclarationList splits on every top-level semicolon,
		// so `color:red;` yields a second, empty span — as does `;;`. Counting
		// those as malformed made the first corpus run report hundreds of thousands of "malformed
		// declarations", more than every real drop combined, from nothing but
		// trailing semicolons. A number that large in a report is not a harmless
		// artefact: it is the row a reader looks at first.
		return ""
	}
	if toks[0].kind != cssIdent {
		st.declMalformed()
		return ""
	}
	prop := cssASCIILower(toks[0].value)
	if !cssAllowedProperties[prop] {
		st.propNotAllowed(prop)
		return ""
	}
	rest := cssTrimWhitespace(toks[1:])
	if len(rest) == 0 || rest[0].kind != cssColon {
		st.declMalformed()
		return ""
	}
	value := cssTrimWhitespace(rest[1:])
	if len(value) == 0 || !cssTokensBalanced(value) {
		st.declMalformed()
		return ""
	}
	for _, t := range value {
		if !cssValueTokenOK(t, prop) {
			// The property IS on the allowlist and the VALUE is what failed —
			// counted apart from propNotAllowed because the two have different
			// fixes (#305).
			st.valueRejected(prop)
			return ""
		}
	}
	st.declKept(prop)
	var b strings.Builder
	// The property name is OUR spelling from the allowlist, not the sender's.
	b.WriteString(prop)
	b.WriteByte(':')
	cssWriteTokens(&b, value)
	return b.String()
}

// cssFilterDeclarationList filters a declaration list — the contents of a
// style="" attribute, or of one rule's { } block.
func cssFilterDeclarationList(toks []cssToken, st *cssStats) string {
	var kept []string
	depth := 0
	start := 0
	flush := func(end int) {
		if d := cssFilterDeclaration(toks[start:end], st); d != "" {
			kept = append(kept, d)
		}
	}
	for i, t := range toks {
		switch t.kind {
		case cssFunction, cssLParen, cssLBracket, cssLBrace:
			depth++
		case cssRParen, cssRBracket, cssRBrace:
			if depth > 0 {
				depth--
			}
		case cssSemicolon:
			if depth == 0 {
				flush(i)
				start = i + 1
			}
		}
	}
	flush(len(toks))
	return strings.Join(kept, ";")
}

// cssParser walks a token stream as a stylesheet (§5).
type cssParser struct {
	toks []cssToken
	i    int
}

func (p *cssParser) done() bool { return p.i >= len(p.toks) }

// takeUntilBlock collects prelude tokens up to a top-level '{'. It returns the
// prelude and the terminator's kind (cssLBrace, cssSemicolon or cssEOF); the
// terminator itself is consumed.
//
// stopAtSemicolon distinguishes the two productions in §5.4, and the difference
// is not cosmetic: "consume an at-rule" ends at a top-level ';' (that is what
// makes `@import "x";` a complete rule), while "consume a qualified rule" does
// NOT — a ';' is just another token in its prelude, which is why `foo;p{…}` is
// ONE malformed rule to a browser rather than a dropped statement followed by a
// good rule. Stopping in the wrong place would make our parser and the renderer
// disagree about where a rule begins, which is the whole failure mode this file
// exists to avoid.
func (p *cssParser) takeUntilBlock(stopAtSemicolon bool) ([]cssToken, cssTokenKind) {
	start := p.i
	depth := 0
	for !p.done() {
		switch t := p.toks[p.i]; t.kind {
		case cssFunction, cssLParen, cssLBracket:
			depth++
		case cssRParen, cssRBracket:
			if depth > 0 {
				depth--
			}
		case cssLBrace:
			if depth == 0 {
				prelude := p.toks[start:p.i]
				p.i++ // the '{'
				return prelude, cssLBrace
			}
			depth++
		case cssRBrace:
			if depth > 0 {
				depth--
			}
		case cssSemicolon:
			if depth == 0 && stopAtSemicolon {
				prelude := p.toks[start:p.i]
				p.i++ // the ';'
				return prelude, cssSemicolon
			}
		}
		p.i++
	}
	return p.toks[start:p.i], cssEOF
}

// takeBlock collects the contents of a { } block whose '{' has already been
// consumed, and leaves p positioned after the matching '}'.
func (p *cssParser) takeBlock() []cssToken {
	start := p.i
	depth := 0
	for !p.done() {
		switch t := p.toks[p.i]; t.kind {
		case cssFunction, cssLParen, cssLBracket, cssLBrace:
			depth++
		case cssRParen, cssRBracket:
			if depth > 0 {
				depth--
			}
		case cssRBrace:
			if depth == 0 {
				block := p.toks[start:p.i]
				p.i++ // the '}'
				return block
			}
			depth--
		}
		p.i++
	}
	// Unterminated: the browser closes it at EOF and so do we.
	return p.toks[start:p.i]
}

// cssMaxNesting bounds @media inside @media inside @media. Real mail nests once,
// if at all.
const cssMaxNesting = 4

// cssFilterStylesheet filters a list of rules — the contents of a <style>
// element, or of an allowed at-rule's block.
func cssFilterStylesheet(toks []cssToken, depth int, st *cssStats) string {
	if depth > cssMaxNesting {
		return ""
	}
	p := &cssParser{toks: toks}
	var out []string
	for !p.done() {
		switch t := p.toks[p.i]; t.kind {
		case cssWhitespace, cssCDO, cssCDC:
			// CDO/CDC are the `<!--` / `-->` that old mail wraps a <style> body in.
			// The spec ignores them at the top level of a stylesheet and so do we —
			// and dropping the TOKENS is what keeps their '<' and '>' out of our
			// output, where writeStyleElement would have cost the whole element.
			p.i++
		case cssRBrace:
			p.i++ // a stray closer
		case cssAtKeyword:
			if r := p.atRule(depth, st); r != "" {
				out = append(out, r)
			}
		default:
			if r := p.qualifiedRule(st); r != "" {
				out = append(out, r)
			}
		}
	}
	return strings.Join(out, "")
}

// atRule filters one at-rule. p is positioned on the at-keyword.
func (p *cssParser) atRule(depth int, st *cssStats) string {
	name := cssASCIILower(p.toks[p.i].value)
	p.i++
	prelude, term := p.takeUntilBlock(true)
	var block []cssToken
	if term == cssLBrace {
		block = p.takeBlock()
	}
	if !cssAllowedAtRules[name] || term != cssLBrace {
		// Not allowed, or a statement at-rule (`@import …;`) — we keep none of
		// those. Either way it and its block have been consumed, and discarding
		// them is the whole point.
		st.atRuleNotAllowed(name)
		return ""
	}
	prelude = cssTrimWhitespace(prelude)
	if len(prelude) == 0 || !cssTokensBalanced(prelude) {
		st.atRulePreludeBad(name)
		return ""
	}
	for _, t := range prelude {
		if !cssPreludeTokenOK(t) {
			st.atRulePreludeBad(name)
			return ""
		}
	}
	inner := cssFilterStylesheet(block, depth+1, st)
	if inner == "" {
		st.atRuleEmptyInner(name)
		return ""
	}
	st.atRuleKept(name)
	var b strings.Builder
	b.WriteByte('@')
	b.WriteString(name)
	b.WriteByte(' ')
	cssWriteTokens(&b, prelude)
	b.WriteByte('{')
	b.WriteString(inner)
	b.WriteByte('}')
	return b.String()
}

// qualifiedRule filters one `selector { declarations }` rule.
func (p *cssParser) qualifiedRule(st *cssStats) string {
	prelude, term := p.takeUntilBlock(false)
	if term != cssLBrace {
		// No block: not a rule at all (this is where a bare `color:red` in a
		// <style> body dies, exactly as it does in a browser).
		st.ruleNoBlock()
		return ""
	}
	block := p.takeBlock()
	prelude = cssTrimWhitespace(prelude)
	if len(prelude) == 0 || !cssTokensBalanced(prelude) {
		st.rulePreludeEmpty()
		return ""
	}
	for _, t := range prelude {
		if !cssPreludeTokenOK(t) {
			st.rulePreludeToken(cssPreludeTokenLabel(t))
			return ""
		}
	}
	decls := cssFilterDeclarationList(block, st)
	if decls == "" {
		// The selector was fine and the allowlist took every declaration — a
		// different cause from a refused selector token, and the pair is what
		// attributes a vanished <style> element (#305).
		st.ruleNoDeclsLeft()
		return ""
	}
	var b strings.Builder
	cssWriteTokens(&b, prelude)
	b.WriteByte('{')
	b.WriteString(decls)
	b.WriteByte('}')
	return b.String()
}

// --- the two entry points -----------------------------------------------------

// sanitizeCSSInput strips the control characters a CSS tokenizer has no business
// seeing. A NUL or a stray C0 byte inside a value is only ever there to confuse a
// parser into disagreeing with the one that renders it.
func sanitizeCSSInput(css string) string {
	return strings.Map(func(r rune) rune {
		if r == 0 || (r < 0x20 && r != '\n' && r != '\t' && r != '\r' && r != '\f') {
			return -1
		}
		return r
	}, css)
}

// sanitizeStyleAttr filters a style="" attribute value: a declaration list.
// It returns "" when nothing survives, and the caller drops the attribute.
func sanitizeStyleAttr(css string) string { return sanitizeStyleAttrStats(css, nil) }

// sanitizeStylesheet filters the contents of a <style> element: a list of rules.
// It returns "" when nothing survives, and the caller drops the element.
func sanitizeStylesheet(css string) string { return sanitizeStylesheetStats(css, nil) }

// The …Stats variants take the corpus-report collector (#305). st is nil on
// every render path, it is only written to, and nothing in the filter reads it
// back — so it cannot change what survives. See sanitize_css_stats.go for why
// the counters are threaded through the real filter rather than reconstructed
// beside it.
//
// The two plain entry points above keep their original signatures deliberately:
// they are passed as `func(string) string` values by the fuzz-style table in
// sanitize_test.go, and the security-critical path is not the place to churn a
// signature for a report's benefit.

func sanitizeStyleAttrStats(css string, st *cssStats) string {
	return cssFilterDeclarationList(cssTokenize(sanitizeCSSInput(css)), st)
}

func sanitizeStylesheetStats(css string, st *cssStats) string {
	return cssFilterStylesheet(cssTokenize(sanitizeCSSInput(css)), 0, st)
}
