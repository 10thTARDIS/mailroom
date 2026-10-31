// SPDX-License-Identifier: AGPL-3.0-only

package ingest

// The ml HALF of the rule-suggestion wire contract (#242). Its twin is
// internal/store's TestRuleSuggestionReasons, and the pair exists because the two
// ends of this payload live in packages that CANNOT reference each other:
// internal/ml imports internal/store, so internal/store cannot import
// internal/ml, and store.suggestionReason is necessarily a second declaration of
// ml.FeatureContribution rather than the same type.
//
// internal/ingest is the lowest package that can see BOTH (it already imports
// each), so the assertion that they agree lives here. No database is involved —
// this is a statement about struct tags and bytes, and it belongs in the lane
// that runs without one.

import (
	"encoding/json"
	"reflect"
	"testing"

	"mailroom/internal/ml"
)

// The exact bytes internal/store writes for a rule "alias is
// sub@example.com AND has List-Unsubscribe", induced from 4 of the user's own
// filings. It is a COPY of the golden in internal/store/rule_suggestion_test.go —
// deliberately, because a shared constant would have to live in one of the two
// packages and would then be exactly the coupling that does not exist. Two
// literals that must match is the honest encoding of two structs that must match;
// if they diverge, one of the two tests fails and names the divergence.
const storeWrittenReasons = `[{"key":"","weight":0,"text":"alias is sub@example.com"},` +
	`{"key":"","weight":0,"text":"has a List-Unsubscribe header"},` +
	`{"key":"","weight":0,"text":"backed by 4 messages you filed under this label"}]`

// TestRuleSuggestionReasonsDecodeAsFeatureContributions is the load-bearing
// assertion: what the store wrote is readable by the renderer.
//
// api's decodeContributions unmarshals app.label_suggestions.reasons into
// []ml.FeatureContribution and renders each entry. If the two structs disagree on
// a tag, a type or a name, that decode silently yields zero-valued fields — a
// suggestion whose reasons render as blank rows, with no error anywhere. "The
// absence of an error proves nothing on its own" (CLAUDE.md); this is what
// observing the failure looks like.
func TestRuleSuggestionReasonsDecodeAsFeatureContributions(t *testing.T) {
	var got []ml.FeatureContribution
	if err := json.Unmarshal([]byte(storeWrittenReasons), &got); err != nil {
		t.Fatalf("the store's reasons payload does not decode as []ml.FeatureContribution: %v", err)
	}
	want := []ml.FeatureContribution{
		{Text: "alias is sub@example.com"},
		{Text: "has a List-Unsubscribe header"},
		{Text: "backed by 4 messages you filed under this label"},
	}
	if len(got) != len(want) {
		t.Fatalf("decoded %d contributions, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("contribution %d = %+v, want %+v", i, got[i], want[i])
		}
		// Text must have SURVIVED the decode — this is the field the renderer uses
		// instead of FeatureLabel(Key), and a wrong json tag would leave it empty
		// while everything else looked fine.
		if got[i].Text == "" {
			t.Errorf("contribution %d lost its Text; the renderer would fall back to FeatureLabel(\"\")", i)
		}
	}

	// Round trip: re-encoding through ml.FeatureContribution must reproduce the
	// store's bytes exactly. This is what catches a field ADDED to
	// ml.FeatureContribution without omitempty, and any tag rename.
	back, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if string(back) != storeWrittenReasons {
		t.Errorf("re-encoded payload =\n\t%s\nwant\n\t%s", back, storeWrittenReasons)
	}
}

// TestFeatureContributionShapeIsPinned states ml.FeatureContribution's field set
// literally, because the round trip above cannot see a field added WITH
// omitempty and left at its zero value — which is exactly how this type would
// most plausibly grow.
//
// It is not a style rule. store.suggestionReason has to be a byte-for-byte copy
// of this shape and no compiler checks that, so the shape has to be somewhere a
// diff makes it obvious that the other copy needs the same change.
func TestFeatureContributionShapeIsPinned(t *testing.T) {
	want := []struct {
		name string
		typ  reflect.Kind
		tag  string
	}{
		{"Key", reflect.String, "key"},
		{"Weight", reflect.Float64, "weight"},
		{"Text", reflect.String, "text,omitempty"},
	}
	typ := reflect.TypeOf(ml.FeatureContribution{})
	if typ.NumField() != len(want) {
		t.Fatalf("ml.FeatureContribution has %d fields, want %d — store.suggestionReason "+
			"is a hand-maintained copy of this struct and must be updated too", typ.NumField(), len(want))
	}
	for i, w := range want {
		f := typ.Field(i)
		if f.Name != w.name {
			t.Errorf("field %d is %q, want %q (order matters: encoding/json emits in declaration order)", i, f.Name, w.name)
		}
		if f.Type.Kind() != w.typ {
			t.Errorf("field %s is %s, want %s", f.Name, f.Type.Kind(), w.typ)
		}
		if got := f.Tag.Get("json"); got != w.tag {
			t.Errorf("field %s json tag = %q, want %q", f.Name, got, w.tag)
		}
	}
}
