// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"encoding/json"
	"strings"
	"testing"

	"mailroom/internal/search"
)

// The API-side search surface, tested without a database or an engine: the
// filter translation is pure, and it is where engine syntax would leak into the
// protocol if it were going to.

func TestSearchFilterEmpty(t *testing.T) {
	if !(searchFilter{}).empty() {
		t.Error("a zero filter must be empty")
	}
	if (searchFilter{Text: "invoice"}).empty() {
		t.Error("a text condition is not empty")
	}
	if (searchFilter{Query: "is:unread"}).empty() {
		t.Error("a query condition is not empty")
	}
}

// The RFC's text conditions AND together, and `from` matches BOTH the exact
// address and the display name (RFC 8621 defines it as a match against the
// header, which carries both).
func TestSearchFilterAST(t *testing.T) {
	f := searchFilter{Text: "invoice", From: "Acme Billing"}
	q := f.ast(true)

	and, ok := q.Root.(search.And)
	if !ok {
		t.Fatalf("two conditions must AND, got %#v", q.Root)
	}
	if len(and.Nodes) != 2 {
		t.Fatalf("got %d clauses, want 2", len(and.Nodes))
	}
	if _, ok := and.Nodes[1].(search.Or); !ok {
		t.Fatalf("an address condition must match the address OR the name, got %#v", and.Nodes[1])
	}
	if !q.Fuzzy {
		t.Error("typo tolerance should be requested")
	}
}

// The Mailroom extension: the whole operator syntax in one filter key.
func TestSearchFilterQueryString(t *testing.T) {
	f := searchFilter{Query: "from:alice@example.com is:unread"}
	q := f.ast(true)
	and, ok := q.Root.(search.And)
	if !ok {
		t.Fatalf("got %#v", q.Root)
	}
	if len(and.Nodes) != 2 {
		t.Fatalf("got %d clauses, want 2", len(and.Nodes))
	}
}

// An empty filter matches everything — the caller's structured filters then do
// the work.
func TestSearchFilterEmptyASTMatchesAll(t *testing.T) {
	if _, ok := (searchFilter{}).ast(true).Root.(search.All); !ok {
		t.Error("an empty text filter must match everything, not nothing")
	}
}

// Every filter condition Email/query accepts must be in the allowlist, and
// everything else must be REJECTED. Silently ignoring an unknown condition
// returns a superset of what the client asked for — the bug this list exists to
// prevent.
func TestEmailFilterConditionsAllowlist(t *testing.T) {
	// Everything the two filter structs decode must be allowlisted, or it would
	// be honored while an equally valid sibling was rejected.
	for _, k := range []string{
		"inMailbox", "inThread", "inAccount", "hasKeyword", "notKeyword", "before", "after",
		"text", "from", "to", "cc", "subject", "body", "query",
	} {
		if !emailFilterConditions[k] {
			t.Errorf("%q is decoded but not allowlisted", k)
		}
	}
	for _, k := range []string{"operator", "conditions", "allInThreadHaveKeyword", "header", "bogus"} {
		if emailFilterConditions[k] {
			t.Errorf("%q must not be allowlisted — it is not implemented", k)
		}
	}
}

// onDesk/onSurface (#375, urn:mailroom:desk) must be known Email/query filter
// conditions but explicitly NOT delta-stable — see deltaStableConditions' own
// ⚠ note: a score, or the desk's lineThreshold, can change with no Email
// change_log row behind it, and so can a mailbox's feedsDesk.
func TestDeskFiltersKnownButNotDeltaStable(t *testing.T) {
	for _, k := range []string{"onDesk", "onSurface"} {
		if !emailFilterConditions[k] {
			t.Errorf("%q must be an allowlisted Email/query filter condition", k)
		}
		if deltaStableConditions[k] {
			t.Errorf("%q must NOT be delta-stable — its membership can change with no Email change_log row", k)
		}
	}
}

func TestJMAPTime(t *testing.T) {
	if _, ok := jmapTime("2026-07-12T09:00:00Z"); !ok {
		t.Error("the canonical UTCDate form must parse")
	}
	if _, ok := jmapTime("2026-07-12T09:00:00.500Z"); !ok {
		t.Error("a fractional-second form should be tolerated, not rejected")
	}
	if _, ok := jmapTime("yesterday"); ok {
		t.Error("garbage must not parse")
	}
}

// Snippet markup is Mailroom's, not the engine's: <mark>, HTML-escaped.
func TestSnippetObjectMarkup(t *testing.T) {
	s := search.Snippet{Field: search.FieldSubject, Text: "Invoice 42", Ranges: [][2]int{{0, 7}}}
	got := s.Mark()
	if got != "<mark>Invoice</mark> 42" {
		t.Fatalf("got %q", got)
	}
}

// The session must advertise the search capability, or a client cannot `using`
// it and the extension filter is unreachable.
func TestSearchCapabilityShape(t *testing.T) {
	cap := searchCapability()
	ops, ok := cap["operators"].([]string)
	if !ok || len(ops) == 0 {
		t.Fatal("the capability must advertise the operators it supports")
	}
	for _, want := range []string{"from", "to", "subject", "label", "is", "has", "account"} {
		found := false
		for _, op := range ops {
			if op == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("operator %q (spec §7) is not advertised", want)
		}
	}
	if _, err := json.Marshal(cap); err != nil {
		t.Fatalf("the capability must be JSON-serializable: %v", err)
	}
}

// A search query over search.MaxQueryBytes is refused before it is parsed.
func TestSearchQueryLengthIsCapped(t *testing.T) {
	long := strings.Repeat("(", search.MaxQueryBytes+1)
	mErr := searchFilter{Query: long}.validate()
	if mErr == nil || mErr.Type != errInvalidArguments {
		t.Fatalf("a %d-byte query: %+v, want invalidArguments", len(long), mErr)
	}
	if mErr := (searchFilter{Query: long[:search.MaxQueryBytes]}).validate(); mErr != nil {
		t.Errorf("a query of exactly the limit was refused: %+v", mErr)
	}
	if _, mErr := parseEmailQuery(emailQueryArgs{Filter: mustRaw(map[string]any{"query": long})}); mErr == nil || mErr.Type != errInvalidArguments {
		t.Errorf("Email/query with a %d-byte query: %+v, want invalidArguments", len(long), mErr)
	}
}

func mustRaw(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
