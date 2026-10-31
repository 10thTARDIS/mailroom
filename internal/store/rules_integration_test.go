// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// Integration suite for the T0 rules-engine store layer (mailroom#32, spec §8):
// the change_log invariant (exactly one Rule row per create/update/destroy, none
// for a no-op), name-collision handling that does NOT poison the surrounding
// UserTx, validation rejection of a bad regex / unsupported action, and the
// enabled+priority ordering RulesForEval guarantees. Runs against the throwaway
// Postgres provisioned by scripts/integration-test.sh; skips when the env is
// absent (see harness_test.go).

import (
	"context"
	"errors"
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/rules"
)

func fromEquals(addr string) rules.Condition {
	return rules.Condition{Field: rules.FieldFrom, Op: rules.OpEquals, Value: addr}
}

func addLabelAction(label string) []rules.Action {
	return []rules.Action{{Type: rules.ActionAddLabel, Label: label}}
}

// The change_log invariant (CLAUDE.md): create/real-update/destroy each append
// exactly one Rule change_log row with the right op, a no-op update appends none
// and leaves ChangeState unchanged.
func TestRuleChangeLogInvariant(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)

	ruleOps := func(tx *UserTx, id ulid.ULID) (last ChangeOp, count int) {
		rows, err := tx.tx.Query(ctx, `
			SELECT op FROM app.change_log
			 WHERE user_id = $1 AND object_type = 'Rule' AND object_id = $2`,
			tx.userID.String(), id.String())
		if err != nil {
			t.Fatalf("query change_log: %v", err)
		}
		defer rows.Close()
		var ops []ChangeOp
		for rows.Next() {
			var o ChangeOp
			if err := rows.Scan(&o); err != nil {
				t.Fatalf("scan op: %v", err)
			}
			ops = append(ops, o)
		}
		if len(ops) == 0 {
			return "", 0
		}
		return ops[len(ops)-1], len(ops)
	}

	var id ulid.ULID
	err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		before, err := tx.ChangeState(ctx, ObjectRule)
		if err != nil {
			return err
		}

		// Create.
		id, err = tx.CreateRule(ctx, RuleRow{
			Name: "Invoices-" + NewULID().String(), Priority: 100, Enabled: true,
			Conditions: fromEquals("alice@example.com"), Actions: addLabelAction("Invoices"),
		})
		if err != nil {
			return err
		}
		afterCreate, err := tx.ChangeState(ctx, ObjectRule)
		if err != nil {
			return err
		}
		if afterCreate != before+1 {
			t.Errorf("state after create = %d, want %d", afterCreate, before+1)
		}
		if op, n := ruleOps(tx, id); n != 1 || op != OpCreated {
			t.Errorf("after create: %d rows, last op %q, want 1/created", n, op)
		}

		// Real update.
		newName := "Renamed-" + NewULID().String()
		found, err := tx.UpdateRule(ctx, id, RulePatch{Name: &newName})
		if err != nil {
			return err
		}
		if !found {
			t.Fatal("update found = false")
		}
		afterUpdate, err := tx.ChangeState(ctx, ObjectRule)
		if err != nil {
			return err
		}
		if afterUpdate != afterCreate+1 {
			t.Errorf("state after update = %d, want %d", afterUpdate, afterCreate+1)
		}
		if op, n := ruleOps(tx, id); n != 2 || op != OpUpdated {
			t.Errorf("after update: %d rows, last op %q, want 2/updated", n, op)
		}

		// No-op update.
		found, err = tx.UpdateRule(ctx, id, RulePatch{})
		if err != nil {
			return err
		}
		if !found {
			t.Fatal("no-op update found = false")
		}
		afterNoop, err := tx.ChangeState(ctx, ObjectRule)
		if err != nil {
			return err
		}
		if afterNoop != afterUpdate {
			t.Errorf("state after no-op = %d, want unchanged %d", afterNoop, afterUpdate)
		}
		if _, n := ruleOps(tx, id); n != 2 {
			t.Errorf("after no-op: %d rows, want still 2", n)
		}

		// Delete.
		found, err = tx.DeleteRule(ctx, id)
		if err != nil {
			return err
		}
		if !found {
			t.Fatal("delete found = false")
		}
		afterDelete, err := tx.ChangeState(ctx, ObjectRule)
		if err != nil {
			return err
		}
		if afterDelete != afterNoop+1 {
			t.Errorf("state after delete = %d, want %d", afterDelete, afterNoop+1)
		}
		if op, n := ruleOps(tx, id); n != 3 || op != OpDestroyed {
			t.Errorf("after delete: %d rows, last op %q, want 3/destroyed", n, op)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A name collision surfaces as ErrRuleNameTaken and — thanks to the SAVEPOINT in
// CreateRule — does NOT poison the surrounding UserTx: further creates in the
// same tx still succeed and the tx commits.
func TestRuleNameCollisionDoesNotPoisonTx(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)

	dup := "Dup-" + NewULID().String()
	survivor := "Survivor-" + NewULID().String()
	var survivorID ulid.ULID
	err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		if _, err := tx.CreateRule(ctx, RuleRow{
			Name: dup, Enabled: true, Conditions: fromEquals("a@x.com"), Actions: addLabelAction("L"),
		}); err != nil {
			return err
		}
		if _, err := tx.CreateRule(ctx, RuleRow{
			Name: dup, Enabled: true, Conditions: fromEquals("b@x.com"), Actions: addLabelAction("L"),
		}); !errors.Is(err, ErrRuleNameTaken) {
			t.Errorf("duplicate create err = %v, want ErrRuleNameTaken", err)
		}
		var err error
		survivorID, err = tx.CreateRule(ctx, RuleRow{
			Name: survivor, Enabled: true, Conditions: fromEquals("c@x.com"), Actions: addLabelAction("L"),
		})
		if err != nil {
			t.Errorf("create after collision failed (tx poisoned): %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// The survivor durably committed.
	err = s.UserTx(ctx, aID, func(tx *UserTx) error {
		views, err := tx.GetRules(ctx, []ulid.ULID{survivorID})
		if err != nil {
			return err
		}
		if len(views) != 1 {
			t.Errorf("survivor not committed: got %d", len(views))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A rule whose regex will not compile, and one naming an unsupported action, are
// rejected at the store boundary as ErrInvalidRule — never persisted.
func TestRuleValidationRejectsBadRuleAtStore(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)

	err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		badRegex := rules.Condition{Field: rules.FieldSubject, Op: rules.OpMatches, Value: "([unclosed"}
		if _, err := tx.CreateRule(ctx, RuleRow{
			Name: "BadRegex-" + NewULID().String(), Conditions: badRegex, Actions: addLabelAction("L"),
		}); !errors.Is(err, ErrInvalidRule) {
			t.Errorf("bad regex create err = %v, want ErrInvalidRule", err)
		}
		if _, err := tx.CreateRule(ctx, RuleRow{
			Name: "BadAction-" + NewULID().String(), Conditions: fromEquals("a@x.com"),
			Actions: []rules.Action{{Type: rules.ActionJunk}},
		}); !errors.Is(err, ErrInvalidRule) {
			t.Errorf("unsupported action create err = %v, want ErrInvalidRule", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// RulesForEval returns only ENABLED rules, ordered by priority ascending.
func TestRulesForEvalEnabledPriorityOrdered(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)

	tag := NewULID().String()
	err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		mk := func(name string, prio int, enabled bool) {
			if _, err := tx.CreateRule(ctx, RuleRow{
				Name: name + "-" + tag, Priority: prio, Enabled: enabled,
				Conditions: fromEquals("a@x.com"), Actions: addLabelAction("L"),
			}); err != nil {
				t.Fatalf("create %s: %v", name, err)
			}
		}
		mk("mid", 50, true)
		mk("low", 10, true)
		mk("high", 90, true)
		mk("disabled", 1, false) // lowest priority but disabled → excluded

		rs, err := tx.RulesForEval(ctx)
		if err != nil {
			return err
		}
		if len(rs) != 3 {
			t.Fatalf("RulesForEval returned %d rules, want 3 enabled", len(rs))
		}
		if rs[0].Priority != 10 || rs[1].Priority != 50 || rs[2].Priority != 90 {
			t.Errorf("priorities = %d,%d,%d, want 10,50,90", rs[0].Priority, rs[1].Priority, rs[2].Priority)
		}
		for _, r := range rs {
			if !r.Enabled {
				t.Errorf("RulesForEval returned a disabled rule %s", r.Name)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
