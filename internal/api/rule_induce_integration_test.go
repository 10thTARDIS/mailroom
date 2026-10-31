// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// Rule/induce (#168) and the conflict half of Rule/preview (#156), over the real
// store.
//
// The inducer itself is unit-tested against fixtures in internal/rules. What is
// tested HERE is the wiring: that the facts fed to a correct algorithm actually
// come out of real rows the way the algorithm assumes. A projection bug is
// invisible to the pure tests and fatal in production.

import (
	"context"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// label creates a label with its keyword mapping and returns its id.
func (h *apiHarness) label(t *testing.T, f apiFixture, name string) ulid.ULID {
	t.Helper()
	id := store.NewULID()
	ctx := context.Background()
	if _, err := h.super.Exec(ctx,
		`INSERT INTO app.labels (id, user_id, name) VALUES ($1,$2,$3)`,
		id.String(), f.UserID.String(), name); err != nil {
		t.Fatalf("seed label %q: %v", name, err)
	}
	if _, err := h.super.Exec(ctx,
		`INSERT INTO app.label_keyword_map (user_id, label_id, keyword_atom) VALUES ($1,$2,$3)`,
		f.UserID.String(), id.String(), "MailroomLabel-"+id.String()); err != nil {
		t.Fatalf("seed label keyword: %v", err)
	}
	return id
}

// applyLabel attaches a label with an explicit provenance.
func (h *apiHarness) applyLabel(t *testing.T, f apiFixture, msgID, labelID ulid.ULID, by string) {
	t.Helper()
	if _, err := h.super.Exec(context.Background(),
		`INSERT INTO app.message_labels (user_id, message_id, label_id, applied_by)
		 VALUES ($1,$2,$3,$4)`,
		f.UserID.String(), msgID.String(), labelID.String(), by); err != nil {
		t.Fatalf("apply label: %v", err)
	}
}

// seedBulk adds a message shaped like marketing: List-Id and List-Unsubscribe
// present, the way a retailer's promotional stream actually arrives.
func (h *apiHarness) seedBulk(t *testing.T, f apiFixture, subject, from, domain string, age time.Duration) ulid.ULID {
	t.Helper()
	id := h.seedMessage(t, f, subject, from, age)
	if _, err := h.super.Exec(context.Background(),
		`UPDATE app.messages SET list_id = $2, list_unsubscribe = $3 WHERE id = $1`,
		id.String(), "<deals."+domain+">", "<https://"+domain+"/unsub>"); err != nil {
		t.Fatalf("set bulk headers: %v", err)
	}
	return id
}

// THE ACCEPTANCE TEST, end to end (#156): label two Wayfair ads Promotions and
// the proposed rule must not also catch Wayfair receipts.
func TestRuleInduceDoesNotProposeARuleThatEatsReceipts(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	promotions := h.label(t, a, "Promotions")
	ad1 := h.seedBulk(t, a, "40% off everything", "deals@wayfair.example", "wayfair.example", time.Minute)
	ad2 := h.seedBulk(t, a, "Flash sale ends tonight", "deals@wayfair.example", "wayfair.example", 2*time.Minute)
	h.applyLabel(t, a, ad1, promotions, store.LabelByUser)
	h.applyLabel(t, a, ad2, promotions, store.LabelByUser)

	// Transactional mail from the SAME domain, UNLABELLED — the hard case, and
	// the realistic one. Nothing here collides with an existing label, so only
	// the shared bulk markers can separate these from the ads.
	h.seedMessage(t, a, "Your order has shipped", "orders@wayfair.example", 3*time.Minute)
	h.seedMessage(t, a, "Order confirmation #55012", "orders@wayfair.example", 4*time.Minute)
	h.seedMessage(t, a, "Your password was changed", "service@wayfair.example", 5*time.Minute)

	// Unrelated newsletters, so the degenerate "all bulk mail is Promotions"
	// hypothesis is visibly wrong rather than vacuously harmless.
	for i, d := range []string{"goodnews.example", "localpaper.example", "club.example", "charity.example", "devblog.example"} {
		h.seedBulk(t, a, "Weekly digest", "news@"+d, d, time.Duration(10+i)*time.Minute)
	}

	res := call(t, h.api.ruleInduce, a.UserID, map[string]any{"labelId": promotions.String()})

	if n, _ := res["examples"].(float64); n != 2 {
		t.Fatalf("examples = %v, want 2 (the hand-labelled ads)", res["examples"])
	}
	cands, _ := res["candidates"].([]any)
	if len(cands) == 0 {
		t.Fatal("induced nothing from two clearly-related labelled messages")
	}
	best, _ := cands[0].(map[string]any)

	if n, _ := best["covers"].(float64); n != 2 {
		t.Errorf("best candidate covers %v of 2 examples", best["covers"])
	}
	// THE POINT: no receipts, no password resets, no unrelated newsletters.
	if n, _ := best["collateral"].(float64); n != 0 {
		t.Errorf("best candidate would also label %v other messages (e.g. %v) — receipts are being swept into Promotions",
			best["collateral"], best["samples"])
	}

	// Induction PROPOSES; the user disposes. Nothing may be written.
	var ruleCount int
	if err := h.super.QueryRow(context.Background(),
		`SELECT count(*) FROM app.rules WHERE user_id = $1`, a.UserID.String()).Scan(&ruleCount); err != nil {
		t.Fatalf("count rules: %v", err)
	}
	if ruleCount != 0 {
		t.Errorf("Rule/induce created %d rules; it must only propose", ruleCount)
	}
}

// One example is not evidence: a message shares its own sender, subject and list
// with itself, so the "narrowest consistent rule" from one is that message
// described back.
func TestRuleInduceNeedsMoreThanOneExample(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	promotions := h.label(t, a, "Promotions")
	ad := h.seedBulk(t, a, "40% off", "deals@wayfair.example", "wayfair.example", time.Minute)
	h.applyLabel(t, a, ad, promotions, store.LabelByUser)

	res := call(t, h.api.ruleInduce, a.UserID, map[string]any{"labelId": promotions.String()})
	if n, _ := res["examples"].(float64); n != 1 {
		t.Errorf("examples = %v, want 1", res["examples"])
	}
	if n, _ := res["required"].(float64); n != 2 {
		t.Errorf("required = %v, want 2 — the client needs it to say how many more to label", res["required"])
	}
	if cs, _ := res["candidates"].([]any); len(cs) != 0 {
		t.Errorf("proposed %d rules from a single example", len(cs))
	}
}

// Only the USER's labels are evidence. Learning from a label a rule applied lets
// a wrong rule confirm itself: its output becomes training data, which induces a
// rule agreeing with it, indefinitely.
func TestRuleInduceIgnoresLabelsAppliedByRules(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	promotions := h.label(t, a, "Promotions")
	byUser := h.seedBulk(t, a, "40% off", "deals@wayfair.example", "wayfair.example", time.Minute)
	byRule := h.seedBulk(t, a, "Flash sale", "deals@wayfair.example", "wayfair.example", 2*time.Minute)
	h.applyLabel(t, a, byUser, promotions, store.LabelByUser)
	h.applyLabel(t, a, byRule, promotions, store.LabelByRule)

	res := call(t, h.api.ruleInduce, a.UserID, map[string]any{"labelId": promotions.String()})
	if n, _ := res["examples"].(float64); n != 1 {
		t.Errorf("examples = %v, want 1 — a rule-applied label was counted as the user's own evidence", res["examples"])
	}
}

// Conflicts are the sentence that stops a bad rule being accepted: "would match
// 2, and one of them you called Shopping".
func TestRulePreviewReportsConflictsWithOtherLabels(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	promotions := h.label(t, a, "Promotions")
	shopping := h.label(t, a, "Shopping")
	ad := h.seedMessage(t, a, "40% off", "deals@wayfair.example", time.Minute)
	receipt := h.seedMessage(t, a, "Order confirmation", "orders@wayfair.example", 2*time.Minute)
	h.applyLabel(t, a, ad, promotions, store.LabelByUser)
	h.applyLabel(t, a, receipt, shopping, store.LabelByUser)

	res := call(t, h.api.rulePreview, a.UserID, map[string]any{
		"conditions":    map[string]any{"field": "fromDomain", "op": "equals", "value": "wayfair.example"},
		"targetLabelId": promotions.String(),
	})

	if n, _ := res["matched"].(float64); n != 2 {
		t.Fatalf("matched = %v, want 2", res["matched"])
	}
	if n, _ := res["conflicts"].(float64); n != 1 {
		t.Errorf("conflicts = %v, want 1 — the receipt is filed under another label", res["conflicts"])
	}
	names, _ := res["conflictLabels"].([]any)
	if len(names) != 1 || names[0] != "Shopping" {
		t.Errorf("conflictLabels = %v, want [Shopping] — the sentence needs the name, not an id", names)
	}
}

// A label the RULE applied is not a conflict. Rules disagreeing with each other
// on the user's behalf is noise, not a warning worth stopping for.
func TestRulePreviewDoesNotCountRuleAppliedLabelsAsConflicts(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	promotions := h.label(t, a, "Promotions")
	shopping := h.label(t, a, "Shopping")
	receipt := h.seedMessage(t, a, "Order confirmation", "orders@wayfair.example", time.Minute)
	h.applyLabel(t, a, receipt, shopping, store.LabelByRule)

	res := call(t, h.api.rulePreview, a.UserID, map[string]any{
		"conditions":    map[string]any{"field": "fromDomain", "op": "equals", "value": "wayfair.example"},
		"targetLabelId": promotions.String(),
	})
	if n, _ := res["matched"].(float64); n != 1 {
		t.Fatalf("matched = %v, want 1", res["matched"])
	}
	if n, _ := res["conflicts"].(float64); n != 0 {
		t.Errorf("conflicts = %v, want 0 — only the user's own filing is a conflict", res["conflicts"])
	}
}
