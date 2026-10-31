// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"encoding/json"
	"testing"

	"mailroom/internal/rules"
)

// Unit coverage (NO DATABASE) for the payload half of a rule-sourced label
// suggestion (#242): the bytes ruleSuggestionReasons puts in
// app.label_suggestions.reasons.
//
// ⚠ WHY THE ASSERTION IS ON LITERAL BYTES rather than on a decoded structure.
// This payload is a WIRE FORMAT shared with a package this one cannot import:
// internal/ml imports internal/store, so the reverse edge is a compile error and
// suggestionReason is necessarily a second declaration of ml.FeatureContribution
// (see its doc comment). Nothing in the type system connects them. What connects
// them is this golden string and its twin in internal/ingest's
// TestRuleSuggestionReasonsDecodeAsFeatureContributions, which asserts the SAME
// bytes decode into ml.FeatureContribution with the right fields populated and
// re-encode identically. Change either struct and one of the two goes red.
//
// A decoded-structure assertion here would pass against a payload the renderer
// cannot read, which is the failure the pair exists to make impossible.
const ruleSuggestionReasonsGolden = `[{"key":"","weight":0,"text":"alias is sub@example.com"},` +
	`{"key":"","weight":0,"text":"has a List-Unsubscribe header"},` +
	`{"key":"","weight":0,"text":"backed by 4 messages you filed under this label"}]`

func aliasAndUnsubRule() rules.Condition {
	return rules.Condition{All: []rules.Condition{
		{Field: rules.FieldAlias, Op: rules.OpEquals, Value: "sub@example.com"},
		{Field: rules.FieldListUnsub, Op: rules.OpExists},
	}}
}

func TestRuleSuggestionReasons(t *testing.T) {
	got, err := ruleSuggestionReasons(aliasAndUnsubRule(), 4)
	if err != nil {
		t.Fatalf("ruleSuggestionReasons: %v", err)
	}
	if string(got) != ruleSuggestionReasonsGolden {
		t.Errorf("reasons payload =\n\t%s\nwant\n\t%s", got, ruleSuggestionReasonsGolden)
	}
}

func TestRuleSuggestionReasonsEvidencePhrase(t *testing.T) {
	cond := rules.Condition{Field: rules.FieldSubject, Op: rules.OpContains, Value: "sale"}

	// Singular, because "backed by 1 messages" is the kind of thing a user reads
	// as the system not having been finished.
	got, err := ruleSuggestionReasons(cond, 1)
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"key":"","weight":0,"text":"subject contains sale"},` +
		`{"key":"","weight":0,"text":"backed by 1 message you filed under this label"}]`
	if string(got) != want {
		t.Errorf("payload =\n\t%s\nwant\n\t%s", got, want)
	}

	// NO evidence entry when there is no evidence. A hand-written suggestLabel
	// rule carries no count, and "backed by 0 messages you filed under this label"
	// would assert a measurement nobody took. The conjunct still explains the rule.
	got, err = ruleSuggestionReasons(cond, 0)
	if err != nil {
		t.Fatal(err)
	}
	want = `[{"key":"","weight":0,"text":"subject contains sale"}]`
	if string(got) != want {
		t.Errorf("payload with no evidence =\n\t%s\nwant\n\t%s", got, want)
	}
}

// TestRuleSuggestionReasonsUnrenderableRuleStoresNull pins the one case that
// must yield SQL NULL rather than an empty array. Migration 00019 spends three
// paragraphs on the difference: NULL is "no record of why", '[]' is "looked and
// found nothing". A rule with an empty condition tree and no evidence genuinely
// has no record of why — and it also cannot match anything, so no suggestion can
// have been produced by one.
func TestRuleSuggestionReasonsUnrenderableRuleStoresNull(t *testing.T) {
	got, err := ruleSuggestionReasons(rules.Condition{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("reasons = %q, want nil (SQL NULL) for a rule that cannot describe itself", got)
	}
	// PutLabelSuggestion turns a zero-length payload into SQL NULL, so an empty
	// non-nil slice would be equivalent — but it would also be indistinguishable
	// from '[]' to any future reader of this function, so nil is what it returns.
	if len(got) != 0 {
		t.Errorf("reasons = %q, want empty", got)
	}
}

// TestRuleSuggestionWeightsAreZero states the rule the payload's shape encodes,
// as an assertion rather than only as a comment: an AND rule's conjuncts are
// each individually necessary, so there is no per-conjunct weight to report and
// inventing one would be a fabricated number the surface would then sort by.
func TestRuleSuggestionWeightsAreZero(t *testing.T) {
	got, err := ruleSuggestionReasons(aliasAndUnsubRule(), 4)
	if err != nil {
		t.Fatal(err)
	}
	var decoded []suggestionReason
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded) != 3 {
		t.Fatalf("decoded %d reasons, want 3", len(decoded))
	}
	for i, r := range decoded {
		if r.Weight != 0 {
			t.Errorf("reason %d weight = %v, want 0", i, r.Weight)
		}
		if r.Key != "" {
			t.Errorf("reason %d key = %q, want empty — these are literal conditions, not feature keys", i, r.Key)
		}
		if r.Text == "" {
			t.Errorf("reason %d has no text; Text carries the entire meaning here", i)
		}
	}
}
