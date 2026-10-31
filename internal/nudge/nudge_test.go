// SPDX-License-Identifier: AGPL-3.0-only

package nudge

import (
	"strings"
	"testing"

	"mailroom/internal/kind"
)

func TestMeIs(t *testing.T) {
	me := NewMe([]string{" Me@Example.com "}, []string{"alias.example.net"})
	for addr, want := range map[string]bool{
		"me@example.com":             true,
		"ME@EXAMPLE.COM":             true,
		"me+lists@example.com":       true, // RFC 5233 subaddress of a fixed identity
		"anything@alias.example.net": true,
		"you@example.com":            false,
		"me@other.example":           false,
		"+x@example.com":             false,
		"":                           false,
		"not-an-address":             false,
	} {
		if got := me.Is(addr); got != want {
			t.Errorf("Is(%q) = %v, want %v", addr, got, want)
		}
	}
	if !NewMe(nil, nil).Empty() || me.Empty() {
		t.Error("Empty() wrong")
	}
}

func TestAsks(t *testing.T) {
	cases := []struct {
		name, subject, body string
		q, r                bool
	}{
		{"plain statement", "Lunch", "Thanks for lunch.\n", false, false},
		{"question in body", "Lunch", "Are we still on for Tuesday?\n", true, false},
		{"question in subject", "Tuesday?", "", true, false},
		{"request phrasing", "Docs", "Could you send the file when you get a chance.\n", false, true},
		{"let me know", "Docs", "Let me know.\n", false, true},
		{"url query string is not a question", "Link", "See https://example.com/a?b=c for details.\n", false, false},
		{"quoted question is not this message's", "Re: Lunch", "Sounds good.\n> Are we still on?\n", false, false},
		{"attribution cuts the quote", "Re: Lunch", "Sounds good.\nOn Mon, 1 Jan 2026, A <a@example.com> wrote:\nAre we still on?\n", false, false},
		{"signature cut", "Hi", "Hello.\n-- \nCan you believe it?\n", false, false},
		{"outlook header cuts", "Re: x", "Done.\n-----Original Message-----\nCould you check?\n", false, false},
	}
	for _, c := range cases {
		q, r := Asks(c.subject, c.body)
		if q != c.q || r != c.r {
			t.Errorf("%s: Asks = (%v,%v), want (%v,%v)", c.name, q, r, c.q, c.r)
		}
	}
}

func TestHTMLTextDropsQuotes(t *testing.T) {
	in := `<html><head><style>p{}</style></head><body><div>Sounds good.</div>` +
		`<div class="gmail_quote">On Monday A wrote:<blockquote>Are we still on?</blockquote></div></body></html>`
	got := HTMLText(in)
	if !strings.Contains(got, "Sounds good.") {
		t.Fatalf("new text lost: %q", got)
	}
	if strings.Contains(got, "?") || strings.Contains(got, "p{}") {
		t.Fatalf("quote or style leaked: %q", got)
	}
	ol := `<div>Done.</div><div id="divRplyFwdMsg">From: A</div><div>Could you check?</div>`
	if got := HTMLText(ol); strings.Contains(got, "Could you") {
		t.Fatalf("Outlook reply marker did not end the new text: %q", got)
	}
	if q, _ := Asks("", HTMLText(`<p>Free on Friday?</p>`)); !q {
		t.Fatal("question in HTML-only body not found")
	}
}

func TestBulkAndAutomated(t *testing.T) {
	if IsBulk(kind.Facts{}) || HasListID(kind.Facts{}) || IsAutomated(kind.Facts{}) {
		t.Fatal("empty facts must be none of bulk/list/automated")
	}
	for _, f := range []kind.Facts{
		{ListUnsubscribe: "<mailto:u@example.com>"},
		{ListUnsubscribePost: "List-Unsubscribe=One-Click"},
		{FeedbackID: "a:b:c"},
		{Precedence: " Bulk "},
		{Precedence: "list"},
	} {
		if !IsBulk(f) {
			t.Errorf("IsBulk(%+v) = false", f)
		}
	}
	if IsBulk(kind.Facts{Precedence: "first-class"}) {
		t.Error("Precedence first-class is not bulk")
	}
	if !IsAutomated(kind.Facts{AutoSubmitted: "auto-replied; owner=x"}) || IsAutomated(kind.Facts{AutoSubmitted: "No"}) {
		t.Error("IsAutomated wrong")
	}
}

func TestKindNotMachine(t *testing.T) {
	stranger := kind.Classify(kind.Facts{FromAddress: "a@example.org", FromDomain: "example.org", Subject: "hello"})
	if KindPersonal(stranger) {
		t.Fatalf("a stranger's plain first message decided personal (%v); the variant split assumes it does not", stranger.Kind)
	}
	if !KindNotMachine(stranger) {
		t.Fatalf("a stranger's plain first message is machine mail? kind=%v leader=%v", stranger.Kind, stranger.Leader)
	}
	known := kind.Classify(kind.Facts{FromAddress: "a@example.org", FromDomain: "example.org", HasCorresponded: true})
	if !KindPersonal(known) || !KindNotMachine(known) {
		t.Fatalf("corresponded sender not personal: %v", known.Kind)
	}
	bulk := kind.Classify(kind.Facts{FromAddress: "news@example.org", FromDomain: "example.org",
		ListUnsubscribe: "<mailto:u@example.org>", ListUnsubscribePost: "List-Unsubscribe=One-Click"})
	if KindNotMachine(bulk) {
		t.Fatalf("one-click bulk mail read as not-machine: %v/%v", bulk.Kind, bulk.Leader)
	}
}
