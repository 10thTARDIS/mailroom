// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// THE JMAP SURFACE OF DAY-ONE SETUP (#446), over the real store.
//
// This file exists to pin the WIRE SHAPE, because the client wizard is a
// separate increment written from exactly these field names: a rename here that
// the store tests do not notice is a client that silently reads `undefined`
// forever (#155's own lesson, in the other direction). So it asserts the presence
// and the type of every field the wizard needs, not only the numbers.

import (
	"context"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// setupAPIFixture seeds one plain folder with older mail plus one recent
// message, which is the smallest mailbox on which the census, the preview and
// the apply all say something.
type setupAPIFixture struct {
	h     *apiHarness
	f     apiFixture
	inbox ulid.ULID
	// folder holds three older messages: the population every count below is
	// about.
	folder ulid.ULID
	older  []ulid.ULID
	recent ulid.ULID
}

func newSetupAPIFixture(t *testing.T) *setupAPIFixture {
	t.Helper()
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)
	fx := &setupAPIFixture{h: h, f: a, inbox: a.InboxID}
	fx.folder = h.folder(t, a, "Receipts")
	for i := 0; i < 3; i++ {
		id := h.seedMessage(t, a, "receipt", "billing@shop.example", time.Duration(60*24+i)*time.Hour)
		h.inFolder(t, a, id, fx.folder)
		fx.older = append(fx.older, id)
	}
	fx.recent = h.seedMessage(t, a, "today", "billing@shop.example", time.Hour)
	h.inFolder(t, a, fx.recent, fx.folder)
	return fx
}

func TestSetupCensusWireShape(t *testing.T) {
	fx := newSetupAPIFixture(t)

	res := call(t, fx.h.api.setupCensus, fx.f.UserID, map[string]any{})
	if got, _ := res["windowDays"].(float64); got != setupDefaultWindowDays {
		t.Errorf("windowDays = %v, want the documented default %d", res["windowDays"], setupDefaultWindowDays)
	}
	folders, _ := res["folders"].([]any)
	if len(folders) < 2 {
		t.Fatalf("census returned %d folder(s), want at least 2 (the folder and INBOX): %v", len(folders), res)
	}
	byID := map[string]map[string]any{}
	for _, raw := range folders {
		m, _ := raw.(map[string]any)
		id, _ := m["mailboxId"].(string)
		byID[id] = m
	}
	// The Trash the fixture seeds is SPECIAL-USE and must not be a row.
	if _, present := byID[fx.f.TrashID.String()]; present {
		t.Error("the Trash folder has a census row; special-use folders are mapped without asking")
	}
	row, ok := byID[fx.folder.String()]
	if !ok {
		t.Fatalf("the plain folder has no census row: %v", folders)
	}
	// EVERY FIELD THE WIZARD READS, by name. A missing one is a `undefined` in
	// the client, which reads fine and is wrong forever.
	for _, key := range []string{
		"mailboxId", "accountId", "name", "role", "feedsDesk",
		"live", "inWindow", "older", "unclassified",
		"distinctSenders", "distinctDomains", "sendersFor90", "bulkShare", "shape",
		"existingLabelId", "existingAreaId",
	} {
		if _, present := row[key]; !present {
			t.Errorf("census row is missing %q — the client increment is written from these names", key)
		}
	}
	if n, _ := row["live"].(float64); n != 4 {
		t.Errorf("live = %v, want 4", row["live"])
	}
	if n, _ := row["older"].(float64); n != 3 {
		t.Errorf("older = %v, want 3", row["older"])
	}
	if n, _ := row["inWindow"].(float64); n != 1 {
		t.Errorf("inWindow = %v, want 1", row["inWindow"])
	}
	// existingLabelId/existingAreaId are NULL rather than absent when there is
	// none: "there is no label of this name" is an answer, and a client should not
	// have to distinguish it from "the server did not say".
	if v, present := row["existingLabelId"]; !present || v != nil {
		t.Errorf("existingLabelId = %v (present=%v), want an explicit null", v, present)
	}
	if role, _ := byID[fx.inbox.String()]["role"].(string); role != "inbox" {
		t.Errorf("INBOX role = %q, want %q — the inbox IS a census row", role, "inbox")
	}

	// A window the client offers explicitly is honoured; a non-positive one is
	// refused rather than defaulted.
	res = call(t, fx.h.api.setupCensus, fx.f.UserID, map[string]any{"windowDays": 90})
	if got, _ := res["windowDays"].(float64); got != 90 {
		t.Errorf("windowDays = %v, want 90", res["windowDays"])
	}
	if _, mErr := fx.h.api.setupCensus(context.Background(), fx.f.UserID,
		mustJSON(t, map[string]any{"windowDays": 0})); mErr == nil {
		t.Error("Setup/census accepted windowDays 0")
	}
}

func TestSetupPreviewApplyGetAndUndoWireShape(t *testing.T) {
	fx := newSetupAPIFixture(t)
	ctx := context.Background()

	plan := map[string]any{
		"windowDays": 30,
		"folders": []any{
			map[string]any{
				"mailboxId": fx.folder.String(),
				"action":    "labelArchive",
				"labelName": "Receipts",
				"areaName":  "Receipts",
			},
		},
	}

	// ── preview: writes nothing ───────────────────────────────────────────────
	res := call(t, fx.h.api.setupPreview, fx.f.UserID, map[string]any{"plan": plan})
	counts, _ := res["counts"].(map[string]any)
	if counts == nil {
		t.Fatalf("Setup/preview returned no counts: %v", res)
	}
	for _, key := range []string{"labels", "archived", "moves", "leave", "unclassified", "perFolder", "estimatedSeconds"} {
		if _, present := counts[key]; !present {
			t.Errorf("counts is missing %q", key)
		}
	}
	if n, _ := counts["labels"].(float64); n != 3 {
		t.Errorf("labels = %v, want 3", counts["labels"])
	}
	if n, _ := counts["moves"].(float64); n != 0 {
		t.Errorf("moves = %v, want 0 — a labelArchive plan moves nothing on the server", counts["moves"])
	}
	perFolder, _ := counts["perFolder"].([]any)
	if len(perFolder) == 0 {
		t.Fatal("perFolder is empty; a single total cannot tell an empty plan from an empty folder")
	}
	pf, _ := perFolder[0].(map[string]any)
	for _, key := range []string{"mailboxId", "name", "action", "older", "labels", "archived", "moves"} {
		if _, present := pf[key]; !present {
			t.Errorf("perFolder row is missing %q", key)
		}
	}
	var runsAfterPreview int
	if err := fx.h.super.QueryRow(ctx, `SELECT count(*) FROM app.setup_runs WHERE user_id=$1`,
		fx.f.UserID.String()).Scan(&runsAfterPreview); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if runsAfterPreview != 0 {
		t.Errorf("Setup/preview created %d run(s); it must write nothing", runsAfterPreview)
	}

	// ── apply: the run, and the planned counts it will be held to ─────────────
	res = call(t, fx.h.api.setupApply, fx.f.UserID, map[string]any{"plan": plan})
	runIDStr, _ := res["runId"].(string)
	if runIDStr == "" {
		t.Fatalf("Setup/apply returned no runId: %v", res)
	}
	runID, err := ulid.Parse(runIDStr)
	if err != nil {
		t.Fatalf("runId %q is not a ULID: %v", runIDStr, err)
	}
	if _, present := res["counts"]; !present {
		t.Error("Setup/apply does not echo the counts the run will be judged against")
	}

	// A SECOND apply while one is outstanding is refused with its OWN error type:
	// the request was well formed and would be valid later, so a wizard must be
	// able to tell this from a validation failure.
	_, mErr := fx.h.api.setupApply(ctx, fx.f.UserID, mustJSON(t, map[string]any{"plan": plan}))
	if mErr == nil {
		t.Fatal("Setup/apply created a second run while one was outstanding")
	}
	if mErr.Type != errSetupRunBusy {
		t.Errorf("second Setup/apply error type = %q, want %q", mErr.Type, errSetupRunBusy)
	}

	// Undo is refused while the run has not finished — the same typed error.
	_, mErr = fx.h.api.setupUndo(ctx, fx.f.UserID, mustJSON(t, map[string]any{"runId": runIDStr}))
	if mErr == nil {
		t.Fatal("Setup/undo accepted a run that has not finished")
	}
	if mErr.Type != errSetupRunBusy {
		t.Errorf("Setup/undo on a queued run: error type = %q, want %q", mErr.Type, errSetupRunBusy)
	}

	// ── get: the run object, every field the wizard polls ─────────────────────
	res = call(t, fx.h.api.setupGet, fx.f.UserID, map[string]any{})
	list, _ := res["list"].([]any)
	if len(list) != 1 {
		t.Fatalf("Setup/get returned %d run(s), want 1: %v", len(list), res)
	}
	run, _ := list[0].(map[string]any)
	for _, key := range []string{
		"id", "kind", "status", "windowDays", "plan",
		"plannedLabels", "labelsWritten", "plannedArchived", "archived",
		"plannedMoves", "moved", "leftForLater", "unclassified",
		"error", "createdAt", "startedAt", "finishedAt", "undoneAt",
	} {
		if _, present := run[key]; !present {
			t.Errorf("SetupRun object is missing %q", key)
		}
	}
	if status, _ := run["status"].(string); status != store.SetupStatusQueued {
		t.Errorf("a fresh run's status = %q, want %q", status, store.SetupStatusQueued)
	}
	if n, _ := run["plannedLabels"].(float64); n != 3 {
		t.Errorf("plannedLabels = %v, want 3 — the run records what the preview promised", run["plannedLabels"])
	}
	if res["state"] == nil {
		t.Error("Setup/get returns no state; the client cannot follow the run through /changes without one")
	}
	// An unknown id comes back in notFound, exactly like every other /get.
	res = call(t, fx.h.api.setupGet, fx.f.UserID, map[string]any{"ids": []string{store.NewULID().String()}})
	if nf, _ := res["notFound"].([]any); len(nf) != 1 {
		t.Errorf("Setup/get of an unknown id: notFound = %v, want one entry", res["notFound"])
	}

	// ── drain it, then undo it ────────────────────────────────────────────────
	drain := func(undo bool) {
		for i := 0; i < 50; i++ {
			var p store.SetupProgress
			if err := fx.h.store.UserTx(ctx, fx.f.UserID, func(tx *store.UserTx) error {
				var e error
				if undo {
					p, e = tx.SetupUndoChunk(ctx, runID, 2)
				} else {
					p, e = tx.SetupApplyChunk(ctx, runID, 2)
				}
				return e
			}); err != nil {
				t.Fatalf("chunk: %v", err)
			}
			if p.Done {
				return
			}
		}
		t.Fatal("run did not finish in 50 chunks")
	}
	drain(false)

	// Email.deskArchivedAt is opt-in and renders as a UTCDate once the run has
	// archived the message. It is a DIFFERENT property from drawerArchivedAt,
	// which stays null: nothing moved.
	got := call(t, fx.h.api.emailGet, fx.f.UserID, map[string]any{
		"ids":        []string{fx.older[0].String()},
		"properties": []string{"id", "deskArchivedAt", "drawerArchivedAt"},
	})
	objs, _ := got["list"].([]any)
	if len(objs) != 1 {
		t.Fatalf("Email/get returned %d object(s): %v", len(objs), got)
	}
	obj, _ := objs[0].(map[string]any)
	if s, ok := obj["deskArchivedAt"].(string); !ok || s == "" {
		t.Errorf("deskArchivedAt = %v, want a UTCDate string", obj["deskArchivedAt"])
	}
	if obj["drawerArchivedAt"] != nil {
		t.Errorf("drawerArchivedAt = %v, want null — day-one moved nothing, so no drawer sweep "+
			"archived this message; conflating the two would tell the user their mail was moved",
			obj["drawerArchivedAt"])
	}
	// Not requested, not present: the property is opt-in like every other
	// Mailroom Email extension.
	got = call(t, fx.h.api.emailGet, fx.f.UserID, map[string]any{
		"ids":        []string{fx.older[0].String()},
		"properties": []string{"id"},
	})
	objs, _ = got["list"].([]any)
	obj, _ = objs[0].(map[string]any)
	if _, present := obj["deskArchivedAt"]; present {
		t.Errorf("deskArchivedAt present without being requested: %v", obj)
	}

	// The Email/query filter names the set: it is the only way a client can show
	// "here is what day-one filed away".
	q := call(t, fx.h.api.emailQuery, fx.f.UserID, map[string]any{
		"filter": map[string]any{"deskArchived": true}, "calculateTotal": true,
	})
	if n, _ := q["total"].(float64); n != 3 {
		t.Errorf("deskArchived:true total = %v, want 3", q["total"])
	}
	// It is NOT delta-stable: undoing a run clears the mark on every message it
	// touched inside a background job, so a client holding this query cannot be
	// handed a replayable delta.
	_, mErr = fx.h.api.emailQueryChanges(ctx, fx.f.UserID, mustJSON(t, map[string]any{
		"filter": map[string]any{"deskArchived": true}, "sinceQueryState": "s:1",
	}))
	if mErr == nil || mErr.Type != errCannotCalculateChanges {
		t.Errorf("Email/queryChanges with deskArchived: %+v, want %s", mErr, errCannotCalculateChanges)
	}

	// ── undo, now that it has finished ────────────────────────────────────────
	res = call(t, fx.h.api.setupUndo, fx.f.UserID, map[string]any{"runId": runIDStr})
	if got, _ := res["runId"].(string); got != runIDStr {
		t.Errorf("Setup/undo runId = %q, want %q echoed back", got, runIDStr)
	}
	drain(true)
	res = call(t, fx.h.api.setupGet, fx.f.UserID, map[string]any{"ids": []string{runIDStr}})
	list, _ = res["list"].([]any)
	run, _ = list[0].(map[string]any)
	if status, _ := run["status"].(string); status != store.SetupStatusUndone {
		t.Errorf("status after undo = %q, want %q", status, store.SetupStatusUndone)
	}
	if run["undoneAt"] == nil {
		t.Error("undoneAt is null on an undone run")
	}
	q = call(t, fx.h.api.emailQuery, fx.f.UserID, map[string]any{
		"filter": map[string]any{"deskArchived": true}, "calculateTotal": true,
	})
	if n, _ := q["total"].(float64); n != 0 {
		t.Errorf("deskArchived:true total after the undo = %v, want 0", q["total"])
	}
}

// DeskPreferences.setupCompletedAt is how the client decides to open the WIZARD
// rather than the desk, so both directions have to work over the wire: a date,
// and back to null.
func TestDeskPreferencesSetupCompletedAtOverJMAP(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	res := call(t, h.api.deskPreferencesGet, a.UserID, map[string]any{})
	list, _ := res["list"].([]any)
	if len(list) != 1 {
		t.Fatalf("DeskPreferences/get returned %d record(s), want 1", len(list))
	}
	obj, _ := list[0].(map[string]any)
	v, present := obj["setupCompletedAt"]
	if !present {
		t.Fatal("setupCompletedAt is absent from DeskPreferences; the client cannot tell whether " +
			"to open the day-one wizard")
	}
	if v != nil {
		t.Errorf("setupCompletedAt = %v on a login that has never run day-one, want null", v)
	}

	when := "2026-09-08T10:11:12Z"
	res = call(t, h.api.deskPreferencesSet, a.UserID, map[string]any{
		"update": map[string]any{a.UserID.String(): map[string]any{"setupCompletedAt": when}},
	})
	if !mapHasKey(res, "updated", a.UserID.String()) {
		t.Fatalf("DeskPreferences/set did not accept setupCompletedAt: %v", res)
	}
	res = call(t, h.api.deskPreferencesGet, a.UserID, map[string]any{})
	list, _ = res["list"].([]any)
	obj, _ = list[0].(map[string]any)
	if got, _ := obj["setupCompletedAt"].(string); got != when {
		t.Errorf("setupCompletedAt = %v, want %q", obj["setupCompletedAt"], when)
	}

	// Back to null — "show me the wizard again" has to be expressible.
	res = call(t, h.api.deskPreferencesSet, a.UserID, map[string]any{
		"update": map[string]any{a.UserID.String(): map[string]any{"setupCompletedAt": nil}},
	})
	if !mapHasKey(res, "updated", a.UserID.String()) {
		t.Fatalf("DeskPreferences/set did not accept a null setupCompletedAt: %v", res)
	}
	res = call(t, h.api.deskPreferencesGet, a.UserID, map[string]any{})
	list, _ = res["list"].([]any)
	obj, _ = list[0].(map[string]any)
	if obj["setupCompletedAt"] != nil {
		t.Errorf("setupCompletedAt = %v after clearing, want null", obj["setupCompletedAt"])
	}

	// A malformed date is refused by property name, not swallowed.
	res = call(t, h.api.deskPreferencesSet, a.UserID, map[string]any{
		"update": map[string]any{a.UserID.String(): map[string]any{"setupCompletedAt": "yesterday"}},
	})
	if !mapHasKey(res, "notUpdated", a.UserID.String()) {
		t.Errorf("DeskPreferences/set accepted setupCompletedAt %q: %v", "yesterday", res)
	}
}
