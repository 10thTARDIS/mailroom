// SPDX-License-Identifier: AGPL-3.0-only

package api

// Unit tests (no database) for emailObject's #34 priorityReasons rendering: the
// property is opt-in (only present when the client names it), decodes the stored
// top_features KEYS into { label, weight, polarity }, and is JSON null when the
// message is unscored or its score carries no reasons.
//
// Also covers #242's additions to the SAME renderer: decodeContributions'
// Text-overrides-Key exception (a rule conjunct is data, not a member of
// ml.FeatureLabel's closed key vocabulary — see decodeContributions' doc
// comment for the full argument) and suggestedLabels' new `source` field,
// which is what lets a client tell a calibrated model probability apart from a
// rule's uncalibrated evidence rank without ever inspecting `reasons` first.

import (
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/ml"
	"mailroom/internal/store"
)

func TestEmailObjectPriorityReasonsRendered(t *testing.T) {
	ev := store.EmailView{
		ID: ulid.Make(),
		TopFeatures: []byte(`[{"key":"corr:1","weight":1.6},` +
			`{"key":"list:1","weight":-1.3}]`),
	}

	out := emailObject(ev, map[string]bool{"priorityReasons": true}, nil)
	reasons, ok := out["priorityReasons"].([]map[string]any)
	if !ok {
		t.Fatalf("priorityReasons type = %T, want []map[string]any", out["priorityReasons"])
	}
	if len(reasons) != 2 {
		t.Fatalf("priorityReasons length = %d, want 2 (%+v)", len(reasons), reasons)
	}
	// Keys are decoded to labels, and polarity follows the weight sign.
	if reasons[0]["label"] != "You have written to this sender" {
		t.Errorf("reason[0].label = %v, want %q", reasons[0]["label"], "You have written to this sender")
	}
	if reasons[0]["polarity"] != "up" {
		t.Errorf("reason[0].polarity = %v, want up (positive weight)", reasons[0]["polarity"])
	}
	if reasons[1]["label"] != "Mailing-list mail" {
		t.Errorf("reason[1].label = %v, want %q", reasons[1]["label"], "Mailing-list mail")
	}
	if reasons[1]["polarity"] != "down" {
		t.Errorf("reason[1].polarity = %v, want down (negative weight)", reasons[1]["polarity"])
	}
}

func TestEmailObjectPriorityReasonsOptIn(t *testing.T) {
	ev := store.EmailView{
		ID:          ulid.Make(),
		TopFeatures: []byte(`[{"key":"corr:1","weight":1.6}]`),
	}
	// Not requested -> absent entirely (opt-in, like priorityScore).
	out := emailObject(ev, map[string]bool{"id": true}, nil)
	if _, present := out["priorityReasons"]; present {
		t.Error("priorityReasons present when not requested; it is opt-in")
	}
}

func TestEmailObjectPriorityReasonsNullWhenNoReasons(t *testing.T) {
	// Unscored (nil payload) and empty (`[]`) both render null-but-present.
	for name, tf := range map[string][]byte{
		"unscored":    nil,
		"empty array": []byte("[]"),
	} {
		ev := store.EmailView{ID: ulid.Make(), TopFeatures: tf}
		out := emailObject(ev, map[string]bool{"priorityReasons": true}, nil)
		v, present := out["priorityReasons"]
		if !present {
			t.Errorf("%s: priorityReasons absent, want present-and-null", name)
			continue
		}
		if v != nil {
			t.Errorf("%s: priorityReasons = %v, want null", name, v)
		}
	}
}

// TestDecodeContributionsUsesTextWhenPresent is the #242 half of
// decodeContributions: a rule conjunct ("alias is sub@example.com") is DATA
// the rule inducer wrote, not a key drawn from ml.FeatureLabel's fixed
// vocabulary — so when Text is set it must travel to the wire VERBATIM, never
// through FeatureLabel(Key) (which would not recognize the key at all, since a
// rule's key is not one of the T1 model's fixed feature names).
func TestDecodeContributionsUsesTextWhenPresent(t *testing.T) {
	raw := []byte(`[{"key":"rule:alias","weight":3,"text":"alias is sub@example.com"}]`)
	contribs, ok := decodeContributions(raw)
	if !ok {
		t.Fatalf("decodeContributions ok = false, want true")
	}
	if len(contribs) != 1 {
		t.Fatalf("len(contribs) = %d, want 1", len(contribs))
	}
	if got := contribs[0]["label"]; got != "alias is sub@example.com" {
		t.Errorf("label = %v, want the literal Text verbatim, not a FeatureLabel(Key) lookup", got)
	}
}

// TestDecodeContributionsFallsBackToFeatureLabelWithoutText pins the other
// half of the same exception: a contribution with no Text — every T1 priority
// contribution today — must still resolve through ml.FeatureLabel(Key) exactly
// as it did before #242. The Text field is additive, not a replacement.
func TestDecodeContributionsFallsBackToFeatureLabelWithoutText(t *testing.T) {
	raw := []byte(`[{"key":"corr:1","weight":1.6}]`)
	contribs, ok := decodeContributions(raw)
	if !ok {
		t.Fatalf("decodeContributions ok = false, want true")
	}
	want := ml.FeatureLabel("corr:1")
	if got := contribs[0]["label"]; got != want {
		t.Errorf("label = %v, want ml.FeatureLabel(\"corr:1\") = %q", got, want)
	}
}

// TestSuggestedLabelsCarrySource is #242's surface contract: a client cannot
// tell a calibrated model probability from a rule's uncalibrated evidence rank
// without this field, and NeedsCategory.tsx's SuggestionStrip switches its
// entire rendering (percentage vs. evidence phrase) on it.
func TestSuggestedLabelsCarrySource(t *testing.T) {
	ss := []store.LabelSuggestion{
		{LabelID: ulid.Make(), Confidence: 0.8, Source: store.SuggestionSourceModel},
		{LabelID: ulid.Make(), Confidence: 3, Source: store.SuggestionSourceRule},
	}
	out := suggestedLabels(ss)
	list, ok := out.([]map[string]any)
	if !ok {
		t.Fatalf("suggestedLabels type = %T, want []map[string]any", out)
	}
	if len(list) != 2 {
		t.Fatalf("len(list) = %d, want 2", len(list))
	}
	if got := list[0]["source"]; got != store.SuggestionSourceModel {
		t.Errorf("list[0][source] = %v, want %q", got, store.SuggestionSourceModel)
	}
	if got := list[1]["source"]; got != store.SuggestionSourceRule {
		t.Errorf("list[1][source] = %v, want %q", got, store.SuggestionSourceRule)
	}
}

// TestSuggestedLabelsReasonsNullVsEmptyPreserved re-pins suggestionReasons'
// null/[] contract (see its own doc comment) through the ACTUAL suggestedLabels
// renderer #242 touched, so a change to the source field cannot quietly widen
// or narrow the reasons contract sitting right next to it in the same map
// literal.
func TestSuggestedLabelsReasonsNullVsEmptyPreserved(t *testing.T) {
	ss := []store.LabelSuggestion{
		// No record of why: reasons is nil.
		{LabelID: ulid.Make(), Confidence: 0.5, Source: store.SuggestionSourceModel, Reasons: nil},
		// Explained, and nothing above the noise floor: reasons is `[]`.
		{LabelID: ulid.Make(), Confidence: 0.5, Source: store.SuggestionSourceModel, Reasons: []byte("[]")},
	}
	out := suggestedLabels(ss)
	list, ok := out.([]map[string]any)
	if !ok {
		t.Fatalf("suggestedLabels type = %T, want []map[string]any", out)
	}
	if v := list[0]["reasons"]; v != nil {
		t.Errorf("reasons[0] (nil Reasons) = %v, want null", v)
	}
	got, ok := list[1]["reasons"].([]map[string]any)
	if !ok {
		t.Fatalf("reasons[1] type = %T, want []map[string]any (empty preserved, not collapsed to null)", list[1]["reasons"])
	}
	if len(got) != 0 {
		t.Errorf("reasons[1] length = %d, want 0", len(got))
	}
}
