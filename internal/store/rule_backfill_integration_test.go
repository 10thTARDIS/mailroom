// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// Predicting what a backfill will do (spec §8, mailroom#193).
//
// LabelsOnMessages exists because of one bug, and the bug is worth stating: a
// predictor built from the USER's labels plus the labels THIS rule applied
// reports a message that some OTHER rule has already labelled as about to be
// newly labelled. The apply then correctly changes nothing, so the count is
// wrong, and — worse — wrong the same way on every subsequent run. It never
// converges. Two rules with the same action are enough to produce it, which on a
// mailbox whose rules came from induction is not a corner case.
//
// So the distinction under test is narrow and load-bearing: this reads what is ON
// the message regardless of who put it there, which is the only thing that
// predicts whether an additive update will do anything.

import (
	"context"
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/rules"
)

func TestLabelsOnMessagesSeesLabelsWhoeverAppliedThem(t *testing.T) {
	h := newHarness(t)
	s := h.appStore()
	ctx := context.Background()
	u := seedUser(t, h.super, "onmsg-"+NewULID().String())
	userID, msgID := mustULID(t, u.ID), mustULID(t, u.MessageID)

	ruleA, labelA := seedRuleAndLabel(t, s, userID, "Applied-By-A")
	_, labelHand := seedRuleAndLabel(t, s, userID, "Applied-By-Hand")

	// One label from a RULE, one from the USER — the two attributions that matter.
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		if _, _, e := tx.ApplyEmailUpdate(ctx, msgID, EmailUpdate{
			AddLabelIDs: []ulid.ULID{labelA},
			LabelSource: RuleLabelSource(ruleA),
		}); e != nil {
			return e
		}
		_, _, e := tx.ApplyEmailUpdate(ctx, msgID, EmailUpdate{AddLabelIDs: []ulid.ULID{labelHand}})
		return e
	}); err != nil {
		t.Fatalf("label the message: %v", err)
	}

	var onMsg, byRuleA map[ulid.ULID][]ulid.ULID
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		var e error
		if onMsg, e = tx.LabelsOnMessages(ctx, []ulid.ULID{msgID}); e != nil {
			return e
		}
		byRuleA, e = tx.LabelsAppliedByRule(ctx, ruleA, []ulid.ULID{msgID})
		return e
	}); err != nil {
		t.Fatalf("read back: %v", err)
	}

	got := map[ulid.ULID]bool{}
	for _, lid := range onMsg[msgID] {
		got[lid] = true
	}
	if !got[labelA] || !got[labelHand] {
		t.Errorf("LabelsOnMessages = %v, want both the rule-applied and the hand-applied label",
			onMsg[msgID])
	}

	// THE CONTRAST IS THE POINT. LabelsAppliedByRule is deliberately narrow — it
	// is what makes an undo incapable of removing something the user applied — and
	// using it to predict an apply is exactly the mistake this pair guards against.
	if len(byRuleA[msgID]) != 1 || byRuleA[msgID][0] != labelA {
		t.Errorf("LabelsAppliedByRule = %v, want only rule A's own label", byRuleA[msgID])
	}
}

// A second rule adding a label the FIRST rule already applied must be predicted
// as a no-op. This is the non-convergence bug reduced to its smallest form: two
// rules, one label, one message.
func TestASecondRuleAddingTheSameLabelChangesNothing(t *testing.T) {
	h := newHarness(t)
	s := h.appStore()
	ctx := context.Background()
	u := seedUser(t, h.super, "onmsg2-"+NewULID().String())
	userID, msgID := mustULID(t, u.ID), mustULID(t, u.MessageID)

	ruleA, labelID := seedRuleAndLabel(t, s, userID, "Shared-Label")

	// A second rule pointing at the SAME label, as accept-rules run twice produces.
	var ruleB ulid.ULID
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		var e error
		ruleB, e = tx.CreateRule(ctx, RuleRow{
			Name:       "second copy",
			Priority:   100,
			Conditions: rules.Condition{Field: rules.FieldFromDomain, Op: rules.OpEquals, Value: "shop.example"},
			Actions:    []rules.Action{{Type: rules.ActionAddLabel, Label: labelID.String()}},
			Enabled:    true,
		})
		return e
	}); err != nil {
		t.Fatalf("seed second rule: %v", err)
	}

	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		state, e := tx.ChangeState(ctx, ObjectEmail)
		if e != nil {
			return e
		}
		// Rule A labels it: the state must advance.
		_, afterA, e := tx.ApplyEmailUpdate(ctx, msgID, EmailUpdate{
			AddLabelIDs: []ulid.ULID{labelID},
			LabelSource: RuleLabelSource(ruleA),
		})
		if e != nil {
			return e
		}
		if afterA <= state {
			t.Errorf("rule A's apply did not advance the Email state (%d -> %d)", state, afterA)
		}

		// Rule B asks for the same label. Nothing to do, so the state must NOT move —
		// and the label must keep rule A's attribution rather than being re-stamped.
		_, afterB, e := tx.ApplyEmailUpdate(ctx, msgID, EmailUpdate{
			AddLabelIDs: []ulid.ULID{labelID},
			LabelSource: RuleLabelSource(ruleB),
		})
		if e != nil {
			return e
		}
		if afterB > afterA {
			t.Errorf("rule B re-applied a label already present (%d -> %d); a backfill would "+
				"report it as newly labelled on every run", afterA, afterB)
		}

		byB, e := tx.LabelsAppliedByRule(ctx, ruleB, []ulid.ULID{msgID})
		if e != nil {
			return e
		}
		if len(byB[msgID]) != 0 {
			t.Errorf("rule B took over attribution for a label rule A applied: %v", byB[msgID])
		}
		return nil
	}); err != nil {
		t.Fatalf("apply twice: %v", err)
	}
}
