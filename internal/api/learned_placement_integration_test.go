// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// The wire half of learned placement (#714): Email.areaProvenance says who
// put a message in its area, including "learned", for the local-only area
// labels labelProvenance has never been able to describe; and
// DeskPreferences.learnedPlacementEnabled is the off switch, default true.

import (
	"crypto/sha256"
	"testing"

	"mailroom/internal/store"
)

func TestEmailAreaProvenance(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	resAreas := call(t, h.api.areaSet, a.UserID, map[string]any{
		"create": map[string]any{"x": map[string]any{"name": "LearnedArea"}},
	})
	created, _ := resAreas["created"].(map[string]any)
	x, ok := created["x"].(map[string]any)
	if !ok {
		t.Fatalf("area create failed: %#v", resAreas)
	}
	xLabel, _ := x["labelId"].(string)

	seedMsg := func(tag, appliedBy string) string {
		hash := sha256.Sum256([]byte("areaprov-" + a.UserID.String() + "-" + tag))
		id := store.NewULID().String()
		mustExecAPI(t, h, `INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id)
			VALUES ($1,$2,10,20,$3)`, a.UserID.String(), hash[:], store.NewULID().String())
		mustExecAPI(t, h, `INSERT INTO app.messages (id, user_id, content_hash, subject, received_at, size_bytes)
			VALUES ($1,$2,$3,$4,now(),10)`, id, a.UserID.String(), hash[:], tag)
		if appliedBy != "" {
			mustExecAPI(t, h, `INSERT INTO app.message_labels (user_id, message_id, label_id, applied_by) VALUES ($1,$2,$3,$4)`,
				a.UserID.String(), id, xLabel, appliedBy)
		}
		return id
	}
	learned := seedMsg("learned", store.LabelByLearned)
	byUser := seedMsg("user", store.LabelByUser)
	unsorted := seedMsg("unsorted", "")

	res := call(t, h.api.emailGet, a.UserID, map[string]any{
		"ids":        []string{learned, byUser, unsorted},
		"properties": []string{"id", "area", "areaProvenance"},
	})
	list, _ := res["list"].([]any)
	got := map[string]any{}
	for _, item := range list {
		obj, _ := item.(map[string]any)
		id, _ := obj["id"].(string)
		got[id] = obj["areaProvenance"]
		if _, present := obj["areaProvenance"]; !present {
			t.Errorf("%s: areaProvenance absent although requested", id)
		}
	}
	if got[learned] != "learned" {
		t.Errorf("learned message areaProvenance = %v, want \"learned\"", got[learned])
	}
	if got[byUser] != "user" {
		t.Errorf("user-filed message areaProvenance = %v, want \"user\"", got[byUser])
	}
	if got[unsorted] != nil {
		t.Errorf("unsorted message areaProvenance = %v, want null", got[unsorted])
	}

	// Opt-in: not there unless asked for.
	res = call(t, h.api.emailGet, a.UserID, map[string]any{
		"ids": []string{learned}, "properties": []string{"id", "area"},
	})
	list, _ = res["list"].([]any)
	if obj, _ := list[0].(map[string]any); obj != nil {
		if _, present := obj["areaProvenance"]; present {
			t.Error("areaProvenance present although not requested")
		}
	}
}

func TestDeskPreferencesLearnedPlacementSwitch(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	get := func() map[string]any {
		res := call(t, h.api.deskPreferencesGet, a.UserID, map[string]any{})
		list, _ := res["list"].([]any)
		obj, _ := list[0].(map[string]any)
		return obj
	}
	if got, ok := get()["learnedPlacementEnabled"].(bool); !ok || !got {
		t.Errorf("learnedPlacementEnabled = %v, want true (the operator's default)", get()["learnedPlacementEnabled"])
	}
	setRes := call(t, h.api.deskPreferencesSet, a.UserID, map[string]any{
		"update": map[string]any{a.UserID.String(): map[string]any{"learnedPlacementEnabled": false}},
	})
	updated, _ := setRes["updated"].(map[string]any)
	if _, ok := updated[a.UserID.String()]; !ok {
		t.Fatalf("learnedPlacementEnabled update refused: %#v", setRes)
	}
	if got, _ := get()["learnedPlacementEnabled"].(bool); got {
		t.Error("learnedPlacementEnabled after switching off = true, want false")
	}
	setRes = call(t, h.api.deskPreferencesSet, a.UserID, map[string]any{
		"update": map[string]any{a.UserID.String(): map[string]any{"learnedPlacementEnabled": "no"}},
	})
	notUpdated, _ := setRes["notUpdated"].(map[string]any)
	if _, ok := notUpdated[a.UserID.String()]; !ok {
		t.Errorf("a non-boolean learnedPlacementEnabled must be refused: %#v", setRes)
	}
}
