// SPDX-License-Identifier: AGPL-3.0-only

package search

import (
	"reflect"
	"testing"
	"time"
)

// The parser is pure: no engine, no database, no filesystem. These tests pin the
// operator syntax users type (matrix §5) against the AST the backends compile.

func TestParseFreeText(t *testing.T) {
	q := Parse("invoice overdue", ParseOptions{})
	want := And{Nodes: []Node{
		TermNode(FieldAny, "invoice", MatchToken),
		TermNode(FieldAny, "overdue", MatchToken),
	}}
	if !reflect.DeepEqual(q.Root, Node(want)) {
		t.Fatalf("free text: got %#v, want %#v", q.Root, want)
	}
	if !q.Fuzzy {
		t.Error("typo tolerance should be on by default")
	}
}

func TestParseOperators(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want Node
	}{
		{"from", "from:Alice@Example.COM",
			Term{Field: FieldFrom, Value: "alice@example.com", Match: MatchExact, Boost: 1}},
		{"to", "to:bob@example.com",
			Term{Field: FieldTo, Value: "bob@example.com", Match: MatchExact, Boost: 1}},
		{"cc folds into to", "cc:bob@example.com",
			Term{Field: FieldTo, Value: "bob@example.com", Match: MatchExact, Boost: 1}},
		{"subject", "subject:invoice",
			Term{Field: FieldSubject, Value: "invoice", Match: MatchToken, Boost: 1}},
		{"body", "body:refund",
			Term{Field: FieldBody, Value: "refund", Match: MatchToken, Boost: 1}},
		{"label keeps the NAME for the resolver", "label:Work",
			Term{Field: FieldLabel, Value: "Work", Match: MatchExact, Boost: 1}},
		{"account keeps the NAME", "account:personal",
			Term{Field: FieldAccount, Value: "personal", Match: MatchExact, Boost: 1}},
		{"is:unread", "is:unread",
			Term{Field: FieldFlag, Value: FlagUnread, Match: MatchExact, Boost: 1}},
		{"is:flagged is a synonym of starred", "is:flagged",
			Term{Field: FieldFlag, Value: FlagStarred, Match: MatchExact, Boost: 1}},
		{"has:attachment", "has:attachment",
			Term{Field: FieldFlag, Value: FlagAttachment, Match: MatchExact, Boost: 1}},
		{"is:spam is a synonym of junk", "is:spam",
			Term{Field: FieldFlag, Value: FlagJunk, Match: MatchExact, Boost: 1}},
		{"phrase", `"quarterly revenue"`,
			Term{Field: FieldAny, Value: "quarterly revenue", Match: MatchPhrase, Boost: 1}},
		{"explicit prefix", "overd*",
			Term{Field: FieldAny, Value: "overd", Match: MatchPrefix, Boost: 1}},
		{"quoted operator value", `label:"Client Work"`,
			Term{Field: FieldLabel, Value: "Client Work", Match: MatchExact, Boost: 1}},
		{"subject phrase", `subject:"quarterly revenue"`,
			Term{Field: FieldSubject, Value: "quarterly revenue", Match: MatchPhrase, Boost: 1}},
		{"negation", "-is:read",
			Not{Node: Term{Field: FieldFlag, Value: FlagRead, Match: MatchExact, Boost: 1}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Parse(tc.in, ParseOptions{}).Root
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Parse(%q):\n got  %#v\n want %#v", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseDates(t *testing.T) {
	day := func(y int, m time.Month, d int) time.Time {
		return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	}
	tests := []struct {
		in   string
		want DateRange
	}{
		{"after:2026-01-01", DateRange{From: day(2026, 1, 1)}},
		{"before:2026-07-01", DateRange{To: day(2026, 7, 1)}},
		{"on:2026-07-12", DateRange{From: day(2026, 7, 12), To: day(2026, 7, 13)}},
		{"date:2026-07", DateRange{From: day(2026, 7, 1), To: day(2026, 8, 1)}},
		{"date:2026", DateRange{From: day(2026, 1, 1), To: day(2027, 1, 1)}},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got := Parse(tc.in, ParseOptions{}).Root
			if !reflect.DeepEqual(got, Node(tc.want)) {
				t.Fatalf("Parse(%q): got %#v, want %#v", tc.in, got, tc.want)
			}
		})
	}
}

// A colon does NOT make an operator. A pasted URL or Message-ID must stay
// searchable text — a search box that silently turns a paste into a bogus field
// query and returns nothing is worse than useless.
func TestParseNonOperatorColon(t *testing.T) {
	for _, in := range []string{"https://example.com/invoice", "notafield:value"} {
		got := Parse(in, ParseOptions{}).Root
		want := Term{Field: FieldAny, Value: in, Match: MatchToken, Boost: 1}
		if !reflect.DeepEqual(got, Node(want)) {
			t.Fatalf("Parse(%q): got %#v, want free text %#v", in, got, want)
		}
	}
}

// An unknown is:/has: value must match NOTHING, not silently drop the
// constraint (which would return the whole mailbox).
func TestParseUnknownFlagMatchesNothing(t *testing.T) {
	got := Parse("is:sparkly", ParseOptions{}).Root
	if _, ok := got.(None); !ok {
		t.Fatalf("is:sparkly: got %#v, want None", got)
	}
}

// As-you-type: the LAST word is a prefix while the user is still typing it.
func TestParsePrefixMode(t *testing.T) {
	q := Parse("invoice quart", ParseOptions{Prefix: true})
	and, ok := q.Root.(And)
	if !ok || len(and.Nodes) != 2 {
		t.Fatalf("got %#v", q.Root)
	}
	if first := and.Nodes[0].(Term); first.Match != MatchToken {
		t.Errorf("first term should be a whole word, got %v", first.Match)
	}
	if last := and.Nodes[1].(Term); last.Match != MatchPrefix || last.Value != "quart" {
		t.Errorf("last term should be a prefix, got %#v", last)
	}
}

func TestParseEmptyMatchesEverything(t *testing.T) {
	for _, in := range []string{"", "   ", "\t\n"} {
		if _, ok := Parse(in, ParseOptions{}).Root.(All); !ok {
			t.Fatalf("Parse(%q) should match everything", in)
		}
	}
}

func TestParseCombined(t *testing.T) {
	q := Parse(`from:alice@example.com is:unread -label:Noise "wire transfer"`, ParseOptions{})
	and, ok := q.Root.(And)
	if !ok {
		t.Fatalf("want And, got %#v", q.Root)
	}
	if len(and.Nodes) != 4 {
		t.Fatalf("want 4 clauses, got %d: %#v", len(and.Nodes), and.Nodes)
	}
	if _, ok := and.Nodes[2].(Not); !ok {
		t.Errorf("clause 3 should be a negation, got %#v", and.Nodes[2])
	}
	if ph := and.Nodes[3].(Term); ph.Match != MatchPhrase || ph.Value != "wire transfer" {
		t.Errorf("clause 4 should be the phrase, got %#v", ph)
	}
}
