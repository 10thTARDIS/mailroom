// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// Rule/tighten and the corrections count on the Rule object (#170).
//
// The store tests prove a correction gets RECORDED. These prove it is worth
// recording: that the record reaches a client, that it turns into a narrower
// rule which actually excludes the mail the user rejected, that a proposal which
// would NOT fix the problem is withheld rather than dressed up, and that fixing
// the rule retires the evidence against it.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// correct records that the user took a rule-applied label back off a message,
// the way ApplyEmailUpdate's removal path does.
func (h *apiHarness) correct(t *testing.T, f apiFixture, ruleID, msgID, labelID ulid.ULID) {
	t.Helper()
	if _, err := h.super.Exec(context.Background(),
		`INSERT INTO app.rule_corrections (user_id, rule_id, message_id, label_id)
		 VALUES ($1,$2,$3,$4)`,
		f.UserID.String(), ruleID.String(), msgID.String(), labelID.String()); err != nil {
		t.Fatalf("record correction: %v", err)
	}
}

// createRule saves a rule through the real Rule/set and returns its id.
func (h *apiHarness) createRule(t *testing.T, f apiFixture, name string, conds, actions any) ulid.ULID {
	t.Helper()
	res := call(t, h.api.ruleSet, f.UserID, map[string]any{
		"create": map[string]any{
			"r": map[string]any{"name": name, "conditions": conds, "actions": actions},
		},
	})
	created, _ := res["created"].(map[string]any)
	obj, ok := created["r"].(map[string]any)
	if !ok {
		t.Fatalf("rule %q not created: %v", name, res["notCreated"])
	}
	id, err := ulid.Parse(obj["id"].(string))
	if err != nil {
		t.Fatalf("created rule id: %v", err)
	}
	return id
}

// domainRule is the rule everyone writes first and nobody writes twice:
// "everything from this retailer is Promotions", which eats the receipts.
func domainRule(labelID ulid.ULID) (conds, actions any) {
	return map[string]any{"field": "fromDomain", "op": "equals", "value": "wayfair.example"},
		[]any{map[string]any{"type": "addLabel", "label": labelID.String()}}
}

// THE POINT OF #170. A too-broad rule labels the ads correctly and the receipts
// wrongly; the user takes the label off the receipts; the proposal that comes
// back must keep every ad and catch no receipt.
func TestRuleTightenProposesARuleThatExcludesTheCorrectedMail(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	promotions := h.label(t, a, "Promotions")
	conds, actions := domainRule(promotions)
	ruleID := h.createRule(t, a, "wayfair-promos", conds, actions)

	// What the rule got RIGHT: marketing, still carrying the label it applied.
	for i, subj := range []string{"40% off everything", "Flash sale ends tonight", "Last chance: rugs"} {
		id := h.seedBulk(t, a, subj, "deals@wayfair.example", "wayfair.example", time.Duration(i+1)*time.Minute)
		h.applyLabel(t, a, id, promotions, store.LabelByRule)
	}
	// What it got WRONG: transactional mail from the same domain, no
	// List-Unsubscribe, label removed by the user.
	r1 := h.seedMessage(t, a, "Your order has shipped", "orders@wayfair.example", 10*time.Minute)
	r2 := h.seedMessage(t, a, "Order confirmation #55012", "orders@wayfair.example", 11*time.Minute)
	h.correct(t, a, ruleID, r1, promotions)
	h.correct(t, a, ruleID, r2, promotions)
	// And one it has not reached an opinion about yet.
	h.seedMessage(t, a, "Your password was changed", "service@wayfair.example", 12*time.Minute)

	res := call(t, h.api.ruleTighten, a.UserID, map[string]any{"ruleId": ruleID.String()})

	if n, _ := res["corrections"].(float64); n != 2 {
		t.Fatalf("corrections = %v, want 2", res["corrections"])
	}
	if n, _ := res["correctionsInWindow"].(float64); n != 2 {
		t.Fatalf("correctionsInWindow = %v, want 2", res["correctionsInWindow"])
	}
	current, _ := res["current"].(map[string]any)
	if n, _ := current["matched"].(float64); n != 6 {
		t.Errorf("current.matched = %v, want 6 (3 ads + 2 receipts + 1 password reset)", current["matched"])
	}
	if n, _ := current["keeps"].(float64); n != 3 {
		t.Errorf("current.keeps = %v, want 3", current["keeps"])
	}
	if n, _ := current["misfires"].(float64); n != 2 {
		t.Errorf("current.misfires = %v, want 2", current["misfires"])
	}

	props, _ := res["proposals"].([]any)
	if len(props) == 0 {
		t.Fatal("no narrower rule proposed, though the ads carry List-Unsubscribe and the receipts do not")
	}
	best, _ := props[0].(map[string]any)
	if n, _ := best["misfires"].(float64); n != 0 {
		t.Errorf("proposal still catches %v corrected messages — it fixes nothing", best["misfires"])
	}
	if n, _ := best["fixes"].(float64); n != 2 {
		t.Errorf("fixes = %v, want 2", best["fixes"])
	}
	if n, _ := best["keeps"].(float64); n != 3 {
		t.Errorf("proposal keeps %v of 3 correctly-labelled ads", best["keeps"])
	}
	if n, _ := best["drops"].(float64); n != 0 {
		t.Errorf("proposal drops %v correctly-labelled ads (%v)", best["drops"], best["samples"])
	}

	// It PROPOSES. The rule on disk is untouched until the user saves, and so is
	// the evidence against it.
	var raw string
	if err := h.super.QueryRow(context.Background(),
		`SELECT conditions::text FROM app.rules WHERE id = $1`, ruleID.String()).Scan(&raw); err != nil {
		t.Fatalf("read rule back: %v", err)
	}
	if strings.Contains(raw, "listUnsubscribe") {
		t.Errorf("Rule/tighten rewrote the stored rule: %s", raw)
	}
	var left int
	if err := h.super.QueryRow(context.Background(),
		`SELECT count(*) FROM app.rule_corrections WHERE rule_id = $1`, ruleID.String()).Scan(&left); err != nil {
		t.Fatalf("count corrections: %v", err)
	}
	if left != 2 {
		t.Errorf("Rule/tighten consumed the corrections (%d left); only saving the fix may", left)
	}
}

// When the mail the user rejected is indistinguishable from the mail they kept
// on the facts a rule can test, there IS no narrower rule. Saying so is the
// honest answer; offering a condition that merely looks discriminating and still
// catches the same messages would be worse than offering nothing.
func TestRuleTightenWithholdsAProposalThatWouldNotFixIt(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	promotions := h.label(t, a, "Promotions")
	conds, actions := domainRule(promotions)
	ruleID := h.createRule(t, a, "wayfair-promos", conds, actions)

	for i, subj := range []string{"40% off everything", "Flash sale ends tonight"} {
		id := h.seedBulk(t, a, subj, "deals@wayfair.example", "wayfair.example", time.Duration(i+1)*time.Minute)
		h.applyLabel(t, a, id, promotions, store.LabelByRule)
	}
	// Same sender, same list, same unsubscribe header — every fact identical to
	// the two above. Nothing a condition can test separates it.
	twin := h.seedBulk(t, a, "Members-only preview", "deals@wayfair.example", "wayfair.example", 3*time.Minute)
	h.correct(t, a, ruleID, twin, promotions)

	res := call(t, h.api.ruleTighten, a.UserID, map[string]any{"ruleId": ruleID.String()})
	if n, _ := res["correctionsInWindow"].(float64); n != 1 {
		t.Fatalf("correctionsInWindow = %v, want 1", res["correctionsInWindow"])
	}
	if props, _ := res["proposals"].([]any); len(props) != 0 {
		t.Errorf("proposed %d rules that cannot exclude the corrected message: %v", len(props), props)
	}
}

// A correction the scan cannot see cannot be verified fixed. Reporting proposals
// anyway would be reporting them blind.
func TestRuleTightenSaysNothingWhenTheCorrectionsAreOutsideTheWindow(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	promotions := h.label(t, a, "Promotions")
	conds, actions := domainRule(promotions)
	ruleID := h.createRule(t, a, "wayfair-promos", conds, actions)

	for i, subj := range []string{"40% off everything", "Flash sale ends tonight"} {
		id := h.seedBulk(t, a, subj, "deals@wayfair.example", "wayfair.example", time.Duration(i+1)*time.Minute)
		h.applyLabel(t, a, id, promotions, store.LabelByRule)
	}
	// Corrected, then TRASHED — so RulePreviewCandidates never returns it.
	gone := h.seedMessage(t, a, "Your order has shipped", "orders@wayfair.example", 10*time.Minute)
	h.correct(t, a, ruleID, gone, promotions)
	if _, err := h.super.Exec(context.Background(),
		`UPDATE app.messages SET trashed_at = now() WHERE id = $1`, gone.String()); err != nil {
		t.Fatalf("trash: %v", err)
	}

	res := call(t, h.api.ruleTighten, a.UserID, map[string]any{"ruleId": ruleID.String()})
	if n, _ := res["corrections"].(float64); n != 1 {
		t.Errorf("corrections = %v, want 1 — the rule was still wrong", res["corrections"])
	}
	if n, _ := res["correctionsInWindow"].(float64); n != 0 {
		t.Errorf("correctionsInWindow = %v, want 0", res["correctionsInWindow"])
	}
	if props, _ := res["proposals"].([]any); len(props) != 0 {
		t.Errorf("proposed %d rules with nothing to verify them against", len(props))
	}
}

// A message the user corrected and then re-filed BY HAND is not evidence any
// more: they changed their mind, and a proposal built to exclude it would be
// built to exclude mail they have since said belongs.
func TestRuleTightenIgnoresACorrectionTheUserReversed(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	promotions := h.label(t, a, "Promotions")
	conds, actions := domainRule(promotions)
	ruleID := h.createRule(t, a, "wayfair-promos", conds, actions)

	ad := h.seedBulk(t, a, "40% off everything", "deals@wayfair.example", "wayfair.example", time.Minute)
	h.applyLabel(t, a, ad, promotions, store.LabelByRule)

	reversed := h.seedMessage(t, a, "Your order has shipped", "orders@wayfair.example", 2*time.Minute)
	h.correct(t, a, ruleID, reversed, promotions)
	h.applyLabel(t, a, reversed, promotions, store.LabelByUser)

	res := call(t, h.api.ruleTighten, a.UserID, map[string]any{"ruleId": ruleID.String()})
	if n, _ := res["corrections"].(float64); n != 1 {
		t.Errorf("corrections = %v, want 1 — the row still records that it happened", res["corrections"])
	}
	if n, _ := res["correctionsInWindow"].(float64); n != 0 {
		t.Errorf("correctionsInWindow = %v: a re-filed message is still being counted against the rule", res["correctionsInWindow"])
	}
	current, _ := res["current"].(map[string]any)
	if n, _ := current["keeps"].(float64); n != 2 {
		t.Errorf("current.keeps = %v, want 2 — the re-filed message counts as kept", current["keeps"])
	}
}

// The count has to REACH a client, or nothing above it matters.
func TestRuleGetReportsCorrections(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	promotions := h.label(t, a, "Promotions")
	conds, actions := domainRule(promotions)
	ruleID := h.createRule(t, a, "wayfair-promos", conds, actions)
	quiet := h.createRule(t, a, "quiet",
		map[string]any{"field": "from", "op": "equals", "value": "bob@example.com"},
		[]any{map[string]any{"type": "addLabel", "label": promotions.String()}})

	msg := h.seedMessage(t, a, "Your order has shipped", "orders@wayfair.example", time.Minute)
	h.correct(t, a, ruleID, msg, promotions)

	got := call(t, h.api.ruleGet, a.UserID, map[string]any{
		"ids": []string{ruleID.String(), quiet.String()},
	})
	list, _ := got["list"].([]any)
	if len(list) != 2 {
		t.Fatalf("Rule/get returned %d objects, want 2", len(list))
	}
	byID := map[string]map[string]any{}
	for _, o := range list {
		m, _ := o.(map[string]any)
		byID[m["id"].(string)] = m
	}
	if n, _ := byID[ruleID.String()]["corrections"].(float64); n != 1 {
		t.Errorf("corrections = %v, want 1", byID[ruleID.String()]["corrections"])
	}
	// A rule nobody has corrected reports 0, not nothing: the client renders a
	// column, and a missing key would read as "unknown" rather than "clean".
	n, ok := byID[quiet.String()]["corrections"].(float64)
	if !ok || n != 0 {
		t.Errorf("uncorrected rule's corrections = %v, want 0", byID[quiet.String()]["corrections"])
	}
}

// Fixing the rule retires the evidence. Carrying it across the edit would keep
// accusing the new rule of the old one's mistakes, and the offer to narrow would
// never go away.
func TestEditingConditionsClearsCorrectionsButRenamingDoesNot(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	promotions := h.label(t, a, "Promotions")
	conds, actions := domainRule(promotions)
	ruleID := h.createRule(t, a, "wayfair-promos", conds, actions)
	msg := h.seedMessage(t, a, "Your order has shipped", "orders@wayfair.example", time.Minute)
	h.correct(t, a, ruleID, msg, promotions)

	corrections := func() float64 {
		got := call(t, h.api.ruleGet, a.UserID, map[string]any{"ids": []string{ruleID.String()}})
		list, _ := got["list"].([]any)
		m, _ := list[0].(map[string]any)
		n, _ := m["corrections"].(float64)
		return n
	}

	// A rename changes nothing about what the rule MATCHES, so the record still
	// describes it.
	call(t, h.api.ruleSet, a.UserID, map[string]any{
		"update": map[string]any{ruleID.String(): map[string]any{"name": "wayfair promos"}},
	})
	if n := corrections(); n != 1 {
		t.Errorf("renaming the rule changed its corrections to %v", n)
	}

	// AND A RENAME THROUGH OUR OWN EDITOR, which sends the whole rule on every
	// save. Testing only the minimal patch above would have passed while the real
	// client silently wiped the record on any edit at all.
	call(t, h.api.ruleSet, a.UserID, map[string]any{
		"update": map[string]any{ruleID.String(): map[string]any{
			"name":       "wayfair promotions",
			"priority":   100,
			"conditions": conds,
			"actions":    actions,
			"enabled":    true,
		}},
	})
	if n := corrections(); n != 1 {
		t.Errorf("a full-object save with UNCHANGED conditions cleared the corrections (%v)", n)
	}

	// Narrowing it does.
	call(t, h.api.ruleSet, a.UserID, map[string]any{
		"update": map[string]any{ruleID.String(): map[string]any{
			"conditions": map[string]any{"all": []any{
				map[string]any{"field": "fromDomain", "op": "equals", "value": "wayfair.example"},
				map[string]any{"field": "listUnsubscribe", "op": "exists"},
			}},
		}},
	})
	if n := corrections(); n != 0 {
		t.Errorf("corrections = %v after the conditions were fixed, want 0", n)
	}
}

// corrections is server-set. A client cannot edit a rule's history any more than
// it can edit its own.
func TestRuleSetRejectsCorrectionsInAPatch(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	promotions := h.label(t, a, "Promotions")
	conds, actions := domainRule(promotions)
	ruleID := h.createRule(t, a, "wayfair-promos", conds, actions)

	res := call(t, h.api.ruleSet, a.UserID, map[string]any{
		"update": map[string]any{ruleID.String(): map[string]any{"corrections": 0}},
	})
	notUpdated, _ := res["notUpdated"].(map[string]any)
	e, ok := notUpdated[ruleID.String()].(map[string]any)
	if !ok {
		t.Fatalf("a patch setting corrections was accepted: %v", res)
	}
	if e["type"] != "invalidProperties" {
		t.Errorf("error type = %v, want invalidProperties", e["type"])
	}
}
