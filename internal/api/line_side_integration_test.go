// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// THE LINE OVERRIDE, END TO END (#464): Triage/correct now moves the message
// across the desk's line IMMEDIATELY, and every reader of the line agrees
// about where it went — Area/getSummary's counts, Email/query's `onSurface`,
// Email/changes' cursor and Email.lineSide.
//
// Modelled on email_desk_filters_integration_test.go (the same area-X fixture,
// the same scored/unscored shapes, the same Email/query-vs-Area/getSummary
// differential), because the property under test is exactly the agreement that
// file already pins — now with a user's placement in the middle of it.
//
// ⚠ THE ASSERTIONS ARE ON ID SETS, NOT COUNTS, AND THAT IS DELIBERATE. Moving
// one message up and another down returns the summary's three numbers to where
// they started, so a correction that changed NOTHING would satisfy any
// count-only check at step 2. Only the membership tells the two apart.

import (
	"crypto/sha256"
	"sort"
	"testing"

	"mailroom/internal/store"
)

func TestLineSideMovesTheMessageAcrossTheLine(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	// seedTwo's own fixture message lives in the Inbox with no score; trashing
	// it keeps every set below exact, exactly as the desk-filters test does.
	mustExecAPI(t, h, `UPDATE app.messages SET trashed_at = now() WHERE user_id = $1 AND id = $2`,
		a.UserID.String(), a.MessageID.String())

	// Area X, with every fixture message labelled into it via its own label.
	resAreas := call(t, h.api.areaSet, a.UserID, map[string]any{
		"create": map[string]any{"x": map[string]any{"name": "AreaX"}},
	})
	created, _ := resAreas["created"].(map[string]any)
	x, ok := created["x"].(map[string]any)
	if !ok {
		t.Fatalf("AreaX create failed: %#v", resAreas)
	}
	xID, _ := x["id"].(string)
	xLabel, _ := x["labelId"].(string)

	seedMsg := func(tag string, score *float32) string {
		hash := sha256.Sum256([]byte("lineside-" + a.UserID.String() + "-" + tag))
		id := store.NewULID().String()
		mustExecAPI(t, h, `INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id)
			VALUES ($1,$2,10,20,$3)`, a.UserID.String(), hash[:], store.NewULID().String())
		mustExecAPI(t, h, `INSERT INTO app.messages (id, user_id, content_hash, subject, received_at, size_bytes)
			VALUES ($1,$2,$3,$4,now(),10)`, id, a.UserID.String(), hash[:], tag)
		if score != nil {
			mustExecAPI(t, h, `INSERT INTO app.triage_scores (user_id, message_id, score, model_version)
				VALUES ($1,$2,$3,'v0')`, a.UserID.String(), id, *score)
		}
		mustExecAPI(t, h, `INSERT INTO app.message_labels (user_id, message_id, label_id) VALUES ($1,$2,$3)`,
			a.UserID.String(), id, xLabel)
		uid := testUID()
		mustExecAPI(t, h, `INSERT INTO app.message_provenance (id, user_id, message_id, account_id, mailbox_id, uid, uidvalidity)
			VALUES ($1,$2,$3,$4,$5,$6,1)`,
			store.NewULID().String(), a.UserID.String(), id, a.AccountID.String(), a.InboxID.String(), uid)
		return id
	}
	score := func(v float32) *float32 { return &v }

	above := seedMsg("above", score(0.9))  // 0.9, threshold 0.6 -> surface
	below := seedMsg("below", score(0.2))  // 0.2               -> drawer
	unjudged := seedMsg("unjudged", nil)   // no score           -> unscored
	untouched := seedMsg("untouched", nil) // never corrected — the lineSide null control
	name := map[string]string{above: "above(0.9)", below: "below(0.2)", unjudged: "unjudged", untouched: "untouched"}
	pretty := func(ids map[string]bool) []string {
		out := make([]string, 0, len(ids))
		for id := range ids {
			if n, ok := name[id]; ok {
				out = append(out, n)
			} else {
				out = append(out, id)
			}
		}
		sort.Strings(out)
		return out
	}

	dp := call(t, h.api.deskPreferencesSet, a.UserID, map[string]any{
		"update": map[string]any{a.UserID.String(): map[string]any{"lineThreshold": 0.6}},
	})
	if _, ok := dp["updated"].(map[string]any)[a.UserID.String()]; !ok {
		t.Fatalf("setting lineThreshold to 0.6 was refused: %#v", dp)
	}

	idSet := func(res map[string]any) map[string]bool {
		ids, _ := res["ids"].([]any)
		out := make(map[string]bool, len(ids))
		for _, id := range ids {
			out[id.(string)] = true
		}
		return out
	}
	queryIDs := func(t *testing.T, onSurface bool) map[string]bool {
		t.Helper()
		return idSet(call(t, h.api.emailQuery, a.UserID, map[string]any{
			"filter": map[string]any{"inArea": xID, "onDesk": true, "onSurface": onSurface},
		}))
	}
	summaryRowX := func(t *testing.T) map[string]any {
		t.Helper()
		summary := call(t, h.api.areaGetSummary, a.UserID, map[string]any{})
		list, _ := summary["list"].([]any)
		for _, item := range list {
			obj, _ := item.(map[string]any)
			if got, _ := obj["areaId"].(string); got == xID {
				return obj
			}
		}
		t.Fatalf("AreaX missing from Area/getSummary: %#v", list)
		return nil
	}
	sameSet := func(got map[string]bool, want []string) bool {
		if len(got) != len(want) {
			return false
		}
		for _, id := range want {
			if !got[id] {
				return false
			}
		}
		return true
	}

	// assertLine is the differential: the two Email/query sides AND the
	// Area/getSummary row that must agree with them, with BOTH printed on any
	// failure — a count that disagrees with a list is the bug this pins, so a
	// failure message showing only one of them would not say which is wrong.
	assertLine := func(t *testing.T, label string, wantSurface, wantDrawer []string, wantUnscored int) {
		t.Helper()
		surfaceIDs := queryIDs(t, true)
		drawerIDs := queryIDs(t, false)
		row := summaryRowX(t)
		gotSurface, _ := row["surface"].(float64)
		gotDrawer, _ := row["drawer"].(float64)
		gotUnscored, _ := row["unscored"].(float64)

		if !sameSet(surfaceIDs, wantSurface) {
			t.Errorf("%s: Email/query onSurface:true = %v, want %v\n"+
				"    Area/getSummary row X: surface=%v drawer=%v unscored=%v",
				label, pretty(surfaceIDs), pretty(setOf(wantSurface)), gotSurface, gotDrawer, gotUnscored)
		}
		if !sameSet(drawerIDs, wantDrawer) {
			t.Errorf("%s: Email/query onSurface:false = %v, want %v\n"+
				"    Area/getSummary row X: surface=%v drawer=%v unscored=%v",
				label, pretty(drawerIDs), pretty(setOf(wantDrawer)), gotSurface, gotDrawer, gotUnscored)
		}
		// The agreement itself: the query's surface side is the summary's
		// surface+unscored (unjudged mail is never hidden in a drawer), and the
		// query's drawer side is the summary's drawer.
		if float64(len(surfaceIDs)) != gotSurface+gotUnscored {
			t.Errorf("%s: DISAGREEMENT — Email/query onSurface:true has %d ids (%v) but "+
				"Area/getSummary says surface(%v)+unscored(%v) = %v",
				label, len(surfaceIDs), pretty(surfaceIDs), gotSurface, gotUnscored, gotSurface+gotUnscored)
		}
		if float64(len(drawerIDs)) != gotDrawer {
			t.Errorf("%s: DISAGREEMENT — Email/query onSurface:false has %d ids (%v) but "+
				"Area/getSummary says drawer = %v",
				label, len(drawerIDs), pretty(drawerIDs), gotDrawer)
		}
		if int(gotUnscored) != wantUnscored {
			t.Errorf("%s: Area/getSummary unscored = %v, want %d (surface=%v drawer=%v)",
				label, gotUnscored, wantUnscored, gotSurface, gotDrawer)
		}
	}

	// The baseline, before any correction: the score alone decides.
	assertLine(t, "before any correction",
		[]string{above, unjudged, untouched}, []string{below}, 2)

	// The Email state BEFORE the corrections, for the /changes assertion below.
	stateBefore, _ := call(t, h.api.emailGet, a.UserID,
		map[string]any{"ids": []string{above}, "properties": []string{"id"}})["state"].(string)
	if stateBefore == "" {
		t.Fatal("Email/get returned no state to use as a /changes cursor")
	}

	correct := func(t *testing.T, id, value string) {
		t.Helper()
		res := call(t, h.api.triageCorrect, a.UserID, map[string]any{
			"corrections": map[string]any{id: value},
		})
		corrected, _ := res["corrected"].([]any)
		if len(corrected) != 1 {
			t.Fatalf("Triage/correct {%s: %s}: corrected = %v, notCorrected = %v",
				name[id], value, corrected, res["notCorrected"])
		}
	}

	// 1. The below-the-line message, pulled UP. It must now be on the surface
	// in the list AND in the count, and gone from the drawer side.
	correct(t, below, "important")
	assertLine(t, "after correcting below(0.2) to important",
		[]string{above, below, unjudged, untouched}, []string{}, 2)

	// 2. Symmetric: the above-the-line message, pushed DOWN. Note the summary's
	// three numbers are now identical to the baseline's — only the membership
	// shows that anything happened at all.
	correct(t, above, "notImportant")
	assertLine(t, "after correcting above(0.9) to notImportant",
		[]string{below, unjudged, untouched}, []string{above}, 2)

	// 3. An UNSCORED message placed in the drawer by hand: it leaves `unscored`
	// (nothing scored it, but somebody placed it), counts in `drawer`, and is
	// selected by onSurface:false.
	correct(t, unjudged, "notImportant")
	assertLine(t, "after correcting the unscored message to notImportant",
		[]string{below, untouched}, []string{above, unjudged}, 1)

	// 4. Every corrected id is in Email/changes since the cursor taken before
	// them — the change_log row SetLineSide appends in the same transaction. A
	// client that never re-fetches the whole list has no other way to learn the
	// message moved.
	changes := call(t, h.api.emailChanges, a.UserID, map[string]any{"sinceState": stateBefore})
	updated := map[string]bool{}
	for _, id := range changes["updated"].([]any) {
		updated[id.(string)] = true
	}
	for _, id := range []string{below, above, unjudged} {
		if !updated[id] {
			t.Errorf("Email/changes since %s: %s (%s) is not in updated = %v — the correction "+
				"moved it across the line with no change_log row, so no client will ever hear about it",
				stateBefore, name[id], id, pretty(updated))
		}
	}
	if updated[untouched] {
		t.Errorf("Email/changes since %s lists the never-corrected message as updated: %v",
			stateBefore, pretty(updated))
	}

	// 5. Email.lineSide reports the USER'S placement, and null where they made
	// none — it is the override, not the effective side.
	res := call(t, h.api.emailGet, a.UserID, map[string]any{
		"ids":        []string{below, above, unjudged, untouched},
		"properties": []string{"lineSide"},
	})
	got := map[string]any{}
	for _, item := range res["list"].([]any) {
		obj := item.(map[string]any)
		got[obj["id"].(string)] = obj["lineSide"]
	}
	for id, want := range map[string]any{
		below: "surface", above: "drawer", unjudged: "drawer", untouched: nil,
	} {
		if got[id] != want {
			t.Errorf("Email/get lineSide for %s = %#v, want %#v", name[id], got[id], want)
		}
	}
}

// setOf is the little helper assertLine's failure messages need to render a
// WANT list through the same pretty-printer as a GOT set.
func setOf(ids []string) map[string]bool {
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out
}
