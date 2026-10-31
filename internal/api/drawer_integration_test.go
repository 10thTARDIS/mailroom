// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// The JMAP surface of the drawer auto-archive sweep (#443): Area/getSummary's
// archivedByDrawer count, Email.drawerArchivedAt, and Area/set round-tripping
// autoArchive/autoArchiveDays. The store suite (internal/store) covers the
// sweep's own SQL-level guarantees; this file covers what a CLIENT sees.

import (
	"testing"
)

func TestAreaGetSummaryShowsArchivedByDrawer(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	res := call(t, h.api.areaSet, a.UserID, map[string]any{
		"create": map[string]any{"work": map[string]any{"name": "Work"}},
	})
	work, ok := res["created"].(map[string]any)["work"].(map[string]any)
	if !ok {
		t.Fatalf("Work area create failed: %#v", res)
	}
	workID, _ := work["id"].(string)

	// A drawer_archives row is all Area/getSummary's archivedByDrawer needs —
	// it is an all-time count over that table, independent of live mail state.
	mustExecAPI(t, h, `INSERT INTO app.drawer_archives (user_id, message_id, area_id, area_name, period_days)
		VALUES ($1, $2, $3, 'Work', 30)`, a.UserID.String(), a.MessageID.String(), workID)

	summary := call(t, h.api.areaGetSummary, a.UserID, map[string]any{})
	list, _ := summary["list"].([]any)

	var workRow, unsortedRow map[string]any
	for _, item := range list {
		obj, _ := item.(map[string]any)
		if id, _ := obj["areaId"].(string); id == workID {
			workRow = obj
		}
		if obj["areaId"] == nil {
			unsortedRow = obj
		}
	}
	if workRow == nil {
		t.Fatal("Work area missing from the summary")
	}
	if n, _ := workRow["archivedByDrawer"].(float64); n != 1 {
		t.Errorf("Work.archivedByDrawer = %v, want 1", workRow["archivedByDrawer"])
	}
	if unsortedRow == nil {
		t.Fatal("the Needs-sorting row is missing from the summary")
	}
	if n, _ := unsortedRow["archivedByDrawer"].(float64); n != 0 {
		t.Errorf("Needs-sorting.archivedByDrawer = %v, want 0 — nothing sweeps unsorted mail", unsortedRow["archivedByDrawer"])
	}
}

func TestEmailGetDrawerArchivedAt(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	res := call(t, h.api.areaSet, a.UserID, map[string]any{
		"create": map[string]any{"work": map[string]any{"name": "Work"}},
	})
	work, ok := res["created"].(map[string]any)["work"].(map[string]any)
	if !ok {
		t.Fatalf("Work area create failed: %#v", res)
	}
	workID, _ := work["id"].(string)

	// Before any sweep: null.
	got := call(t, h.api.emailGet, a.UserID, map[string]any{
		"ids":        []string{a.MessageID.String()},
		"properties": []string{"id", "drawerArchivedAt"},
	})
	obj := got["list"].([]any)[0].(map[string]any)
	if obj["drawerArchivedAt"] != nil {
		t.Fatalf("drawerArchivedAt on a never-swept message = %v, want nil", obj["drawerArchivedAt"])
	}

	mustExecAPI(t, h, `INSERT INTO app.drawer_archives (user_id, message_id, area_id, area_name, period_days)
		VALUES ($1, $2, $3, 'Work', 30)`, a.UserID.String(), a.MessageID.String(), workID)

	got = call(t, h.api.emailGet, a.UserID, map[string]any{
		"ids":        []string{a.MessageID.String()},
		"properties": []string{"id", "drawerArchivedAt"},
	})
	obj = got["list"].([]any)[0].(map[string]any)
	at, ok := obj["drawerArchivedAt"].(string)
	if !ok || at == "" {
		t.Fatalf("drawerArchivedAt after a sweep = %v, want a UTCDate string", obj["drawerArchivedAt"])
	}

	// Absent from `properties` entirely: not even present as a key, like every
	// other opt-in Mailroom Email extension.
	got = call(t, h.api.emailGet, a.UserID, map[string]any{
		"ids":        []string{a.MessageID.String()},
		"properties": []string{"id"},
	})
	obj = got["list"].([]any)[0].(map[string]any)
	if _, present := obj["drawerArchivedAt"]; present {
		t.Errorf("drawerArchivedAt present without being requested: %v", obj)
	}
}

// drawerArchivedAt is SERVER-DERIVED and READ-ONLY, exactly like flaggedAt
// (#444) and area (#442): a patch naming it falls through the same generic
// "unsupported Email property in patch" refusal every other read-only
// Mailroom Email extension uses, not a bespoke check.
func TestEmailSetRefusesDrawerArchivedAtInPatch(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	res := call(t, h.api.emailSet, a.UserID, map[string]any{
		"update": map[string]any{
			a.MessageID.String(): map[string]any{"drawerArchivedAt": "2026-01-01T00:00:00Z"},
		},
	})
	se, ok := res["notUpdated"].(map[string]any)[a.MessageID.String()]
	if !ok {
		t.Fatalf("patch naming drawerArchivedAt accepted: %v", res)
	}
	if typ := se.(map[string]any)["type"]; typ != setInvalidProps {
		t.Errorf("error type = %v, want %s", typ, setInvalidProps)
	}
}

func TestAreaSetRoundTripsAutoArchive(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	res := call(t, h.api.areaSet, a.UserID, map[string]any{
		"create": map[string]any{
			"work": map[string]any{"name": "Work", "autoArchive": false, "autoArchiveDays": 45},
		},
	})
	work, ok := res["created"].(map[string]any)["work"].(map[string]any)
	if !ok {
		t.Fatalf("Work area create failed: %#v", res)
	}
	workID, _ := work["id"].(string)
	if got, _ := work["autoArchive"].(bool); got != false {
		t.Errorf("create autoArchive = %v, want false", work["autoArchive"])
	}
	if got, _ := work["autoArchiveDays"].(float64); got != 45 {
		t.Errorf("create autoArchiveDays = %v, want 45", work["autoArchiveDays"])
	}

	// A create that OMITS autoArchive defaults to true (store.AreaRow's own
	// nil -> true default).
	res = call(t, h.api.areaSet, a.UserID, map[string]any{
		"create": map[string]any{"plain": map[string]any{"name": "Plain"}},
	})
	plain, ok := res["created"].(map[string]any)["plain"].(map[string]any)
	if !ok {
		t.Fatalf("Plain area create failed: %#v", res)
	}
	if got, _ := plain["autoArchive"].(bool); got != true {
		t.Errorf("create with no autoArchive = %v, want true (the default)", plain["autoArchive"])
	}
	if plain["autoArchiveDays"] != nil {
		t.Errorf("create with no autoArchiveDays = %v, want null (the login default)", plain["autoArchiveDays"])
	}

	// Update: flip autoArchive to true and autoArchiveDays back to null (the
	// login default) — the round trip the spec asks for.
	res = call(t, h.api.areaSet, a.UserID, map[string]any{
		"update": map[string]any{
			workID: map[string]any{"autoArchive": true, "autoArchiveDays": nil},
		},
	})
	if _, ok := res["updated"].(map[string]any)[workID]; !ok {
		t.Fatalf("autoArchive/autoArchiveDays update refused: %#v", res)
	}

	got := call(t, h.api.areaGet, a.UserID, map[string]any{"ids": []string{workID}})
	list, _ := got["list"].([]any)
	if len(list) != 1 {
		t.Fatalf("Area/get by id returned %d objects, want 1", len(list))
	}
	obj := list[0].(map[string]any)
	if v, _ := obj["autoArchive"].(bool); v != true {
		t.Errorf("autoArchive after update = %v, want true", obj["autoArchive"])
	}
	if obj["autoArchiveDays"] != nil {
		t.Errorf("autoArchiveDays after clearing to null = %v, want null", obj["autoArchiveDays"])
	}
}
