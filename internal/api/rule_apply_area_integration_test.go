// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// Rule/apply for a rule that files into an AREA (#708 item 3).
//
// The defect this closes: existing mail already carries a desk (or day-one
// setup) placement label, the resolver is labels-first by area position, and
// a backfill that only ADDS Updates' label to mail in Primary (position 0)
// moves nothing — while reporting it changed every message. So an area-filing
// apply removes the other areas' AUTOMATION placement labels, and never a
// user's or another rule's: those messages stay where they are and are
// counted, per outcome, in the preview before anything runs and in the result
// after.

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// defaultAreas materialises the user's default area plan and returns it by
// name — Primary at position 0 and Updates after Promotions, as shipped.
func (h *apiHarness) defaultAreas(t *testing.T, f apiFixture) map[string]store.AreaView {
	t.Helper()
	out := map[string]store.AreaView{}
	if err := h.store.UserTx(context.Background(), f.UserID, func(tx *store.UserTx) error {
		views, e := tx.ListAreas(context.Background())
		for _, v := range views {
			out[v.Name] = v
		}
		return e
	}); err != nil {
		t.Fatalf("list areas: %v", err)
	}
	for _, n := range []string{"Primary", "Promotions", "Updates", "Forums"} {
		if _, ok := out[n]; !ok {
			t.Fatalf("default plan has no %q area: %v", n, out)
		}
	}
	if out["Primary"].Position >= out["Updates"].Position || out["Updates"].Position >= out["Forums"].Position {
		t.Fatalf("premise: Primary < Updates < Forums by position, got %d/%d/%d",
			out["Primary"].Position, out["Updates"].Position, out["Forums"].Position)
	}
	return out
}

// labelByRule attaches a label attributed to a specific rule.
func (h *apiHarness) labelByRule(t *testing.T, f apiFixture, msgID, labelID, ruleID ulid.ULID) {
	t.Helper()
	mustExecAPI(t, h, `INSERT INTO app.message_labels (user_id, message_id, label_id, applied_by, applied_by_rule_id)
		VALUES ($1,$2,$3,'rule',$4)`, f.UserID.String(), msgID.String(), labelID.String(), ruleID.String())
}

// live gives a seeded message live provenance in the fixture's account, so a
// mutation of it HAS a writeback target. Without this, "no intents were
// enqueued" is true of every update and proves nothing.
func (h *apiHarness) live(t *testing.T, f apiFixture, msgID ulid.ULID) {
	t.Helper()
	mustExecAPI(t, h, `INSERT INTO app.message_provenance (id, user_id, message_id, account_id, mailbox_id, uid, uidvalidity)
		VALUES ($1,$2,$3,$4,$5,$6,1)`,
		store.NewULID().String(), f.UserID.String(), msgID.String(), f.AccountID.String(), f.InboxID.String(), testUID())
}

// labelRows is a message's label set with full provenance, sorted — the thing
// an undo has to restore EXACTLY.
func (h *apiHarness) labelRows(t *testing.T, f apiFixture, msgID ulid.ULID) string {
	t.Helper()
	rows, err := h.super.Query(context.Background(),
		`SELECT label_id || ':' || applied_by || ':' || COALESCE(applied_by_rule_id, '-')
		   FROM app.message_labels WHERE user_id = $1 AND message_id = $2`,
		f.UserID.String(), msgID.String())
	if err != nil {
		t.Fatalf("read label rows: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan label row: %v", err)
		}
		out = append(out, s)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

// areaOf is a message's resolved area id through the one resolver the desk
// uses, or "" for Needs sorting.
func (h *apiHarness) areaOf(t *testing.T, f apiFixture, msgID ulid.ULID) string {
	t.Helper()
	var got string
	if err := h.store.UserTx(context.Background(), f.UserID, func(tx *store.UserTx) error {
		res, e := tx.ResolveAreas(context.Background(), []ulid.ULID{msgID})
		if r, ok := res[msgID]; ok && r.AreaID != nil {
			got = r.AreaID.String()
		}
		return e
	}); err != nil {
		t.Fatalf("resolve area: %v", err)
	}
	return got
}

func (h *apiHarness) userCount(t *testing.T, f apiFixture, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := h.super.QueryRow(context.Background(), sql, append([]any{f.UserID.String()}, args...)...).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func num(m map[string]any, k string) int {
	v, _ := m[k].(float64)
	return int(v)
}

// areaFilingCounts reads the per-outcome object as "move/already/user/rule/other".
func areaFilingCounts(t *testing.T, res map[string]any) string {
	t.Helper()
	af, ok := res["areaFiling"].(map[string]any)
	if !ok {
		t.Fatalf("areaFiling = %#v, want an object for an area-filing rule", res["areaFiling"])
	}
	return strings.Join([]string{
		strconv.Itoa(num(af, "move")), strconv.Itoa(num(af, "alreadyThere")),
		strconv.Itoa(num(af, "contestedByUser")), strconv.Itoa(num(af, "contestedByRule")), strconv.Itoa(num(af, "contestedOther")),
	}, "/")
}

// THE FEATURE, every outcome in one mailbox, preview -> apply -> undo.
func TestRuleApplyFilesExistingMailIntoAnArea(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)
	areas := h.defaultAreas(t, a)
	primary, promos, updates, forums := areas["Primary"], areas["Promotions"], areas["Updates"], areas["Forums"]

	const sender = "news@shop.example"
	conds := map[string]any{"field": "from", "op": "equals", "value": sender}
	actions := []any{map[string]any{"type": "addLabel", "label": updates.LabelID.String()}}
	other := h.createRule(t, a, "forums-by-rule", map[string]any{"field": "subject", "op": "contains", "value": "zzz-never"},
		[]any{map[string]any{"type": "addLabel", "label": forums.LabelID.String()}})

	seed := func(subject string, i int) ulid.ULID {
		id := h.seedMessage(t, a, subject, sender, time.Duration(i)*time.Minute)
		h.live(t, a, id)
		return id
	}
	deskPrimary := seed("desk placed it in Primary", 1)
	h.applyLabel(t, a, deskPrimary, primary.LabelID, store.LabelByDesk)
	userPrimary := seed("the user filed it in Primary", 2)
	h.applyLabel(t, a, userPrimary, primary.LabelID, store.LabelByUser)
	userForums := seed("the user filed it in Forums, AFTER Updates", 3)
	h.applyLabel(t, a, userForums, forums.LabelID, store.LabelByUser)
	ruleForums := seed("another rule filed it in Forums", 4)
	h.labelByRule(t, a, ruleForums, forums.LabelID, other)
	setupPromos := seed("day-one setup put it in Promotions", 5)
	h.applyLabel(t, a, setupPromos, promos.LabelID, store.LabelBySetup)
	already := seed("already in Updates", 6)
	h.applyLabel(t, a, already, updates.LabelID, store.LabelByDesk)
	byTab := seed("in Primary by its tab verdict alone", 7)
	mustExecAPI(t, h, `UPDATE app.messages SET classified_tab = 'primary' WHERE id = $1`, byTab.String())
	// A day-one FOLDER label that feeds Promotions: a topic, not the area's
	// own label, so it is not taken off even though setup applied it.
	folder := h.label(t, a, "Shop folder")
	mustExecAPI(t, h, `INSERT INTO app.area_feeds (id, user_id, area_id, label_id) VALUES ($1,$2,$3,$4)`,
		store.NewULID().String(), a.UserID.String(), promos.ID.String(), folder.String())
	setupTopic := seed("day-one filed it under a folder label feeding Promotions", 8)
	h.applyLabel(t, a, setupTopic, folder, store.LabelBySetup)
	// Not matched: another sender, desk-placed in Primary.
	unmatched := h.seedMessage(t, a, "someone else", "friend@elsewhere.example", 9*time.Minute)
	h.live(t, a, unmatched)
	h.applyLabel(t, a, unmatched, primary.LabelID, store.LabelByDesk)
	// An open suggestion on a message that moves: the backfill is not the user
	// answering it, so it must survive.
	h.suggest(t, a, deskPrimary, a.LabelID, 0.9)

	all := []ulid.ULID{deskPrimary, userPrimary, userForums, ruleForums, setupPromos, already, byTab, setupTopic, unmatched}
	beforeRows := map[ulid.ULID]string{}
	beforeArea := map[ulid.ULID]string{}
	beforeChanges := map[ulid.ULID]int{}
	for _, id := range all {
		beforeRows[id] = h.labelRows(t, a, id)
		beforeArea[id] = h.areaOf(t, a, id)
		beforeChanges[id] = h.countEmailChanges(t, a.UserID, id)
	}
	// Premises: each message is where its fixture says.
	for id, want := range map[ulid.ULID]ulid.ULID{
		deskPrimary: primary.ID, userPrimary: primary.ID, userForums: forums.ID, ruleForums: forums.ID,
		setupPromos: promos.ID, already: updates.ID, byTab: primary.ID, setupTopic: promos.ID, unmatched: primary.ID,
	} {
		if beforeArea[id] != want.String() {
			t.Fatalf("premise: %s resolves to %q, want %s", id, beforeArea[id], want)
		}
	}
	intentsBefore := h.userCount(t, a, `SELECT count(*) FROM app.writeback_intents WHERE user_id = $1`)

	// 1. THE REPORT FIRST. Per outcome, summing to matched, and writing nothing.
	pre := call(t, h.api.rulePreview, a.UserID, map[string]any{
		"conditions": conds, "targetLabelId": updates.LabelID.String(),
	})
	if num(pre, "matched") != 8 {
		t.Fatalf("preview matched = %v, want 8", pre["matched"])
	}
	if got := areaFilingCounts(t, pre); got != "3/1/2/1/1" {
		t.Errorf("preview move/already/user/rule/other = %s, want 3/1/2/1/1", got)
	}
	for _, id := range all {
		if h.labelRows(t, a, id) != beforeRows[id] || h.countEmailChanges(t, a.UserID, id) != beforeChanges[id] {
			t.Fatalf("Rule/preview wrote to %s", id)
		}
	}

	// 2. APPLY.
	ruleID := h.createRule(t, a, "shop news is Updates", conds, actions)
	res := h.applyRule(t, a, ruleID)
	if got := areaFilingCounts(t, res); got != "3/1/2/1/1" {
		t.Errorf("apply move/already/user/rule/other = %s, want 3/1/2/1/1 (the preview's own numbers)", got)
	}
	if num(res, "changed") != 3 || num(res, "alreadyDone") != 1 {
		t.Errorf("changed=%v alreadyDone=%v, want 3 and 1", res["changed"], res["alreadyDone"])
	}

	// Where everything is now.
	for id, want := range map[ulid.ULID]ulid.ULID{
		deskPrimary: updates.ID, setupPromos: updates.ID, byTab: updates.ID, already: updates.ID,
		userPrimary: primary.ID, userForums: forums.ID, ruleForums: forums.ID, setupTopic: promos.ID, unmatched: primary.ID,
	} {
		if got := h.areaOf(t, a, id); got != want.String() {
			t.Errorf("after apply %s resolves to %q, want %s", id, got, want)
		}
	}
	if h.hasLabelRow(t, a.UserID, deskPrimary, primary.LabelID.String()) {
		t.Error("the desk's Primary label is still on the moved message")
	}
	if h.hasLabelRow(t, a.UserID, setupPromos, promos.LabelID.String()) {
		t.Error("day-one setup's Promotions label is still on the moved message")
	}
	if by := h.labelledBy(t, deskPrimary, updates.LabelID); by != store.LabelByRule {
		t.Errorf("moved message's Updates label applied_by = %q, want rule", by)
	}
	// The user's filing and the other rule's are untouched — not even the
	// target label was added.
	for _, id := range []ulid.ULID{userPrimary, userForums, ruleForums, setupTopic, unmatched} {
		if got := h.labelRows(t, a, id); got != beforeRows[id] {
			t.Errorf("%s label rows changed: %s -> %s", id, beforeRows[id], got)
		}
		if got := h.countEmailChanges(t, a.UserID, id); got != beforeChanges[id] {
			t.Errorf("%s gained %d change rows, want 0", id, got-beforeChanges[id])
		}
	}
	// THE INVARIANT: one Email change row per moved message.
	for _, id := range []ulid.ULID{deskPrimary, setupPromos, byTab} {
		if got := h.countEmailChanges(t, a.UserID, id) - beforeChanges[id]; got != 1 {
			t.Errorf("%s gained %d Email change rows, want exactly 1", id, got)
		}
	}
	// Local-only labels: nothing goes to the mail server.
	if got := h.userCount(t, a, `SELECT count(*) FROM app.writeback_intents WHERE user_id = $1`) - intentsBefore; got != 0 {
		t.Errorf("area-filing apply enqueued %d writeback intents, want 0 (area labels are local-only)", got)
	}
	// Removing a desk/setup label is not the user correcting a rule.
	if n := h.userCount(t, a, `SELECT count(*) FROM app.rule_corrections WHERE user_id = $1`); n != 0 {
		t.Errorf("rule_corrections = %d after apply, want 0", n)
	}
	if n := h.userCount(t, a, `SELECT count(*) FROM app.label_suggestions WHERE user_id = $1 AND message_id = $2`,
		deskPrimary.String()); n != 1 {
		t.Errorf("open suggestions on the moved message = %d, want 1 (a backfill is not the user answering)", n)
	}
	removed, _ := res["removedAreaLabels"].(map[string]any)
	if len(removed) != 2 {
		t.Fatalf("removedAreaLabels = %#v, want entries for the desk and setup messages", res["removedAreaLabels"])
	}

	// 3. UNDO restores every message's label set EXACTLY, provenance included.
	ids, _ := res["messageIds"].([]any)
	restore := map[string]any{}
	for k, v := range removed {
		restore[k] = v
	}
	// Junk a client might send back: not an area label, and a user attribution.
	// Both must be ignored, not trusted.
	restore[deskPrimary.String()] = append(removed[deskPrimary.String()].([]any),
		map[string]any{"labelId": a.LabelID.String(), "appliedBy": "desk"},
		map[string]any{"labelId": forums.LabelID.String(), "appliedBy": "user"})
	undo := call(t, h.api.ruleApply, a.UserID, map[string]any{
		"ruleId": ruleID.String(), "undo": true, "messageIds": ids, "restoreAreaLabels": restore,
	})
	if num(undo, "undone") != 3 {
		t.Errorf("undone = %v, want 3", undo["undone"])
	}
	for _, id := range all {
		if got := h.labelRows(t, a, id); got != beforeRows[id] {
			t.Errorf("after undo %s label rows = %s, want exactly %s", id, got, beforeRows[id])
		}
		if got := h.areaOf(t, a, id); got != beforeArea[id] {
			t.Errorf("after undo %s resolves to %q, want %q", id, got, beforeArea[id])
		}
	}
	if n := h.userCount(t, a, `SELECT count(*) FROM app.rule_corrections WHERE user_id = $1`); n != 0 {
		t.Errorf("rule_corrections = %d after undo, want 0", n)
	}
	if got := h.userCount(t, a, `SELECT count(*) FROM app.writeback_intents WHERE user_id = $1`) - intentsBefore; got != 0 {
		t.Errorf("apply+undo enqueued %d writeback intents, want 0", got)
	}
	for _, id := range []ulid.ULID{deskPrimary, setupPromos, byTab} {
		if got := h.countEmailChanges(t, a.UserID, id) - beforeChanges[id]; got != 2 {
			t.Errorf("%s has %d Email change rows after apply+undo, want 2 (one each)", id, got)
		}
	}
}

// THE NEGATIVE CONTROL: a rule whose label is NOT an area's own label behaves
// exactly as before — the desk's placement label stays, nothing is reported as
// area filing — and the writeback count used above can see an intent when one
// is due (a mirrored label on a live message), so its zero there is a finding.
func TestRuleApplyNonAreaLabelRuleIsUnchanged(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)
	areas := h.defaultAreas(t, a)
	primary := areas["Primary"]

	receipts := h.label(t, a, "Receipts") // mirrored: it has a keyword atom
	const sender = "orders@shop.example"
	ruleID := h.createRule(t, a, "receipts",
		map[string]any{"field": "from", "op": "equals", "value": sender},
		[]any{map[string]any{"type": "addLabel", "label": receipts.String()}})
	msg := h.seedMessage(t, a, "your receipt", sender, time.Minute)
	h.live(t, a, msg)
	h.applyLabel(t, a, msg, primary.LabelID, store.LabelByDesk)
	intentsBefore := h.userCount(t, a, `SELECT count(*) FROM app.writeback_intents WHERE user_id = $1`)

	pre := call(t, h.api.rulePreview, a.UserID, map[string]any{
		"conditions":    map[string]any{"field": "from", "op": "equals", "value": sender},
		"targetLabelId": receipts.String(),
	})
	if pre["areaFiling"] != nil {
		t.Errorf("preview areaFiling = %#v for a non-area label, want null", pre["areaFiling"])
	}

	res := h.applyRule(t, a, ruleID)
	if num(res, "changed") != 1 {
		t.Fatalf("changed = %v, want 1", res["changed"])
	}
	if res["areaFiling"] != nil {
		t.Errorf("areaFiling = %#v for a non-area label, want null", res["areaFiling"])
	}
	if removed, _ := res["removedAreaLabels"].(map[string]any); len(removed) != 0 {
		t.Errorf("removedAreaLabels = %#v, want empty", removed)
	}
	if by := h.labelledBy(t, msg, primary.LabelID); by != store.LabelByDesk {
		t.Errorf("the desk's Primary label was disturbed (applied_by=%q) by a non-area rule", by)
	}
	if by := h.labelledBy(t, msg, receipts); by != store.LabelByRule {
		t.Errorf("Receipts applied_by = %q, want rule", by)
	}
	if got := h.userCount(t, a, `SELECT count(*) FROM app.writeback_intents WHERE user_id = $1`) - intentsBefore; got != 1 {
		t.Errorf("mirrored-label apply enqueued %d intents, want 1 — without it the zero in the area test proves nothing", got)
	}
}
