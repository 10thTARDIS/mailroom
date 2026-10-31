// SPDX-License-Identifier: AGPL-3.0-only

package main

// diffAreaPlan is a pure function over stored areas.AreaViews + a resolved
// label-name map and a parsed plan — no database — covering create / keep /
// feeds add+remove / a feed MOVING between two stored areas / an area stored
// but not in the plan.

import (
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/areas"
	"mailroom/internal/store"
	"mailroom/internal/tabs"
)

func areaView(id ulid.ULID, name string, position int, ownLabel ulid.ULID, feeds ...store.AreaFeed) store.AreaView {
	all := append([]store.AreaFeed{{LabelID: &ownLabel}}, feeds...)
	return store.AreaView{ID: id, Name: name, Position: position, LabelID: ownLabel, Feeds: all}
}

func tabFeed(t tabs.Tab) store.AreaFeed     { return store.AreaFeed{Tab: t} }
func labelFeed(id ulid.ULID) store.AreaFeed { lid := id; return store.AreaFeed{LabelID: &lid} }

func TestDiffAreaPlanCreate(t *testing.T) {
	plan := areas.Config{Areas: []areas.Area{{Name: "Notices", Labels: []string{"Shopping", "Information"}}}}
	diff := diffAreaPlan(nil, nil, plan)
	if len(diff.Lines) != 1 || diff.Lines[0].Kind != planLineCreate {
		t.Fatalf("Lines = %+v, want one Create line", diff.Lines)
	}
	if diff.Lines[0].Name != "Notices" {
		t.Errorf("Name = %q, want Notices", diff.Lines[0].Name)
	}
	if len(diff.StoredNotInPlan) != 0 {
		t.Errorf("StoredNotInPlan = %+v, want none", diff.StoredNotInPlan)
	}
}

func TestDiffAreaPlanKeepWhenIdentical(t *testing.T) {
	personalID, ownLabel := store.NewULID(), store.NewULID()
	stored := []store.AreaView{areaView(personalID, "Personal", 0, ownLabel)}
	plan := areas.Config{Areas: []areas.Area{{Name: "Personal", Labels: []string{"Personal"}}}}
	labelNames := map[ulid.ULID]string{ownLabel: "Personal"}

	diff := diffAreaPlan(stored, labelNames, plan)
	if len(diff.Lines) != 1 || diff.Lines[0].Kind != planLineKeep {
		t.Fatalf("Lines = %+v, want one Keep line (identical plan/stored)", diff.Lines)
	}
}

func TestDiffAreaPlanFeedsAddAndRemove(t *testing.T) {
	newsID, ownLabel, eventsID := store.NewULID(), store.NewULID(), store.NewULID()
	stored := []store.AreaView{
		areaView(newsID, "News", 0, ownLabel, tabFeed(tabs.Updates), labelFeed(eventsID)),
	}
	labelNames := map[ulid.ULID]string{ownLabel: "News", eventsID: "Events"}
	plan := areas.Config{Areas: []areas.Area{
		{Name: "News", Tabs: []tabs.Tab{tabs.Forums}, Labels: []string{"News"}},
	}}

	diff := diffAreaPlan(stored, labelNames, plan)
	if len(diff.Lines) != 1 || diff.Lines[0].Kind != planLineUpdate {
		t.Fatalf("Lines = %+v, want one Update line", diff.Lines)
	}
	l := diff.Lines[0]
	if len(l.AddTabs) != 1 || l.AddTabs[0] != tabs.Forums {
		t.Errorf("AddTabs = %v, want [forums]", l.AddTabs)
	}
	if len(l.RemoveTabs) != 1 || l.RemoveTabs[0] != tabs.Updates {
		t.Errorf("RemoveTabs = %v, want [updates]", l.RemoveTabs)
	}
	if len(l.RemoveLabels) != 1 || l.RemoveLabels[0] != "Events" {
		t.Errorf("RemoveLabels = %v, want [Events]", l.RemoveLabels)
	}
	if len(l.AddLabels) != 0 {
		t.Errorf("AddLabels = %v, want none (News names its own label, already present)", l.AddLabels)
	}
}

// TestDiffAreaPlanOwnLabelIsNeverARemove: a plan line that drops the area's
// own label name from its `label:` list must not print it as a removal — the
// own label is the area's identity, not a feed a plan edit can take away.
func TestDiffAreaPlanOwnLabelIsNeverARemove(t *testing.T) {
	id, ownLabel := store.NewULID(), store.NewULID()
	stored := []store.AreaView{areaView(id, "Focus", 0, ownLabel)}
	labelNames := map[ulid.ULID]string{ownLabel: "Focus"}
	// The plan says nothing about Focus's own label at all — a very ordinary
	// plan line, since the own label does not need restating.
	plan := areas.Config{Areas: []areas.Area{{Name: "Focus", Tabs: []tabs.Tab{tabs.Primary}}}}

	diff := diffAreaPlan(stored, labelNames, plan)
	if len(diff.Lines) != 1 {
		t.Fatalf("Lines = %+v, want one line", diff.Lines)
	}
	for _, rm := range diff.Lines[0].RemoveLabels {
		if rm == "Focus" {
			t.Fatalf("the own label was listed as a removal: %+v", diff.Lines[0])
		}
	}
}

// TestDiffAreaPlanMoveBetweenAreas: a feed the plan gives to one area while
// another stored area still holds it prints as `-` on the loser and `+` on
// the winner — the two independent per-area diffs are enough to produce a
// move with no special-casing.
func TestDiffAreaPlanMoveBetweenAreas(t *testing.T) {
	newsID, moviesID := store.NewULID(), store.NewULID()
	newsLabel, moviesLabel, eventsID := store.NewULID(), store.NewULID(), store.NewULID()
	stored := []store.AreaView{
		areaView(newsID, "News", 0, newsLabel, labelFeed(eventsID)),
		areaView(moviesID, "Movies", 1, moviesLabel),
	}
	labelNames := map[ulid.ULID]string{newsLabel: "News", moviesLabel: "Movies", eventsID: "Events"}
	plan := areas.Config{Areas: []areas.Area{
		{Name: "News", Labels: []string{"News"}},
		{Name: "Movies", Labels: []string{"Movies", "Events"}},
	}}

	diff := diffAreaPlan(stored, labelNames, plan)
	if len(diff.Lines) != 2 {
		t.Fatalf("Lines = %+v, want two lines", diff.Lines)
	}
	byName := map[string]areaPlanLine{}
	for _, l := range diff.Lines {
		byName[l.Name] = l
	}
	news := byName["News"]
	if news.Kind != planLineUpdate || len(news.RemoveLabels) != 1 || news.RemoveLabels[0] != "Events" {
		t.Errorf("News = %+v, want an Update removing Events", news)
	}
	movies := byName["Movies"]
	if movies.Kind != planLineUpdate || len(movies.AddLabels) != 1 || movies.AddLabels[0] != "Events" {
		t.Errorf("Movies = %+v, want an Update adding Events", movies)
	}
}

// TestDiffAreaPlanStoredNotInPlanIsListedNotTouched: an area the plan does
// not mention at all is neither Create nor Update — it is named separately,
// and diffAreaPlan itself never mutates anything (it is pure), so "listed"
// is the only thing to assert.
func TestDiffAreaPlanStoredNotInPlanIsListedNotTouched(t *testing.T) {
	keepID, ownLabel := store.NewULID(), store.NewULID()
	stored := []store.AreaView{areaView(keepID, "Archive Only", 0, ownLabel)}
	labelNames := map[ulid.ULID]string{ownLabel: "Archive Only"}
	plan := areas.Config{Areas: []areas.Area{{Name: "Something Else", Tabs: []tabs.Tab{tabs.Primary}}}}

	diff := diffAreaPlan(stored, labelNames, plan)
	if len(diff.Lines) != 1 || diff.Lines[0].Kind != planLineCreate {
		t.Fatalf("Lines = %+v, want one Create (Something Else)", diff.Lines)
	}
	if len(diff.StoredNotInPlan) != 1 || diff.StoredNotInPlan[0].Name != "Archive Only" {
		t.Fatalf("StoredNotInPlan = %+v, want [Archive Only]", diff.StoredNotInPlan)
	}
}

// TestDiffAreaPlanPositionChangeAloneIsAnUpdate: identical feeds but a
// different line order must still be an Update (the store's position needs
// to move to match the plan's own order), not a Keep.
func TestDiffAreaPlanPositionChangeAloneIsAnUpdate(t *testing.T) {
	id, ownLabel := store.NewULID(), store.NewULID()
	stored := []store.AreaView{areaView(id, "Solo", 3, ownLabel)}
	labelNames := map[ulid.ULID]string{ownLabel: "Solo"}
	plan := areas.Config{Areas: []areas.Area{{Name: "Solo", Labels: []string{"Solo"}}}}

	diff := diffAreaPlan(stored, labelNames, plan)
	if len(diff.Lines) != 1 || diff.Lines[0].Kind != planLineUpdate {
		t.Fatalf("Lines = %+v, want one Update (position 3 -> 0)", diff.Lines)
	}
	if diff.Lines[0].Position != 0 {
		t.Errorf("Position = %d, want 0 (the plan's own line order)", diff.Lines[0].Position)
	}
}

// TestDiffAreaPlanStripsFeedsTheDefaultPlanHolds: the case the operator hits
// on day one. The default plan (one area per tab) is materialised on first
// contact, so the stored Primary and Updates areas already hold tab:primary
// and tab:updates. A plan that gives those tabs to Personal and News, and
// never mentions Primary or Updates, must take the tabs away from the kept
// areas (which stay) — otherwise pass 3 hits ErrFeedTaken against an area
// the plan cannot even name, and the first real plan can never be applied.
func TestDiffAreaPlanStripsFeedsTheDefaultPlanHolds(t *testing.T) {
	primaryID, updatesID := store.NewULID(), store.NewULID()
	primaryLabel, updatesLabel := store.NewULID(), store.NewULID()
	stored := []store.AreaView{
		areaView(primaryID, "Primary", 0, primaryLabel, tabFeed(tabs.Primary)),
		areaView(updatesID, "Updates", 3, updatesLabel, tabFeed(tabs.Updates)),
	}
	labelNames := map[ulid.ULID]string{primaryLabel: "Primary", updatesLabel: "Updates"}
	plan := areas.Config{Areas: []areas.Area{
		{Name: "Personal", Tabs: []tabs.Tab{tabs.Primary}},
		{Name: "News", Tabs: []tabs.Tab{tabs.Updates, tabs.Forums}},
	}}

	diff := diffAreaPlan(stored, labelNames, plan)
	if len(diff.Lines) != 2 || diff.Lines[0].Kind != planLineCreate || diff.Lines[1].Kind != planLineCreate {
		t.Fatalf("Lines = %+v, want two Creates", diff.Lines)
	}
	if len(diff.StoredNotInPlan) != 2 {
		t.Fatalf("StoredNotInPlan = %+v, want Primary and Updates still listed (kept, not deleted)", diff.StoredNotInPlan)
	}
	if len(diff.Strips) != 2 {
		t.Fatalf("Strips = %+v, want Primary and Updates each giving up one tab", diff.Strips)
	}
	byName := map[string]areaPlanStrip{}
	for _, s := range diff.Strips {
		byName[s.View.Name] = s
	}
	p := byName["Primary"]
	if len(p.RemoveTabs) != 1 || p.RemoveTabs[0] != tabs.Primary || p.MovedTo["tab:primary"] != "Personal" {
		t.Errorf("Primary strip = %+v, want -tab:primary moved to Personal", p)
	}
	u := byName["Updates"]
	if len(u.RemoveTabs) != 1 || u.RemoveTabs[0] != tabs.Updates || u.MovedTo["tab:updates"] != "News" {
		t.Errorf("Updates strip = %+v, want -tab:updates moved to News", u)
	}
	if len(diff.Conflicts) != 0 {
		t.Errorf("Conflicts = %v, want none", diff.Conflicts)
	}
}

// TestDiffAreaPlanOwnLabelOfKeptAreaIsAConflict: a plan cannot take a kept
// area's OWN label as a feed — the own label is that area's identity. The
// diff must say so up front so --apply refuses before writing anything,
// instead of failing on the last statement of a transaction.
func TestDiffAreaPlanOwnLabelOfKeptAreaIsAConflict(t *testing.T) {
	focusID, focusLabel := store.NewULID(), store.NewULID()
	stored := []store.AreaView{areaView(focusID, "Focus", 0, focusLabel)}
	labelNames := map[ulid.ULID]string{focusLabel: "Focus"}
	plan := areas.Config{Areas: []areas.Area{{Name: "Deep Work", Labels: []string{"Focus"}}}}

	diff := diffAreaPlan(stored, labelNames, plan)
	if len(diff.Conflicts) != 1 || !strings.Contains(diff.Conflicts[0], "Focus") || !strings.Contains(diff.Conflicts[0], "Deep Work") {
		t.Fatalf("Conflicts = %v, want one naming the label, the claiming area and the kept area", diff.Conflicts)
	}
	if len(diff.Strips) != 0 {
		t.Errorf("Strips = %+v, want none: an own label is never stripped", diff.Strips)
	}
}
