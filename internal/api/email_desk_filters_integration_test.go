// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// Email/query's `onDesk` and `onSurface` filters (#375 increment 1): the SAME
// desk-membership and surface/drawer predicates Area/getSummary counts over
// (store.deskMessageSQL, internal/store/areas_resolve.go), now selectable by
// Email/query so a desk tab's query and its count can never disagree.

import (
	"context"
	"crypto/sha256"
	"testing"

	"mailroom/internal/store"
)

func TestEmailQueryDeskFilters(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	// seedTwo's own fixture message (a.MessageID) lives in the Inbox with no
	// score and no relevant label — it would otherwise inflate every
	// onDesk/onSurface set below with a message this test never seeded (the
	// same wrinkle email_area_integration_test.go's `unsorted` case works
	// AROUND by naming it explicitly; here, trashing it is simpler and keeps
	// every assertion below an exact set). Trashed mail is never on the desk.
	mustExecAPI(t, h, `UPDATE app.messages SET trashed_at = now() WHERE user_id = $1 AND id = $2`,
		a.UserID.String(), a.MessageID.String())

	// Area X: every fixture message below (a-g) is labelled into it via its
	// own label, copying email_area_integration_test.go's shape.
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

	// A folder that feeds the desk like INBOX (Lists), and Archive, which does
	// not — the shape area_summary_integration_test.go uses for the same
	// distinction.
	listsMailbox := store.NewULID().String()
	mustExecAPI(t, h, `INSERT INTO app.mailboxes (id, user_id, account_id, imap_name, display_name, feeds_desk)
		VALUES ($1,$2,$3,'Lists','Lists',true)`, listsMailbox, a.UserID.String(), a.AccountID.String())
	archiveMailbox := store.NewULID().String()
	mustExecAPI(t, h, `INSERT INTO app.mailboxes (id, user_id, account_id, imap_name, display_name, special_use)
		VALUES ($1,$2,$3,'Archive','Archive','\Archive')`, archiveMailbox, a.UserID.String(), a.AccountID.String())

	seedMsg := func(tag string, score *float32) string {
		hash := sha256.Sum256([]byte("deskfilters-" + a.UserID.String() + "-" + tag))
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
		return id
	}
	provenance := func(msg, account, mailbox string) {
		uid := testUID()
		mustExecAPI(t, h, `INSERT INTO app.message_provenance (id, user_id, message_id, account_id, mailbox_id, uid, uidvalidity)
			VALUES ($1,$2,$3,$4,$5,$6,1)`, store.NewULID().String(), a.UserID.String(), msg, account, mailbox, uid)
	}
	trash := func(msg string) {
		mustExecAPI(t, h, `UPDATE app.messages SET trashed_at = now() WHERE user_id = $1 AND id = $2`,
			a.UserID.String(), msg)
	}
	score := func(v float32) *float32 { return &v }

	// The fixture table from the spec: id / where (live provenance) / score.
	msgA := seedMsg("a", score(0.9)) // INBOX, 0.9            -> desk, surface
	provenance(msgA, a.AccountID.String(), a.InboxID.String())

	msgB := seedMsg("b", score(0.2)) // INBOX, 0.2            -> desk, drawer
	provenance(msgB, a.AccountID.String(), a.InboxID.String())

	msgC := seedMsg("c", nil) // INBOX, unscored              -> desk, surface (unscored)
	provenance(msgC, a.AccountID.String(), a.InboxID.String())

	msgD := seedMsg("d", score(0.9)) // Archive only, 0.9     -> NOT desk; surface
	provenance(msgD, a.AccountID.String(), archiveMailbox)

	msgE := seedMsg("e", score(0.9)) // Lists (feeds_desk), 0.9 -> desk, surface
	provenance(msgE, a.AccountID.String(), listsMailbox)

	msgF := seedMsg("f", score(0.9)) // INBOX, trashed, 0.9   -> NOT desk
	provenance(msgF, a.AccountID.String(), a.InboxID.String())
	trash(msgF)

	msgG := seedMsg("g", score(0.6)) // INBOX, score exactly 0.6 -> desk, surface (>= inclusive)
	provenance(msgG, a.AccountID.String(), a.InboxID.String())

	// lineThreshold = 0.6 via DeskPreferences/set.
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
	wantSet := func(ids ...string) map[string]bool {
		out := make(map[string]bool, len(ids))
		for _, id := range ids {
			out[id] = true
		}
		return out
	}
	eqSet := func(t *testing.T, label string, got, want map[string]bool) {
		t.Helper()
		if len(got) != len(want) {
			t.Errorf("%s ids = %v (%d), want %v (%d)", label, got, len(got), want, len(want))
			return
		}
		for id := range want {
			if !got[id] {
				t.Errorf("%s ids = %v, want %v", label, got, want)
				return
			}
		}
	}

	// 1. {onDesk:true} -> {a,b,c,e,g}. {onDesk:false} -> contains d, none of
	// a,b,c,e,g.
	//
	// f is deliberately NOT asserted present in {onDesk:false}: QueryEmails
	// applies its OWN, pre-existing "m.trashed_at IS NULL" condition to every
	// query that does not explicitly target the Trash mailbox (see its ⚠
	// comment — "a message IN [Trash] is only visible when the query asks for
	// it... a trashed message is gone"), so f is invisible to Email/query
	// altogether, independent of onDesk. f's job in this fixture is fully
	// discharged by the onDesk:true assertion above correctly excluding it
	// (proving trashed mail is never "on the desk" even when onDesk itself
	// would otherwise match its INBOX provenance) — this is a spec-example
	// versus code mismatch found by running the test, not a bug in onDesk.
	res := call(t, h.api.emailQuery, a.UserID, map[string]any{"filter": map[string]any{"onDesk": true}})
	eqSet(t, "onDesk:true", idSet(res), wantSet(msgA, msgB, msgC, msgE, msgG))

	res = call(t, h.api.emailQuery, a.UserID, map[string]any{"filter": map[string]any{"onDesk": false}})
	notDesk := idSet(res)
	if !notDesk[msgD] {
		t.Errorf("onDesk:false ids = %v, want to contain d (%s)", notDesk, msgD)
	}
	for _, id := range []string{msgA, msgB, msgC, msgE, msgG} {
		if notDesk[id] {
			t.Errorf("onDesk:false ids = %v, must not contain %s", notDesk, id)
		}
	}
	if notDesk[msgF] {
		t.Errorf("onDesk:false ids = %v contains f (%s), but f is trashed and QueryEmails' "+
			"own base filter should have hidden it regardless of onDesk", notDesk, msgF)
	}

	// 2. {onDesk:true, onSurface:true} -> {a,c,e,g}.
	// {onDesk:true, onSurface:false} -> {b}.
	res = call(t, h.api.emailQuery, a.UserID, map[string]any{
		"filter": map[string]any{"onDesk": true, "onSurface": true},
	})
	eqSet(t, "onDesk:true,onSurface:true", idSet(res), wantSet(msgA, msgC, msgE, msgG))

	res = call(t, h.api.emailQuery, a.UserID, map[string]any{
		"filter": map[string]any{"onDesk": true, "onSurface": false},
	})
	eqSet(t, "onDesk:true,onSurface:false", idSet(res), wantSet(msgB))

	// 3. {onSurface:true} alone includes d (surface is independent of desk
	// membership).
	res = call(t, h.api.emailQuery, a.UserID, map[string]any{"filter": map[string]any{"onSurface": true}})
	if got := idSet(res); !got[msgD] {
		t.Errorf("onSurface:true alone ids = %v, want to include d (%s)", got, msgD)
	}

	// 4. {inArea:X, onDesk:true, onSurface:true} count == Area/getSummary row
	// X's surface+unscored, and {inArea:X, onDesk:true, onSurface:false} count
	// == that row's drawer — using Area/getSummary with NO threshold argument,
	// so both read the stored preference. This must hold on BOTH sides of a
	// threshold change (the differential).
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
	checkSummaryAgreement := func(t *testing.T, label string) {
		t.Helper()
		surfaceRes := call(t, h.api.emailQuery, a.UserID, map[string]any{
			"filter": map[string]any{"inArea": xID, "onDesk": true, "onSurface": true},
		})
		drawerRes := call(t, h.api.emailQuery, a.UserID, map[string]any{
			"filter": map[string]any{"inArea": xID, "onDesk": true, "onSurface": false},
		})
		surfaceCount := len(idSet(surfaceRes))
		drawerCount := len(idSet(drawerRes))

		row := summaryRowX(t)
		wantSurface := row["surface"].(float64) + row["unscored"].(float64)
		wantDrawer := row["drawer"].(float64)
		if float64(surfaceCount) != wantSurface {
			t.Errorf("%s: Email/query {inArea,onDesk:true,onSurface:true} count = %d, "+
				"Area/getSummary surface(%v)+unscored(%v) = %v",
				label, surfaceCount, row["surface"], row["unscored"], wantSurface)
		}
		if float64(drawerCount) != wantDrawer {
			t.Errorf("%s: Email/query {inArea,onDesk:true,onSurface:false} count = %d, "+
				"Area/getSummary drawer = %v", label, drawerCount, wantDrawer)
		}
	}
	checkSummaryAgreement(t, "threshold 0.6")

	dp = call(t, h.api.deskPreferencesSet, a.UserID, map[string]any{
		"update": map[string]any{a.UserID.String(): map[string]any{"lineThreshold": 0.95}},
	})
	if _, ok := dp["updated"].(map[string]any)[a.UserID.String()]; !ok {
		t.Fatalf("raising lineThreshold to 0.95 was refused: %#v", dp)
	}
	// a, e (0.9) and g (0.6) must have crossed into the drawer.
	res = call(t, h.api.emailQuery, a.UserID, map[string]any{
		"filter": map[string]any{"inArea": xID, "onDesk": true, "onSurface": false},
	})
	if got := idSet(res); !got[msgA] || !got[msgE] || !got[msgG] {
		t.Errorf("after raising lineThreshold to 0.95, onDesk+onSurface:false ids = %v, "+
			"want to include a (%s), e (%s), g (%s)", got, msgA, msgE, msgG)
	}
	checkSummaryAgreement(t, "threshold 0.95")

	// 5. inAccount + onDesk scoping: a second account, its own INBOX, one
	// scored message h in it. {onDesk:true, inAccount:acct1} excludes h;
	// {onDesk:true, inAccount:acct2} returns exactly {h}.
	otherAccount := store.NewULID().String()
	mustExecAPI(t, h, `INSERT INTO app.accounts (id, user_id, name, imap_host, smtp_host, credentials_ct, credentials_dek)
		VALUES ($1,$2,'other','mail.example.com','smtp.example.com',$3,$4)`,
		otherAccount, a.UserID.String(), []byte{0x01}, store.NewULID().String())
	otherInbox := store.NewULID().String()
	mustExecAPI(t, h, `INSERT INTO app.mailboxes (id, user_id, account_id, imap_name, display_name)
		VALUES ($1,$2,$3,'INBOX','Inbox')`, otherInbox, a.UserID.String(), otherAccount)

	msgH := seedMsg("h", score(0.9))
	provenance(msgH, otherAccount, otherInbox)

	res = call(t, h.api.emailQuery, a.UserID, map[string]any{
		"filter": map[string]any{"onDesk": true, "inAccount": a.AccountID.String()},
	})
	eqSet(t, "onDesk:true,inAccount:acct1", idSet(res), wantSet(msgA, msgB, msgC, msgE, msgG))

	res = call(t, h.api.emailQuery, a.UserID, map[string]any{
		"filter": map[string]any{"onDesk": true, "inAccount": otherAccount},
	})
	eqSet(t, "onDesk:true,inAccount:acct2", idSet(res), wantSet(msgH))

	// 6. Email/queryChanges with either filter falls back to
	// cannotCalculateChanges — the ⚠ note on deltaStableConditions (#375),
	// mirroring the existing inArea assertion.
	_, mErr := h.api.emailQueryChanges(context.Background(), a.UserID, mustJSON(t, map[string]any{
		"sinceQueryState": "s:0",
		"filter":          map[string]any{"onDesk": true},
	}))
	if mErr == nil || mErr.Type != errCannotCalculateChanges {
		t.Errorf("Email/queryChanges with onDesk: got %+v, want cannotCalculateChanges", mErr)
	}
	_, mErr = h.api.emailQueryChanges(context.Background(), a.UserID, mustJSON(t, map[string]any{
		"sinceQueryState": "s:0",
		"filter":          map[string]any{"onSurface": true},
	}))
	if mErr == nil || mErr.Type != errCannotCalculateChanges {
		t.Errorf("Email/queryChanges with onSurface: got %+v, want cannotCalculateChanges", mErr)
	}
}
