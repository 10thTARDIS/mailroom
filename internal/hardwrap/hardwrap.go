// SPDX-License-Identifier: AGPL-3.0-only

// Package hardwrap decides whether a plain-text message body looks
// HARD-WRAPPED: broken into lines of a fixed width by the sender's mail program
// (the old 72-column habit) rather than by the author, so that on a narrow screen
// every line wraps a second time and leaves a ragged one-word line behind (#35).
//
// THIS IS THE ONE FUNCTION, ON PURPOSE. Today it only feeds a read-only report
// (`mailroom-admin plaintext-report`), which measures how much mail the rule would
// touch BEFORE anyone decides whether to change what is shown. If it is ever used
// to change a display, it must be this code, so the report and the display cannot
// disagree about what "looks wrapped" means. A client that has to run the same
// rule in the browser ports Analyze and keeps the testdata vectors in
// hardwrap_test.go as its conformance cases.
//
// The rule is explicit and conservative. Every threshold is a named constant
// below, and Analyze names the reason it refused, so a count of "not wrapped" can
// be broken down rather than read as one number.
//
//   - The signature (everything from a "-- " line on) and quoted lines (starting
//     with ">") are set aside first, and never counted for or against.
//   - At least MinBodyLines non-blank lines must remain.
//   - Not one remaining line may start with a list marker, or be indented like
//     code. One such line vetoes the whole message: the author laid this text out.
//   - Lines are grouped into paragraphs at blank lines. The "wrap candidates" are
//     every paragraph line except its last (the last line of a paragraph is short
//     by nature). There must be at least MinCandidates of them, and at least
//     WrapFraction of them must be between WrapMinCols and WrapMaxCols wide.
//
// A message that passes would have its Joins counted: the line breaks inside a
// paragraph that follow a candidate line in that width band. A short line in the
// middle of a paragraph is a deliberate break and is not joined.
package hardwrap

import (
	"strings"
	"unicode/utf8"
)

// The thresholds. Changing one changes the report's counts; the tests pin that.
const (
	// MinBodyLines is the fewest non-blank body lines (after the signature and
	// quotes are set aside) a message needs to be judged at all.
	MinBodyLines = 4
	// WrapMinCols and WrapMaxCols bound the width, in characters, of a line that
	// looks wrapped at a fixed column. A typical hard wrap sits at 72 or 76.
	WrapMinCols = 60
	WrapMaxCols = 80
	// WrapFraction is the share of wrap candidates that must lie in the band.
	WrapFraction = 0.8
	// MinCandidates is the fewest wrap candidates (paragraph lines other than a
	// paragraph's last) needed: one long line is a long line, not a wrapped text.
	MinCandidates = 3
	// signatureDelimiter is RFC 3676's "-- " line (also seen without the space).
	signatureDelimiter = "-- "
	// codeIndentSpaces is the indentation, in spaces, that reads as code or a
	// preformatted block. A tab counts as one level of it too.
	codeIndentSpaces = 2
)

// Reason says why a body was not judged wrapped. The zero value, ReasonNone,
// means it WAS judged wrapped.
type Reason string

const (
	ReasonNone Reason = ""
	// ReasonSignatureOnly: nothing but a signature (or nothing at all) was left.
	ReasonSignatureOnly Reason = "signature only"
	// ReasonQuotes: quoted lines were set aside and too little of the author's own
	// text remained to judge.
	ReasonQuotes Reason = "mostly quoted text"
	// ReasonTooShort: fewer than MinBodyLines lines to judge.
	ReasonTooShort Reason = "too short"
	// ReasonListMarkers: a line starts with a bullet or a number.
	ReasonListMarkers Reason = "list markers"
	// ReasonCodeIndent: a line is indented like code.
	ReasonCodeIndent Reason = "code-like indentation"
	// ReasonSingleLineParagraphs: too few paragraph lines to measure (every
	// paragraph is a single line, so there is nothing wrapped to rejoin).
	ReasonSingleLineParagraphs Reason = "single-line paragraphs"
	// ReasonMixedLengths: the candidate lines are not mostly in the width band.
	ReasonMixedLengths Reason = "mixed line lengths"
)

// Reasons lists every refusal reason in the order a report prints them.
var Reasons = []Reason{
	ReasonSignatureOnly, ReasonQuotes, ReasonTooShort, ReasonListMarkers,
	ReasonCodeIndent, ReasonSingleLineParagraphs, ReasonMixedLengths,
}

// Result is Analyze's verdict.
type Result struct {
	// Wrapped is true when the body looks hard-wrapped.
	Wrapped bool
	// Reason is why not, when !Wrapped.
	Reason Reason
	// Joins is the number of line breaks that would be replaced by a space.
	// Always 0 when !Wrapped.
	Joins int
}

// Analyze judges one plain-text body. text may use LF, CRLF or CR line ends.
func Analyze(text string) Result {
	lines := splitLines(text)

	// 1. Set the signature aside: everything from the first delimiter on.
	for i, l := range lines {
		if l == signatureDelimiter || l == "--" {
			lines = lines[:i]
			break
		}
	}
	// 2. Set quoted lines aside.
	var body []string
	quoted := 0
	for _, l := range lines {
		if strings.HasPrefix(strings.TrimLeft(l, " "), ">") {
			quoted++
			continue
		}
		body = append(body, l)
	}
	// Lines are measured without trailing whitespace.
	nonBlank := 0
	for i, l := range body {
		body[i] = strings.TrimRight(l, " \t")
		if body[i] != "" {
			nonBlank++
		}
	}
	switch {
	case nonBlank == 0 && quoted == 0:
		return Result{Reason: ReasonSignatureOnly}
	case nonBlank == 0:
		return Result{Reason: ReasonQuotes}
	case nonBlank < MinBodyLines:
		if quoted > 0 {
			return Result{Reason: ReasonQuotes}
		}
		return Result{Reason: ReasonTooShort}
	}

	// 3. Markers that mean the author laid the text out.
	for _, l := range body {
		if l != "" && isListMarker(l) {
			return Result{Reason: ReasonListMarkers}
		}
	}
	for _, l := range body {
		if l != "" && isCodeIndented(l) {
			return Result{Reason: ReasonCodeIndent}
		}
	}

	// 4. Paragraphs, wrap candidates, and the width band.
	candidates, inBand, joins := 0, 0, 0
	var para []string
	flush := func() {
		for i := 0; i+1 < len(para); i++ {
			candidates++
			if w := utf8.RuneCountInString(para[i]); w >= WrapMinCols && w <= WrapMaxCols {
				inBand++
				joins++
			}
		}
		para = para[:0]
	}
	for _, l := range body {
		if l == "" {
			flush()
			continue
		}
		para = append(para, l)
	}
	flush()

	if candidates < MinCandidates {
		return Result{Reason: ReasonSingleLineParagraphs}
	}
	if float64(inBand) < WrapFraction*float64(candidates) {
		return Result{Reason: ReasonMixedLengths}
	}
	return Result{Wrapped: true, Joins: joins}
}

func splitLines(text string) []string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	return strings.Split(text, "\n")
}

// isListMarker reports a bullet ("- ", "* ", "+ ", "• ") or an enumerator ("1.",
// "2)", "a)") at the start of a line, after any indentation.
func isListMarker(l string) bool {
	s := strings.TrimLeft(l, " \t")
	if s == "" {
		return false
	}
	for _, b := range []string{"- ", "* ", "+ ", "• ", "· ", "– "} {
		if strings.HasPrefix(s, b) {
			return true
		}
	}
	// 1. / 12) / a) followed by a space.
	i := 0
	for i < len(s) && i < 3 && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == 0 && len(s) > 0 && ((s[0] >= 'a' && s[0] <= 'z') || (s[0] >= 'A' && s[0] <= 'Z')) {
		i = 1
	}
	return i > 0 && i+1 < len(s) && (s[i] == '.' || s[i] == ')') && s[i+1] == ' '
}

// isCodeIndented reports a line indented by a tab or codeIndentSpaces spaces.
func isCodeIndented(l string) bool {
	if strings.HasPrefix(l, "\t") {
		return true
	}
	n := 0
	for n < len(l) && l[n] == ' ' {
		n++
	}
	return n >= codeIndentSpaces
}
