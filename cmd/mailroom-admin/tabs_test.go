// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"testing"

	"mailroom/internal/store"
	"mailroom/internal/tabs"
)

// The `tabs` report exists to SIZE increment 2 before it is written, so its
// arithmetic is load-bearing: the number it prints for "unlabelled" is the
// classifier's workload, and the number it prints for "labelled but unmapped" is
// a mapping gap that no classifier would fix. Conflating them would send the
// next increment after the wrong problem.

func mapping(labels int, mapped ...tabs.Tab) store.MessageTabMapping {
	return store.MessageTabMapping{ID: store.NewULID(), Labels: labels, Mapped: mapped}
}

func TestTabStatsSeparatesTheTwoUnresolvedPopulations(t *testing.T) {
	enabled := map[tabs.Tab]bool{
		tabs.Primary: true, tabs.Social: true, tabs.Promotions: true,
		tabs.Updates: true, tabs.Forums: true,
	}
	s := newTabStats()
	// Unlabelled: nothing to join to.
	s.record(mapping(0), enabled)
	s.record(mapping(0), enabled)
	s.record(mapping(0), enabled)
	// Labelled, but the label carries no tab.
	s.record(mapping(1), enabled)
	s.record(mapping(2), enabled)
	// Decided.
	s.record(mapping(1, tabs.Updates), enabled)

	if s.total != 6 {
		t.Errorf("total = %d, want 6", s.total)
	}
	if s.unlabelled != 3 {
		t.Errorf("unlabelled = %d, want 3", s.unlabelled)
	}
	if s.labelledUnmapped != 2 {
		t.Errorf("labelledUnmapped = %d, want 2 — a message WITH labels whose labels have no tab is a mapping gap, not a classifier workload", s.labelledUnmapped)
	}
	if s.byTab[tabs.Updates] != 1 {
		t.Errorf("updates = %d, want 1", s.byTab[tabs.Updates])
	}
	// The three populations account for every scanned message: a report whose
	// parts do not sum to the whole is one a reader cannot act on.
	decided := 0
	for _, n := range s.byTab {
		decided += n
	}
	if got := decided + s.unlabelled + s.labelledUnmapped; got != s.total {
		t.Errorf("decided(%d) + unlabelled(%d) + labelledUnmapped(%d) = %d, want total %d",
			decided, s.unlabelled, s.labelledUnmapped, got, s.total)
	}
}

// TestTabStatsFoldColumnFollowsTheEnabledSet: the fold column must show where
// mail ACTUALLY lands, not where the mapping points, or an operator who switched
// a tab off would read a count for a tab they cannot see.
func TestTabStatsFoldColumnFollowsTheEnabledSet(t *testing.T) {
	enabled := map[tabs.Tab]bool{
		tabs.Primary: true, tabs.Social: true, tabs.Promotions: false,
		tabs.Updates: true, tabs.Forums: true,
	}
	s := newTabStats()
	s.record(mapping(1, tabs.Promotions), enabled)
	s.record(mapping(1, tabs.Promotions), enabled)
	s.record(mapping(1, tabs.Updates), enabled)

	if s.byTab[tabs.Promotions] != 2 {
		t.Errorf("raw promotions = %d, want 2 — the mapping still points there", s.byTab[tabs.Promotions])
	}
	if s.byTabFolded[tabs.Promotions] != 0 {
		t.Errorf("folded promotions = %d, want 0 — the tab is disabled", s.byTabFolded[tabs.Promotions])
	}
	if s.byTabFolded[tabs.Primary] != 2 {
		t.Errorf("folded primary = %d, want 2 — disabled-tab mail lands in Primary", s.byTabFolded[tabs.Primary])
	}
	if s.byTabFolded[tabs.Updates] != 1 {
		t.Errorf("folded updates = %d, want 1", s.byTabFolded[tabs.Updates])
	}
}

// TestTabStatsCountsThePrecedenceTieBreak. Today this is zero on real mail and
// the report says so; if it stops being zero, the arbitrary promotions/updates
// ordering is deciding something and the operator needs to see which pair.
func TestTabStatsCountsThePrecedenceTieBreak(t *testing.T) {
	enabled := map[tabs.Tab]bool{
		tabs.Primary: true, tabs.Social: true, tabs.Promotions: true,
		tabs.Updates: true, tabs.Forums: true,
	}
	s := newTabStats()
	s.record(mapping(1, tabs.Updates), enabled)                  // agreed, not contested
	s.record(mapping(2, tabs.Updates, tabs.Updates), enabled)    // agreed, not contested
	s.record(mapping(2, tabs.Promotions, tabs.Updates), enabled) // the arbitrary pair
	s.record(mapping(2, tabs.Updates, tabs.Promotions), enabled) // same pair, other order
	s.record(mapping(3, tabs.Forums, tabs.Social, tabs.Updates), enabled)

	if s.contestedTotal != 3 {
		t.Errorf("contestedTotal = %d, want 3", s.contestedTotal)
	}
	if got := s.contested["promotions over updates"]; got != 2 {
		t.Errorf("contested[promotions over updates] = %d, want 2 (order of the input must not matter)", got)
	}
	if got := s.contested["forums over social+updates"]; got != 1 {
		t.Errorf("contested[forums over social+updates] = %d, want 1; have %v", got, s.contested)
	}
	if s.byTab[tabs.Updates] != 2 {
		t.Errorf("updates won %d, want 2", s.byTab[tabs.Updates])
	}
	if s.byTab[tabs.Promotions] != 2 {
		t.Errorf("promotions won %d, want 2", s.byTab[tabs.Promotions])
	}
	if s.byTab[tabs.Forums] != 1 {
		t.Errorf("forums won %d, want 1", s.byTab[tabs.Forums])
	}
}
