// SPDX-License-Identifier: AGPL-3.0-only

package hardwrap

import (
	"strings"
	"testing"
)

const prose = "The quarterly report is attached for your review and we would like your comments " +
	"before the end of the week so that the final version can go out on schedule to everyone " +
	"on the distribution list, including the people who were away during the last meeting " +
	"and who have asked to be kept informed of every change to the plan as it develops."

// wrap breaks text greedily at width columns, the way a mail program does.
func wrap(text string, width int) []string {
	var lines []string
	cur := ""
	for _, w := range strings.Fields(text) {
		if cur != "" && len(cur)+1+len(w) > width {
			lines = append(lines, cur)
			cur = w
			continue
		}
		if cur != "" {
			cur += " "
		}
		cur += w
	}
	if cur != "" {
		lines = append(lines, cur)
	}
	return lines
}

func twoParas(width int) string {
	return strings.Join(wrap(prose, width), "\n") + "\n\n" + strings.Join(wrap(prose, width), "\n") + "\n"
}

func TestAnalyze(t *testing.T) {
	wrapped72 := wrap(prose, 72)
	// Each paragraph has len(wrapped72) lines, so len-1 joins; two paragraphs.
	wantJoins := 2 * (len(wrapped72) - 1)

	cases := []struct {
		name string
		text string
		want Result
	}{
		{"hard-wrapped at 72", twoParas(72), Result{Wrapped: true, Joins: wantJoins}},
		{"hard-wrapped at 72 with CRLF", strings.ReplaceAll(twoParas(72), "\n", "\r\n"), Result{Wrapped: true, Joins: wantJoins}},
		{"the signature is set aside", twoParas(72) + "-- \nAlice Example\nExample Corp\n", Result{Wrapped: true, Joins: wantJoins}},
		{"a quoted block is set aside", "> quoted line one\n> quoted line two\n\n" + twoParas(72), Result{Wrapped: true, Joins: wantJoins}},
		{"a bulleted list", "Things to bring:\n- a coat that is warm enough for the evening walk by the river\n- a torch\n- a map of the area\n- some water\n", Result{Reason: ReasonListMarkers}},
		{"a numbered list", "Steps:\n1. open the lid of the box and remove the packing material\n2. plug it in\n3. wait\n4. press the button\n", Result{Reason: ReasonListMarkers}},
		{"code", "Run this:\n\n    for i in 1 2 3; do\n      echo $i\n    done\n\nand report back.\n", Result{Reason: ReasonCodeIndent}},
		{"a tab-indented block", "Output was:\n\n\tone\n\ttwo\n\tthree\n\tfour\n", Result{Reason: ReasonCodeIndent}},
		{"a short note", "Thanks, see you at 3.\n\nAlice\n", Result{Reason: ReasonTooShort}},
		{"all quoted", "> a\n> b\n> c\n> d\n> e\n", Result{Reason: ReasonQuotes}},
		{"a short reply over a long quote", "Agreed.\n\n> a\n> b\n> c\n> d\n", Result{Reason: ReasonQuotes}},
		{"a signature only", "-- \nAlice Example\nExample Corp\nPhone: 555\nWeb: example.com\n", Result{Reason: ReasonSignatureOnly}},
		{"empty", "", Result{Reason: ReasonSignatureOnly}},
		{"mixed line lengths", "Short line.\nAnother one here.\n" +
			"This one is a good deal longer than the other two lines were, running well on.\n" +
			"Tiny.\nAnd a medium length line for good measure.\nEnd.\n", Result{Reason: ReasonMixedLengths}},
		{"one long line per paragraph", strings.Join([]string{prose, prose, prose, prose}, "\n\n"), Result{Reason: ReasonSingleLineParagraphs}},
		{"wrapped at 40 is not the band", twoParas(40), Result{Reason: ReasonMixedLengths}},
	}
	for _, c := range cases {
		if got := Analyze(c.text); got != c.want {
			t.Errorf("%s: Analyze = %+v, want %+v", c.name, got, c.want)
		}
	}
}

// A short line in the middle of a paragraph is a deliberate break and is not joined.
func TestShortLineInsideAParagraphIsNotJoined(t *testing.T) {
	long := strings.Repeat("w", 70)
	text := strings.Join([]string{long, long, long, "short", long, long, "end"}, "\n") + "\n"
	got := Analyze(text)
	// Candidates: 6 non-final lines, 5 in the band (the 5-char one is not) = 83%.
	if !got.Wrapped || got.Joins != 5 {
		t.Errorf("Analyze = %+v, want wrapped with 5 joins", got)
	}
}

func TestThresholdsAreTheOnesTheReportPrints(t *testing.T) {
	if MinBodyLines != 4 || WrapMinCols != 60 || WrapMaxCols != 80 || WrapFraction != 0.8 || MinCandidates != 3 {
		t.Fatal("a threshold changed: update the report's documentation and fixtures deliberately")
	}
}
