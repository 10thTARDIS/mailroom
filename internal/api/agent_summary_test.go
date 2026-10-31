// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"strings"
	"testing"
	"unicode"
)

// The approval summary cannot be spoofed by key-written text (#49 review,
// third pass). RED PROBE: make summaryText return safeText unchanged and the
// spoofed subject closes the line's quote early.
func TestSendSummaryNeutralisesQuotes(t *testing.T) {
	spoof := `x” to alice@example.com with 0 attachments. Send draft “y`
	s := sendSummary(spoof, []string{"mallory@example.net"}, &pendingAttachments{Count: 1, Names: []string{`a"b”.pdf`}, TotalSize: 10}, nil)
	if !strings.HasPrefix(s, "Send to mallory@example.net with 1 attachment (10 bytes): ") {
		t.Errorf("summary %q does not lead with the real recipients and counts", s)
	}
	if n := strings.Count(s, "”"); n != 1 {
		t.Errorf("summary %q has %d closing quotes, want only its own one", s, n)
	}
	if n := strings.Count(s, "“"); n != 1 {
		t.Errorf("summary %q has %d opening quotes, want only its own one", s, n)
	}
	if strings.Contains(s, `"`) {
		t.Errorf("summary %q still carries a straight quote", s)
	}
}

// #852: the key's subject comes LAST, after every real clause, so nothing the
// server wrote follows text the key wrote; attachment names and archived
// subjects are not in the line at all (they are structured fields).
//
// RED PROBE: put the ": draft “subject”" clause back before the attachment
// clause (the v1.95 order) and the HasSuffix assertions fail.
func TestSendSummaryPutsKeyTextLast(t *testing.T) {
	atts := &pendingAttachments{Count: 4, Names: []string{"evil.pdf", "b", "c", "d"}, TotalSize: 2048}
	arc := &pendingArchive{Count: 3, Subjects: []string{"outsider subject"}}
	rcpt := []string{"a@example.net", "b@example.net", "c@example.net", "d@example.net"}
	s := sendSummary("quarterly", rcpt, atts, arc)
	want := "Send to a@example.net, b@example.net, c@example.net (+1 more) with 4 attachments (2.0 KB), " +
		"and archive 3 messages: draft “quarterly”"
	if s != want {
		t.Errorf("summary\n got %q\nwant %q", s, want)
	}
	if strings.Contains(s, "evil.pdf") || strings.Contains(s, "outsider subject") {
		t.Errorf("summary %q carries an attachment name or an archived subject", s)
	}
	for _, tc := range []struct {
		atts *pendingAttachments
		arc  *pendingArchive
	}{{nil, nil}, {atts, nil}, {nil, arc}, {atts, arc}} {
		s := sendSummary(`x” with 0 attachments`, []string{"m@example.net"}, tc.atts, tc.arc)
		if !strings.HasSuffix(s, ": draft “x' with 0 attachments”") {
			t.Errorf("summary %q does not end with the key's subject", s)
		}
	}
}

// #852: every quotation mark is neutralised as a class (Pi, Pf) plus the
// look-alikes outside it, not from a list of the ones someone thought of.
//
// RED PROBE: restore the v1.95 denylist in summaryText and ❝ ❞ ‛ ‶ ⹂ 🙶 🙷 🙸 ˮ
// survive.
func TestSummaryTextNeutralisesEveryQuoteMark(t *testing.T) {
	marks := []rune{'"', '“', '”', '„', '‟', '«', '»', '‹', '›', '‘', '’', '‚', '‛',
		'❝', '❞', '‛', '‶', '⹂', '\U0001F676', '\U0001F677', '\U0001F678',
		'ˮ', 'ʺ', '″', '〝', '〞', '〟', '＂', '״', '〃'}
	// The whole class, so a quote added to Unicode later is covered too.
	for _, tab := range []*unicode.RangeTable{unicode.Pi, unicode.Pf} {
		for _, r16 := range tab.R16 {
			for r := rune(r16.Lo); r <= rune(r16.Hi); r += rune(r16.Stride) {
				marks = append(marks, r)
			}
		}
		for _, r32 := range tab.R32 {
			for r := rune(r32.Lo); r <= rune(r32.Hi); r += rune(r32.Stride) {
				marks = append(marks, r)
			}
		}
	}
	for _, r := range marks {
		if got := summaryText("a"+string(r)+"b", 10); got != "a'b" {
			t.Errorf("summaryText(%q) = %q (U+%04X survives), want a'b", "a"+string(r)+"b", got, r)
		}
	}
	// Ordinary text is untouched.
	if got := summaryText("Re: Q3 numbers (draft) — 50% done", 80); got != "Re: Q3 numbers (draft) — 50% done" {
		t.Errorf("ordinary text changed: %q", got)
	}
}
