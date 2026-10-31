// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package ingest

// Integration coverage for RULE-SOURCED label suggestions at ingest (#242): the
// rule-path twin of TestClassifierSuggestsButNeverApplies.
//
// The invariant is the same one and it now has two producers: a `suggestLabel`
// action must write an app.label_suggestions row and must NEVER write
// app.message_labels. It needs its own test because the two paths share no code —
// they are separate arms of store.applyRuleAction writing to different tables,
// which is deliberately what makes "a suggestion was applied as a label"
// structurally impossible, and equally what makes the classifier's test say
// nothing about this one.
//
// It is also the only end-to-end check that the pieces are actually WIRED: rules
// validation accepting the action, EvaluateAttributed carrying the rule's
// conditions through, the store rendering them, and the row landing with
// source='rule'. Each half is unit-tested; "it ran and changed nothing" is what
// happens when they are individually correct and never meet.
//
// Runs against the throwaway Postgres from scripts/integration-test.sh; skips
// when absent.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/ml"
	"mailroom/internal/rules"
	"mailroom/internal/store"
)

// suggestRule creates a label and a rule that SUGGESTS it for mail from
// alice@example.com with "sale" in the subject — both facts rawMessage supplies.
// evidence is stamped on the action the way an accepted induced candidate would
// carry Candidate.Positives.
func suggestRule(t *testing.T, f *fixture, name string, evidence int) (labelID, ruleID ulid.ULID) {
	t.Helper()
	ctx := context.Background()
	if err := f.st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
		var err error
		labelID, err = tx.CreateLabel(ctx, store.LabelRow{Name: name + "-" + store.NewULID().String()})
		if err != nil {
			return err
		}
		ruleID, err = tx.CreateRule(ctx, store.RuleRow{
			Name: name + "-" + store.NewULID().String(), Enabled: true,
			Conditions: rules.Condition{All: []rules.Condition{
				{Field: rules.FieldFrom, Op: rules.OpEquals, Value: "alice@example.com"},
				{Field: rules.FieldSubject, Op: rules.OpContains, Value: "sale"},
			}},
			Actions: []rules.Action{{
				Type: rules.ActionSuggestLabel, Label: labelID.String(), Evidence: evidence,
			}},
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return labelID, ruleID
}

// TestSuggestLabelRuleSuggestsButNeverApplies is the load-bearing one. A rule
// that matches — proven by the suggestion row existing — must leave
// app.message_labels untouched.
func TestSuggestLabelRuleSuggestsButNeverApplies(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	labelID, _ := suggestRule(t, f, "Promotions", 4)

	raw := rawMessage("Big Sale", "rule-suggest-1@example.com", nil)
	res, err := f.ing.Ingest(ctx, f.userID, f.dek, f.dekID, raw, f.prov(f.mailboxA, 1101, nil))
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if !res.Created {
		t.Fatal("expected a new canonical message")
	}

	// ⚠ THE TWO HALVES ARE BOTH t.Errorf AND BOTH ALWAYS RUN. Neither is a Fatalf,
	// deliberately: swapping this action's execution onto addLabel's arm makes the
	// suggestion count 0 AND the label count 1, and a Fatalf on the first would
	// hide the second — the report would say "the rule did not fire" when what
	// actually happened is that it fired and labelled the message, which is the
	// opposite diagnosis. Measured, not assumed: that exact probe was run against
	// an earlier draft of this test and produced exactly that misleading output.
	suggestions := f.scalar(t, `SELECT count(*) FROM app.label_suggestions WHERE message_id=$1 AND label_id=$2`,
		res.MessageID.String(), labelID.String())
	if suggestions != 1 {
		t.Errorf("label_suggestions rows for (msg,label) = %d, want 1 — the suggestLabel rule wrote no question", suggestions)
	}
	// THE LABEL WAS NOT APPLIED — the assertion the whole action type exists for.
	// It is not vacuous while the assertion above holds: a suggestion row proves
	// the rule matched, this message, in this ingest, so a zero here is "it
	// declined to label" and not "nothing happened".
	if n := f.scalar(t, `SELECT count(*) FROM app.message_labels WHERE message_id=$1 AND label_id=$2`,
		res.MessageID.String(), labelID.String()); n != 0 {
		t.Errorf("message_labels rows for (msg,label) = %d, want 0 — a suggestLabel rule applied the label", n)
	}
	// Nothing else got labelled either, in case a future arm resolves a different
	// label id and attaches that.
	if n := f.scalar(t, `SELECT count(*) FROM app.message_labels WHERE message_id=$1`,
		res.MessageID.String()); n != 0 {
		t.Errorf("message_labels rows for the message = %d, want 0", n)
	}
	if suggestions == 0 {
		// Everything below reads the row's columns; without a row those are
		// ErrNoRows noise on top of the failures already reported.
		t.FailNow()
	}

	var source string
	var confidence float64
	var modelVersion int
	if err := f.super.QueryRow(ctx,
		`SELECT source, confidence, model_version FROM app.label_suggestions
		  WHERE message_id=$1 AND label_id=$2`,
		res.MessageID.String(), labelID.String()).Scan(&source, &confidence, &modelVersion); err != nil {
		t.Fatal(err)
	}
	// source is what lets the client know confidence is an ordering rank here and
	// must NOT be rendered as a percentage (#242). Getting it wrong turns 0.8 into
	// "80% confident", a statistic nobody measured.
	if source != store.SuggestionSourceRule {
		t.Errorf("source = %q, want %q", source, store.SuggestionSourceRule)
	}
	if want := rules.EvidenceConfidence(4); confidence != want {
		t.Errorf("confidence = %v, want %v (EvidenceConfidence of the action's evidence)", confidence, want)
	}
	if modelVersion != 0 {
		t.Errorf("model_version = %d, want 0 — no model is involved in this path", modelVersion)
	}

	// THE change_log INVARIANT. PutLabelSuggestion deliberately appends no row: a
	// suggestion arriving with a new message rides that message's own Email
	// `created` row. An `updated` row here would carry a HIGHER seq than the
	// `created` one for the same object, which is the ordering hazard
	// ApplyRuleActions' doc comment describes.
	if n := f.changeCount(t, "Email", "created"); n != 1 {
		t.Errorf("Email created change rows = %d, want 1", n)
	}
	if n := f.changeCount(t, "Email", "updated"); n != 0 {
		t.Errorf("Email updated change rows = %d, want 0 — a suggestion must not append its own change_log row", n)
	}

	// And the message itself is fine — a rule never loses mail.
	if n := f.scalar(t, `SELECT count(*) FROM app.messages WHERE id=$1 AND trashed_at IS NULL`,
		res.MessageID.String()); n != 1 {
		t.Errorf("message present-and-untrashed = %d, want 1", n)
	}
}

// TestSuggestLabelRuleUnknownLabelIsACleanSkip covers the fail-soft half: a rule
// naming a label that no longer exists must write nothing and must not fail the
// ingest. It is the case a user creates by deleting a label a rule points at,
// which is an ordinary thing to do.
func TestSuggestLabelRuleUnknownLabelIsACleanSkip(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// A syntactically valid label id that resolves to nothing — exactly what a
	// since-deleted label leaves behind in a rule.
	ghost := store.NewULID()
	if err := f.st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
		_, err := tx.CreateRule(ctx, store.RuleRow{
			Name: "ghost-" + store.NewULID().String(), Enabled: true,
			Conditions: rules.Condition{Field: rules.FieldFrom, Op: rules.OpEquals, Value: "alice@example.com"},
			Actions: []rules.Action{{
				Type: rules.ActionSuggestLabel, Label: ghost.String(), Evidence: 3,
			}},
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}

	raw := rawMessage("Big Sale", "rule-suggest-ghost@example.com", nil)
	res, err := f.ing.Ingest(ctx, f.userID, f.dek, f.dekID, raw, f.prov(f.mailboxA, 1102, nil))
	// THE MAIL-SAFETY ASSERTION: a broken rule does not fail an ingest.
	if err != nil {
		t.Fatalf("ingest failed because of a rule naming a missing label: %v", err)
	}
	if !res.Created {
		t.Fatal("the message was not stored")
	}
	if n := f.scalar(t, `SELECT count(*) FROM app.messages WHERE id=$1 AND trashed_at IS NULL`,
		res.MessageID.String()); n != 1 {
		t.Fatalf("message present-and-untrashed = %d, want 1", n)
	}
	// Nothing was written on either side — not a suggestion pointing at a
	// non-existent label, and certainly not a label row.
	if n := f.scalar(t, `SELECT count(*) FROM app.label_suggestions WHERE message_id=$1`,
		res.MessageID.String()); n != 0 {
		t.Errorf("label_suggestions rows = %d, want 0 for an unresolvable label", n)
	}
	if n := f.scalar(t, `SELECT count(*) FROM app.message_labels WHERE message_id=$1`,
		res.MessageID.String()); n != 0 {
		t.Errorf("message_labels rows = %d, want 0", n)
	}
}

// TestSuggestLabelRuleReasonsRoundTrip proves the stored explanation survives the
// database and decodes as the renderer reads it (#176's contract, #242's payload).
//
// It reads the raw jsonb through the store's own accessor rather than a direct
// SELECT, so it exercises SuggestionsForMessages' scan — which is where a column
// added to the INSERT and forgotten in the SELECT would show up.
func TestSuggestLabelRuleReasonsRoundTrip(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	labelID, _ := suggestRule(t, f, "Promotions", 4)

	raw := rawMessage("Big Sale", "rule-suggest-reasons@example.com", nil)
	res, err := f.ing.Ingest(ctx, f.userID, f.dek, f.dekID, raw, f.prov(f.mailboxA, 1103, nil))
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}

	var got store.LabelSuggestion
	if err := f.st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
		byMsg, err := tx.SuggestionsForMessages(ctx, []ulid.ULID{res.MessageID})
		if err != nil {
			return err
		}
		ss := byMsg[res.MessageID]
		if len(ss) != 1 {
			t.Fatalf("SuggestionsForMessages returned %d suggestions, want 1", len(ss))
		}
		got = ss[0]
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if got.LabelID != labelID {
		t.Errorf("label id = %s, want %s", got.LabelID, labelID)
	}
	if got.Source != store.SuggestionSourceRule {
		t.Errorf("Source = %q, want %q — SuggestionsForMessages must select the column", got.Source, store.SuggestionSourceRule)
	}
	if len(got.Reasons) == 0 {
		t.Fatal("Reasons is empty; a rule always knows why it fired")
	}

	var contribs []ml.FeatureContribution
	if err := json.Unmarshal(got.Reasons, &contribs); err != nil {
		t.Fatalf("stored reasons do not decode as []ml.FeatureContribution: %v (raw: %s)", err, got.Reasons)
	}
	// The rendered conjuncts, in condition order, then the evidence sentence.
	want := []string{
		"sender is alice@example.com",
		"subject contains sale",
		"backed by 4 messages you filed under this label",
	}
	if len(contribs) != len(want) {
		t.Fatalf("decoded %d reasons %+v, want %d", len(contribs), contribs, len(want))
	}
	for i, w := range want {
		if contribs[i].Text != w {
			t.Errorf("reason %d text = %q, want %q", i, contribs[i].Text, w)
		}
		// Weight 0 is the deliberate statement that an AND rule's conjuncts have no
		// individual weights — see store.ruleSuggestionReasons.
		if contribs[i].Weight != 0 {
			t.Errorf("reason %d weight = %v, want 0", i, contribs[i].Weight)
		}
		if contribs[i].Key != "" {
			t.Errorf("reason %d key = %q, want empty", i, contribs[i].Key)
		}
	}
}

// TestSuggestLabelRuleReasonsFollowATightenedRule is the reason the attribution
// carries the LIVE condition tree rather than text baked at rule-creation time.
//
// Narrowing a rule (what Rule/tighten does — an UpdateRule with new conditions)
// must change what its future suggestions SAY. With pre-rendered text stored on
// the action, this test would show a suggestion citing a condition the rule no
// longer has: a stored explanation that is a claim about the code, and false.
func TestSuggestLabelRuleReasonsFollowATightenedRule(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	_, ruleID := suggestRule(t, f, "Promotions", 4)

	// Tighten it: same action, narrower conditions.
	tightened := rules.Condition{All: []rules.Condition{
		{Field: rules.FieldFrom, Op: rules.OpEquals, Value: "alice@example.com"},
		{Field: rules.FieldSubject, Op: rules.OpContains, Value: "big sale"},
	}}
	if err := f.st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
		found, err := tx.UpdateRule(ctx, ruleID, store.RulePatch{Conditions: &tightened})
		if err != nil {
			return err
		}
		if !found {
			t.Fatal("rule not found for tightening")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	raw := rawMessage("Big Sale", "rule-suggest-tightened@example.com", nil)
	res, err := f.ing.Ingest(ctx, f.userID, f.dek, f.dekID, raw, f.prov(f.mailboxA, 1104, nil))
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}

	var rawReasons []byte
	if err := f.super.QueryRow(ctx,
		`SELECT reasons FROM app.label_suggestions WHERE message_id=$1`,
		res.MessageID.String()).Scan(&rawReasons); err != nil {
		t.Fatalf("read reasons (did the tightened rule still match?): %v", err)
	}
	var contribs []ml.FeatureContribution
	if err := json.Unmarshal(rawReasons, &contribs); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range contribs {
		if c.Text == "subject contains big sale" {
			found = true
		}
		if c.Text == "subject contains sale" {
			t.Errorf("the suggestion cites the rule's PRE-TIGHTENING condition %q; reasons were baked, not rendered live", c.Text)
		}
	}
	if !found {
		t.Errorf("reasons %+v do not mention the tightened condition", contribs)
	}
}
