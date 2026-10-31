// SPDX-License-Identifier: AGPL-3.0-only

package rules

import (
	"testing"

	"github.com/oklog/ulid/v2"
)

func facts() MessageFacts {
	return MessageFacts{
		From:            "Alice@Example.com",
		FromDomain:      "example.com",
		To:              []string{"bob@example.net", "carol@example.org"},
		Subject:         "Your July Invoice",
		Alias:           "sales@mycompany.test",
		ListID:          "<list.example.com>",
		ListUnsubscribe: "<mailto:unsub@example.com>",
		Headers: map[string]string{
			"x-mailer":     "AcmeMailer 1.0",
			"x-spam-score": "9.5",
			"precedence":   "bulk",
		},
	}
}

func leaf(field, op, value string) Condition {
	return Condition{Field: field, Op: op, Value: value}
}

func TestLeafConditions(t *testing.T) {
	f := facts()
	tests := []struct {
		name string
		c    Condition
		want bool
	}{
		{"from equals case-insensitive", leaf(FieldFrom, OpEquals, "alice@example.com"), true},
		{"from equals mismatch", leaf(FieldFrom, OpEquals, "eve@example.com"), false},
		{"from contains", leaf(FieldFrom, OpContains, "alice@"), true},
		{"fromDomain equals", leaf(FieldFromDomain, OpEquals, "EXAMPLE.COM"), true},
		{"fromDomain mismatch", leaf(FieldFromDomain, OpEquals, "example.net"), false},
		{"to any recipient equals", leaf(FieldTo, OpEquals, "carol@example.org"), true},
		{"to no recipient equals", leaf(FieldTo, OpEquals, "dave@example.io"), false},
		{"to contains", leaf(FieldTo, OpContains, "@example.net"), true},
		{"subject contains ci", leaf(FieldSubject, OpContains, "invoice"), true},
		{"subject contains miss", leaf(FieldSubject, OpContains, "receipt"), false},
		{"alias equals ci", leaf(FieldAlias, OpEquals, "SALES@mycompany.test"), true},
		{"listId contains", leaf(FieldListID, OpContains, "list.example.com"), true},
		{"listUnsubscribe exists", Condition{Field: FieldListUnsub, Op: OpExists}, true},
		{"header contains ci key+value", Condition{Field: FieldHeader, Header: "X-Mailer", Op: OpContains, Value: "acmemailer"}, true},
		{"header exists", Condition{Field: FieldHeader, Header: "Precedence", Op: OpExists}, true},
		{"header absent exists false", Condition{Field: FieldHeader, Header: "X-Nope", Op: OpExists}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.c.match(f); got != tc.want {
				t.Errorf("match = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestListUnsubscribeMissing(t *testing.T) {
	f := facts()
	f.ListUnsubscribe = ""
	if (Condition{Field: FieldListUnsub, Op: OpExists}).match(f) {
		t.Error("listUnsubscribe exists should be false when absent")
	}
}

func TestBooleanTree(t *testing.T) {
	f := facts()

	allTrue := Condition{All: []Condition{
		leaf(FieldFromDomain, OpEquals, "example.com"),
		leaf(FieldSubject, OpContains, "invoice"),
	}}
	if !allTrue.match(f) {
		t.Error("all(true,true) should match")
	}

	allOneFalse := Condition{All: []Condition{
		leaf(FieldFromDomain, OpEquals, "example.com"),
		leaf(FieldSubject, OpContains, "receipt"),
	}}
	if allOneFalse.match(f) {
		t.Error("all(true,false) should not match")
	}

	anyOneTrue := Condition{Any: []Condition{
		leaf(FieldSubject, OpContains, "receipt"),
		leaf(FieldFrom, OpEquals, "alice@example.com"),
	}}
	if !anyOneTrue.match(f) {
		t.Error("any(false,true) should match")
	}

	nested := Condition{All: []Condition{
		{Any: []Condition{
			leaf(FieldFrom, OpEquals, "eve@example.com"),
			leaf(FieldFrom, OpEquals, "alice@example.com"),
		}},
		leaf(FieldFromDomain, OpEquals, "example.com"),
	}}
	if !nested.match(f) {
		t.Error("nested all(any(...),leaf) should match")
	}

	// An empty/degenerate condition matches nothing (safe default).
	if (Condition{}).match(f) {
		t.Error("empty condition must match nothing (safe default)")
	}
}

func TestRegexMatchGoodAndBad(t *testing.T) {
	f := facts()
	if !leaf(FieldSubject, OpMatches, `(?i)^your .* invoice$`).match(f) {
		t.Error("good regex should match subject")
	}
	// A bad (uncompilable) pattern must be treated as NON-MATCHING, never panic.
	if leaf(FieldSubject, OpMatches, `([unclosed`).match(f) {
		t.Error("bad regex should be non-matching")
	}
	if !leaf(FieldFrom, OpMatches, `(?i)alice@`).match(f) {
		t.Error("regex on from should match")
	}
	if !leaf(FieldTo, OpMatches, `@example\.(net|org)$`).match(f) {
		t.Error("regex on to should match a recipient")
	}
}

func TestEvaluatePriorityOrderAndAllApply(t *testing.T) {
	f := facts()
	rs := []Rule{
		{
			ID: ulid.Make(), Priority: 10, Enabled: true,
			Conditions: leaf(FieldFromDomain, OpEquals, "example.com"),
			Actions:    []Action{{Type: ActionAddLabel, Label: "L1"}},
		},
		{
			ID: ulid.Make(), Priority: 20, Enabled: true,
			Conditions: leaf(FieldSubject, OpContains, "invoice"),
			Actions:    []Action{{Type: ActionMarkPriority}, {Type: ActionAddLabel, Label: "L2"}},
		},
		{
			ID: ulid.Make(), Priority: 30, Enabled: true,
			Conditions: leaf(FieldSubject, OpContains, "no-such"),
			Actions:    []Action{{Type: ActionAddLabel, Label: "L3"}},
		},
		{
			ID: ulid.Make(), Priority: 40, Enabled: false, // disabled: never applies
			Conditions: leaf(FieldFrom, OpEquals, "alice@example.com"),
			Actions:    []Action{{Type: ActionAddLabel, Label: "L4"}},
		},
	}
	got := Evaluate(rs, f)
	want := []Action{
		{Type: ActionAddLabel, Label: "L1"},
		{Type: ActionMarkPriority},
		{Type: ActionAddLabel, Label: "L2"},
	}
	if len(got) != len(want) {
		t.Fatalf("Evaluate returned %d actions, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("action[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestEvaluateStopHalts(t *testing.T) {
	f := facts()
	rs := []Rule{
		{
			Priority: 10, Enabled: true,
			Conditions: leaf(FieldFromDomain, OpEquals, "example.com"),
			Actions:    []Action{{Type: ActionAddLabel, Label: "L1"}, {Type: ActionStop}},
		},
		{
			Priority: 20, Enabled: true,
			Conditions: leaf(FieldSubject, OpContains, "invoice"),
			Actions:    []Action{{Type: ActionAddLabel, Label: "L2"}},
		},
	}
	got := Evaluate(rs, f)
	if len(got) != 1 || got[0].Label != "L1" {
		t.Errorf("stop should halt after rule 1: got %+v", got)
	}
}

func TestValidateConditionsGood(t *testing.T) {
	good := []Condition{
		leaf(FieldFrom, OpEquals, "a@b.com"),
		leaf(FieldSubject, OpMatches, `^\[ANN\]`),
		{Field: FieldListUnsub, Op: OpExists},
		{Field: FieldHeader, Header: "X-Foo", Op: OpContains, Value: "bar"},
		{All: []Condition{leaf(FieldFrom, OpContains, "a@"), {Any: []Condition{leaf(FieldSubject, OpContains, "x")}}}},
	}
	for i, c := range good {
		if err := ValidateConditions(c); err != nil {
			t.Errorf("good condition %d rejected: %v", i, err)
		}
	}
}

func TestValidateConditionsBad(t *testing.T) {
	bad := []Condition{
		{Field: "nope", Op: OpEquals, Value: "x"},                 // unknown field
		{Field: FieldFrom, Op: "startsWith", Value: "x"},          // unknown op
		{Field: FieldFrom, Op: OpEquals},                          // value required
		{Field: FieldHeader, Op: OpExists},                        // header name required
		{Field: FieldSubject, Op: OpMatches, Value: "([unclosed"}, // uncompilable regex
		{}, // neither branch nor leaf
		{All: []Condition{leaf(FieldFrom, OpExists, "")}, Any: []Condition{leaf(FieldFrom, OpExists, "")}}, // both all+any
	}
	for i, c := range bad {
		if err := ValidateConditions(c); err == nil {
			t.Errorf("bad condition %d accepted, want error", i)
		}
	}
}

func TestValidateActions(t *testing.T) {
	if err := ValidateActions([]Action{{Type: ActionAddLabel, Label: "L"}}); err != nil {
		t.Errorf("valid addLabel rejected: %v", err)
	}
	if err := ValidateActions([]Action{{Type: ActionMarkPriority}, {Type: ActionSkipInbox}, {Type: ActionFlag}, {Type: ActionStop}}); err != nil {
		t.Errorf("valid safe actions rejected: %v", err)
	}
	if err := ValidateActions(nil); err == nil {
		t.Error("empty action list accepted, want error")
	}
	if err := ValidateActions([]Action{{Type: ActionAddLabel}}); err == nil {
		t.Error("addLabel with no label accepted, want error")
	}
	if err := ValidateActions([]Action{{Type: "frobnicate"}}); err == nil {
		t.Error("unknown action accepted, want error")
	}
	for _, typ := range []string{ActionMoveToMailbox, ActionJunk} {
		if err := ValidateActions([]Action{{Type: typ}}); err == nil {
			t.Errorf("deferred action %q accepted, want unsupported error", typ)
		}
	}
}

func TestParseRejectsUnknownActionViaValidate(t *testing.T) {
	acts, err := ParseActions([]byte(`[{"type":"junk"}]`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := ValidateActions(acts); err == nil {
		t.Error("junk action must be rejected by validation")
	}
}

func TestHeaderNames(t *testing.T) {
	rs := []Rule{
		{Conditions: Condition{All: []Condition{
			{Field: FieldHeader, Header: "X-Foo", Op: OpExists},
			{Any: []Condition{{Field: FieldHeader, Header: "x-foo", Op: OpContains, Value: "y"}, leaf(FieldFrom, OpExists, "")}},
		}}},
		{Conditions: Condition{Field: FieldHeader, Header: "X-Bar", Op: OpExists}},
	}
	names := HeaderNames(rs)
	// X-Foo appears twice (case-insensitively deduped) plus X-Bar → 2 distinct.
	if len(names) != 2 {
		t.Fatalf("HeaderNames = %v, want 2 distinct", names)
	}
}

// UsesField decides whether a caller must load message bodies before it can
// evaluate a rule HONESTLY (mailroom#193). A false negative is the dangerous
// direction: the rule then gets evaluated against facts with an empty To, matches
// strictly less than it should, and the caller reports a confident wrong number
// instead of refusing. So the cases that matter are the ones where the field is
// buried — under Any, under All, and mixed with fields that are on the row.
func TestUsesField(t *testing.T) {
	nested := Condition{All: []Condition{
		leaf(FieldFromDomain, OpEquals, "example.com"),
		{Any: []Condition{
			leaf(FieldSubject, OpContains, "receipt"),
			{All: []Condition{leaf(FieldTo, OpEquals, "you@example.com")}},
		}},
	}}
	if !UsesField(nested, FieldTo) {
		t.Fatal("UsesField missed `to` nested under Any/All — a caller would evaluate it against an empty To")
	}
	if !UsesField(nested, FieldSubject) || !UsesField(nested, FieldFromDomain) {
		t.Fatal("UsesField missed a field that is present")
	}
	if UsesField(nested, FieldHeader) {
		t.Fatal("UsesField found a field the tree does not test")
	}
	// A bare leaf, which is what most induced rules are.
	if !UsesField(leaf(FieldAlias, OpEquals, "shop@example.com"), FieldAlias) {
		t.Fatal("UsesField missed a bare leaf")
	}
	if UsesField(Condition{}, FieldTo) {
		t.Fatal("the empty condition tests nothing")
	}
}
