// SPDX-License-Identifier: AGPL-3.0-only

package main

// The classification materializearealabels.go's report rests on is a pure
// function over areas.Config + store.AreaReportRow, exactly like areaStats
// (areareport_test.go) — hand-built rows, no database, because a backfill
// report whose classification is wrong is worse than one that never ran.

import (
	"testing"

	"mailroom/internal/areas"
	"mailroom/internal/store"
	"mailroom/internal/tabs"
)

// twoAreaMaterializePlan: Family is fed by an EXTRA label (Vip) beyond its own
// (Family); Shopping is fed by tabs.Promotions AND an EXTRA label (Deals)
// beyond its own (Shopping). Mirrors "Family" to IMAP so both branches of the
// IMAP MIRRORING section get exercised by TestPrintMaterializeReportDoesNotPanic
// below.
func twoAreaMaterializePlan() areaPlan {
	cfg := areas.Config{Areas: []areas.Area{
		{Name: "Family", Labels: []string{"Family", "Vip"}},
		{Name: "Shopping", Tabs: []tabs.Tab{tabs.Promotions}, Labels: []string{"Shopping", "Deals"}},
	}}
	return areaPlan{
		cfg:          cfg,
		ownLabelName: map[string]string{"Family": "Family", "Shopping": "Shopping"},
		mirrors:      map[string]bool{"Family": true, "Shopping": false},
	}
}

func mrow(inInbox bool, labels []string, tab tabs.Tab, tabKnown bool) store.AreaReportRow {
	r := store.AreaReportRow{ID: store.NewULID(), InInbox: inInbox, Labels: labels}
	if tabKnown {
		r.MappedTab, r.MappedKnown = tab, true
	}
	return r
}

// TestMaterializeClassifiesTheFourPopulations pins the classification named in
// mailroom#442 increment 3's spec — would-label, already, label-decided, and
// the three unsorted reasons — one row apiece, so a wrong bucket cannot hide
// behind a shared fixture.
//
// "already" is reached via ByLabel, not ByTab: see record's own doc comment
// for why a row carrying an area's OWN label always resolves ByLabel (the own
// label is itself a feed), which is what makes "ByTab with the own label
// present" unreachable and "already" a ByLabel-with-the-own-label case
// instead.
func TestMaterializeClassifiesTheFourPopulations(t *testing.T) {
	plan := twoAreaMaterializePlan()

	wouldLabel := mrow(false, nil, tabs.Promotions, true)          // ByTab: Shopping's own label is necessarily absent
	already := mrow(false, []string{"Shopping"}, "", false)        // ByLabel via Shopping's OWN label
	labelDecided := mrow(false, []string{"Vip"}, "", false)        // ByLabel via the EXTRA feed
	unsortedNone := mrow(false, nil, "", false)                    // no label, no verdict
	unsortedLabelGap := mrow(false, []string{"Random"}, "", false) // labelled, feeds nothing
	unsortedTabGap := mrow(false, nil, tabs.Updates, true)         // tab known, feeds nothing

	stats := newMaterializeStats(plan.cfg, false)
	for _, r := range []store.AreaReportRow{wouldLabel, already, labelDecided, unsortedNone, unsortedLabelGap, unsortedTabGap} {
		stats.record(plan.cfg, plan, r)
	}

	shopping := stats.all.perArea["Shopping"]
	if shopping == nil {
		t.Fatal("no Shopping area count")
	}
	if shopping.wouldLabel != 1 {
		t.Errorf("Shopping.wouldLabel = %d, want 1", shopping.wouldLabel)
	}
	if shopping.already != 1 {
		t.Errorf("Shopping.already = %d, want 1", shopping.already)
	}

	family := stats.all.perArea["Family"]
	if family == nil {
		t.Fatal("no Family area count")
	}
	if family.labelDecided != 1 {
		t.Errorf("Family.labelDecided = %d, want 1", family.labelDecided)
	}
	if family.wouldLabel != 0 || family.already != 0 {
		t.Errorf("Family should have no ByTab rows at all: %+v", family)
	}

	if got := stats.all.unsorted[areas.UnsortedNoLabelNoVerdict]; got != 1 {
		t.Errorf("UnsortedNoLabelNoVerdict = %d, want 1", got)
	}
	if got := stats.all.unsorted[areas.UnsortedLabelsFeedNothing]; got != 1 {
		t.Errorf("UnsortedLabelsFeedNothing = %d, want 1", got)
	}
	if got := stats.all.unsorted[areas.UnsortedTabFeedsNothing]; got != 1 {
		t.Errorf("UnsortedTabFeedsNothing = %d, want 1", got)
	}

	// Every row landed exactly once: 3 areas-rows + 3 unsorted-rows == 6 total.
	if stats.all.total != 6 {
		t.Errorf("stats.all.total = %d, want 6", stats.all.total)
	}
}

// TestMaterializeTieResolvesToTheWinnerAsLabelDecided: a message whose labels
// feed TWO areas — via each area's EXTRA feed, not its own label, so the
// winner lands as label-decided rather than already — is decided by
// cfg.Resolve's own tie break (earliest position wins) before this
// classification ever sees it, so it must land for the WINNER alone — never
// counted against the loser, and never miscounted as would-label or unsorted
// just because two labels applied.
func TestMaterializeTieResolvesToTheWinnerAsLabelDecided(t *testing.T) {
	plan := twoAreaMaterializePlan() // Family (position 0) before Shopping (position 1)
	tie := mrow(false, []string{"Vip", "Deals"}, "", false)

	stats := newMaterializeStats(plan.cfg, false)
	stats.record(plan.cfg, plan, tie)

	family := stats.all.perArea["Family"]
	shopping := stats.all.perArea["Shopping"]
	if family == nil || shopping == nil {
		t.Fatal("missing area counts")
	}
	if family.labelDecided != 1 {
		t.Errorf("Family.labelDecided = %d, want 1 (earlier position wins the tie)", family.labelDecided)
	}
	if shopping.labelDecided != 0 || shopping.wouldLabel != 0 || shopping.already != 0 {
		t.Errorf("Shopping counted the tied row too: %+v, want all zero", shopping)
	}
	if stats.all.total != 1 {
		t.Errorf("stats.all.total = %d, want 1 (one row, decided once)", stats.all.total)
	}
}

// TestMaterializeInboxIsASubsetOfAll mirrors areareport_test.go's own check:
// a row only folds into the inbox population when InInbox is true, and every
// inbox row also reaches all mail.
func TestMaterializeInboxIsASubsetOfAll(t *testing.T) {
	plan := twoAreaMaterializePlan()
	rows := []store.AreaReportRow{
		mrow(true, nil, tabs.Promotions, true),
		mrow(false, nil, tabs.Promotions, true),
		mrow(false, nil, "", false),
	}
	stats := newMaterializeStats(plan.cfg, false)
	for _, r := range rows {
		stats.record(plan.cfg, plan, r)
	}
	if stats.all.total != 3 {
		t.Fatalf("all.total = %d, want 3", stats.all.total)
	}
	if stats.inbox.total != 1 {
		t.Fatalf("inbox.total = %d, want 1 (only the InInbox row)", stats.inbox.total)
	}
	if stats.inbox.perArea["Shopping"].wouldLabel != 1 {
		t.Errorf("inbox Shopping.wouldLabel = %d, want 1", stats.inbox.perArea["Shopping"].wouldLabel)
	}
	if stats.all.perArea["Shopping"].wouldLabel != 2 {
		t.Errorf("all Shopping.wouldLabel = %d, want 2", stats.all.perArea["Shopping"].wouldLabel)
	}
}

// TestHasLabelNamedIsCaseInsensitive: a label written by hand will not
// reliably match an own label's stored name byte-for-byte (the report-before-
// the-fix posture this whole file follows), so the already/would-label split
// must not depend on exact case.
func TestHasLabelNamedIsCaseInsensitive(t *testing.T) {
	if !hasLabelNamed([]string{"  Shopping  "}, "shopping") {
		t.Error("hasLabelNamed should match case-insensitively and ignore surrounding whitespace")
	}
	if hasLabelNamed([]string{"Family"}, "shopping") {
		t.Error("hasLabelNamed matched an unrelated label")
	}
}

// TestPrintMaterializeReportDoesNotPanic is a smoke test over the printer:
// nothing here asserts on stdout content (areareport_test.go does not either,
// for the same reason — the printer is a thin renderer over data the tests
// above already pin), but a nil per-area count or a mirroring lookup miss
// must not panic on a real, if small, plan.
func TestPrintMaterializeReportDoesNotPanic(t *testing.T) {
	plan := twoAreaMaterializePlan()
	stats := newMaterializeStats(plan.cfg, true)
	stats.record(plan.cfg, plan, mrow(true, nil, tabs.Promotions, true))
	stats.record(plan.cfg, plan, mrow(false, []string{"Vip"}, "", false))
	printMaterializeReport(plan, stats, true)
}
