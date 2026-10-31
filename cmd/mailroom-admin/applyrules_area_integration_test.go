// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package main

import (
	"context"
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/rules"
	"mailroom/internal/store"
)

// apply-rules over a rule that files into an area is a SWAP, not an add (#728):
// the rule editor's apply has done that since #708, through store.PlanAreaFiling,
// and the backfill used to add the label and leave the message in its earlier
// area. Mail the user filed by hand stays where it is.
func TestApplyRulesAreaRuleSwapsAreaLabels(t *testing.T) {
	h := newUntangleHarness(t)
	ctx := context.Background()
	uid := seedUntangleUser(t, h.super, "applyrules-area-"+store.NewULID().String())
	other := seedUntangleUser(t, h.super, "applyrules-other-"+store.NewULID().String())

	areaLabel := map[string]ulid.ULID{}
	areaID := map[string]ulid.ULID{}
	for _, u := range []ulid.ULID{uid, other} {
		if err := h.store.UserTx(ctx, u, func(tx *store.UserTx) error {
			views, e := tx.ListAreas(ctx)
			if u == uid {
				for _, v := range views {
					areaLabel[v.Name] = v.LabelID
					areaID[v.Name] = v.ID
				}
			}
			return e
		}); err != nil {
			t.Fatalf("list areas: %v", err)
		}
	}
	primary, updates := areaLabel["Primary"], areaLabel["Updates"]

	const sender = "orders@shop.example"
	msg := func(subject string) ulid.ULID {
		id := seedUntangleMessage(t, h.super, uid, subject)
		if _, err := h.super.Exec(ctx, `UPDATE app.messages SET from_address=$2 WHERE id=$1`, id.String(), sender); err != nil {
			t.Fatalf("set sender: %v", err)
		}
		return id
	}
	attach := func(m, l ulid.ULID, by string) {
		if _, err := h.super.Exec(ctx,
			`INSERT INTO app.message_labels (user_id, message_id, label_id, applied_by) VALUES ($1,$2,$3,$4)`,
			uid.String(), m.String(), l.String(), by); err != nil {
			t.Fatalf("attach: %v", err)
		}
	}
	desk := msg("filed by the desk")
	attach(desk, primary, store.LabelByDesk)
	hand := msg("filed by hand")
	attach(hand, primary, store.LabelByUser)
	bare := msg("no area label")

	var ruleID ulid.ULID
	if err := h.store.UserTx(ctx, uid, func(tx *store.UserTx) error {
		var e error
		ruleID, e = tx.CreateRule(ctx, store.RuleRow{
			Name: "shop mail is Updates", Priority: 100, Enabled: true,
			Conditions: rules.Condition{Field: "from", Op: "equals", Value: sender},
			Actions:    []rules.Action{{Type: rules.ActionAddLabel, Label: updates.String()}},
		})
		return e
	}); err != nil {
		t.Fatalf("create rule: %v", err)
	}

	has := func(m, l ulid.ULID) bool {
		var n int
		if err := h.super.QueryRow(ctx,
			`SELECT count(*) FROM app.message_labels WHERE user_id=$1 AND message_id=$2 AND label_id=$3`,
			uid.String(), m.String(), l.String()).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		return n > 0
	}
	areaOf := func(m ulid.ULID) string {
		var got string
		if err := h.store.UserTx(ctx, uid, func(tx *store.UserTx) error {
			res, e := tx.ResolveAreas(ctx, []ulid.ULID{m})
			if r, ok := res[m]; ok && r.AreaID != nil {
				got = r.AreaID.String()
			}
			return e
		}); err != nil {
			t.Fatalf("resolve: %v", err)
		}
		return got
	}
	run := func(apply bool) *backfillRun {
		r := &backfillRun{ctx: ctx, userID: uid, limit: 100, only: ruleID.String(), apply: apply, maxMut: 100, showConf: 5}
		if err := h.store.UserTx(ctx, uid, r.do); err != nil {
			t.Fatalf("backfill: %v", err)
		}
		return r
	}

	// Report mode: predicts the move and writes nothing.
	before := emailChangeState(t, h.store, uid)
	rep := run(false)
	if got := emailChangeState(t, h.store, uid); got != before {
		t.Fatalf("report mode appended change_log rows: %d -> %d", before, got)
	}
	a := rep.outcomes[0].area
	if a == nil || a.move != 2 || a.contestedByUser != 1 || a.alreadyThere != 0 {
		t.Fatalf("report area outcomes = %+v, want move 2 (desk-filed + unfiled), contestedByUser 1", a)
	}

	r := run(true)
	if r.mutations != 2 {
		t.Errorf("mutations = %d, want 2 (the desk-filed message and the unfiled one)", r.mutations)
	}
	if o := r.outcomes[0]; o.mispredicted != 0 {
		t.Errorf("mispredicted = %d, want 0", o.mispredicted)
	}
	// The desk's label came off and the target wins.
	if has(desk, primary) {
		t.Error("the desk's Primary label is still on the message: apply-rules only added")
	}
	if got := areaOf(desk); got != areaID["Updates"].String() {
		t.Errorf("desk-filed message resolves to %q, want Updates", got)
	}
	if got := areaOf(bare); got != areaID["Updates"].String() {
		t.Errorf("unfiled message resolves to %q, want Updates", got)
	}
	// Hand-filed mail stays: the user's own filing outranks a rule, and the
	// rule's label is not added over it either.
	if !has(hand, primary) || has(hand, updates) {
		t.Errorf("hand-filed message was touched: primary=%v updates=%v", has(hand, primary), has(hand, updates))
	}
	// Exactly one change_log row per changed message.
	if got := emailChangeState(t, h.store, uid) - before; got != 2 {
		t.Errorf("Email change_log rows = %d, want 2", got)
	}

	// A second run finds nothing left to do.
	r2 := run(true)
	if r2.mutations != 0 {
		t.Errorf("second run mutations = %d, want 0", r2.mutations)
	}

	// RLS: another user never sees these rows.
	var otherRows int
	if err := h.store.UserTx(ctx, other, func(tx *store.UserTx) error {
		c, e := tx.RulePreviewCandidates(ctx, 100)
		otherRows = len(c)
		return e
	}); err != nil {
		t.Fatalf("other user: %v", err)
	}
	if otherRows != 0 {
		t.Errorf("another user sees %d candidate messages, want 0", otherRows)
	}
}
