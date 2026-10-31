// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// Taking back what a rule did (spec §8, mailroom#170).
//
// This used to be a dead end: removing a label deleted the row and its
// attribution together, so a wrong rule kept firing on every future match and
// nothing anywhere recorded that a human had disagreed. These tests pin the
// three things that make the record meaningful — it captures a rule's mistake,
// it does NOT capture a person's own change of mind, and it counts messages
// rather than events.

import (
	"context"
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/rules"
)

// seedRuleAndLabel creates a label and an enabled rule that applies it.
func seedRuleAndLabel(t *testing.T, s *Store, userID ulid.ULID, name string) (ruleID, labelID ulid.ULID) {
	t.Helper()
	ctx := context.Background()
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		var e error
		labelID, e = tx.CreateLabel(ctx, LabelRow{Name: name, MirrorToIMAP: true})
		if e != nil {
			return e
		}
		ruleID, e = tx.CreateRule(ctx, RuleRow{
			Name:       name + " rule",
			Priority:   100,
			Conditions: rules.Condition{Field: rules.FieldFromDomain, Op: rules.OpEquals, Value: "shop.example"},
			Actions:    []rules.Action{{Type: rules.ActionAddLabel, Label: labelID.String()}},
			Enabled:    true,
		})
		return e
	}); err != nil {
		t.Fatalf("seed rule and label: %v", err)
	}
	return ruleID, labelID
}

func corrections(t *testing.T, s *Store, userID ulid.ULID) map[ulid.ULID]int {
	t.Helper()
	var out map[ulid.ULID]int
	if err := s.UserTx(context.Background(), userID, func(tx *UserTx) error {
		var e error
		out, e = tx.CountRuleCorrections(context.Background())
		return e
	}); err != nil {
		t.Fatalf("count corrections: %v", err)
	}
	return out
}

func TestRemovingARuleLabelIsRecordedAsACorrection(t *testing.T) {
	h := newHarness(t)
	s := h.appStore()
	ctx := context.Background()
	u := seedUser(t, h.super, "corr-"+NewULID().String())
	userID, msgID := mustULID(t, u.ID), mustULID(t, u.MessageID)

	ruleID, labelID := seedRuleAndLabel(t, s, userID, "Promotions")

	// The RULE labels the message.
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		_, _, e := tx.ApplyEmailUpdate(ctx, msgID, EmailUpdate{
			AddLabelIDs: []ulid.ULID{labelID},
			LabelSource: RuleLabelSource(ruleID),
		})
		return e
	}); err != nil {
		t.Fatalf("rule labels: %v", err)
	}
	if n := corrections(t, s, userID)[ruleID]; n != 0 {
		t.Fatalf("a rule nobody has corrected reports %d corrections", n)
	}

	// The USER takes it back.
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		_, _, e := tx.ApplyEmailUpdate(ctx, msgID, EmailUpdate{RemoveLabelIDs: []ulid.ULID{labelID}})
		return e
	}); err != nil {
		t.Fatalf("user removes: %v", err)
	}

	if n := corrections(t, s, userID)[ruleID]; n != 1 {
		t.Errorf("correction not recorded: got %d, want 1", n)
	}

	// THE MESSAGE IDS ARE THE POINT, not the count: a narrower rule can only be
	// proposed against the messages that were actually wrong.
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		list, e := tx.RuleCorrections(ctx, ruleID, 10)
		if e != nil {
			return e
		}
		if len(list) != 1 || list[0].MessageID != msgID || list[0].LabelID != labelID {
			t.Errorf("RuleCorrections = %+v, want the one message and label", list)
		}
		return nil
	}); err != nil {
		t.Fatalf("read back: %v", err)
	}
}

// Removing a label the USER applied is not a correction. There is no automation
// to blame, and recording one would accuse whichever rule happened to exist of a
// mistake nobody made.
func TestRemovingAHandAppliedLabelIsNotACorrection(t *testing.T) {
	h := newHarness(t)
	s := h.appStore()
	ctx := context.Background()
	u := seedUser(t, h.super, "corr-user-"+NewULID().String())
	userID, msgID := mustULID(t, u.ID), mustULID(t, u.MessageID)

	ruleID, labelID := seedRuleAndLabel(t, s, userID, "Mine")

	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		// Applied BY HAND — zero LabelSource — then removed by hand.
		if _, _, e := tx.ApplyEmailUpdate(ctx, msgID, EmailUpdate{AddLabelIDs: []ulid.ULID{labelID}}); e != nil {
			return e
		}
		_, _, e := tx.ApplyEmailUpdate(ctx, msgID, EmailUpdate{RemoveLabelIDs: []ulid.ULID{labelID}})
		return e
	}); err != nil {
		t.Fatalf("hand label then remove: %v", err)
	}

	// Asserted against THIS rule, not against an empty map: seedUser plants a
	// rule_corrections row of its own so the RLS table inventory has something to
	// probe, and a global emptiness check would be measuring the fixture.
	if n := corrections(t, s, userID)[ruleID]; n != 0 {
		t.Errorf("removing a hand-applied label recorded %d corrections against the rule", n)
	}
}

// One message wronged twice is ONE message. Counting events would measure how
// stubborn a rule is rather than how wrong it is.
func TestRepeatedCorrectionOfTheSameMessageCountsOnce(t *testing.T) {
	h := newHarness(t)
	s := h.appStore()
	ctx := context.Background()
	u := seedUser(t, h.super, "corr-rep-"+NewULID().String())
	userID, msgID := mustULID(t, u.ID), mustULID(t, u.MessageID)

	ruleID, labelID := seedRuleAndLabel(t, s, userID, "Promotions")

	for i := 0; i < 3; i++ {
		if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
			if _, _, e := tx.ApplyEmailUpdate(ctx, msgID, EmailUpdate{
				AddLabelIDs: []ulid.ULID{labelID},
				LabelSource: RuleLabelSource(ruleID),
			}); e != nil {
				return e
			}
			_, _, e := tx.ApplyEmailUpdate(ctx, msgID, EmailUpdate{RemoveLabelIDs: []ulid.ULID{labelID}})
			return e
		}); err != nil {
			t.Fatalf("round %d: %v", i, err)
		}
	}

	if n := corrections(t, s, userID)[ruleID]; n != 1 {
		t.Errorf("three corrections of ONE message counted as %d; want 1", n)
	}
}

// Acting on the evidence clears it: the corrections describe the rule AS IT WAS,
// and carrying them across an edit would keep accusing the new rule of the old
// one's mistakes.
func TestClearingCorrectionsForgetsThem(t *testing.T) {
	h := newHarness(t)
	s := h.appStore()
	ctx := context.Background()
	u := seedUser(t, h.super, "corr-clear-"+NewULID().String())
	userID, msgID := mustULID(t, u.ID), mustULID(t, u.MessageID)

	ruleID, labelID := seedRuleAndLabel(t, s, userID, "Promotions")
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		if _, _, e := tx.ApplyEmailUpdate(ctx, msgID, EmailUpdate{
			AddLabelIDs: []ulid.ULID{labelID},
			LabelSource: RuleLabelSource(ruleID),
		}); e != nil {
			return e
		}
		_, _, e := tx.ApplyEmailUpdate(ctx, msgID, EmailUpdate{RemoveLabelIDs: []ulid.ULID{labelID}})
		return e
	}); err != nil {
		t.Fatalf("label then correct: %v", err)
	}

	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		n, e := tx.ClearRuleCorrections(ctx, ruleID)
		if e != nil {
			return e
		}
		if n != 1 {
			t.Errorf("ClearRuleCorrections removed %d rows, want 1", n)
		}
		return nil
	}); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if n := corrections(t, s, userID)[ruleID]; n != 0 {
		t.Errorf("corrections survived clearing: %d", n)
	}
}

// A correction is a change to what a client renders for that RULE, so it owes a
// Rule change_log row — otherwise the rules list keeps showing "0 misfiled"
// until something unrelated happens to touch the rule.
func TestCorrectionAppendsARuleChangeRow(t *testing.T) {
	h := newHarness(t)
	s := h.appStore()
	ctx := context.Background()
	u := seedUser(t, h.super, "corr-chg-"+NewULID().String())
	userID, msgID := mustULID(t, u.ID), mustULID(t, u.MessageID)

	ruleID, labelID := seedRuleAndLabel(t, s, userID, "Promotions")
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		_, _, e := tx.ApplyEmailUpdate(ctx, msgID, EmailUpdate{
			AddLabelIDs: []ulid.ULID{labelID},
			LabelSource: RuleLabelSource(ruleID),
		})
		return e
	}); err != nil {
		t.Fatalf("rule labels: %v", err)
	}

	var before int64
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		var e error
		before, e = tx.ChangeState(ctx, ObjectRule)
		return e
	}); err != nil {
		t.Fatalf("read rule change state: %v", err)
	}

	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		_, _, e := tx.ApplyEmailUpdate(ctx, msgID, EmailUpdate{RemoveLabelIDs: []ulid.ULID{labelID}})
		return e
	}); err != nil {
		t.Fatalf("user removes: %v", err)
	}

	var after int64
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		var e error
		after, e = tx.ChangeState(ctx, ObjectRule)
		return e
	}); err != nil {
		t.Fatalf("read rule change state: %v", err)
	}
	if after <= before {
		t.Errorf("no Rule change row for the correction: state %d -> %d", before, after)
	}
}
