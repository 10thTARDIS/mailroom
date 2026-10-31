// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// Email/query's `inArea`/`unsorted` filters and Email.area (#442 increment 2).

import (
	"context"
	"crypto/sha256"
	"testing"

	"mailroom/internal/store"
)

func TestEmailQueryInArea(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	resAreas := call(t, h.api.areaSet, a.UserID, map[string]any{
		"create": map[string]any{
			"x": map[string]any{"name": "AreaX"},
			"y": map[string]any{"name": "AreaY"},
		},
	})
	created, _ := resAreas["created"].(map[string]any)
	x, ok := created["x"].(map[string]any)
	if !ok {
		t.Fatalf("AreaX create failed: %#v", resAreas)
	}
	y, ok := created["y"].(map[string]any)
	if !ok {
		t.Fatalf("AreaY create failed: %#v", resAreas)
	}
	xID, _ := x["id"].(string)
	yID, _ := y["id"].(string)
	xLabel, _ := x["labelId"].(string)
	yLabel, _ := y["labelId"].(string)

	seedMsg := func(tag, labelID string) string {
		hash := sha256.Sum256([]byte("emailarea-" + a.UserID.String() + "-" + tag))
		id := store.NewULID().String()
		mustExecAPI(t, h, `INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id)
			VALUES ($1,$2,10,20,$3)`, a.UserID.String(), hash[:], store.NewULID().String())
		mustExecAPI(t, h, `INSERT INTO app.messages (id, user_id, content_hash, subject, received_at, size_bytes)
			VALUES ($1,$2,$3,$4,now(),10)`, id, a.UserID.String(), hash[:], tag)
		if labelID != "" {
			mustExecAPI(t, h, `INSERT INTO app.message_labels (user_id, message_id, label_id) VALUES ($1,$2,$3)`,
				a.UserID.String(), id, labelID)
		}
		return id
	}

	inX := seedMsg("in-x", xLabel)
	inY := seedMsg("in-y", yLabel)
	unsortedMsg := seedMsg("unsorted", "")

	// inArea returns exactly the expected id.
	res := call(t, h.api.emailQuery, a.UserID, map[string]any{
		"filter": map[string]any{"inArea": xID},
	})
	ids, _ := res["ids"].([]any)
	if len(ids) != 1 || ids[0] != inX {
		t.Errorf("inArea=X ids = %v, want [%s]", ids, inX)
	}

	res = call(t, h.api.emailQuery, a.UserID, map[string]any{
		"filter": map[string]any{"inArea": yID},
	})
	ids, _ = res["ids"].([]any)
	if len(ids) != 1 || ids[0] != inY {
		t.Errorf("inArea=Y ids = %v, want [%s]", ids, inY)
	}

	// unsorted: true returns every unsorted message — this test's own
	// deliberately-unsorted one AND seedTwo's own fixture message (a.MessageID),
	// which feeds no area either.
	res = call(t, h.api.emailQuery, a.UserID, map[string]any{
		"filter": map[string]any{"unsorted": true},
	})
	ids, _ = res["ids"].([]any)
	got := map[string]bool{}
	for _, id := range ids {
		got[id.(string)] = true
	}
	if !got[unsortedMsg] || !got[a.MessageID.String()] || len(ids) != 2 {
		t.Errorf("unsorted ids = %v, want exactly [%s, %s]", ids, unsortedMsg, a.MessageID.String())
	}

	// inArea naming an area that does not exist is invalidArguments, not a
	// silently empty result.
	_, mErr := h.api.emailQuery(context.Background(), a.UserID, mustJSON(t, map[string]any{
		"filter": map[string]any{"inArea": store.NewULID().String()},
	}))
	if mErr == nil || mErr.Type != errInvalidArguments {
		t.Errorf("inArea of an unknown id: got %+v, want invalidArguments", mErr)
	}

	// inArea + unsorted:true are mutually exclusive.
	_, mErr = h.api.emailQuery(context.Background(), a.UserID, mustJSON(t, map[string]any{
		"filter": map[string]any{"inArea": xID, "unsorted": true},
	}))
	if mErr == nil || mErr.Type != errInvalidArguments {
		t.Errorf("inArea+unsorted: got %+v, want invalidArguments", mErr)
	}

	// unsorted: false is a no-op, not "claimed by some area" — asking for it
	// alongside no other filter returns every message.
	res = call(t, h.api.emailQuery, a.UserID, map[string]any{
		"filter": map[string]any{"unsorted": false},
	})
	ids, _ = res["ids"].([]any)
	if len(ids) != 4 {
		t.Errorf("unsorted:false ids = %v, want all 4 seeded messages (a no-op filter)", ids)
	}

	// Email/queryChanges with inArea falls back to cannotCalculateChanges —
	// an area is derived state with no Email change_log row of its own.
	_, mErr = h.api.emailQueryChanges(context.Background(), a.UserID, mustJSON(t, map[string]any{
		"sinceQueryState": "s:0",
		"filter":          map[string]any{"inArea": xID},
	}))
	if mErr == nil || mErr.Type != errCannotCalculateChanges {
		t.Errorf("Email/queryChanges with inArea: got %+v, want cannotCalculateChanges", mErr)
	}

	// Email/get with properties: ["area"] returns the id, or null for a
	// message no area claims.
	getRes := call(t, h.api.emailGet, a.UserID, map[string]any{
		"ids":        []string{inX, unsortedMsg},
		"properties": []string{"id", "area"},
	})
	list, _ := getRes["list"].([]any)
	byID := make(map[string]map[string]any, len(list))
	for _, item := range list {
		obj, _ := item.(map[string]any)
		byID[obj["id"].(string)] = obj
	}
	if area, _ := byID[inX]["area"].(string); area != xID {
		t.Errorf("Email.area for in-X = %v, want %q", byID[inX]["area"], xID)
	}
	if byID[unsortedMsg]["area"] != nil {
		t.Errorf("Email.area for the unsorted message = %v, want null", byID[unsortedMsg]["area"])
	}
}
