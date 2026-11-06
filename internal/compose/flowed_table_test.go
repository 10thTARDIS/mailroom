// SPDX-License-Identifier: AGPL-3.0-only

package compose

// An aligned plain-text table (web/src/app/ui/editor/plainText.ts, ADR-051 §3)
// goes out as format=flowed (#93). The question is whether flowing rewraps it.
//
// Findings, held by the tests below:
//   - a row of up to flowWidth characters is not touched: it leaves byte for byte
//     as written (a row that starts with a space, because its first cell is empty,
//     is space-stuffed by one space, which every flowed reader removes);
//   - a row longer than that IS soft-wrapped at its spaces, as any long line is.
//     The wrap loses nothing (a flowed reader joins the lines and gets the row
//     back exactly), but a reader that does not understand format=flowed sees the
//     row broken. That is accepted: the plain part of a message with a wide table
//     is the fallback, the HTML part is what carries the table, and a flowed
//     reader re-wraps to its own width in any case. The column cap in plainText.ts
//     (40 characters) is what keeps a typical row short.
//
// The table text is NOT written by hand where it can be avoided: the fixture the
// web round-trip test writes carries the plain text of every case it builds.

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"unicode/utf8"
)

const editorFixture = "../../web/src/app/ui/editor/testdata/sanitizer-roundtrip.json"

func tableTexts(t *testing.T) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(editorFixture)
	if err != nil {
		t.Fatalf("reading the editor fixture (regenerate with `npx vitest run src/app/ui/editor/roundTrip.test.tsx -u` in web/): %v", err)
	}
	var cases []struct {
		Name string `json:"name"`
		HTML string `json:"html"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, c := range cases {
		if strings.Contains(c.HTML, "<table") {
			out[c.Name] = c.Text
		}
	}
	if len(out) < 5 {
		t.Fatalf("the fixture holds %d table cases; expected at least 5", len(out))
	}
	return out
}

func TestFlowedKeepsAlignedTablesUntouched(t *testing.T) {
	for name, text := range tableTexts(t) {
		t.Run(name, func(t *testing.T) {
			wire := flowText(text)
			if got := unflowRFC3676(wire); got != strings.ReplaceAll(text, "\r\n", "\n") {
				t.Fatalf("a flowed reader does not get the table back\n  want: %q\n  got:  %q", text, got)
			}
			for _, line := range strings.Split(text, "\n") {
				if utf8.RuneCountInString(line) > flowWidth {
					t.Fatalf("the fixture's table row is wider than a flowed line (%d): use a narrower case", utf8.RuneCountInString(line))
				}
			}
			// Every row fits: no soft break was added anywhere.
			for _, l := range strings.Split(strings.TrimSuffix(wire, "\r\n"), "\r\n") {
				if strings.HasSuffix(l, " ") {
					t.Errorf("a short table row was soft-wrapped or left a trailing space: %q", l)
				}
			}
			// And the rows are the rows (space-stuffing aside).
			var got []string
			for _, l := range strings.Split(strings.TrimSuffix(wire, "\r\n"), "\r\n") {
				got = append(got, strings.TrimPrefix(l, " "))
			}
			var want []string
			for _, l := range strings.Split(text, "\n") {
				want = append(want, strings.TrimPrefix(l, " "))
			}
			if strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Errorf("flowing changed the table\n  want: %q\n  got:  %q", want, got)
			}
		})
	}
}

func TestFlowedWideTableRowIsSoftWrappedAndLossless(t *testing.T) {
	// Three columns at the 40-character cap: a 124-character row.
	cell := strings.Repeat("word ", 7) + "end"
	row := cell + strings.Repeat(" ", 40-len(cell)) + "  " + cell + strings.Repeat(" ", 40-len(cell)) + "  " + cell
	text := "Heading\n" + row + "\nlast"
	wire := flowText(text)
	lines := strings.Split(strings.TrimSuffix(wire, "\r\n"), "\r\n")
	if len(lines) <= 3 {
		t.Fatalf("control: a %d-character row was expected to be soft-wrapped, got %d lines: %q", utf8.RuneCountInString(row), len(lines), lines)
	}
	soft := 0
	for _, l := range lines {
		if strings.HasSuffix(l, " ") {
			soft++
		}
	}
	if soft == 0 {
		t.Fatal("control: a wrapped row must carry soft breaks")
	}
	if got := unflowRFC3676(wire); got != text {
		t.Fatalf("a flowed reader does not get the wide row back\n  want: %q\n  got:  %q", text, got)
	}
}

func TestFlowedTableOracleCanFail(t *testing.T) {
	// The reader above is not a pass-through: a text it is NOT given back differs.
	wire := flowText("a  b\nc  d")
	if unflowRFC3676(wire) == "a  b\nc  X" {
		t.Fatal("the unflow oracle returned something it was not given")
	}
	// An empty first cell (the row starts with spaces) is space-stuffed and restored.
	text := "  x\nlong  y"
	w := flowText(text)
	if !strings.HasPrefix(w, "   x\r\n") {
		t.Fatalf("control: a row starting with spaces should be space-stuffed, got %q", w)
	}
	if unflowRFC3676(w) != text {
		t.Fatalf("stuffing was not undone: %q", unflowRFC3676(w))
	}
}
