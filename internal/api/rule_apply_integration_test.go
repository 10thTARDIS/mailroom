// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// Rule/apply — running a saved rule over mail that already arrived (#191).
//
// The feature is easy; the ways it can quietly go wrong are the tests. It must
// append change_log rows (the ingest applier does not, and reusing it would have
// made the labels invisible to sync); it must not re-apply a label the user has
// taken back; it must not rewrite a hand-applied label's attribution, which the
// inducer treats as evidence; it must not perform the placement actions; and
// undoing it must not read as the user disagreeing with the rule several hundred
// times.

import (
	"context"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// labelledBy reports the provenance of a label on a message, or "" if absent.
func (h *apiHarness) labelledBy(t *testing.T, msgID, labelID ulid.ULID) string {
	t.Helper()
	var by string
	err := h.super.QueryRow(context.Background(),
		`SELECT applied_by FROM app.message_labels WHERE message_id = $1 AND label_id = $2`,
		msgID.String(), labelID.String()).Scan(&by)
	if err != nil {
		return ""
	}
	return by
}

// applyRule runs Rule/apply and returns the decoded result.
func (h *apiHarness) applyRule(t *testing.T, f apiFixture, ruleID ulid.ULID) map[string]any {
	t.Helper()
	return call(t, h.api.ruleApply, f.UserID, map[string]any{"ruleId": ruleID.String()})
}

// THE FEATURE. A rule saved today labels the mail you already have.
func TestRuleApplyLabelsExistingMail(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	promotions := h.label(t, a, "Promotions")
	conds, actions := domainRule(promotions)
	ruleID := h.createRule(t, a, "wayfair-promos", conds, actions)

	var want []ulid.ULID
	for i, subj := range []string{"40% off", "Flash sale", "Rugs half price"} {
		want = append(want, h.seedBulk(t, a, subj, "deals@wayfair.example", "wayfair.example",
			time.Duration(i+1)*time.Minute))
	}
	// Mail the rule does not match, to prove the scan is selective.
	other := h.seedMessage(t, a, "Dentist appointment", "front@dentist.example", 9*time.Minute)

	res := h.applyRule(t, a, ruleID)
	if n, _ := res["matched"].(float64); n != 3 {
		t.Fatalf("matched = %v, want 3", res["matched"])
	}
	if n, _ := res["changed"].(float64); n != 3 {
		t.Fatalf("changed = %v, want 3", res["changed"])
	}
	for _, id := range want {
		if by := h.labelledBy(t, id, promotions); by != store.LabelByRule {
			t.Errorf("message %s applied_by = %q, want %q", id, by, store.LabelByRule)
		}
	}
	if by := h.labelledBy(t, other, promotions); by != "" {
		t.Errorf("a non-matching message was labelled (%q)", by)
	}

	// THE LOAD-BEARING INVARIANT. The ingest applier appends no change_log row —
	// reusing it here would have left these labels invisible to /changes, to the
	// search indexer and to IMAP writeback, which is a bug you would not notice
	// until a second client disagreed with the first.
	var rows int
	if err := h.super.QueryRow(context.Background(),
		`SELECT count(*) FROM app.change_log
		  WHERE user_id = $1 AND object_type = 'Email' AND op = 'updated'
		    AND object_id = ANY($2)`,
		a.UserID.String(), []string{want[0].String(), want[1].String(), want[2].String()}).Scan(&rows); err != nil {
		t.Fatalf("count change rows: %v", err)
	}
	if rows != 3 {
		t.Errorf("change_log has %d Email/updated rows for the 3 labelled messages, want 3", rows)
	}
}

// Running it again changes nothing, and says so rather than reporting a silent
// zero the user cannot interpret.
func TestRuleApplyIsIdempotent(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	promotions := h.label(t, a, "Promotions")
	conds, actions := domainRule(promotions)
	ruleID := h.createRule(t, a, "wayfair-promos", conds, actions)
	h.seedBulk(t, a, "40% off", "deals@wayfair.example", "wayfair.example", time.Minute)
	h.seedBulk(t, a, "Flash sale", "deals@wayfair.example", "wayfair.example", 2*time.Minute)

	first := h.applyRule(t, a, ruleID)
	if n, _ := first["changed"].(float64); n != 2 {
		t.Fatalf("first run changed %v, want 2", first["changed"])
	}

	second := h.applyRule(t, a, ruleID)
	if n, _ := second["changed"].(float64); n != 0 {
		t.Errorf("second run changed %v messages; it must be a no-op", second["changed"])
	}
	if n, _ := second["alreadyDone"].(float64); n != 2 {
		t.Errorf("alreadyDone = %v, want 2 — the client cannot say \"nothing left to do\" without it", second["alreadyDone"])
	}
}

// THE INTERLOCK WITH #170. A backfill that re-applies a label the user took off
// has silently undone their own correction — and would do it again every run.
func TestRuleApplyNeverReAppliesACorrectedLabel(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	promotions := h.label(t, a, "Promotions")
	conds, actions := domainRule(promotions)
	ruleID := h.createRule(t, a, "wayfair-promos", conds, actions)

	keep := h.seedBulk(t, a, "40% off", "deals@wayfair.example", "wayfair.example", time.Minute)
	// One the rule got wrong before, and the user corrected.
	rejected := h.seedMessage(t, a, "Your order has shipped", "orders@wayfair.example", 2*time.Minute)
	h.correct(t, a, ruleID, rejected, promotions)

	res := h.applyRule(t, a, ruleID)
	if n, _ := res["matched"].(float64); n != 2 {
		t.Fatalf("matched = %v, want 2 (the rule still matches both)", res["matched"])
	}
	if n, _ := res["skippedCorrected"].(float64); n != 1 {
		t.Errorf("skippedCorrected = %v, want 1", res["skippedCorrected"])
	}
	if by := h.labelledBy(t, rejected, promotions); by != "" {
		t.Errorf("the backfill re-applied a label the user had removed (applied_by=%q)", by)
	}
	if by := h.labelledBy(t, keep, promotions); by != store.LabelByRule {
		t.Errorf("the message the rule gets right was not labelled (applied_by=%q)", by)
	}
}

// A hand-applied label stays the USER's. The inducer treats hand-applied labels
// as its only evidence, so a backfill rewriting the attribution would let a rule
// launder its own output into ground truth and confirm itself forever.
func TestRuleApplyDoesNotStealAHandAppliedLabel(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	promotions := h.label(t, a, "Promotions")
	conds, actions := domainRule(promotions)
	ruleID := h.createRule(t, a, "wayfair-promos", conds, actions)

	mine := h.seedBulk(t, a, "40% off", "deals@wayfair.example", "wayfair.example", time.Minute)
	h.applyLabel(t, a, mine, promotions, store.LabelByUser)

	h.applyRule(t, a, ruleID)

	if by := h.labelledBy(t, mine, promotions); by != store.LabelByUser {
		t.Errorf("applied_by = %q after the backfill, want %q — the rule took credit for the user's own label",
			by, store.LabelByUser)
	}
}

// skipInbox/archive are about where mail LANDS. Performing them against history
// would move mail the user has already triaged, so they are refused — and NAMED,
// so the client can say which part of the rule did not run rather than leaving
// the user to notice that nothing moved.
func TestRuleApplyRefusesThePlacementActionsAndSaysSo(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	promotions := h.label(t, a, "Promotions")
	ruleID := h.createRule(t, a, "wayfair-promos",
		map[string]any{"field": "fromDomain", "op": "equals", "value": "wayfair.example"},
		[]any{
			map[string]any{"type": "addLabel", "label": promotions.String()},
			map[string]any{"type": "skipInbox"},
		})
	msg := h.seedBulk(t, a, "40% off", "deals@wayfair.example", "wayfair.example", time.Minute)

	res := h.applyRule(t, a, ruleID)
	if n, _ := res["changed"].(float64); n != 1 {
		t.Fatalf("changed = %v, want 1 — the label half must still run", res["changed"])
	}
	skipped, _ := res["skippedActions"].([]any)
	if len(skipped) != 1 || skipped[0] != "skipInbox" {
		t.Errorf("skippedActions = %v, want [skipInbox]", res["skippedActions"])
	}
	var moved *string
	if err := h.super.QueryRow(context.Background(),
		`SELECT moved_mailbox_id FROM app.messages WHERE id = $1`, msg.String()).Scan(&moved); err != nil {
		t.Fatalf("read placement: %v", err)
	}
	if moved != nil && *moved != "" {
		t.Errorf("the backfill moved a message out of the inbox (%v)", *moved)
	}
}

// A rule the user switched off must not run over their history: it is the same
// rule either way, and obeying it anyway would be the automation arguing with
// them.
func TestRuleApplyRefusesADisabledRule(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	promotions := h.label(t, a, "Promotions")
	conds, actions := domainRule(promotions)
	ruleID := h.createRule(t, a, "wayfair-promos", conds, actions)
	call(t, h.api.ruleSet, a.UserID, map[string]any{
		"update": map[string]any{ruleID.String(): map[string]any{"enabled": false}},
	})
	msg := h.seedBulk(t, a, "40% off", "deals@wayfair.example", "wayfair.example", time.Minute)

	_, mErr := h.api.ruleApply(context.Background(), a.UserID,
		mustJSON(t, map[string]any{"ruleId": ruleID.String()}))
	if mErr == nil {
		t.Fatal("Rule/apply ran a disabled rule")
	}
	if by := h.labelledBy(t, msg, promotions); by != "" {
		t.Errorf("a disabled rule labelled mail (%q)", by)
	}
}

// UNDOING AN OPERATION IS NOT DISAGREEING WITH THE RULE. Routing the undo
// through the ordinary removal path would record one correction per message and
// make a rule that did exactly what it was asked look catastrophically wrong.
func TestRuleApplyUndoRecordsNoCorrections(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	promotions := h.label(t, a, "Promotions")
	conds, actions := domainRule(promotions)
	ruleID := h.createRule(t, a, "wayfair-promos", conds, actions)
	for i, s := range []string{"40% off", "Flash sale", "Rugs"} {
		h.seedBulk(t, a, s, "deals@wayfair.example", "wayfair.example", time.Duration(i+1)*time.Minute)
	}

	applied := h.applyRule(t, a, ruleID)
	ids, _ := applied["messageIds"].([]any)
	if len(ids) != 3 {
		t.Fatalf("apply returned %d message ids, want 3 — the undo has nothing to work from", len(ids))
	}

	undone := call(t, h.api.ruleApply, a.UserID, map[string]any{
		"ruleId": ruleID.String(), "undo": true, "messageIds": ids,
	})
	if n, _ := undone["undone"].(float64); n != 3 {
		t.Errorf("undone = %v, want 3", undone["undone"])
	}

	var labels, corrections int
	ctx := context.Background()
	if err := h.super.QueryRow(ctx,
		`SELECT count(*) FROM app.message_labels WHERE user_id=$1 AND label_id=$2`,
		a.UserID.String(), promotions.String()).Scan(&labels); err != nil {
		t.Fatal(err)
	}
	if labels != 0 {
		t.Errorf("%d labels survived the undo", labels)
	}
	if err := h.super.QueryRow(ctx,
		`SELECT count(*) FROM app.rule_corrections WHERE rule_id=$1`, ruleID.String()).Scan(&corrections); err != nil {
		t.Fatal(err)
	}
	if corrections != 0 {
		t.Errorf("undoing a backfill recorded %d corrections; the rule did what it was asked", corrections)
	}

	// And the rule still reports clean, which is what the user actually sees.
	got := call(t, h.api.ruleGet, a.UserID, map[string]any{"ids": []string{ruleID.String()}})
	list, _ := got["list"].([]any)
	m, _ := list[0].(map[string]any)
	if n, _ := m["corrections"].(float64); n != 0 {
		t.Errorf("rule reports %v corrections after an undo", m["corrections"])
	}
}

// The undo can only remove what the RULE applied. A hand-applied label on a
// message named in the undo must survive — otherwise "undo" would be a way to
// delete the user's own filing.
func TestRuleApplyUndoLeavesHandAppliedLabelsAlone(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	promotions := h.label(t, a, "Promotions")
	keepsake := h.label(t, a, "Keep")
	conds, actions := domainRule(promotions)
	ruleID := h.createRule(t, a, "wayfair-promos", conds, actions)

	msg := h.seedBulk(t, a, "40% off", "deals@wayfair.example", "wayfair.example", time.Minute)
	h.applyLabel(t, a, msg, keepsake, store.LabelByUser)

	h.applyRule(t, a, ruleID)
	call(t, h.api.ruleApply, a.UserID, map[string]any{
		"ruleId": ruleID.String(), "undo": true, "messageIds": []string{msg.String()},
	})

	if by := h.labelledBy(t, msg, keepsake); by != store.LabelByUser {
		t.Errorf("the undo removed a hand-applied label (applied_by=%q)", by)
	}
	if by := h.labelledBy(t, msg, promotions); by != "" {
		t.Errorf("the rule's own label survived the undo (%q)", by)
	}
}
