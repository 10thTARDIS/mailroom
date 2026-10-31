// SPDX-License-Identifier: AGPL-3.0-only

package tabs

import (
	"reflect"
	"testing"
)

// TestPrecedenceIsNarrowestFirst pins the stated tie-break, pair by pair, in
// both directions of the input order.
//
// BOTH DIRECTIONS MATTERS: the rule has to depend on the tabs, not on which
// label the join happened to return first. A FromLabels that simply kept the
// first entry would pass half of these and fail the other half, which is exactly
// the failure this table is shaped to catch.
func TestPrecedenceIsNarrowestFirst(t *testing.T) {
	pairs := []struct {
		wins, loses Tab
		why         string
	}{
		{Forums, Social, "list traffic is a protocol fact (RFC 2919)"},
		{Forums, Promotions, "list traffic is a protocol fact"},
		{Forums, Updates, "list traffic is a protocol fact"},
		{Forums, Primary, "any positive claim beats the residue"},
		{Social, Promotions, "a named class of sender is narrower than marketing bulk"},
		{Social, Updates, "a named class of sender is narrower than machine bulk"},
		{Social, Primary, "any positive claim beats the residue"},
		// The arbitrary pair. Pinned so a change to it is a deliberate edit to a
		// test rather than a silent behaviour change — see the comment on
		// `precedence`, and `mailroom-admin tabs`, which counts how often this
		// actually has to decide anything (currently: never).
		{Promotions, Updates, "arbitrary — both are broad bulk classes"},
		{Promotions, Primary, "any positive claim beats the residue"},
		{Updates, Primary, "any positive claim beats the residue"},
	}
	for _, p := range pairs {
		for _, in := range [][]Tab{{p.wins, p.loses}, {p.loses, p.wins}} {
			got, contested, ok := FromLabels(in)
			if !ok {
				t.Errorf("FromLabels(%v): not decided", in)
				continue
			}
			if got != p.wins {
				t.Errorf("FromLabels(%v) = %q, want %q (%s)", in, got, p.wins, p.why)
			}
			if !reflect.DeepEqual(contested, []Tab{p.loses}) {
				t.Errorf("FromLabels(%v) contested = %v, want [%q]", in, contested, p.loses)
			}
		}
	}
}

// TestFromLabelsAgreementIsNotContested: labels that all map to the same tab did
// not invoke the tie-break, and must not be reported as though they had. The
// report treats a non-zero contested count as a signal that the arbitrary pair
// is now deciding real mail, so a false positive here would send a reader
// looking for a problem that does not exist.
func TestFromLabelsAgreementIsNotContested(t *testing.T) {
	got, contested, ok := FromLabels([]Tab{Updates, Updates, Updates})
	if !ok || got != Updates {
		t.Fatalf("FromLabels(updates x3) = %q, %v; want updates, true", got, ok)
	}
	if len(contested) != 0 {
		t.Errorf("contested = %v, want empty: the labels agreed", contested)
	}
}

// TestFromLabelsUndecided: no labels, or no MAPPED labels, is not an error and
// not Primary — it is the fall-through to the classifier. Answering Primary here
// would silently file every unmapped message as personal mail, which is the
// failure mode #219 warns about from the other direction (the kind aggregate
// returns personal 0.0%, so a Primary built on a default would be both wrong and
// invisible).
func TestFromLabelsUndecided(t *testing.T) {
	for _, in := range [][]Tab{nil, {}, {Tab("")}, {Tab("bogus")}} {
		got, contested, ok := FromLabels(in)
		if ok {
			t.Errorf("FromLabels(%v) decided %q; want not decided", in, got)
		}
		if got != "" || contested != nil {
			t.Errorf("FromLabels(%v) = (%q, %v); want zero values when undecided", in, got, contested)
		}
	}
}

// TestFromLabelsIgnoresUnknownAlongsideKnown: a value outside the vocabulary
// must not be able to win, and must not suppress a real one.
func TestFromLabelsIgnoresUnknownAlongsideKnown(t *testing.T) {
	got, contested, ok := FromLabels([]Tab{Tab("bogus"), Updates})
	if !ok || got != Updates {
		t.Fatalf("FromLabels(bogus, updates) = %q, %v; want updates, true", got, ok)
	}
	if len(contested) != 0 {
		t.Errorf("contested = %v; an unknown value is not a contesting tab", contested)
	}
}

// TestFoldSendsADisabledTabToPrimary is the behaviour that makes Primary
// undisableable in the schema: mail from a switched-off tab has to land
// somewhere the user can see.
func TestFoldSendsADisabledTabToPrimary(t *testing.T) {
	enabled := map[Tab]bool{Primary: true, Social: true, Promotions: false, Updates: true, Forums: false}
	cases := map[Tab]Tab{
		Social:     Social,
		Updates:    Updates,
		Promotions: Primary, // disabled
		Forums:     Primary, // disabled
		Primary:    Primary,
	}
	for in, want := range cases {
		if got := Fold(in, enabled); got != want {
			t.Errorf("Fold(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestParseRejectsOutsideTheVocabulary. Refused rather than coerced: a mapping
// the operator set and the server quietly changed is worse than one it rejected.
func TestParseRejectsOutsideTheVocabulary(t *testing.T) {
	for _, s := range []string{"", "Primary", "PRIMARY", "inbox", "forum", "promotion"} {
		if _, ok := Parse(s); ok {
			t.Errorf("Parse(%q) accepted; the vocabulary is exactly %v", s, All)
		}
	}
	for _, want := range All {
		got, ok := Parse(string(want))
		if !ok || got != want {
			t.Errorf("Parse(%q) = (%q, %v); want (%q, true)", want, got, ok, want)
		}
	}
}

// TestPrecedenceCoversTheWholeVocabulary. Without this, adding a sixth tab and
// forgetting to rank it would leave it silently sorted behind everything — it
// would lose every tie-break it ever entered, and no test above would notice
// because they all name their tabs explicitly.
func TestPrecedenceCoversTheWholeVocabulary(t *testing.T) {
	if len(precedence) != len(All) {
		t.Fatalf("precedence has %d tabs, the vocabulary has %d", len(precedence), len(All))
	}
	for _, tab := range All {
		if rank(tab) >= len(precedence) {
			t.Errorf("tab %q is not ranked in the precedence order", tab)
		}
	}
	seen := map[int]Tab{}
	for _, tab := range All {
		r := rank(tab)
		if other, dup := seen[r]; dup {
			t.Errorf("tabs %q and %q share precedence rank %d", tab, other, r)
		}
		seen[r] = tab
	}
}
