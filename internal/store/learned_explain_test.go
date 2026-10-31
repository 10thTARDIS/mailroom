// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"slices"
	"testing"

	"mailroom/internal/placement"
)

// The counter's reason is a closed set: every reason the decision can produce
// maps to its own label, and anything else is "other", so a future reason can
// never mint a new series (#840).
func TestLearnedPlacementLabelIsClosed(t *testing.T) {
	labels := LearnedPlacementLabels()
	for _, r := range []placement.Reason{
		placement.NoEvidence, placement.Thin, placement.Mixed, placement.Contradicted, placement.NoKey,
		placement.DomainBlocked, placement.Unauthenticated, placement.SenderWarned, placement.AmbiguousFrom,
		LearnedOff, LearnedAgreesWithDesk, LearnedGated, LearnedAlreadyPlaced,
	} {
		got := LearnedPlacementResult{Reason: r}.Label()
		if got == LearnedLabelOther {
			t.Errorf("reason %q maps to other; it has a label of its own", r)
		}
		if !slices.Contains(labels, got) {
			t.Errorf("reason %q -> %q, which LearnedPlacementLabels does not list", r, got)
		}
	}
	if got := (LearnedPlacementResult{Placed: true, Reason: placement.Confident}).Label(); got != LearnedLabelPlaced {
		t.Errorf("a placed result = %q, want %q", got, LearnedLabelPlaced)
	}
	// "placed" is the success; a rule's earlier placement must not share it.
	if got := (LearnedPlacementResult{Reason: LearnedAlreadyPlaced}).Label(); got != LearnedLabelAlreadyPlaced {
		t.Errorf("a rule-placed result = %q, want %q", got, LearnedLabelAlreadyPlaced)
	}
	for _, r := range []placement.Reason{"bogus", "", placement.Confident} {
		if got := (LearnedPlacementResult{Reason: r}).Label(); got != LearnedLabelOther {
			t.Errorf("unknown reason %q = %q, want %q", r, got, LearnedLabelOther)
		}
	}
	seen := map[string]bool{}
	for _, l := range labels {
		if seen[l] {
			t.Errorf("label %q listed twice", l)
		}
		seen[l] = true
	}
}
