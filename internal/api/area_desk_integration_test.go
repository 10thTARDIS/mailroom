// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// Area/getDesk over the wire (#665): what the client now sends INSTEAD of
// Area/getSummary plus one Email/query per section must answer exactly what
// those calls answered — the summary rows and state of the one, and the ids,
// total and queryState of each of the others, sent with the filters the web
// client's sectionFilter builds. The store's agreement test covers the SQL at
// volume; this one covers the handler: the stored line, the section order,
// notFound, the clamp, the account scope.

import (
	"crypto/sha256"
	"reflect"
	"testing"
	"time"

	"mailroom/internal/store"
)

func TestAreaGetDeskMatchesTheCallsItReplaces(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)
	u := a.UserID.String()
	// seedTwo's own Inbox message would land in Needs sorting and blur the
	// exact counts below; trashed mail is never on the desk.
	mustExecAPI(t, h, `UPDATE app.messages SET trashed_at = now() WHERE user_id = $1 AND id = $2`,
		u, a.MessageID.String())

	mkArea := func(name string) (id, label string) {
		res := call(t, h.api.areaSet, a.UserID, map[string]any{
			"create": map[string]any{"n": map[string]any{"name": name}},
		})
		created, _ := res["created"].(map[string]any)
		obj, ok := created["n"].(map[string]any)
		if !ok {
			t.Fatalf("create area %s: %#v", name, res)
		}
		return obj["id"].(string), obj["labelId"].(string)
	}
	xID, xLabel := mkArea("AreaX")
	yID, yLabel := mkArea("AreaY")

	thread := store.NewULID().String()
	mustExecAPI(t, h, `INSERT INTO app.threads (id, user_id, subject) VALUES ($1,$2,'t')`, thread, u)
	base := time.Now().UTC().Add(-time.Hour)
	n := 0
	seed := func(label string, score *float32, threadID string, snoozed bool) string {
		n++
		hash := sha256.Sum256([]byte("getdesk-" + u + "-" + string(rune('a'+n))))
		id := store.NewULID().String()
		mustExecAPI(t, h, `INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id)
			VALUES ($1,$2,10,20,$3)`, u, hash[:], store.NewULID().String())
		var th any
		if threadID != "" {
			th = threadID
		}
		var snz any
		if snoozed {
			snz = time.Now().Add(24 * time.Hour)
		}
		mustExecAPI(t, h, `INSERT INTO app.messages (id, user_id, content_hash, thread_id, subject, received_at, size_bytes, snoozed_until)
			VALUES ($1,$2,$3,$4,'s',$5,10,$6)`, id, u, hash[:], th, base.Add(time.Duration(n)*time.Minute), snz)
		if score != nil {
			mustExecAPI(t, h, `INSERT INTO app.triage_scores (user_id, message_id, score, model_version)
				VALUES ($1,$2,$3,'v0')`, u, id, *score)
		}
		if label != "" {
			mustExecAPI(t, h, `INSERT INTO app.message_labels (user_id, message_id, label_id) VALUES ($1,$2,$3)`, u, id, label)
		}
		mustExecAPI(t, h, `INSERT INTO app.message_provenance (id, user_id, message_id, account_id, mailbox_id, uid, uidvalidity)
			VALUES ($1,$2,$3,$4,$5,$6,1)`, store.NewULID().String(), u, id, a.AccountID.String(), a.InboxID.String(), testUID())
		return id
	}
	f := func(v float32) *float32 { return &v }
	seed(xLabel, f(0.9), "", false)     // X, surface
	seed(xLabel, f(0.2), "", false)     // X, drawer
	seed(xLabel, nil, "", false)        // X, unscored -> surface
	seed(xLabel, f(0.8), thread, false) // X, a thread of two: one row
	seed(xLabel, f(0.7), thread, false)
	seed(xLabel, f(0.9), "", true)   // X, snoozed: counted, not listed
	seed(yLabel, f(0.95), "", false) // Y, surface
	seed(yLabel, f(0.1), "", false)  // Y, drawer
	seed("", f(0.1), "", false)      // Needs sorting, below the line: still listed
	seed("", nil, "", false)         // Needs sorting

	// The stored line the desk reads when no threshold is sent.
	call(t, h.api.deskPreferencesSet, a.UserID, map[string]any{
		"update": map[string]any{u: map[string]any{"lineThreshold": 0.6}},
	})

	for _, scope := range []map[string]any{{}, {"mailAccountId": a.AccountID.String()}} {
		scoped := len(scope) > 0
		filterFor := func(areaID any) map[string]any {
			f := map[string]any{"onDesk": true, "isSnoozed": false}
			if areaID == nil {
				f["unsorted"] = true
			} else {
				f["inArea"], f["onSurface"] = areaID, true
			}
			if scoped {
				f["inAccount"] = a.AccountID.String()
			}
			return f
		}
		sections := []map[string]any{{"areaId": xID, "limit": 1}, {"areaId": yID}, {"areaId": nil}}
		args := map[string]any{"sections": sections}
		for k, v := range scope {
			args[k] = v
		}
		desk := call(t, h.api.areaGetDesk, a.UserID, args)
		summary := call(t, h.api.areaGetSummary, a.UserID, scope)
		for _, k := range []string{"state", "threshold", "list"} {
			if !reflect.DeepEqual(desk[k], summary[k]) {
				t.Errorf("scoped=%v %s: getDesk %v, getSummary %v", scoped, k, desk[k], summary[k])
			}
		}
		got := asArr(desk["sections"])
		if len(got) != len(sections) {
			t.Fatalf("scoped=%v: %d sections for %d asked: %v", scoped, len(got), len(sections), desk)
		}
		for i, s := range sections {
			limit := 50
			if l, ok := s["limit"].(int); ok {
				limit = l
			}
			q := call(t, h.api.emailQuery, a.UserID, map[string]any{
				"filter": filterFor(s["areaId"]), "sort": []map[string]any{{"property": "returnedAt", "isAscending": false}},
				"position": 0, "limit": limit, "calculateTotal": true, "collapseThreads": true,
			})
			sec, _ := got[i].(map[string]any)
			if sec["areaId"] != s["areaId"] || !reflect.DeepEqual(sec["ids"], q["ids"]) || sec["total"] != q["total"] {
				t.Errorf("scoped=%v section %d: getDesk %v; Email/query ids %v total %v", scoped, i, sec, q["ids"], q["total"])
			}
			if desk["queryState"] != q["queryState"] {
				t.Errorf("getDesk queryState %v, Email/query %v", desk["queryState"], q["queryState"])
			}
		}
		// Non-vacuity: X's window cut its list, the thread collapsed, and
		// Needs sorting kept its below-the-line message.
		x, _ := got[0].(map[string]any)
		if x["total"] != float64(3) || len(asArr(x["ids"])) != 1 {
			t.Errorf("scoped=%v: area X = %v, want 3 rows (surface, unscored, one thread) windowed to 1", scoped, x)
		}
		if ns, _ := got[2].(map[string]any); ns["total"] != float64(2) {
			t.Errorf("scoped=%v: Needs sorting = %v, want both messages", scoped, ns)
		}
	}

	// A preview threshold moves the counts exactly as getSummary's does.
	pre := call(t, h.api.areaGetDesk, a.UserID, map[string]any{"threshold": 0.1})
	preSum := call(t, h.api.areaGetSummary, a.UserID, map[string]any{"threshold": 0.1})
	if !reflect.DeepEqual(pre["list"], preSum["list"]) || pre["threshold"] != preSum["threshold"] {
		t.Errorf("threshold 0.1: getDesk %v/%v, getSummary %v/%v", pre["threshold"], pre["list"], preSum["threshold"], preSum["list"])
	}
	if len(asArr(pre["sections"])) != 0 {
		t.Errorf("no sections asked, got %v", pre["sections"])
	}

	// An area destroyed elsewhere is notFound, not a failed desk; a window
	// past maxObjectsInGet is clamped and says so.
	ghost := store.NewULID().String()
	res := call(t, h.api.areaGetDesk, a.UserID, map[string]any{
		"sections": []map[string]any{{"areaId": ghost}, {"areaId": yID, "limit": 501}},
	})
	if !strInArr(res["notFound"], ghost) {
		t.Errorf("ghost area not in notFound: %v", res["notFound"])
	}
	secs := asArr(res["sections"])
	if len(secs) != 1 {
		t.Fatalf("want only Y back, got %v", secs)
	}
	if y, _ := secs[0].(map[string]any); y["areaId"] != yID || y["limit"] != float64(500) {
		t.Errorf("Y = %v, want areaId %s and limit 500 (clamped)", y, yID)
	}

	// Malformed arguments are refused, not guessed at.
	for _, bad := range []map[string]any{
		{"threshold": 1.5},
		{"sections": []map[string]any{{"areaId": "nope"}}},
		{"sections": []map[string]any{{"areaId": nil, "limit": -1}}},
		{"sections": []map[string]any{{"areaId": nil, "position": -1}}},
	} {
		if _, mErr := h.api.areaGetDesk(t.Context(), a.UserID, mustJSON(t, bad)); mErr == nil || mErr.Type != errInvalidArguments {
			t.Errorf("%v: %+v, want invalidArguments", bad, mErr)
		}
	}
}
