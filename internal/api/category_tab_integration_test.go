// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// CategoryTab over the wire (#219).
//
// The claims worth proving here are not "a row round-trips":
//
//  1. THE STRIP EXISTS ON FIRST CONTACT, with a state string that already
//     accounts for it. A client's very first CategoryTab/get is what creates the
//     rows, so a state read on the wrong side of that would hand back a cursor
//     that replays five creates on the next /changes.
//  2. THE SET IS FIXED. Create and destroy are refused per object, not honoured
//     and not fatal — a client that tries either still gets its valid updates
//     applied.
//  3. PRIMARY CANNOT BE DISABLED, and the refusal names the property so a UI can
//     point at the switch that will not move.
//  4. ONE BAD OBJECT DOES NOT TAKE DOWN A BATCH. This is the savepoint trap this
//     project has already shipped once: a failed statement aborts the whole
//     transaction, so the per-object error handling above it becomes decoration
//     and every GOOD object in the same call is lost. Which is why the batch
//     below contains a bad object AND a good one, and asserts on the good one.

import (
	"testing"

	"mailroom/internal/tabs"
)

// tabsByName indexes a CategoryTab/get list result by tab name.
func tabsByName(t *testing.T, res map[string]any) map[string]map[string]any {
	t.Helper()
	list, _ := res["list"].([]any)
	out := make(map[string]map[string]any, len(list))
	for _, item := range list {
		obj, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("list item is not an object: %#v", item)
		}
		name, _ := obj["tab"].(string)
		out[name] = obj
	}
	return out
}

func TestCategoryTabStripExistsOnFirstContact(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	res := call(t, h.api.categoryTabGet, a.UserID, map[string]any{})
	byName := tabsByName(t, res)
	if len(byName) != len(tabs.All) {
		t.Fatalf("first CategoryTab/get returned %d tabs, want %d: %#v", len(byName), len(tabs.All), res)
	}
	for _, want := range tabs.All {
		obj, ok := byName[string(want)]
		if !ok {
			t.Errorf("tab %q missing from the strip", want)
			continue
		}
		// All five ship ENABLED, Forums included. It ships empty against
		// a typical mapping and that is deliberate (#219): a tab that must
		// be switched on before it can be rearranged is a tab nobody discovers.
		if enabled, _ := obj["enabled"].(bool); !enabled {
			t.Errorf("tab %q shipped disabled; all five ship enabled", want)
		}
	}

	// THE STATE MUST ALREADY COVER THE CREATES. A second /get with no intervening
	// write must report the same state — if the first read had taken its state
	// before materialising, this would move and every client would resync.
	firstState, _ := res["state"].(string)
	again := call(t, h.api.categoryTabGet, a.UserID, map[string]any{})
	if secondState, _ := again["state"].(string); secondState != firstState {
		t.Errorf("state moved between two identical reads: %q then %q", firstState, secondState)
	}

	// And a /changes from that state reports nothing outstanding.
	changes := call(t, h.api.categoryTabChanges, a.UserID, map[string]any{"sinceState": firstState})
	for _, bucket := range []string{"created", "updated", "destroyed"} {
		if arr, _ := changes[bucket].([]any); len(arr) != 0 {
			t.Errorf("CategoryTab/changes since the get's own state reported %d %s: %#v",
				len(arr), bucket, changes)
		}
	}
}

func TestCategoryTabSetRefusesCreateAndDestroyButStillApplies(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	byName := tabsByName(t, call(t, h.api.categoryTabGet, a.UserID, map[string]any{}))
	forumsID, _ := byName[string(tabs.Forums)]["id"].(string)
	socialID, _ := byName[string(tabs.Social)]["id"].(string)
	if forumsID == "" || socialID == "" {
		t.Fatalf("missing tab ids: %#v", byName)
	}

	// ONE BATCH: a create (refused), a destroy (refused), a patch naming an
	// immutable property (refused), and a GOOD update. The good one is the
	// assertion that matters — see the savepoint note in this file's header.
	res := call(t, h.api.categoryTabSet, a.UserID, map[string]any{
		"create":  map[string]any{"t1": map[string]any{"tab": "primary"}},
		"destroy": []string{forumsID},
		"update": map[string]any{
			forumsID: map[string]any{"tab": "social"},
			socialID: map[string]any{"enabled": false, "position": 9},
		},
	})

	if notCreated, _ := res["notCreated"].(map[string]any); notCreated["t1"] == nil {
		t.Errorf("a create was not refused: %#v", res)
	}
	if created, _ := res["created"].(map[string]any); len(created) != 0 {
		t.Errorf("a CategoryTab was created: %#v", created)
	}
	if notDestroyed, _ := res["notDestroyed"].(map[string]any); notDestroyed[forumsID] == nil {
		t.Errorf("a destroy was not refused: %#v", res)
	}
	if notUpdated, _ := res["notUpdated"].(map[string]any); notUpdated[forumsID] == nil {
		t.Errorf("patching the immutable `tab` was not refused: %#v", res)
	}
	updated, _ := res["updated"].(map[string]any)
	if _, ok := updated[socialID]; !ok {
		t.Fatalf("THE GOOD UPDATE IN THE BATCH WAS LOST: %#v", res)
	}

	after := tabsByName(t, call(t, h.api.categoryTabGet, a.UserID, map[string]any{}))
	social := after[string(tabs.Social)]
	if enabled, _ := social["enabled"].(bool); enabled {
		t.Error("social is still enabled; the good update did not land")
	}
	if pos, _ := social["position"].(float64); int(pos) != 9 {
		t.Errorf("social position = %v, want 9", social["position"])
	}
	// The refused destroy did not remove it.
	if _, ok := after[string(tabs.Forums)]; !ok {
		t.Error("forums was destroyed despite the refusal")
	}
}

func TestCategoryTabPrimaryCannotBeDisabledOverTheWire(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	byName := tabsByName(t, call(t, h.api.categoryTabGet, a.UserID, map[string]any{}))
	primaryID, _ := byName[string(tabs.Primary)]["id"].(string)
	updatesID, _ := byName[string(tabs.Updates)]["id"].(string)

	res := call(t, h.api.categoryTabSet, a.UserID, map[string]any{
		"update": map[string]any{
			primaryID: map[string]any{"enabled": false},
			updatesID: map[string]any{"position": 1},
		},
	})

	notUpdated, _ := res["notUpdated"].(map[string]any)
	serr, ok := notUpdated[primaryID].(map[string]any)
	if !ok {
		t.Fatalf("disabling primary was not refused: %#v", res)
	}
	if typ, _ := serr["type"].(string); typ != setInvalidProps {
		t.Errorf("refusal type = %q, want %q", typ, setInvalidProps)
	}
	props, _ := serr["properties"].([]any)
	if len(props) != 1 || props[0] != "enabled" {
		t.Errorf("refusal did not name `enabled`: %#v", serr)
	}
	// The other update in the same call still applied — a constraint violation must
	// not abort the transaction around it. Tested by KEY PRESENCE: RFC 8620 §5.3
	// puts `null` in `updated` for an object with no server-set properties, so
	// checking the value for non-nil would report every successful update as lost.
	updated, _ := res["updated"].(map[string]any)
	if _, ok := updated[updatesID]; !ok {
		t.Fatalf("the valid update alongside the refused one was lost: %#v", res)
	}

	after := tabsByName(t, call(t, h.api.categoryTabGet, a.UserID, map[string]any{}))
	if enabled, _ := after[string(tabs.Primary)]["enabled"].(bool); !enabled {
		t.Error("primary ended up disabled")
	}
}

// TestLabelCarriesItsTab is the other half of the mapping: a client sets a
// label's tab through Label/set, reads it back on the Label object, and clears
// it with null. Repointing a label is how the operator rearranges the mapping,
// so this is the surface that requirement actually lands on.
func TestLabelCarriesItsTab(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	res := call(t, h.api.labelSet, a.UserID, map[string]any{
		"create": map[string]any{
			"l1": map[string]any{"name": "Theater", "tab": "promotions"},
			// A bad tab alongside a good one: refused per object, and the good one
			// must still be created.
			"l2": map[string]any{"name": "Bogus", "tab": "inbox"},
		},
	})
	created, _ := res["created"].(map[string]any)
	good, ok := created["l1"].(map[string]any)
	if !ok {
		t.Fatalf("the valid label create was lost: %#v", res)
	}
	if got, _ := good["tab"].(string); got != "promotions" {
		t.Errorf("created label tab = %q, want promotions", got)
	}
	notCreated, _ := res["notCreated"].(map[string]any)
	serr, ok := notCreated["l2"].(map[string]any)
	if !ok {
		t.Fatalf("a label with an unknown tab was created: %#v", res)
	}
	if props, _ := serr["properties"].([]any); len(props) != 1 || props[0] != "tab" {
		t.Errorf("refusal did not name `tab`: %#v", serr)
	}

	id, _ := good["id"].(string)
	// Re-point it, then unmap it with null.
	call(t, h.api.labelSet, a.UserID, map[string]any{
		"update": map[string]any{id: map[string]any{"tab": "updates"}},
	})
	got := call(t, h.api.labelGet, a.UserID, map[string]any{"ids": []string{id}})
	list, _ := got["list"].([]any)
	if len(list) != 1 {
		t.Fatalf("Label/get returned %d objects", len(list))
	}
	obj, _ := list[0].(map[string]any)
	if tab, _ := obj["tab"].(string); tab != "updates" {
		t.Errorf("label tab after re-pointing = %v, want updates", obj["tab"])
	}

	call(t, h.api.labelSet, a.UserID, map[string]any{
		"update": map[string]any{id: map[string]any{"tab": nil}},
	})
	got = call(t, h.api.labelGet, a.UserID, map[string]any{"ids": []string{id}})
	list, _ = got["list"].([]any)
	obj, _ = list[0].(map[string]any)
	if obj["tab"] != nil {
		t.Errorf("label tab after unmapping = %v, want null", obj["tab"])
	}
}

// TestCategoryTabsAreAdvertised keeps the session honest: a client that cannot
// see the capability cannot call the methods (capability.go fails closed), so an
// unadvertised URN would make the whole surface unreachable — the bug this
// project shipped once already.
func TestCategoryTabsAreAdvertised(t *testing.T) {
	caps := capabilityObjects()
	obj, ok := caps[capCategoryTabs].(map[string]any)
	if !ok {
		t.Fatalf("%s is not advertised in the session capabilities", capCategoryTabs)
	}
	names, _ := obj["tabs"].([]string)
	if len(names) != len(tabs.All) {
		t.Fatalf("capability advertises %v, want the whole vocabulary %v", names, tabs.All)
	}
	for i, want := range tabs.All {
		if names[i] != string(want) {
			t.Errorf("capability tab %d = %q, want %q", i, names[i], want)
		}
	}
	for _, m := range []string{"CategoryTab/get", "CategoryTab/changes", "CategoryTab/set"} {
		if methodCapabilities[m] != capCategoryTabs {
			t.Errorf("%s is gated on %q, want %q", m, methodCapabilities[m], capCategoryTabs)
		}
	}
}
