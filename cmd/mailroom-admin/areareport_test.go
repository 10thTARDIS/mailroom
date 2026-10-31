// SPDX-License-Identifier: AGPL-3.0-only

package main

// The area report's arithmetic is the thing #442 ships FIRST (CLAUDE.md:
// build the report before the fix), so it is tested the same way tabStats is
// (tabs_test.go): hand-built rows, no database, because a report whose parts
// silently fail to sum to the whole is worse than one that never ran.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mailroom/internal/areas"
	"mailroom/internal/store"
	"mailroom/internal/tabs"
)

// twoAreaFixture is deliberately narrower than areas.Default(): only
// Promotions is fed by a tab, so Updates/Social/Forums/Primary all fall
// through to "tab feeds no area" — the one reason Default() can never
// exercise, since Default feeds every tab by construction.
func twoAreaFixture() areas.Config {
	return areas.Config{Areas: []areas.Area{
		{Name: "Family", Labels: []string{"Family", "Friends"}},
		{Name: "Shopping", Tabs: []tabs.Tab{tabs.Promotions}, Labels: []string{"Deals"}},
	}}
}

// row builds a store.AreaReportRow with the resolved tab driven through
// MappedTab: ResolvedTab() prefers the mapped half over the classified half
// exactly like the real resolver, so setting MappedTab here exercises the
// same code path a real mapped-label row would.
func row(inInbox bool, labels []string, tab tabs.Tab, tabKnown bool, score float32, scored bool) store.AreaReportRow {
	r := store.AreaReportRow{ID: store.NewULID(), InInbox: inInbox, Labels: labels, Score: score, Scored: scored}
	if tabKnown {
		r.MappedTab, r.MappedKnown = tab, true
	}
	return r
}

// runFixture resolves every row in rows against cfg and returns the stats,
// mirroring exactly what runAreaReport's page loop does.
func runFixture(cfg areas.Config, threshold float32, rows []store.AreaReportRow) *areaStats {
	s := newAreaStats(cfg)
	for _, r := range rows {
		s.record(r, cfg, threshold)
	}
	return s
}

// reconcile asserts the two invariants the report's footer promises: every
// area's total plus every unsorted reason's count sums to the population
// total, and within each area surface+drawer+unscored sums to its total.
func reconcile(t *testing.T, label string, cfg areas.Config, p *areaPopulation) {
	t.Helper()
	sum := 0
	for _, a := range cfg.Areas {
		c := p.perArea[a.Name]
		if c == nil {
			continue
		}
		sum += c.total
		if got := c.surface + c.drawer + c.unscored; got != c.total {
			t.Errorf("%s/%s: surface(%d)+drawer(%d)+unscored(%d) = %d, want total %d",
				label, a.Name, c.surface, c.drawer, c.unscored, got, c.total)
		}
		if got := c.byLabel + c.byTab; got != c.total {
			t.Errorf("%s/%s: byLabel(%d)+byTab(%d) = %d, want total %d",
				label, a.Name, c.byLabel, c.byTab, got, c.total)
		}
	}
	for _, n := range p.unsorted {
		sum += n
	}
	if sum != p.total {
		t.Errorf("%s: areas+unsorted = %d, want population total %d", label, sum, p.total)
	}
	if got := p.scored + p.unscored; got != p.total {
		t.Errorf("%s: scored(%d)+unscored(%d) = %d, want total %d", label, p.scored, p.unscored, got, p.total)
	}
}

func TestAreaStatsReconcileUnderDefault(t *testing.T) {
	cfg := areas.Default()
	rows := []store.AreaReportRow{
		row(true, nil, tabs.Primary, true, 0.9, true),
		row(false, nil, tabs.Social, true, 0.1, true),
		row(true, []string{"whatever"}, "", false, 0, false),
		row(false, nil, "", false, 0, false),
	}
	s := runFixture(cfg, 0.5, rows)
	reconcile(t, "inbox", cfg, s.inbox)
	reconcile(t, "all", cfg, s.all)
}

func TestAreaStatsReconcileUnderTwoAreaPlan(t *testing.T) {
	cfg := twoAreaFixture()
	rows := []store.AreaReportRow{
		row(true, []string{"Family"}, "", false, 0.7, true),
		row(true, []string{"Deals", "Family"}, "", false, 0.2, true), // a tie
		row(false, nil, tabs.Promotions, true, 0, false),
		row(false, nil, tabs.Updates, true, 0, false), // tab feeds nothing
		row(false, []string{"Random"}, tabs.Social, true, 0, false),
		row(false, nil, "", false, 0, false),
	}
	s := runFixture(cfg, 0.5, rows)
	reconcile(t, "inbox", cfg, s.inbox)
	reconcile(t, "all", cfg, s.all)
}

// TestAreaStatsInboxIsASubsetOfAll: every message that reaches the inbox
// population also reaches "all mail" — a report that let inbox drift above
// or sideways from all would be counting messages twice or from nowhere.
func TestAreaStatsInboxIsASubsetOfAll(t *testing.T) {
	cfg := twoAreaFixture()
	rows := []store.AreaReportRow{
		row(true, []string{"Family"}, "", false, 0.9, true),
		row(true, nil, tabs.Promotions, true, 0, false),
		row(false, []string{"Family"}, "", false, 0.9, true),
		row(false, nil, "", false, 0, false),
	}
	s := runFixture(cfg, 0.5, rows)

	// Exact, not merely <=: two of the four rows are in the inbox, so the inbox
	// population must be exactly 2 and all mail exactly 4. A "subset" check
	// alone stayed green when every row was counted into the inbox (probed
	// while writing this), which is the one regression this test exists for.
	if s.all.total != 4 {
		t.Fatalf("all.total = %d, want 4", s.all.total)
	}
	if s.inbox.total != 2 {
		t.Fatalf("inbox.total = %d, want 2 (only rows with InInbox count)", s.inbox.total)
	}
	if got := s.inbox.perArea["Family"].total; got != 1 {
		t.Errorf("inbox Family = %d, want 1", got)
	}
	if got := s.all.perArea["Family"].total; got != 2 {
		t.Errorf("all Family = %d, want 2", got)
	}
	for name, c := range s.inbox.perArea {
		allC := s.all.perArea[name]
		if allC == nil || c.total > allC.total {
			t.Errorf("area %q: inbox total %d exceeds all-mail total", name, c.total)
		}
	}
	for reason, n := range s.inbox.unsorted {
		if n > s.all.unsorted[reason] {
			t.Errorf("reason %v: inbox count %d exceeds all-mail count %d", reason, n, s.all.unsorted[reason])
		}
	}
}

// TestAreaStatsSeparatesTheThreeUnsortedReasons: the whole point of splitting
// them is that a single "unresolved" number cannot distinguish three
// populations with three different fixes.
func TestAreaStatsSeparatesTheThreeUnsortedReasons(t *testing.T) {
	cfg := twoAreaFixture()
	rows := []store.AreaReportRow{
		row(false, nil, "", false, 0, false),                        // no label, no verdict
		row(false, []string{"Random"}, "", false, 0, false),         // labelled, feeds nothing
		row(false, []string{"Random"}, tabs.Social, true, 0, false), // labelled wins priority over tab
		row(false, nil, tabs.Updates, true, 0, false),               // tab known, feeds nothing
	}
	s := runFixture(cfg, 0.5, rows)

	if got := s.all.unsorted[areas.UnsortedNoLabelNoVerdict]; got != 1 {
		t.Errorf("UnsortedNoLabelNoVerdict = %d, want 1", got)
	}
	if got := s.all.unsorted[areas.UnsortedLabelsFeedNothing]; got != 2 {
		t.Errorf("UnsortedLabelsFeedNothing = %d, want 2 (a labelled message with a non-feeding tab still counts here)", got)
	}
	if got := s.all.unsorted[areas.UnsortedTabFeedsNothing]; got != 1 {
		t.Errorf("UnsortedTabFeedsNothing = %d, want 1", got)
	}
	reconcile(t, "all", cfg, s.all)
}

// TestAreaStatsSurfaceDrawerUnscoredPartition: the threshold boundary is
// inclusive (score == threshold is SURFACE, matching the report header's own
// "surface = score >= threshold"), and an unscored row is neither.
func TestAreaStatsSurfaceDrawerUnscoredPartition(t *testing.T) {
	cfg := twoAreaFixture()
	rows := []store.AreaReportRow{
		row(false, []string{"Family"}, "", false, 0.5, true),  // exactly at threshold: surface
		row(false, []string{"Family"}, "", false, 0.49, true), // just under: drawer
		row(false, []string{"Family"}, "", false, 0, false),   // unscored
	}
	s := runFixture(cfg, 0.5, rows)
	c := s.all.perArea["Family"]
	if c == nil {
		t.Fatal("no Family area count")
	}
	if c.surface != 1 {
		t.Errorf("surface = %d, want 1 (score == threshold counts as surface)", c.surface)
	}
	if c.drawer != 1 {
		t.Errorf("drawer = %d, want 1", c.drawer)
	}
	if c.unscored != 1 {
		t.Errorf("unscored = %d, want 1", c.unscored)
	}
	reconcile(t, "all", cfg, s.all)
}

// TestAreaStatsCountsATieWithTheRightKey pins the exact "Winner over Loser"
// string the report prints, not just that some count went up.
func TestAreaStatsCountsATieWithTheRightKey(t *testing.T) {
	cfg := twoAreaFixture()
	rows := []store.AreaReportRow{
		row(false, []string{"Deals", "Family"}, "", false, 0, false),
		row(false, []string{"Family", "Deals"}, "", false, 0, false), // order on the message must not matter
	}
	s := runFixture(cfg, 0.5, rows)
	if s.all.contestedTotal != 2 {
		t.Errorf("contestedTotal = %d, want 2", s.all.contestedTotal)
	}
	if got := s.all.contested["Family over Shopping"]; got != 2 {
		t.Errorf(`contested["Family over Shopping"] = %d, want 2; have %v`, got, s.all.contested)
	}
}

// TestAreaStatsNegativeControlNoTies: a fixture with no overlapping labels
// must leave contestedTotal at zero — proving the counter above can actually
// observe zero rather than always firing.
func TestAreaStatsNegativeControlNoTies(t *testing.T) {
	cfg := twoAreaFixture()
	rows := []store.AreaReportRow{
		row(false, []string{"Family"}, "", false, 0, false),
		row(false, []string{"Friends"}, "", false, 0, false),
		row(false, nil, tabs.Promotions, true, 0, false),
	}
	s := runFixture(cfg, 0.5, rows)
	if s.all.contestedTotal != 0 {
		t.Errorf("contestedTotal = %d, want 0 (no message's labels span two areas)", s.all.contestedTotal)
	}
	if len(s.all.contested) != 0 {
		t.Errorf("contested = %v, want empty", s.all.contested)
	}
}

// ---- buildAreaConfig ----

func TestBuildAreaConfigNoPlanUsesDefault(t *testing.T) {
	cfg, errs, err := buildAreaConfig("", nil)
	if err != nil {
		t.Fatalf("buildAreaConfig: %v", err)
	}
	if len(errs) != 0 {
		t.Fatalf("errs = %v, want none", errs)
	}
	if len(cfg.Areas) != 5 {
		t.Errorf("len(cfg.Areas) = %d, want 5 (areas.Default())", len(cfg.Areas))
	}
}

func TestBuildAreaConfigGoodPlanFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plan.txt")
	if err := os.WriteFile(path, []byte("Shopping = tab:promotions, label:Deals\n"), 0o600); err != nil {
		t.Fatalf("write plan: %v", err)
	}
	cfg, errs, err := buildAreaConfig(path, []string{"Deals"})
	if err != nil {
		t.Fatalf("buildAreaConfig: %v", err)
	}
	if len(errs) != 0 {
		t.Fatalf("errs = %v, want none", errs)
	}
	if len(cfg.Areas) != 1 || cfg.Areas[0].Name != "Shopping" {
		t.Errorf("cfg.Areas = %+v, want one area named Shopping", cfg.Areas)
	}
}

func TestBuildAreaConfigBadLineYieldsLineErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plan.txt")
	if err := os.WriteFile(path, []byte("Shopping = tab:not-a-tab\n"), 0o600); err != nil {
		t.Fatalf("write plan: %v", err)
	}
	cfg, errs, err := buildAreaConfig(path, nil)
	if err != nil {
		t.Fatalf("buildAreaConfig: unexpected top-level error: %v", err)
	}
	if len(errs) != 1 {
		t.Fatalf("errs = %v, want exactly 1", errs)
	}
	if len(cfg.Areas) != 0 {
		t.Errorf("cfg.Areas = %+v, want none", cfg.Areas)
	}
}

func TestBuildAreaConfigMissingFileIsAnError(t *testing.T) {
	_, _, err := buildAreaConfig(filepath.Join(t.TempDir(), "does-not-exist.txt"), nil)
	if err == nil {
		t.Fatal("buildAreaConfig: err = nil, want an error for a missing file")
	}
}

// TestUnfedLabelLinesSeparatesIndirectFromGap: on the first real run the
// section listed a large label as "not in any area" although every
// one of them reached an area through the label's tab. A reader must be able to
// tell "indirect, fine" from "gap, act" without knowing the mapping.
func TestUnfedLabelLinesSeparatesIndirectFromGap(t *testing.T) {
	cfg := areas.Config{Areas: []areas.Area{
		{Name: "Promotions", Tabs: []tabs.Tab{tabs.Promotions}},
		{Name: "Notices", Labels: []string{"Shopping"}},
	}}
	labels := []store.LabelView{
		{Name: "Promotions", Tab: tabs.Promotions}, // indirect: its tab feeds an area
		{Name: "Shopping"},                         // fed directly: must not appear at all
		{Name: "Tech"},                             // gap: no tab
		{Name: "Forums stuff", Tab: tabs.Forums},   // gap: its tab feeds nothing here
	}
	counts := map[string]int{"Promotions": 827, "Shopping": 201, "Tech": 55, "Forums stuff": 3}

	viaTab, none := unfedLabelLines(cfg, labels, counts)

	if len(viaTab) != 1 || !strings.Contains(viaTab[0], "Promotions") || !strings.Contains(viaTab[0], "reaches Promotions via tab:promotions") {
		t.Errorf("viaTab = %q, want one Promotions line saying it reaches the area via its tab", viaTab)
	}
	if len(none) != 2 {
		t.Fatalf("none = %q, want exactly the two real gaps", none)
	}
	if !strings.Contains(none[0], "Tech") || !strings.Contains(none[0], "no tab") {
		t.Errorf("none[0] = %q, want Tech marked as having no tab", none[0])
	}
	if !strings.Contains(none[1], "Forums stuff") || !strings.Contains(none[1], "which no area feeds") {
		t.Errorf("none[1] = %q, want the forums-mapped label marked as a tab gap", none[1])
	}
	for _, line := range append(viaTab, none...) {
		if strings.Contains(line, "Shopping") {
			t.Errorf("a directly fed label must not be listed: %q", line)
		}
	}
}
