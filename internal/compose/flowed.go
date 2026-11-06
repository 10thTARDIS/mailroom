// SPDX-License-Identifier: AGPL-3.0-only

package compose

// RFC 3676 format=flowed ENCODING (#93).
//
// The plain-text part of every composed message is sent as
// `text/plain; charset=utf-8; format=flowed; delsp=no`. A long paragraph is
// soft-wrapped near flowWidth characters, each soft break being a trailing
// space before the CRLF, so a receiving app can rewrap the paragraph to its own
// screen while the line breaks the user typed stay hard breaks. The reading
// side is web/src/core/flowed.ts (unflow); the Go test decoder in
// flowed_test.go is written from RFC 3676 §4 independently of this encoder.
//
// The rules, each load-bearing for a round trip:
//   - Every line the user typed is one paragraph. Its trailing spaces are
//     stripped first (§4.1): left in, they would be read as accidental soft
//     breaks and glue the next line on. The exception is the signature
//     separator "-- ", which must stay exactly that line (§4.3).
//   - A paragraph wraps at word boundaries; a word longer than the width (a
//     URL) stays whole on its own line. Wrapping counts characters (runes), and
//     only ever breaks at a space, so no UTF-8 sequence or combining mark is
//     split.
//   - A physical line at quote depth 0 that begins with a space, ">" or
//     "From " is space-stuffed (§4.4). This applies to continuation lines too:
//     a wrap can land a ">" or "From" at the start of a line the user never
//     started that way.
//   - Quoted lines carry their ">" depth on every wrapped line, and a soft
//     break never crosses a change of depth (each user line is wrapped on its
//     own, so it cannot).
//   - A soft-break line is never exactly "-- ": the reader would take it for
//     the signature separator and refuse to join it.

import (
	"strings"
	"unicode/utf8"
)

// flowWidth is the target line length in characters, counting a soft break's
// trailing space. flowMax is the hard ceiling, which only a single unbreakable
// token (a URL) may exceed.
const (
	flowWidth = 72
	flowMax   = 78
)

// flowedContentType is the Content-Type of every composed plain-text part.
const flowedContentType = "text/plain; charset=utf-8; format=flowed; delsp=no"

// flowText encodes body (any line endings) as RFC 3676 flowed text with CRLF
// line endings and a trailing CRLF.
func flowText(body string) string {
	body = normalizeEOL(body)
	body = strings.TrimSuffix(body, "\r\n")
	var out strings.Builder
	for _, line := range strings.Split(body, "\r\n") {
		flowLine(&out, line)
	}
	return out.String()
}

// flowLine appends one user line, wrapped, each physical line ending in CRLF.
func flowLine(out *strings.Builder, line string) {
	depth := 0
	for depth < len(line) && line[depth] == '>' {
		depth++
	}
	content := line[depth:]
	if depth > 0 {
		// The space after the quote marks is part of the mark, not the text.
		content = strings.TrimPrefix(content, " ")
	}

	prefix := ""
	if depth > 0 {
		prefix = strings.Repeat(">", depth) + " "
	}

	if content == "-- " || (depth == 0 && line == "-- ") {
		out.WriteString(prefix + "-- \r\n")
		return
	}
	content = strings.TrimRight(content, " ")
	if content == "" {
		out.WriteString(strings.TrimRight(prefix, " ") + "\r\n")
		return
	}

	// Words, each with the run of spaces that follows it. A leading run of
	// spaces belongs to the first word.
	var tokens []string
	start := 0
	i := 0
	for i < len(content) {
		for i < len(content) && content[i] == ' ' {
			i++
		}
		for i < len(content) && content[i] != ' ' {
			i++
		}
		for i < len(content) && content[i] == ' ' {
			i++
		}
		tokens = append(tokens, content[start:i])
		start = i
	}

	cur := ""
	curLen := 0
	emit := func() {
		l := prefix + cur
		if depth == 0 && (strings.HasPrefix(l, " ") || strings.HasPrefix(l, ">") || strings.HasPrefix(l, "From ")) {
			l = " " + l
		}
		out.WriteString(l + "\r\n")
	}
	limit := flowWidth - utf8.RuneCountInString(prefix)
	for _, tok := range tokens {
		tl := utf8.RuneCountInString(tok)
		if cur != "" && curLen+tl > limit && cur != "-- " {
			emit()
			cur, curLen = "", 0
		}
		cur += tok
		curLen += tl
	}
	emit()
}
