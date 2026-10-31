// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// DeskPreferences/* and ClientPreferences/* over the wire (#442 increment 2,
// #514).

import (
	"context"
	"testing"

	"mailroom/internal/store"
)

func TestDeskPreferencesSingleton(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	res := call(t, h.api.deskPreferencesGet, a.UserID, map[string]any{})
	list, _ := res["list"].([]any)
	if len(list) != 1 {
		t.Fatalf("DeskPreferences/get returned %d objects, want 1: %#v", len(list), res)
	}
	obj, _ := list[0].(map[string]any)
	if id, _ := obj["id"].(string); id != a.UserID.String() {
		t.Errorf("DeskPreferences id = %q, want the user id %q", id, a.UserID.String())
	}

	// create is forbidden — the record cannot be minted a second way.
	setRes := call(t, h.api.deskPreferencesSet, a.UserID, map[string]any{
		"create": map[string]any{"c1": map[string]any{"lineThreshold": 0.5}},
	})
	notCreated, _ := setRes["notCreated"].(map[string]any)
	serr, ok := notCreated["c1"].(map[string]any)
	if !ok {
		t.Fatalf("create was not refused: %#v", setRes)
	}
	if typ, _ := serr["type"].(string); typ != setForbidden {
		t.Errorf("create refusal type = %q, want %q", typ, setForbidden)
	}

	// destroy is likewise forbidden.
	setRes = call(t, h.api.deskPreferencesSet, a.UserID, map[string]any{
		"destroy": []string{a.UserID.String()},
	})
	notDestroyed, _ := setRes["notDestroyed"].(map[string]any)
	if _, ok := notDestroyed[a.UserID.String()].(map[string]any); !ok {
		t.Fatalf("destroy was not refused: %#v", setRes)
	}

	// An out-of-range lineThreshold is refused, naming the property.
	setRes = call(t, h.api.deskPreferencesSet, a.UserID, map[string]any{
		"update": map[string]any{a.UserID.String(): map[string]any{"lineThreshold": 1.5}},
	})
	notUpdated, _ := setRes["notUpdated"].(map[string]any)
	serr, ok = notUpdated[a.UserID.String()].(map[string]any)
	if !ok {
		t.Fatalf("lineThreshold 1.5 was not refused: %#v", setRes)
	}
	if props, _ := serr["properties"].([]any); len(props) != 1 || props[0] != "lineThreshold" {
		t.Errorf("refusal did not name lineThreshold: %#v", serr)
	}

	// A valid update lands, and /changes since the old state names exactly
	// the user's own id as updated.
	oldState, _ := setRes["oldState"].(string)
	setRes = call(t, h.api.deskPreferencesSet, a.UserID, map[string]any{
		"update": map[string]any{a.UserID.String(): map[string]any{"lineThreshold": 0.7}},
	})
	updated, _ := setRes["updated"].(map[string]any)
	if _, ok := updated[a.UserID.String()]; !ok {
		t.Fatalf("a valid lineThreshold update was refused: %#v", setRes)
	}

	changes := call(t, h.api.deskPreferencesChanges, a.UserID, map[string]any{"sinceState": oldState})
	upd, _ := changes["updated"].([]any)
	if len(upd) != 1 || upd[0] != a.UserID.String() {
		t.Errorf("DeskPreferences/changes updated = %v, want exactly [%q]", upd, a.UserID.String())
	}

	res = call(t, h.api.deskPreferencesGet, a.UserID, map[string]any{})
	list, _ = res["list"].([]any)
	obj, _ = list[0].(map[string]any)
	if got, _ := obj["lineThreshold"].(float64); got != 0.7 {
		t.Errorf("lineThreshold after update = %v, want 0.7", got)
	}

	// drawerSweepEnabled (#443): default false, and DeskPreferences/set flips
	// it exactly like any other field — the negative control that the drawer
	// sweep's master switch is a real wire property, not decoration.
	if got, ok := obj["drawerSweepEnabled"].(bool); !ok || got {
		t.Errorf("drawerSweepEnabled = %v, want false (the default)", obj["drawerSweepEnabled"])
	}
	setRes = call(t, h.api.deskPreferencesSet, a.UserID, map[string]any{
		"update": map[string]any{a.UserID.String(): map[string]any{"drawerSweepEnabled": true}},
	})
	updated, _ = setRes["updated"].(map[string]any)
	if _, ok := updated[a.UserID.String()]; !ok {
		t.Fatalf("drawerSweepEnabled update was refused: %#v", setRes)
	}
	res = call(t, h.api.deskPreferencesGet, a.UserID, map[string]any{})
	list, _ = res["list"].([]any)
	obj, _ = list[0].(map[string]any)
	if got, _ := obj["drawerSweepEnabled"].(bool); !got {
		t.Errorf("drawerSweepEnabled after update = %v, want true", obj["drawerSweepEnabled"])
	}

	// Only the caller's own id is a valid /get or /set target.
	res = call(t, h.api.deskPreferencesGet, a.UserID, map[string]any{"ids": []string{"not-a-real-id"}})
	if !strInArr(res["notFound"], "not-a-real-id") {
		t.Errorf("a foreign id was not reported notFound: %#v", res)
	}
}

func TestClientPreferencesPerFamily(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	res := call(t, h.api.clientPreferencesGet, a.UserID, map[string]any{})
	list, _ := res["list"].([]any)
	if len(list) != 3 {
		t.Fatalf("ClientPreferences/get returned %d objects, want 3: %#v", len(list), res)
	}
	byFamily := make(map[string]map[string]any, 3)
	for _, item := range list {
		obj, _ := item.(map[string]any)
		fam, _ := obj["family"].(string)
		byFamily[fam] = obj
	}
	web, ok := byFamily["web"]
	if !ok {
		t.Fatalf("no web family in the response: %#v", res)
	}
	phone, ok := byFamily["phone"]
	if !ok {
		t.Fatalf("no phone family in the response: %#v", res)
	}
	webID, _ := web["id"].(string)
	phoneID, _ := phone["id"].(string)
	phoneThemeBefore, _ := phone["theme"].(string)

	setRes := call(t, h.api.clientPreferencesSet, a.UserID, map[string]any{
		"update": map[string]any{webID: map[string]any{"theme": "dark"}},
	})
	updated, _ := setRes["updated"].(map[string]any)
	if _, ok := updated[webID]; !ok {
		t.Fatalf("web theme update was refused: %#v", setRes)
	}

	// Phone is untouched.
	res = call(t, h.api.clientPreferencesGet, a.UserID, map[string]any{"ids": []string{webID, phoneID}})
	list, _ = res["list"].([]any)
	for _, item := range list {
		obj, _ := item.(map[string]any)
		switch obj["id"] {
		case webID:
			if theme, _ := obj["theme"].(string); theme != "dark" {
				t.Errorf("web theme = %q after update, want dark", theme)
			}
		case phoneID:
			if theme, _ := obj["theme"].(string); theme != phoneThemeBefore {
				t.Errorf("phone theme = %q, want unchanged %q", theme, phoneThemeBefore)
			}
		}
	}

	// An unknown theme is refused, naming the property.
	setRes = call(t, h.api.clientPreferencesSet, a.UserID, map[string]any{
		"update": map[string]any{webID: map[string]any{"theme": "sepia"}},
	})
	notUpdated, _ := setRes["notUpdated"].(map[string]any)
	serr, ok := notUpdated[webID].(map[string]any)
	if !ok {
		t.Fatalf("theme \"sepia\" was not refused: %#v", setRes)
	}
	if props, _ := serr["properties"].([]any); len(props) != 1 || props[0] != "theme" {
		t.Errorf("refusal did not name theme: %#v", serr)
	}

	// `family` is immutable.
	setRes = call(t, h.api.clientPreferencesSet, a.UserID, map[string]any{
		"update": map[string]any{webID: map[string]any{"family": "phone"}},
	})
	notUpdated, _ = setRes["notUpdated"].(map[string]any)
	serr, ok = notUpdated[webID].(map[string]any)
	if !ok {
		t.Fatalf("patching family was not refused: %#v", setRes)
	}
	if props, _ := serr["properties"].([]any); len(props) != 1 || props[0] != "family" {
		t.Errorf("refusal did not name family: %#v", serr)
	}

	// create/destroy are forbidden — the three families are a fixed set.
	setRes = call(t, h.api.clientPreferencesSet, a.UserID, map[string]any{
		"create":  map[string]any{"c1": map[string]any{"family": "web"}},
		"destroy": []string{webID},
	})
	notCreated, _ := setRes["notCreated"].(map[string]any)
	if _, ok := notCreated["c1"].(map[string]any); !ok {
		t.Errorf("create was not refused: %#v", setRes)
	}
	notDestroyed, _ := setRes["notDestroyed"].(map[string]any)
	if _, ok := notDestroyed[webID].(map[string]any); !ok {
		t.Errorf("destroy was not refused: %#v", setRes)
	}
}

// READING COMFORT OVER THE WIRE (#514, migration 00052): the two new fields
// round-trip, a value outside either enum is refused by NAME, and a row that
// was materialised before the migration reads the defaults rather than an
// empty string.
func TestClientPreferencesReadingComfort(t *testing.T) {
	h := newAPIHarness(t)
	a, b := seedTwo(t, h.super)

	webID := func(user apiFixture) (string, map[string]any) {
		t.Helper()
		res := call(t, h.api.clientPreferencesGet, user.UserID, map[string]any{})
		list, _ := res["list"].([]any)
		for _, item := range list {
			obj, _ := item.(map[string]any)
			if fam, _ := obj["family"].(string); fam == "web" {
				id, _ := obj["id"].(string)
				return id, obj
			}
		}
		t.Fatalf("no web family in the response: %#v", res)
		return "", nil
	}

	// ---- the defaults are today's behaviour ------------------------------
	//
	// A fresh row must read `m` / `oneLine`: the migration's job was to add two
	// settings without changing what anybody is already looking at, and a
	// default of anything else would restyle every existing client on deploy.
	id, obj := webID(a)
	if got, _ := obj["textSize"].(string); got != "m" {
		t.Errorf("fresh textSize = %q, want m", got)
	}
	if got, _ := obj["rowLayout"].(string); got != "oneLine" {
		t.Errorf("fresh rowLayout = %q, want oneLine", got)
	}

	// ---- the round trip ---------------------------------------------------
	setRes := call(t, h.api.clientPreferencesSet, a.UserID, map[string]any{
		"update": map[string]any{id: map[string]any{"textSize": "xl", "rowLayout": "multiLine"}},
	})
	if updated, _ := setRes["updated"].(map[string]any); updated == nil {
		t.Fatalf("the reading-comfort update was refused: %#v", setRes)
	} else if _, ok := updated[id]; !ok {
		t.Fatalf("the reading-comfort update was refused: %#v", setRes)
	}
	_, obj = webID(a)
	if got, _ := obj["textSize"].(string); got != "xl" {
		t.Errorf("textSize = %q after update, want xl", got)
	}
	if got, _ := obj["rowLayout"].(string); got != "multiLine" {
		t.Errorf("rowLayout = %q after update, want multiLine", got)
	}

	// ---- a value outside the enum is refused, naming the property ---------
	//
	// Both directions, because a validator that names the wrong property is a
	// settings form that highlights the wrong control.
	for _, bad := range []struct{ prop, value string }{
		{"textSize", "huge"},
		{"rowLayout", "twoLine"},
	} {
		setRes = call(t, h.api.clientPreferencesSet, a.UserID, map[string]any{
			"update": map[string]any{id: map[string]any{bad.prop: bad.value}},
		})
		notUpdated, _ := setRes["notUpdated"].(map[string]any)
		serr, ok := notUpdated[id].(map[string]any)
		if !ok {
			t.Fatalf("%s %q was not refused: %#v", bad.prop, bad.value, setRes)
		}
		if props, _ := serr["properties"].([]any); len(props) != 1 || props[0] != bad.prop {
			t.Errorf("refusing %s %q did not name it: %#v", bad.prop, bad.value, serr)
		}
	}
	// And the refusal changed nothing.
	_, obj = webID(a)
	if got, _ := obj["textSize"].(string); got != "xl" {
		t.Errorf("textSize = %q after a refused patch, want xl", got)
	}

	// ---- a row materialised BEFORE the migration --------------------------
	//
	// ⚠ THIS IS THE CASE `ALTER TABLE ... ADD COLUMN NOT NULL DEFAULT` EXISTS
	// TO COVER, and it is the one an integration test against a fully migrated
	// database would otherwise never exercise: every row it creates is created
	// by today's code. So this inserts a row with EXACTLY the column list the
	// pre-00052 store used (id, user_id, family — see GetClientPreferences),
	// which is what a row written by the shipped v1.68 server looks like, and
	// asks the API to read it back. If the ALTER had shipped without defaults,
	// or the store had stopped selecting the columns, this reads "" and the
	// client would apply an unknown vocabulary word to <html>.
	preID := store.NewULID()
	if _, err := h.super.Exec(context.Background(),
		`INSERT INTO app.client_preferences (id, user_id, family) VALUES ($1, $2, 'web')`,
		preID.String(), b.UserID.String()); err != nil {
		t.Fatalf("insert a pre-migration client_preferences row: %v", err)
	}
	gotID, obj := webID(b)
	if gotID != preID.String() {
		t.Fatalf("ClientPreferences/get materialised a new row (%s) instead of reading the pre-migration one (%s)", gotID, preID)
	}
	if got, _ := obj["textSize"].(string); got != "m" {
		t.Errorf("pre-migration row textSize = %q, want m", got)
	}
	if got, _ := obj["rowLayout"].(string); got != "oneLine" {
		t.Errorf("pre-migration row rowLayout = %q, want oneLine", got)
	}
}
