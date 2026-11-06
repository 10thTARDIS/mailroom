// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/hardwrap"
)

const ptProse = "The quarterly report is attached for your review and we would like your comments " +
	"before the end of the week so that the final version can go out on schedule to everyone " +
	"on the distribution list, including the people who were away during the last meeting " +
	"and who have asked to be kept informed of every change to the plan as it develops."

func ptWrap(text string, width int) string {
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
	return strings.Join(append(lines, cur), "\r\n")
}

func ptMsg(contentType, body string) []byte {
	return []byte("From: a@example.com\r\nTo: b@example.com\r\nSubject: s\r\nMIME-Version: 1.0\r\n" +
		"Content-Type: " + contentType + "\r\n\r\n" + body + "\r\n")
}

// ptFixtures are synthetic: one message per category, with the exact outcome each
// must land in. The wrapped ones are built from ptProse, so each has a known
// number of line breaks to join.
func ptFixtures() []struct {
	name string
	raw  []byte
} {
	wrapped := ptWrap(ptProse, 72)
	joinsPerPara := strings.Count(wrapped, "\r\n") // 4 or so; every non-final line is in band
	_ = joinsPerPara
	return []struct {
		name string
		raw  []byte
	}{
		{"flowed", ptMsg(`text/plain; charset=utf-8; format=flowed`, ptWrap(ptProse, 72)+"\r\n\r\n"+ptWrap(ptProse, 72))},
		{"wrapped at 72 (A)", ptMsg("text/plain; charset=utf-8", wrapped+"\r\n\r\n"+wrapped)},
		{"wrapped at 72 (B, one paragraph)", ptMsg("text/plain; charset=utf-8", wrapped)},
		{"list", ptMsg("text/plain", "Bring:\r\n- a warm coat for the long walk along the river this evening\r\n- a torch\r\n- a map\r\n- water\r\n")},
		{"code", ptMsg("text/plain", "Run:\r\n\r\n    make all\r\n    make test\r\n    make install\r\n\r\nThen report back to me.\r\n")},
		{"quotes", ptMsg("text/plain", "Agreed.\r\n\r\n> a\r\n> b\r\n> c\r\n> d\r\n")},
		{"short note", ptMsg("text/plain", "Thanks, see you at 3.\r\n\r\nAlice\r\n")},
		{"mixed", ptMsg("text/plain", "Short line.\r\nAnother one here.\r\n"+
			"This one is a good deal longer than the other two lines were, running well on.\r\n"+
			"Tiny.\r\nAnd a medium length line for good measure.\r\nEnd.\r\n")},
		{"signature only", ptMsg("text/plain", "-- \r\nAlice Example\r\nExample Corp\r\nPhone: 555\r\nWeb: example.com\r\n")},
		{"html", ptMsg("text/html", "<p>"+ptProse+"</p>")},
		{"not mime at all", []byte{}},
	}
}

func TestPlaintextReportCountsEachCategoryExactly(t *testing.T) {
	a := newPtAgg()
	for i, f := range ptFixtures() {
		a.add(ulid.ULID{byte(i + 1)}, f.raw)
	}
	wrapped := ptWrap(ptProse, 72)
	joinsA := strings.Count(wrapped, "\r\n")
	if joinsA < 3 {
		t.Fatalf("fixture too short to be a test: %d breaks", joinsA)
	}

	want := map[string]int{
		"scanned": 11 /* the empty one is a message with no text */, "unreadable": 0,
		"notPlain": 2 /* html, empty */, "plain": 9,
		"flowed": 1, "unmarked": 8, "looksWrap": 2, "wouldChange": 2, "joins": 3*joinsA,
	}
	got := map[string]int{
		"scanned": a.scanned, "unreadable": a.unreadable, "notPlain": a.notPlain, "plain": a.plain,
		"flowed": a.flowed, "unmarked": a.unmarked, "looksWrap": a.looksWrap, "wouldChange": a.wouldChange, "joins": a.joins,
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s = %d, want %d", k, got[k], w)
		}
	}
	wantReasons := map[hardwrap.Reason]int{
		hardwrap.ReasonListMarkers: 1, hardwrap.ReasonCodeIndent: 1, hardwrap.ReasonQuotes: 1,
		hardwrap.ReasonTooShort: 1, hardwrap.ReasonMixedLengths: 1, hardwrap.ReasonSignatureOnly: 1,
	}
	for _, r := range hardwrap.Reasons {
		if a.reasons[r] != wantReasons[r] {
			t.Errorf("reason %q = %d, want %d", r, a.reasons[r], wantReasons[r])
		}
	}
	// Every unmarked message is accounted for exactly once.
	sum := a.looksWrap
	for _, v := range a.reasons {
		sum += v
	}
	if sum != a.unmarked {
		t.Errorf("looks-wrapped + refusals = %d, want unmarked = %d", sum, a.unmarked)
	}
}

func TestPlaintextReportPrintsBoundsAndParametersAndNoContent(t *testing.T) {
	a := newPtAgg()
	for i, f := range ptFixtures() {
		a.add(ulid.ULID{byte(i + 1)}, f.raw)
	}
	var buf bytes.Buffer
	a.print(&buf, 0)
	out := buf.String()
	for _, want := range []string{
		"shown as text/plain:        9",
		"format=flowed:            1",
		"look hard-wrapped:      2",
		"would change:           2",
		"between 60 and 80 characters",
		"the counts cover every message of the user.",
		"sample of would-change messages (hashed ids):",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	// NEVER MESSAGE CONTENT: nothing from any fixture body, subject or address.
	for _, leak := range []string{"quarterly", "distribution", "Alice", "a@example.com", "Subject", "torch"} {
		if strings.Contains(out, leak) {
			t.Errorf("output leaks %q:\n%s", leak, out)
		}
	}

	// The --limit bound is reported, and only when it cut the scan short.
	a.limited = true
	buf.Reset()
	a.print(&buf, 5)
	if !strings.Contains(buf.String(), "LOWER BOUND") || !strings.Contains(buf.String(), "--limit 5") {
		t.Errorf("a cut-short scan does not report its bound:\n%s", buf.String())
	}
	a.limited = false
	buf.Reset()
	a.print(&buf, 5)
	if strings.Contains(buf.String(), "LOWER BOUND") || !strings.Contains(buf.String(), "was not reached") {
		t.Errorf("an uncut scan claims a bound:\n%s", buf.String())
	}
}

func TestPlaintextReportHashedSamplesAreNotTheIDs(t *testing.T) {
	id := ulid.ULID{1, 2, 3}
	if h := ptHashID(id); len(h) != 12 || strings.Contains(id.String(), h) || h == ptHashID(ulid.ULID{1, 2, 4}) {
		t.Errorf("ptHashID(%s) = %q: want 12 hex characters, unrelated to the id and distinct per id", id, h)
	}
}
