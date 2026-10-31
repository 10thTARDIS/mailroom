// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package ingest

// The #242 follow-up guard, exercised where it actually has to work: ONE ingest,
// ONE transaction, a rule that FILES the message and a rule that would QUESTION
// it.
//
// store.PutRuleSuggestion refuses to write for a message that already carries a
// label, because a suggestion row satisfies one arm of the needs-a-category
// predicate and would drag already-categorised mail back into the review queue
// (a real share of suggestions on a real mailbox). Its own store test proves the
// SELECT sees a label committed earlier. This one proves the part that could
// still be wrong in production and invisible there: whether it sees a label
// applied moments ago in the SAME transaction, by an earlier rule, during the
// same delivery.
//
// ⚠ IT WORKS ONLY BECAUSE OF TWO PROPERTIES, AND THIS IS THE ONLY TEST THAT
// TOUCHES EITHER:
//
//  1. ORDERING. RulesForEval returns enabled rules ORDER BY priority, id
//     ascending; EvaluateAttributed emits their actions in that order; and
//     ApplyRuleActions applies them in that order. suggest-rules creates its
//     suggestion rules at priority 200 against an ordinary rule's default of 100
//     (see cmd/mailroom-admin/suggestrules.go's suggestRulePriority), so the
//     addLabel has already run by the time the suggestLabel arm is reached.
//     Reverse the two priorities and the suggestion is written — the guard is
//     ordering-dependent, not ordering-proof, and this test is where that shows.
//  2. READ-YOUR-OWN-WRITES. The EXISTS runs in the transaction that did the
//     INSERT, so it sees an uncommitted row. Nothing outside this transaction
//     would.
//
// The second message in each test is not decoration: without a case where the
// suggestion IS written, "0 suggestion rows" is equally consistent with a
// suggestion rule that never fired at all, which is the opposite diagnosis.

import (
	"context"
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/rules"
	"mailroom/internal/store"
)

// filedAndSuggestedRules creates the pair: an addLabel rule at priority 100
// keyed on the SUBJECT, and a suggestLabel rule at priority 200 keyed on the
// SENDER. Both fire on the same message; only the sender rule fires on the
// control message. The two labels are deliberately different — the guard's claim
// is "this message already has a category", not "this message already has THIS
// category".
func filedAndSuggestedRules(t *testing.T, f *fixture, filedPriority, suggestPriority int) (filed, suggested ulid.ULID) {
	t.Helper()
	ctx := context.Background()
	if err := f.st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
		var err error
		if filed, err = tx.CreateLabel(ctx, store.LabelRow{Name: "Filed-" + store.NewULID().String()}); err != nil {
			return err
		}
		if suggested, err = tx.CreateLabel(ctx, store.LabelRow{Name: "Suggested-" + store.NewULID().String()}); err != nil {
			return err
		}
		if _, err = tx.CreateRule(ctx, store.RuleRow{
			Name: "file-invoices-" + store.NewULID().String(), Enabled: true,
			Priority:   filedPriority,
			Conditions: rules.Condition{Field: rules.FieldSubject, Op: rules.OpContains, Value: "invoice"},
			Actions:    []rules.Action{{Type: rules.ActionAddLabel, Label: filed.String()}},
		}); err != nil {
			return err
		}
		_, err = tx.CreateRule(ctx, store.RuleRow{
			Name: "suggest-from-alice-" + store.NewULID().String(), Enabled: true,
			Priority:   suggestPriority,
			Conditions: rules.Condition{Field: rules.FieldFrom, Op: rules.OpEquals, Value: "alice@example.com"},
			Actions: []rules.Action{{
				Type: rules.ActionSuggestLabel, Label: suggested.String(), Evidence: 4,
			}},
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return filed, suggested
}

// TestSuggestLabelRuleStaysQuietOnMailAnEarlierRuleJustFiled is the load-bearing
// one: same delivery, same transaction, label wins and the question is dropped.
func TestSuggestLabelRuleStaysQuietOnMailAnEarlierRuleJustFiled(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	// The production arrangement: ordinary rules at 100, induced suggestions at 200.
	filed, suggested := filedAndSuggestedRules(t, f, 100, 200)

	// MATCHED BY BOTH — the subject carries "invoice" and the sender is alice.
	both := rawMessage("Invoice 42", "rule-filed-both@example.com", nil)
	resBoth, err := f.ing.Ingest(ctx, f.userID, f.dek, f.dekID, both, f.prov(f.mailboxA, 1201, nil))
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if !resBoth.Created {
		t.Fatal("expected a new canonical message")
	}

	// MATCHED ONLY BY THE SUGGESTION RULE — the control. Everything below reads as
	// "the guard fired" only if this message gets its suggestion.
	only := rawMessage("Just saying hello", "rule-filed-only@example.com", nil)
	resOnly, err := f.ing.Ingest(ctx, f.userID, f.dek, f.dekID, only, f.prov(f.mailboxA, 1202, nil))
	if err != nil {
		t.Fatalf("ingest control: %v", err)
	}
	if !resOnly.Created {
		t.Fatal("expected a new canonical message for the control")
	}

	// ⚠ EVERY ASSERTION IS t.Errorf AND ALL OF THEM RUN. The four numbers below
	// diagnose four different failures and a Fatalf on the first would hide the
	// rest — "the label did not land" and "the suggestion was written anyway" are
	// opposite conclusions drawn from the same run.

	// THE LABEL LANDED. This is what makes the zero underneath meaningful: without
	// it, "no suggestion" could just as well mean the addLabel rule never fired
	// and the suggestion rule was skipped for some unrelated reason.
	if n := f.scalar(t, `SELECT count(*) FROM app.message_labels WHERE message_id=$1 AND label_id=$2`,
		resBoth.MessageID.String(), filed.String()); n != 1 {
		t.Errorf("message_labels for (both-matched msg, filed label) = %d, want 1 — the addLabel rule "+
			"did not fire, so the suggestion assertion below proves nothing", n)
	}
	// AND NO QUESTION WAS ASKED ABOUT IT. Not for the suggested label, and not for
	// anything else: the guard is about the MESSAGE being filed, so it must leave
	// app.label_suggestions empty for it entirely.
	if n := f.scalar(t, `SELECT count(*) FROM app.label_suggestions WHERE message_id=$1`,
		resBoth.MessageID.String()); n != 0 {
		t.Errorf("label_suggestions rows for the filed message = %d, want 0 — a rule filed this "+
			"message in this very transaction and the suggestion was written anyway, which puts "+
			"already-categorised mail back into the needs-a-category queue (#242 follow-up). "+
			"Check ApplyRuleActions: every suggestLabel must run in a SECOND pass, after all "+
			"other actions of the delivery, so the guard's EXISTS sees the label this "+
			"transaction just wrote. Priority is NOT what protects this — see the "+
			"runs-first test below", n)
	}

	// THE CONTROL: unfiled mail still gets its suggestion, with the rule's evidence.
	if n := f.scalar(t, `SELECT count(*) FROM app.label_suggestions WHERE message_id=$1 AND label_id=$2`,
		resOnly.MessageID.String(), suggested.String()); n != 1 {
		t.Errorf("label_suggestions rows for the UNFILED message = %d, want 1 — the suggestion rule "+
			"produces nothing at all, so the zero above is a broken fixture rather than the guard", n)
	}
	// ...and nothing labelled it, since a suggestLabel action never applies a label.
	if n := f.scalar(t, `SELECT count(*) FROM app.message_labels WHERE message_id=$1`,
		resOnly.MessageID.String()); n != 0 {
		t.Errorf("message_labels rows for the unfiled message = %d, want 0", n)
	}

	// MAIL SAFETY, as on every rule path: both messages are stored and untrashed.
	// A guard that silently skipped a write is one thing; a guard that aborted an
	// ingest would be a mail-loss bug.
	for _, id := range []ulid.ULID{resBoth.MessageID, resOnly.MessageID} {
		if n := f.scalar(t, `SELECT count(*) FROM app.messages WHERE id=$1 AND trashed_at IS NULL`,
			id.String()); n != 1 {
			t.Errorf("message %s present-and-untrashed = %d, want 1", id, n)
		}
	}
	// The suggestion appends no change_log row of its own, and neither does the
	// label at ingest: two deliveries, two Email `created` rows, no `updated`.
	if n := f.changeCount(t, "Email", "created"); n != 2 {
		t.Errorf("Email created change rows = %d, want 2", n)
	}
	if n := f.changeCount(t, "Email", "updated"); n != 0 {
		t.Errorf("Email updated change rows = %d, want 0 — a rule action at ingest must not append "+
			"a row carrying a HIGHER seq than the message's own `created` row", n)
	}
}

// TestSuggestLabelRuleStaysQuietEvenWhenItsRuleRunsFirst is the same guarantee
// with the priorities INVERTED, and it is the one that says the guard is
// structural rather than lucky.
//
// ⚠ THIS CASE USED TO WRITE THE SUGGESTION. It was found by probing the test
// above: swapping the two priorities made it go red, which meant the guard held
// only because `suggest-rules` happens to create its rules at 200 while ordinary
// rules default to 100. That is not a property of the system, it is a property of
// one command's default — and app.rules.priority's OWN default is 100, so a
// suggestLabel rule written by hand through Rule/set sits at the same priority as
// every addLabel rule, leaving RulesForEval's `ORDER BY priority, id` to settle it
// by CREATION ORDER. A user who wrote their suggestion rule first would have hit
// the queue-inflation defect on a path nobody thought to test.
//
// The fix was to stop depending on the order at all: ApplyRuleActions now applies
// every suggestLabel in a second pass, after all other actions of the delivery.
// So this test pins the INVERSION specifically — suggestion rule at 100, labelling
// rule at 200, suggestion rule therefore evaluated FIRST — and still expects
// silence. If someone collapses that back into a single loop, this is the test
// that goes red while the one above stays green.
func TestSuggestLabelRuleStaysQuietEvenWhenItsRuleRunsFirst(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	// INVERTED: the suggestion rule now sorts BEFORE the labelling rule.
	filed, suggested := filedAndSuggestedRules(t, f, 200, 100)

	both := rawMessage("Invoice 43", "inverted-both@example.com", nil)
	resBoth, err := f.ing.Ingest(ctx, f.userID, f.dek, f.dekID, both, f.prov(f.mailboxA, 1301, nil))
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	only := rawMessage("Nothing to file here", "inverted-only@example.com", nil)
	resOnly, err := f.ing.Ingest(ctx, f.userID, f.dek, f.dekID, only, f.prov(f.mailboxA, 1302, nil))
	if err != nil {
		t.Fatalf("ingest control: %v", err)
	}

	if n := f.scalar(t, `SELECT count(*) FROM app.message_labels WHERE message_id=$1 AND label_id=$2`,
		resBoth.MessageID.String(), filed.String()); n != 1 {
		t.Errorf("message_labels for (both-matched msg, filed label) = %d, want 1 — the addLabel rule "+
			"did not fire, so the zero below proves nothing", n)
	}
	if n := f.scalar(t, `SELECT count(*) FROM app.label_suggestions WHERE message_id=$1`,
		resBoth.MessageID.String()); n != 0 {
		t.Errorf("label_suggestions rows = %d, want 0 — the suggestion rule sorted FIRST and its "+
			"question was written before the label landed. ApplyRuleActions has stopped deferring "+
			"suggestLabel to a second pass, so the guard is back to depending on rule priority "+
			"(#242 follow-up)", n)
	}
	// The control, again: without it a zero above is indistinguishable from a
	// suggestion rule that never fired at these priorities at all.
	if n := f.scalar(t, `SELECT count(*) FROM app.label_suggestions WHERE message_id=$1 AND label_id=$2`,
		resOnly.MessageID.String(), suggested.String()); n != 1 {
		t.Errorf("label_suggestions rows for the UNFILED message = %d, want 1 — the suggestion rule "+
			"produces nothing at this priority, so the zero above is a broken fixture, not the guard", n)
	}
	for _, id := range []ulid.ULID{resBoth.MessageID, resOnly.MessageID} {
		if n := f.scalar(t, `SELECT count(*) FROM app.messages WHERE id=$1 AND trashed_at IS NULL`,
			id.String()); n != 1 {
			t.Errorf("message %s present-and-untrashed = %d, want 1", id, n)
		}
	}
}
