// SPDX-License-Identifier: AGPL-3.0-only

package rules

import (
	"math"
	"strings"
	"testing"
)

// Unit coverage for the two pure projections behind rule-sourced label
// suggestions (#242). No database: these are the functions the store calls to
// decide what a suggestion row SORTS BY and what it SAYS, and both are total
// functions of a Condition tree and an int.

// TestEvidenceConfidenceIsAMonotonicBoundedRank pins the three properties the
// value is actually used for — and only those, because it is an ordering rank
// and not a probability (see EvidenceConfidence's doc comment). No exact value
// is asserted as though it meant something; the exact values are checked only to
// catch a typo'd formula.
func TestEvidenceConfidenceIsAMonotonicBoundedRank(t *testing.T) {
	// No evidence sorts last among suggestions, but is still a suggestion.
	if got := EvidenceConfidence(0); got != 0 {
		t.Errorf("EvidenceConfidence(0) = %v, want 0", got)
	}
	// Negative is not a real input (Action.Evidence is unvalidated, so it is
	// reachable) and must behave like "none" rather than producing a negative
	// sort key that would order BELOW an unsuggested message's NULL.
	if got := EvidenceConfidence(-7); got != 0 {
		t.Errorf("EvidenceConfidence(-7) = %v, want 0", got)
	}

	// STRICTLY INCREASING: more of the user's own filings must sort higher. This
	// is the property the needs-a-category ORDER BY depends on; without it the
	// column may as well be a constant, which is the exact failure the doc comment
	// says the column exists to avoid.
	prev := EvidenceConfidence(0)
	for n := 1; n <= 1000; n++ {
		got := EvidenceConfidence(n)
		if got <= prev {
			t.Fatalf("EvidenceConfidence(%d) = %v is not > EvidenceConfidence(%d) = %v", n, got, n-1, prev)
		}
		// BOUNDED IN [0,1), never 1.0. A model's certainty can legitimately be
		// exactly 1.0; a rule rank must never collide with it, and confidence is
		// documented as being in [0,1] for the model source.
		if got < 0 || got >= 1 {
			t.Fatalf("EvidenceConfidence(%d) = %v, want in [0,1)", n, got)
		}
		prev = got
	}

	// The formula itself, spot-checked so a rewrite that keeps monotonicity but
	// changes the curve is visible in a diff rather than silent.
	for _, tc := range []struct {
		n    int
		want float64
	}{{1, 0.5}, {2, 2.0 / 3.0}, {4, 0.8}, {9, 0.9}} {
		if got := EvidenceConfidence(tc.n); math.Abs(got-tc.want) > 1e-12 {
			t.Errorf("EvidenceConfidence(%d) = %v, want %v", tc.n, got, tc.want)
		}
	}
}

// TestDescribeConditionLeaves covers every Field and every Op constant in
// rules.go, plus the two fallbacks. A conjunct that renders to nothing would
// make a rule look narrower on the queue than it is, so "no phrase" is a failure
// mode this has to be able to see: every case here asserts a non-empty result.
func TestDescribeConditionLeaves(t *testing.T) {
	tests := []struct {
		name string
		c    Condition
		want string
	}{
		// The three phrasings named in #242 as what a queue row should read like.
		{"alias equals", leaf(FieldAlias, OpEquals, "sub@example.com"), "alias is sub@example.com"},
		{"listUnsub exists", Condition{Field: FieldListUnsub, Op: OpExists}, "has a List-Unsubscribe header"},
		{"subject contains", leaf(FieldSubject, OpContains, "sale"), "subject contains sale"},

		{"from equals", leaf(FieldFrom, OpEquals, "alice@example.com"), "sender is alice@example.com"},
		{"from contains", leaf(FieldFrom, OpContains, "alice@"), "sender contains alice@"},
		{"from matches", leaf(FieldFrom, OpMatches, "^alice"), "sender matches ^alice"},
		{"from exists", Condition{Field: FieldFrom, Op: OpExists}, "has a sender address"},

		{"fromDomain equals", leaf(FieldFromDomain, OpEquals, "example.com"), "sender domain is example.com"},
		{"fromDomain exists", Condition{Field: FieldFromDomain, Op: OpExists}, "has a sender domain"},

		// Indefinite and singular: matchList matches if ANY recipient does.
		{"to equals", leaf(FieldTo, OpEquals, "bob@example.net"), "a recipient is bob@example.net"},
		{"to exists", Condition{Field: FieldTo, Op: OpExists}, "has a recipient"},

		{"subject matches", leaf(FieldSubject, OpMatches, "[0-9]+"), "subject matches [0-9]+"},
		{"subject exists", Condition{Field: FieldSubject, Op: OpExists}, "has a subject"},

		{"alias contains", leaf(FieldAlias, OpContains, "sub"), "alias contains sub"},
		{"alias exists", Condition{Field: FieldAlias, Op: OpExists}, "has an alias"},

		{"listId equals", leaf(FieldListID, OpEquals, "<list.example.com>"), "the List-Id header is <list.example.com>"},
		{"listId exists", Condition{Field: FieldListID, Op: OpExists}, "has a List-Id header"},

		{"listUnsub contains", leaf(FieldListUnsub, OpContains, "mailto:"), "the List-Unsubscribe header contains mailto:"},

		{"header contains", Condition{Field: FieldHeader, Header: "X-Mailer", Op: OpContains, Value: "acme"}, "the X-Mailer header contains acme"},
		{"header exists", Condition{Field: FieldHeader, Header: "X-Mailer", Op: OpExists}, "has a X-Mailer header"},
		// Validation rejects a nameless header leaf, so this is the "a stored row
		// somehow has one" path: it must still say something rather than render
		// "the  header".
		{"header unnamed exists", Condition{Field: FieldHeader, Op: OpExists}, "has an unnamed header"},

		// Fallbacks: an unknown field or op names ITSELF rather than vanishing.
		{"unknown field", leaf("frobnicate", OpEquals, "v"), "frobnicate is v"},
		{"unknown field exists", Condition{Field: "frobnicate", Op: OpExists}, "has a frobnicate"},
		{"unknown op", leaf(FieldSubject, "frobnicates", "v"), "subject frobnicates v"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := DescribeCondition(tc.c)
			if len(got) != 1 {
				t.Fatalf("DescribeCondition = %q, want exactly one phrase", got)
			}
			if got[0] != tc.want {
				t.Errorf("DescribeCondition = %q, want %q", got[0], tc.want)
			}
		})
	}
}

// TestDescribeConditionEveryFieldConstantIsCovered is a completeness guard:
// every Field constant validField accepts must have a real arm in fieldPhrase,
// not fall through to the self-naming default.
//
// It reads fieldPhrase's `known` return rather than inspecting the phrase,
// because two fields (subject, alias) legitimately render as their own key — so
// "the output equals the field name" cannot tell a deliberate phrase from the
// fallback, and a check built that way would fail on correct code while missing
// the case it was written for.
//
// Without a guard, adding a ninth field would render as "listArchive is x"
// forever: working, plausible, and never noticed.
func TestDescribeConditionEveryFieldConstantIsCovered(t *testing.T) {
	fields := []string{
		FieldFrom, FieldFromDomain, FieldTo, FieldSubject, FieldAlias,
		FieldListID, FieldListUnsub, FieldHeader,
	}
	for _, f := range fields {
		if !validField(f) {
			t.Errorf("field %q is not accepted by validField; this list has drifted", f)
		}
		c := Condition{Field: f, Op: OpEquals, Value: "x"}
		if f == FieldHeader {
			c.Header = "X-Test"
		}
		subject, presence, known := fieldPhrase(c)
		if !known {
			t.Errorf("field %q fell through to fieldPhrase's default arm; give it a phrase", f)
		}
		if strings.TrimSpace(subject) == "" || strings.TrimSpace(presence) == "" {
			t.Errorf("field %q rendered blank: subject=%q presence=%q", f, subject, presence)
		}
		if got := DescribeCondition(c); len(got) != 1 {
			t.Errorf("field %q: DescribeCondition = %q, want one phrase", f, got)
		}
	}
	// The negative control: an unknown field MUST report known=false, or the
	// assertion above is vacuous — it would pass against a fieldPhrase that
	// returned true unconditionally.
	if _, _, known := fieldPhrase(Condition{Field: "listArchive"}); known {
		t.Error("fieldPhrase reported an unknown field as known; the completeness check above proves nothing")
	}
}

// TestDescribeConditionNesting pins the boolean structure surviving into the
// text: an AND is several reasons, an OR is ONE reason that says "or", and a
// nested AND inside an OR is parenthesised so it cannot be misread.
func TestDescribeConditionNesting(t *testing.T) {
	alias := leaf(FieldAlias, OpEquals, "sub@example.com")
	unsub := Condition{Field: FieldListUnsub, Op: OpExists}
	subj := leaf(FieldSubject, OpContains, "sale")

	t.Run("all is one phrase per conjunct", func(t *testing.T) {
		got := DescribeCondition(Condition{All: []Condition{alias, unsub}})
		want := []string{"alias is sub@example.com", "has a List-Unsubscribe header"}
		assertPhrases(t, got, want)
	})

	t.Run("any collapses to a single or-phrase", func(t *testing.T) {
		got := DescribeCondition(Condition{Any: []Condition{alias, subj}})
		assertPhrases(t, got, []string{"alias is sub@example.com or subject contains sale"})
	})

	t.Run("nested all inside all flattens", func(t *testing.T) {
		got := DescribeCondition(Condition{All: []Condition{
			alias,
			{All: []Condition{unsub, subj}},
		}})
		assertPhrases(t, got, []string{
			"alias is sub@example.com",
			"has a List-Unsubscribe header",
			"subject contains sale",
		})
	})

	t.Run("any inside all is one conjunct", func(t *testing.T) {
		got := DescribeCondition(Condition{All: []Condition{
			alias,
			{Any: []Condition{unsub, subj}},
		}})
		assertPhrases(t, got, []string{
			"alias is sub@example.com",
			"has a List-Unsubscribe header or subject contains sale",
		})
	})

	t.Run("all inside any is parenthesised", func(t *testing.T) {
		got := DescribeCondition(Condition{Any: []Condition{
			{All: []Condition{alias, unsub}},
			subj,
		}})
		assertPhrases(t, got, []string{
			"(alias is sub@example.com and has a List-Unsubscribe header) or subject contains sale",
		})
	})

	// A node that is neither a group nor a leaf matches NOTHING (Condition.match's
	// safe default), so describing it as anything would be a claim about a rule
	// that cannot fire.
	t.Run("empty node describes nothing", func(t *testing.T) {
		if got := DescribeCondition(Condition{}); got != nil {
			t.Errorf("DescribeCondition(empty) = %q, want nil", got)
		}
		if got := DescribeCondition(Condition{All: []Condition{{}}}); got != nil {
			t.Errorf("DescribeCondition(all(empty)) = %q, want nil", got)
		}
		if got := DescribeCondition(Condition{Any: []Condition{{}}}); got != nil {
			t.Errorf("DescribeCondition(any(empty)) = %q, want nil", got)
		}
	})

	// An unrenderable branch of an `any` is dropped rather than joined as an empty
	// string, which would read as "x or  or y".
	t.Run("any skips unrenderable branches", func(t *testing.T) {
		got := DescribeCondition(Condition{Any: []Condition{alias, {}, subj}})
		assertPhrases(t, got, []string{"alias is sub@example.com or subject contains sale"})
	})
}

// TestDescribeConditionOnInducedRules runs the real inducer's output through the
// renderer, so the phrasing is pinned against the condition shapes that will
// ACTUALLY reach it rather than only against hand-written trees. The Wayfair
// acceptance case (induce.go) is the one that matters: its rule is a conjunction
// ending in the bulk marker, and both halves must be legible on a queue row.
func TestDescribeConditionOnInducedRules(t *testing.T) {
	promo := func(subject string) MessageFacts {
		return MessageFacts{
			From: "deals@wayfair.example", FromDomain: "wayfair.example",
			Subject: subject, Alias: "shopping@example.com",
			ListUnsubscribe: "<mailto:unsub@wayfair.example>",
		}
	}
	receipt := func(subject string) MessageFacts {
		return MessageFacts{
			From: "orders@wayfair.example", FromDomain: "wayfair.example",
			Subject: subject, Alias: "shopping@example.com",
		}
	}
	examples := []Example{
		{Facts: promo("70% off rugs"), Positive: true},
		{Facts: promo("Flash sale"), Positive: true},
		{Facts: receipt("Your order has shipped")},
		{Facts: receipt("Order confirmation")},
		{Facts: receipt("Password reset")},
		{Facts: receipt("Delivery update")},
		{Facts: receipt("Your account")},
		{Facts: receipt("Return received")},
	}
	cands := Induce(examples)
	if len(cands) == 0 {
		t.Fatal("Induce proposed nothing; the fixture no longer exercises the renderer")
	}
	for _, c := range cands {
		got := DescribeCondition(c.Conditions)
		if len(got) == 0 {
			t.Fatalf("induced rule %+v rendered to no phrases", c.Conditions)
		}
		for _, p := range got {
			if strings.TrimSpace(p) == "" {
				t.Errorf("induced rule %+v produced a blank phrase in %q", c.Conditions, got)
			}
		}
	}
}

// TestValidateSuggestLabelAction pins suggestLabel's save-time contract: a label
// is required (same as addLabel) and the error names suggestLabel, not addLabel,
// so a Rule/set SetError does not tell the user to fix an action they never
// wrote. Evidence is deliberately NOT validated — see Action.Evidence.
func TestValidateSuggestLabelAction(t *testing.T) {
	if err := ValidateActions([]Action{{Type: ActionSuggestLabel, Label: "L", Evidence: 4}}); err != nil {
		t.Errorf("valid suggestLabel rejected: %v", err)
	}
	if err := ValidateActions([]Action{{Type: ActionSuggestLabel, Label: "L"}}); err != nil {
		t.Errorf("suggestLabel with no evidence rejected: %v — evidence is advisory", err)
	}
	if err := ValidateActions([]Action{{Type: ActionSuggestLabel, Label: "L", Evidence: -1}}); err != nil {
		t.Errorf("suggestLabel with negative evidence rejected: %v — evidence is advisory", err)
	}
	err := ValidateActions([]Action{{Type: ActionSuggestLabel}})
	if err == nil {
		t.Fatal("suggestLabel with no label accepted, want error")
	}
	if !strings.Contains(err.Error(), ActionSuggestLabel) {
		t.Errorf("error %q does not name %q", err, ActionSuggestLabel)
	}
}

// TestEvaluateAttributedCarriesConditions is the plumbing #242 needs: the
// executor renders a suggestion's reasons from the LIVE tree the evaluator
// matched, so the tree has to survive evaluation. Without it the store would have
// to re-read app.rules per action, or bake text at rule-creation time and let
// Rule/tighten desynchronise it.
func TestEvaluateAttributedCarriesConditions(t *testing.T) {
	cond := Condition{All: []Condition{
		leaf(FieldFromDomain, OpEquals, "example.com"),
		leaf(FieldSubject, OpContains, "invoice"),
	}}
	rs := []Rule{{
		Name: "suggest", Enabled: true, Conditions: cond,
		Actions: []Action{{Type: ActionSuggestLabel, Label: "L", Evidence: 3}},
	}}
	att := EvaluateAttributed(rs, facts())
	if len(att) != 1 {
		t.Fatalf("EvaluateAttributed returned %d actions, want 1", len(att))
	}
	if att[0].Evidence != 3 {
		t.Errorf("Evidence = %d, want 3", att[0].Evidence)
	}
	got := DescribeCondition(att[0].Conditions)
	assertPhrases(t, got, []string{
		"sender domain is example.com",
		"subject contains invoice",
	})
}

func assertPhrases(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d phrases %q, want %d %q", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("phrase %d = %q, want %q", i, got[i], want[i])
		}
	}
}
