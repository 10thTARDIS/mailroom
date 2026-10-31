// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// Rule/preview — the blast radius of a rule before it is saved (#155) — and the
// label provenance that says which rule did what afterwards.
//
// The behaviours worth pinning here are the ones where a plausible
// implementation is quietly wrong:
//
//   - a preview writes NOTHING (it is the one method that evaluates a rule that
//     may never exist);
//   - a bounded scan reports its own bound, so "matched 2" is never mistaken for
//     a whole-mailbox answer;
//   - a malformed condition is an ERROR, not "matched 0" — the engine treats a
//     bad condition as non-matching, so a silent zero would read as "this rule is
//     harmless" about a rule that is broken;
//   - a rule-applied label names its rule, and survives the rule's deletion.

import (
	"context"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/rules"
	"mailroom/internal/store"
)

// seedMessage adds one more message to a user's mailbox with the given subject
// and sender, received `age` before now.
func (h *apiHarness) seedMessage(t *testing.T, f apiFixture, subject, from string, age time.Duration) ulid.ULID {
	t.Helper()
	id := store.NewULID()
	hash := sha256.Sum256([]byte("blob-" + id.String()))
	ctx := context.Background()
	if _, err := h.super.Exec(ctx,
		`INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id)
		 VALUES ($1,$2,100,132,$3)`,
		f.UserID.String(), hash[:], store.NewULID().String()); err != nil {
		t.Fatalf("seed blob: %v", err)
	}
	if _, err := h.super.Exec(ctx,
		`INSERT INTO app.messages (id, user_id, content_hash, thread_id, subject, snippet,
		    from_name, from_address, received_at, size_bytes, message_id_hint)
		 VALUES ($1,$2,$3,$4,$5,'',' ',$6,now() - $7::interval,100,$8)`,
		id.String(), f.UserID.String(), hash[:], f.ThreadID.String(), subject, from,
		age.String(), "<"+id.String()+"@example.com>"); err != nil {
		t.Fatalf("seed message: %v", err)
	}
	return id
}

func TestRulePreviewCountsAndSamplesWithoutWriting(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	h.seedMessage(t, a, "Your Wayfair order shipped", "orders@wayfair.example", time.Minute)
	h.seedMessage(t, a, "Wayfair: 40% off everything", "deals@wayfair.example", 2*time.Minute)
	h.seedMessage(t, a, "lunch?", "friend@example.com", 3*time.Minute)

	res := call(t, h.api.rulePreview, a.UserID, map[string]any{
		"conditions": map[string]any{
			"field": "fromDomain", "op": "equals", "value": "wayfair.example",
		},
	})

	if n, _ := res["matched"].(float64); n != 2 {
		t.Errorf("matched = %v, want 2 (both Wayfair messages, not the friend or the seed)", res["matched"])
	}
	// The whole mailbox fits inside the default window, so the count is exact and
	// the client can present it as such rather than as a lower bound.
	if c, _ := res["complete"].(bool); !c {
		t.Errorf("complete = false, want true — %v messages fit in the scan of %v", res["total"], res["scanned"])
	}
	// A domain rule reads only stored columns, so no blob is opened.
	if rb, _ := res["readBodies"].(bool); rb {
		t.Errorf("readBodies = true, want false — fromDomain is on the row")
	}
	samples, _ := res["samples"].([]any)
	if len(samples) != 2 {
		t.Fatalf("got %d samples, want 2", len(samples))
	}
	// Samples are newest-first, matching the scan order the count came from.
	first, _ := samples[0].(map[string]any)
	if s, _ := first["subject"].(string); s != "Your Wayfair order shipped" {
		t.Errorf("first sample subject = %q, want the most recent match", s)
	}

	// A preview must not create the rule it previewed, nor label anything.
	var ruleRows, autoLabels int
	if err := h.super.QueryRow(context.Background(),
		`SELECT (SELECT count(*) FROM app.rules WHERE user_id = $1),
		        (SELECT count(*) FROM app.message_labels WHERE user_id = $1 AND applied_by <> 'user')`,
		a.UserID.String()).Scan(&ruleRows, &autoLabels); err != nil {
		t.Fatalf("count side effects: %v", err)
	}
	if ruleRows != 0 || autoLabels != 0 {
		t.Errorf("preview left %d rules and %d automated labels behind, want 0 and 0", ruleRows, autoLabels)
	}
}

// A bounded scan must say so. Reporting `matched` without `scanned`/`total`
// would let a client present "2 messages" for a rule that really matches
// hundreds beyond the window.
func TestRulePreviewReportsItsOwnBound(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	for i := 0; i < 4; i++ {
		h.seedMessage(t, a, "bulletin", "list@example.com", time.Duration(i+1)*time.Minute)
	}

	// The bound covers the fixture's own message (received now, so newest) plus
	// the two most recent bulletins. The other two bulletins are outside it — the
	// whole point of the assertion below.
	res := call(t, h.api.rulePreview, a.UserID, map[string]any{
		"conditions": map[string]any{"field": "subject", "op": "equals", "value": "bulletin"},
		"limit":      3,
	})

	if n, _ := res["scanned"].(float64); n != 3 {
		t.Errorf("scanned = %v, want 3 (the requested bound)", res["scanned"])
	}
	if n, _ := res["matched"].(float64); n != 2 {
		t.Errorf("matched = %v, want 2 — the bulletins inside the bound, not all 4", res["matched"])
	}
	if n, _ := res["total"].(float64); n != 5 {
		t.Errorf("total = %v, want 5 (4 seeded + the fixture's own message)", res["total"])
	}
	if c, _ := res["complete"].(bool); c {
		t.Error("complete = true, but the scan saw 2 of 5 — a client would present a lower bound as exact")
	}
}

// A condition the engine would reject is an error here, NOT a zero. The
// evaluator treats a malformed condition as non-matching, so answering "matched
// 0" would tell the user their broken rule is merely harmless.
func TestRulePreviewRejectsMalformedConditions(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	for _, tc := range []struct {
		name string
		cond map[string]any
	}{
		{"unknown field", map[string]any{"field": "recipientMood", "op": "equals", "value": "x"}},
		{"header leaf with no header name", map[string]any{"field": "header", "op": "exists"}},
		{"uncompilable regex", map[string]any{"field": "subject", "op": "matches", "value": "([a-z"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, mErr := h.api.rulePreview(context.Background(), a.UserID,
				mustJSON(t, map[string]any{"conditions": tc.cond}))
			if mErr == nil {
				t.Fatal("preview accepted a condition Rule/set would reject; a client would read the resulting 0 as 'this rule is safe'")
			}
			if mErr.Type != errInvalidArguments {
				t.Errorf("error type = %q, want %q", mErr.Type, errInvalidArguments)
			}
		})
	}
}

// The provenance half of #155: a label a rule applied names that rule, and
// deleting the rule keeps the label while dropping only the attribution.
func TestLabelProvenanceNamesTheRuleAndSurvivesItsDeletion(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)
	ctx := context.Background()

	ruleID := store.NewULID()
	if _, err := h.super.Exec(ctx,
		`INSERT INTO app.rules (id, user_id, name, conditions, actions)
		 VALUES ($1,$2,'Wayfair promos','{"field":"fromDomain","op":"equals","value":"wayfair.example"}','[]')`,
		ruleID.String(), a.UserID.String()); err != nil {
		t.Fatalf("seed rule: %v", err)
	}
	// A label the rule applies, DISTINCT from the one the fixture already applied
	// as the user. Reusing that one would hit the ON CONFLICT DO NOTHING that
	// deliberately preserves the user's own attribution — asserted separately at
	// the end of this test.
	ruleLabelID := store.NewULID()
	ruleLabelAtom := "MailroomLabel-promos-" + ruleLabelID.String()
	if _, err := h.super.Exec(ctx,
		`INSERT INTO app.labels (id, user_id, name) VALUES ($1,$2,$3)`,
		ruleLabelID.String(), a.UserID.String(), "Promos "+ruleLabelID.String()); err != nil {
		t.Fatalf("seed label: %v", err)
	}
	if _, err := h.super.Exec(ctx,
		`INSERT INTO app.label_keyword_map (user_id, label_id, keyword_atom) VALUES ($1,$2,$3)`,
		a.UserID.String(), ruleLabelID.String(), ruleLabelAtom); err != nil {
		t.Fatalf("seed label keyword: %v", err)
	}

	// Apply the label the way ingest does — through the attributed action path.
	if err := h.store.UserTx(ctx, a.UserID, func(tx *store.UserTx) error {
		errs := tx.ApplyRuleActions(ctx, a.MessageID, a.AccountID, a.InboxID,
			[]rules.AttributedAction{{
				Action: rules.Action{Type: rules.ActionAddLabel, Label: ruleLabelID.String()},
				RuleID: ruleID,
			}})
		if len(errs) > 0 {
			t.Fatalf("apply rule actions: %v", errs)
		}
		return nil
	}); err != nil {
		t.Fatalf("user tx: %v", err)
	}

	// entries returns the message's provenance list, keyed by keyword atom.
	entries := func() map[string]map[string]any {
		t.Helper()
		res := call(t, h.api.emailGet, a.UserID, map[string]any{
			"ids":        []string{a.MessageID.String()},
			"properties": []string{"id", "labelProvenance"},
		})
		list, _ := res["list"].([]any)
		if len(list) != 1 {
			t.Fatalf("Email/get returned %d messages, want 1", len(list))
		}
		msg, _ := list[0].(map[string]any)
		raw, _ := msg["labelProvenance"].([]any)
		out := map[string]map[string]any{}
		for _, e := range raw {
			entry, _ := e.(map[string]any)
			atom, _ := entry["keyword"].(string)
			out[atom] = entry
		}
		return out
	}

	all := entries()
	entry, ok := all[ruleLabelAtom]
	if !ok {
		t.Fatalf("no provenance for the rule-applied label: %v", all)
	}
	if by, _ := entry["appliedBy"].(string); by != store.LabelByRule {
		t.Errorf("appliedBy = %q, want %q", by, store.LabelByRule)
	}
	// The label the USER applied is untouched by the rule's work on the same
	// message: attribution is per label, not per message.
	if userEntry, ok := all[a.LabelAtom]; !ok {
		t.Errorf("the user's own label lost its provenance entry: %v", all)
	} else if by, _ := userEntry["appliedBy"].(string); by != store.LabelByUser {
		t.Errorf("the user's own label is now attributed to %q, want %q", by, store.LabelByUser)
	}
	rule, _ := entry["rule"].(map[string]any)
	if rule == nil {
		t.Fatal("rule-applied label reports no rule — 'a rule did this' without saying which is the state #155 exists to fix")
	}
	if name, _ := rule["name"].(string); name != "Wayfair promos" {
		t.Errorf("rule name = %q, want %q — the client must not need a second fetch to name it", name, "Wayfair promos")
	}

	// Deleting the rule must NOT unlabel the mail (ON DELETE SET NULL, never
	// CASCADE): the attribution is disposable, the user-visible label is not.
	if _, err := h.super.Exec(ctx, `DELETE FROM app.rules WHERE id = $1`, ruleID.String()); err != nil {
		t.Fatalf("delete rule: %v", err)
	}
	after, ok := entries()[ruleLabelAtom]
	if !ok {
		t.Fatal("deleting the rule DELETED THE LABEL — ON DELETE CASCADE would silently unlabel every message the rule ever touched")
	}
	if after["rule"] != nil {
		t.Errorf("deleted rule still named: %v", after["rule"])
	}
	if by, _ := after["appliedBy"].(string); by != store.LabelByRule {
		t.Errorf("appliedBy = %q after deleting the rule, want %q — the label is still an automation's work", by, store.LabelByRule)
	}
}

// listId (#155) is what "create a rule from this mailing list" keys on. It must
// be null — not "" — for mail that is not from a list, because the client offers
// that starting point only when there is a list to name: a `listId exists` rule
// would match EVERY mailing list, which is never what someone means while
// looking at one specific newsletter.
func TestEmailExposesListIDOnlyForListMail(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)
	ctx := context.Background()

	listMsg := h.seedMessage(t, a, "Weekly bulletin", "news@list.example", time.Minute)
	if _, err := h.super.Exec(ctx,
		`UPDATE app.messages SET list_id = $2 WHERE id = $1`,
		listMsg.String(), "<weekly.list.example>"); err != nil {
		t.Fatalf("set list_id: %v", err)
	}

	res := call(t, h.api.emailGet, a.UserID, map[string]any{
		"ids":        []string{listMsg.String(), a.MessageID.String()},
		"properties": []string{"id", "listId"},
	})
	byID := map[string]any{}
	for _, e := range res["list"].([]any) {
		m, _ := e.(map[string]any)
		byID[m["id"].(string)] = m["listId"]
	}
	if got := byID[listMsg.String()]; got != "<weekly.list.example>" {
		t.Errorf("listId = %v, want the List-Id value", got)
	}
	if got, ok := byID[a.MessageID.String()]; !ok || got != nil {
		t.Errorf("listId = %v for non-list mail, want null — an empty string would make "+
			"every message look like it belongs to a list", got)
	}
}

// A label the USER applied says so. The default matters: applied_by has existed
// since the baseline schema and every row claimed 'user' because nothing set it,
// so "it says user" only means something once something else can say otherwise.
func TestUserAppliedLabelIsNotAttributedToARule(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	res := call(t, h.api.emailGet, a.UserID, map[string]any{
		"ids":        []string{a.MessageID.String()},
		"properties": []string{"id", "labelProvenance"},
	})
	list, _ := res["list"].([]any)
	msg, _ := list[0].(map[string]any)
	entries, _ := msg["labelProvenance"].([]any)
	if len(entries) != 1 {
		t.Fatalf("got %d provenance entries, want 1 (the fixture's seeded label)", len(entries))
	}
	entry, _ := entries[0].(map[string]any)
	if by, _ := entry["appliedBy"].(string); by != store.LabelByUser {
		t.Errorf("appliedBy = %q, want %q", by, store.LabelByUser)
	}
	if entry["rule"] != nil {
		t.Errorf("a hand-applied label named a rule: %v", entry["rule"])
	}
}
