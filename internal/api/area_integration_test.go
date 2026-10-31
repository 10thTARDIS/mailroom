// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// Area/* over the wire (#442 increment 2), mirroring category_tab_integration_
// test.go's own claims:
//
//  1. THE DEFAULT PLAN EXISTS ON FIRST CONTACT, with a state string that
//     already accounts for it.
//  2. CREATE/UPDATE/DESTROY go through the store's per-mutation savepoints, so
//     a batch with one bad object and one good one still applies the good one
//     — the same savepoint trap category_tab_integration_test.go's header
//     explains.
//  3. THE OWN LABEL OUTLIVES THE AREA: destroying an area never touches the
//     label a message was actually tagged with.

import (
	"strings"
	"testing"

	"mailroom/internal/tabs"
)

// areasByName indexes an Area/get list result by name.
func areasByName(t *testing.T, res map[string]any) map[string]map[string]any {
	t.Helper()
	list, _ := res["list"].([]any)
	out := make(map[string]map[string]any, len(list))
	for _, item := range list {
		obj, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("list item is not an object: %#v", item)
		}
		name, _ := obj["name"].(string)
		out[name] = obj
	}
	return out
}

func TestAreaPlanExistsOnFirstContact(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	res := call(t, h.api.areaGet, a.UserID, map[string]any{})
	list, _ := res["list"].([]any)
	if len(list) != len(tabs.All) {
		t.Fatalf("first Area/get returned %d areas, want %d: %#v", len(list), len(tabs.All), res)
	}
	for i, want := range tabs.All {
		obj, ok := list[i].(map[string]any)
		if !ok {
			t.Fatalf("list[%d] is not an object: %#v", i, list[i])
		}
		wantName := strings.ToUpper(string(want)[:1]) + string(want)[1:]
		if name, _ := obj["name"].(string); name != wantName {
			t.Errorf("area %d name = %q, want %q", i, name, wantName)
		}
		if pos, _ := obj["position"].(float64); int(pos) != i {
			t.Errorf("area %d position = %v, want %d", i, obj["position"], i)
		}
		tabsArr, _ := obj["tabs"].([]any)
		if len(tabsArr) != 1 || tabsArr[0] != string(want) {
			t.Errorf("area %d tabs = %v, want [%q]", i, tabsArr, want)
		}
		labelIDs, _ := obj["labelIds"].([]any)
		if len(labelIDs) != 0 {
			t.Errorf("area %d labelIds = %v, want empty", i, labelIDs)
		}
		if lid, _ := obj["labelId"].(string); lid == "" {
			t.Errorf("area %d has no labelId", i)
		}
	}

	// THE STATE MUST ALREADY COVER THE MATERIALISATION. A second /get with no
	// intervening write must report the same state.
	firstState, _ := res["state"].(string)
	again := call(t, h.api.areaGet, a.UserID, map[string]any{})
	if secondState, _ := again["state"].(string); secondState != firstState {
		t.Errorf("state moved between two identical reads: %q then %q", firstState, secondState)
	}

	changes := call(t, h.api.areaChanges, a.UserID, map[string]any{"sinceState": firstState})
	for _, bucket := range []string{"created", "updated", "destroyed"} {
		if arr, _ := changes[bucket].([]any); len(arr) != 0 {
			t.Errorf("Area/changes since the get's own state reported %d %s: %#v",
				len(arr), bucket, changes)
		}
	}
}

func TestAreaSetCreateUpdateDestroy(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	byName := areasByName(t, call(t, h.api.areaGet, a.UserID, map[string]any{}))
	updatesID, _ := byName["Updates"]["id"].(string)
	if updatesID == "" {
		t.Fatalf("missing default Updates area: %#v", byName)
	}

	// 1. Creating "Notices" claiming a tab the default plan already holds is
	// refused, naming the tab AND the area that holds it.
	res := call(t, h.api.areaSet, a.UserID, map[string]any{
		"create": map[string]any{
			"conflict": map[string]any{"name": "Notices", "tabs": []string{"updates"}},
		},
	})
	notCreated, _ := res["notCreated"].(map[string]any)
	serr, ok := notCreated["conflict"].(map[string]any)
	if !ok {
		t.Fatalf("a create claiming Updates' own tab was not refused: %#v", res)
	}
	if typ, _ := serr["type"].(string); typ != setInvalidProps {
		t.Errorf("refusal type = %q, want %q", typ, setInvalidProps)
	}
	if props, _ := serr["properties"].([]any); len(props) != 1 || props[0] != "tabs" {
		t.Errorf("refusal did not name `tabs`: %#v", serr)
	}
	if desc, _ := serr["description"].(string); !strings.Contains(desc, "Updates") {
		t.Errorf("refusal description does not name the area holding the tab: %q", desc)
	}
	if created, _ := res["created"].(map[string]any); len(created) != 0 {
		t.Errorf("a conflicting area was created: %#v", created)
	}

	// 2. Creating "Notices" with a FRESH label as an additional feed succeeds.
	// The own label is minted separately (not the fresh one).
	labelRes := call(t, h.api.labelSet, a.UserID, map[string]any{
		"create": map[string]any{"vip": map[string]any{"name": "VIP"}},
	})
	vip, ok := labelRes["created"].(map[string]any)["vip"].(map[string]any)
	if !ok {
		t.Fatalf("failed to create the VIP label: %#v", labelRes)
	}
	vipID, _ := vip["id"].(string)

	res = call(t, h.api.areaSet, a.UserID, map[string]any{
		"create": map[string]any{
			"notices": map[string]any{"name": "Notices", "labelIds": []string{vipID}},
		},
	})
	created, ok := res["created"].(map[string]any)["notices"].(map[string]any)
	if !ok {
		t.Fatalf("Notices create failed: %#v", res)
	}
	noticesID, _ := created["id"].(string)
	if noticesID == "" {
		t.Fatalf("created Notices has no id: %#v", created)
	}
	labelIDs, _ := created["labelIds"].([]any)
	if len(labelIDs) != 1 || labelIDs[0] != vipID {
		t.Errorf("Notices labelIds = %v, want [%s]", labelIDs, vipID)
	}
	ownLabelID, _ := created["labelId"].(string)
	if ownLabelID == "" || ownLabelID == vipID {
		t.Errorf("Notices labelId = %q, want a freshly minted label distinct from the additional feed %s",
			ownLabelID, vipID)
	}

	// 3. Move `updates` off the default area...
	res = call(t, h.api.areaSet, a.UserID, map[string]any{
		"update": map[string]any{updatesID: map[string]any{"tabs": []string{}}},
	})
	if _, ok := res["updated"].(map[string]any)[updatesID]; !ok {
		t.Fatalf("freeing Updates' tab feed was refused: %#v", res)
	}

	// ...then onto Notices, which now succeeds.
	res = call(t, h.api.areaSet, a.UserID, map[string]any{
		"update": map[string]any{noticesID: map[string]any{"tabs": []string{"updates"}}},
	})
	if _, ok := res["updated"].(map[string]any)[noticesID]; !ok {
		t.Fatalf("claiming the freed tab onto Notices was refused: %#v", res)
	}

	// 4. THE SAVEPOINT TRAP: one batch with a bad create (the tab is now
	// Notices' own, so a second area claiming it still conflicts) AND a good
	// update in the SAME call. The good update must still land.
	res = call(t, h.api.areaSet, a.UserID, map[string]any{
		"create": map[string]any{
			"conflict2": map[string]any{"name": "Notices2", "tabs": []string{"updates"}},
		},
		"update": map[string]any{
			noticesID: map[string]any{"hiddenFromRail": true},
		},
	})
	if notCreated, _ := res["notCreated"].(map[string]any); notCreated["conflict2"] == nil {
		t.Errorf("the second conflicting create was not refused: %#v", res)
	}
	updated, _ := res["updated"].(map[string]any)
	if _, ok := updated[noticesID]; !ok {
		t.Fatalf("THE GOOD UPDATE IN THE BATCH WAS LOST: %#v", res)
	}
	after := areasByName(t, call(t, h.api.areaGet, a.UserID, map[string]any{}))
	if hidden, _ := after["Notices"]["hiddenFromRail"].(bool); !hidden {
		t.Error("hiddenFromRail did not land despite the batch reporting it updated")
	}

	// 5. `labelId` is immutable.
	res = call(t, h.api.areaSet, a.UserID, map[string]any{
		"update": map[string]any{noticesID: map[string]any{"labelId": vipID}},
	})
	notUpdated, _ := res["notUpdated"].(map[string]any)
	serr, ok = notUpdated[noticesID].(map[string]any)
	if !ok {
		t.Fatalf("patching labelId was not refused: %#v", res)
	}
	if props, _ := serr["properties"].([]any); len(props) != 1 || props[0] != "labelId" {
		t.Errorf("refusal did not name `labelId`: %#v", serr)
	}

	// 6. `autoArchiveDays: null` clears a previously-set value.
	res = call(t, h.api.areaSet, a.UserID, map[string]any{
		"update": map[string]any{noticesID: map[string]any{"autoArchiveDays": 30}},
	})
	if _, ok := res["updated"].(map[string]any)[noticesID]; !ok {
		t.Fatalf("setting autoArchiveDays was refused: %#v", res)
	}
	after = areasByName(t, call(t, h.api.areaGet, a.UserID, map[string]any{}))
	if got, _ := after["Notices"]["autoArchiveDays"].(float64); int(got) != 30 {
		t.Fatalf("autoArchiveDays = %v after setting it, want 30", after["Notices"]["autoArchiveDays"])
	}
	res = call(t, h.api.areaSet, a.UserID, map[string]any{
		"update": map[string]any{noticesID: map[string]any{"autoArchiveDays": nil}},
	})
	if _, ok := res["updated"].(map[string]any)[noticesID]; !ok {
		t.Fatalf("clearing autoArchiveDays was refused: %#v", res)
	}
	after = areasByName(t, call(t, h.api.areaGet, a.UserID, map[string]any{}))
	if after["Notices"]["autoArchiveDays"] != nil {
		t.Errorf("autoArchiveDays = %v after clearing it, want null", after["Notices"]["autoArchiveDays"])
	}

	// 7. Destroying Notices leaves its own label alone — the label survives
	// and Label/get still returns it.
	res = call(t, h.api.areaSet, a.UserID, map[string]any{
		"destroy": []string{noticesID},
	})
	if !strInArr(res["destroyed"], noticesID) {
		t.Fatalf("destroying Notices was refused: %#v", res)
	}
	labelGet := call(t, h.api.labelGet, a.UserID, map[string]any{"ids": []string{ownLabelID}})
	list, _ := labelGet["list"].([]any)
	if len(list) != 1 {
		t.Errorf("Notices' own label did not survive its area's destruction: %#v", labelGet)
	}
}
