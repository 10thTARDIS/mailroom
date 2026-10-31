// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// Integration suite for the canonical Label layer (mailroom#21, spec §6,
// data-model §5): keyword-atom assignment and its collision suffixing against
// the real UNIQUE (user_id, keyword_atom) constraint, rename stability of the
// atom, the mirror_to_imap flip-on/flip-off never-orphan rule, the change_log
// invariant (CLAUDE.md — exactly one row per create/update/destroy, none for a
// no-op), name-collision handling, delete cascade/detach, RLS isolation, and
// the mailbox keyword-acceptance probe. Runs against the throwaway Postgres
// provisioned by scripts/integration-test.sh; skips when the env is absent
// (see harness_test.go).

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/oklog/ulid/v2"
)

// findLabel returns the label with id from views, or fails.
func findLabel(t *testing.T, views []LabelView, id ulid.ULID) LabelView {
	t.Helper()
	for _, v := range views {
		if v.ID == id {
			return v
		}
	}
	t.Fatalf("label %s not found among %d views", id, len(views))
	return LabelView{}
}

// labelKeywordMapRow reads the persisted mapping row directly, bypassing the
// Go projection — the point is to prove real DB state, not just trust the
// return value.
func labelKeywordMapRow(t *testing.T, tx *UserTx, labelID ulid.ULID) (atom string, ok bool) {
	t.Helper()
	err := tx.tx.QueryRow(context.Background(), `
		SELECT keyword_atom FROM app.label_keyword_map
		 WHERE user_id = $1 AND label_id = $2`,
		tx.userID.String(), labelID.String()).Scan(&atom)
	if err != nil {
		if isNoRows(err) {
			return "", false
		}
		t.Fatalf("query label_keyword_map: %v", err)
	}
	return atom, true
}

// CreateLabel with MirrorToIMAP: true assigns a stable MailroomLabel-<slug>
// atom and persists the mapping row; MirrorToIMAP: false leaves the label
// entirely unmapped (spec §6).
func TestCreateLabelKeywordAtom(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)

	var mirroredID, localOnlyID ulid.ULID
	err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		var err error
		mirroredID, err = tx.CreateLabel(ctx, LabelRow{Name: "Receipts-" + NewULID().String(), MirrorToIMAP: true})
		if err != nil {
			return err
		}
		localOnlyID, err = tx.CreateLabel(ctx, LabelRow{Name: "Scratch-" + NewULID().String(), MirrorToIMAP: false})
		if err != nil {
			return err
		}

		views, err := tx.GetLabels(ctx, []ulid.ULID{mirroredID, localOnlyID})
		if err != nil {
			return err
		}
		mirrored := findLabel(t, views, mirroredID)
		local := findLabel(t, views, localOnlyID)

		if mirrored.KeywordAtom == "" || !hasPrefix(mirrored.KeywordAtom, keywordAtomPrefix) {
			t.Errorf("mirrored label atom = %q, want %sreceipts-... ", mirrored.KeywordAtom, keywordAtomPrefix)
		}
		if local.KeywordAtom != "" {
			t.Errorf("local-only label atom = %q, want empty", local.KeywordAtom)
		}

		if atom, ok := labelKeywordMapRow(t, tx, mirroredID); !ok || atom != mirrored.KeywordAtom {
			t.Errorf("label_keyword_map row for mirrored label: atom=%q ok=%v, want %q/true", atom, ok, mirrored.KeywordAtom)
		}
		if _, ok := labelKeywordMapRow(t, tx, localOnlyID); ok {
			t.Error("label_keyword_map row exists for a MirrorToIMAP:false label, want none")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// hasPrefix avoids importing strings twice for a one-line check the tests
// use repeatedly.
func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

// Two label names that slug to the same base collide on the real UNIQUE
// (user_id, keyword_atom) constraint; KeywordAtomForLabel resolves it under a
// SAVEPOINT so the collision does not poison the surrounding UserTx — the
// transaction keeps working and commits.
func TestCreateLabelAtomCollisionSuffixing(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)

	var first, second, third, tailID ulid.ULID
	err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		var err error
		if first, err = tx.CreateLabel(ctx, LabelRow{Name: "Invoices", MirrorToIMAP: true}); err != nil {
			return err
		}
		if second, err = tx.CreateLabel(ctx, LabelRow{Name: "invoices!", MirrorToIMAP: true}); err != nil {
			return err
		}
		if third, err = tx.CreateLabel(ctx, LabelRow{Name: "invoices???", MirrorToIMAP: true}); err != nil {
			return err
		}

		atom1, ok1 := labelKeywordMapRow(t, tx, first)
		atom2, ok2 := labelKeywordMapRow(t, tx, second)
		atom3, ok3 := labelKeywordMapRow(t, tx, third)
		if !ok1 || !ok2 || !ok3 {
			t.Fatalf("expected all three labels mapped, got ok=%v/%v/%v", ok1, ok2, ok3)
		}
		if atom1 != "MailroomLabel-invoices" {
			t.Errorf("first atom = %q, want MailroomLabel-invoices", atom1)
		}
		if atom2 != "MailroomLabel-invoices-2" {
			t.Errorf("second atom = %q, want MailroomLabel-invoices-2", atom2)
		}
		if atom3 != "MailroomLabel-invoices-3" {
			t.Errorf("third atom = %q, want MailroomLabel-invoices-3", atom3)
		}

		// The colliding creates must not have poisoned the transaction: prove
		// it by doing more real work in the SAME UserTx and letting it commit.
		tailID, err = tx.CreateLabel(ctx, LabelRow{Name: "Unrelated-" + NewULID().String(), MirrorToIMAP: false})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	// Commit really happened: a fresh UserTx sees all four labels.
	err = s.UserTx(ctx, aID, func(tx *UserTx) error {
		views, err := tx.GetLabels(ctx, []ulid.ULID{first, second, third, tailID})
		if err != nil {
			return err
		}
		if len(views) != 4 {
			t.Errorf("got %d labels after commit, want 4", len(views))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Renaming a label changes only its display name: the keyword atom is
// persisted on first assignment and never re-derived (spec §6, the core
// invariant), so it must be byte-identical after a rename to a totally
// different name, and ListLabels must still report the label under its new
// name with the same atom.
func TestRenameLabelPreservesAtom(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)

	var id ulid.ULID
	var atomBefore string
	err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		var err error
		if id, err = tx.CreateLabel(ctx, LabelRow{Name: "Old Name", MirrorToIMAP: true}); err != nil {
			return err
		}
		atomBefore, _ = labelKeywordMapRow(t, tx, id)
		if atomBefore == "" {
			t.Fatal("expected an atom to exist before rename")
		}

		found, err := tx.RenameLabel(ctx, id, "Completely Different Name")
		if err != nil {
			return err
		}
		if !found {
			t.Fatal("RenameLabel found = false, want true")
		}

		atomAfter, ok := labelKeywordMapRow(t, tx, id)
		if !ok {
			t.Fatal("label_keyword_map row disappeared after rename")
		}
		if atomAfter != atomBefore {
			t.Errorf("atom after rename = %q, want unchanged %q", atomAfter, atomBefore)
		}

		views, err := tx.ListLabels(ctx)
		if err != nil {
			return err
		}
		lv := findLabel(t, views, id)
		if lv.Name != "Completely Different Name" {
			t.Errorf("ListLabels name = %q, want the new name", lv.Name)
		}
		if lv.KeywordAtom != atomBefore {
			t.Errorf("ListLabels atom = %q, want unchanged %q", lv.KeywordAtom, atomBefore)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Flipping mirror_to_imap on assigns a fresh atom for a label that had none;
// flipping it back off must NOT delete the existing mapping (never orphan
// it — spec §6).
func TestUpdateLabelMirrorToggleNeverOrphansAtom(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)

	trueVal, falseVal := true, false
	var id ulid.ULID
	err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		var err error
		if id, err = tx.CreateLabel(ctx, LabelRow{Name: "Toggle-" + NewULID().String(), MirrorToIMAP: false}); err != nil {
			return err
		}
		if _, ok := labelKeywordMapRow(t, tx, id); ok {
			t.Fatal("expected no atom immediately after create with MirrorToIMAP:false")
		}

		found, err := tx.UpdateLabel(ctx, id, LabelPatch{MirrorToIMAP: &trueVal})
		if err != nil {
			return err
		}
		if !found {
			t.Fatal("UpdateLabel(flip on) found = false, want true")
		}
		atom, ok := labelKeywordMapRow(t, tx, id)
		if !ok || atom == "" {
			t.Fatalf("expected an atom after flipping mirror_to_imap on, got ok=%v atom=%q", ok, atom)
		}

		found, err = tx.UpdateLabel(ctx, id, LabelPatch{MirrorToIMAP: &falseVal})
		if err != nil {
			return err
		}
		if !found {
			t.Fatal("UpdateLabel(flip off) found = false, want true")
		}
		atomAfter, ok := labelKeywordMapRow(t, tx, id)
		if !ok {
			t.Fatal("label_keyword_map row was deleted when mirror_to_imap flipped off — must never orphan the atom")
		}
		if atomAfter != atom {
			t.Errorf("atom after flip-off = %q, want unchanged %q", atomAfter, atom)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// The change_log invariant (CLAUDE.md): every create/real-update/destroy
// appends exactly one Label change_log row with the right op, and a no-op
// UpdateLabel appends none and leaves ChangeState unchanged.
func TestLabelChangeLogInvariant(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)

	changeOp := func(tx *UserTx, id ulid.ULID) (op ChangeOp, count int) {
		rows, err := tx.tx.Query(ctx, `
			SELECT op FROM app.change_log
			 WHERE user_id = $1 AND object_type = 'Label' AND object_id = $2`,
			tx.userID.String(), id.String())
		if err != nil {
			t.Fatalf("query change_log: %v", err)
		}
		defer rows.Close()
		var ops []ChangeOp
		for rows.Next() {
			var o ChangeOp
			if err := rows.Scan(&o); err != nil {
				t.Fatalf("scan change_log op: %v", err)
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
		stateBefore, err := tx.ChangeState(ctx, ObjectLabel)
		if err != nil {
			return err
		}

		// Create: state +1, exactly one row, op created.
		id, err = tx.CreateLabel(ctx, LabelRow{Name: "Invariant-" + NewULID().String(), MirrorToIMAP: false})
		if err != nil {
			return err
		}
		stateAfterCreate, err := tx.ChangeState(ctx, ObjectLabel)
		if err != nil {
			return err
		}
		if stateAfterCreate != stateBefore+1 {
			t.Errorf("state after create = %d, want %d", stateAfterCreate, stateBefore+1)
		}
		op, n := changeOp(tx, id)
		if n != 1 || op != OpCreated {
			t.Errorf("after create: %d change_log rows, last op %q, want 1/created", n, op)
		}

		// Real update: state +1, a second row, op updated.
		newColor := "#ff0000"
		found, err := tx.UpdateLabel(ctx, id, LabelPatch{SetColor: true, Color: newColor})
		if err != nil {
			return err
		}
		if !found {
			t.Fatal("real update found = false, want true")
		}
		stateAfterUpdate, err := tx.ChangeState(ctx, ObjectLabel)
		if err != nil {
			return err
		}
		if stateAfterUpdate != stateAfterCreate+1 {
			t.Errorf("state after real update = %d, want %d", stateAfterUpdate, stateAfterCreate+1)
		}
		op, n = changeOp(tx, id)
		if n != 2 || op != OpUpdated {
			t.Errorf("after real update: %d change_log rows, last op %q, want 2/updated", n, op)
		}

		// No-op update: no state change, no new row.
		found, err = tx.UpdateLabel(ctx, id, LabelPatch{})
		if err != nil {
			return err
		}
		if !found {
			t.Fatal("no-op update found = false, want true (label still exists)")
		}
		stateAfterNoop, err := tx.ChangeState(ctx, ObjectLabel)
		if err != nil {
			return err
		}
		if stateAfterNoop != stateAfterUpdate {
			t.Errorf("state after no-op update = %d, want unchanged %d", stateAfterNoop, stateAfterUpdate)
		}
		_, n = changeOp(tx, id)
		if n != 2 {
			t.Errorf("after no-op update: %d change_log rows, want still 2 (no new row)", n)
		}

		// Delete: state +1, a third row, op destroyed.
		found, err = tx.DeleteLabel(ctx, id)
		if err != nil {
			return err
		}
		if !found {
			t.Fatal("delete found = false, want true")
		}
		stateAfterDelete, err := tx.ChangeState(ctx, ObjectLabel)
		if err != nil {
			return err
		}
		if stateAfterDelete != stateAfterNoop+1 {
			t.Errorf("state after delete = %d, want %d", stateAfterDelete, stateAfterNoop+1)
		}
		op, n = changeOp(tx, id)
		if n != 3 || op != OpDestroyed {
			t.Errorf("after delete: %d change_log rows, last op %q, want 3/destroyed", n, op)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Name collisions against the UNIQUE (user_id, name) constraint surface as
// ErrLabelNameTaken on both create and rename.
//
// NOTE: unlike KeywordAtomForLabel, CreateLabel/UpdateLabel do not wrap their
// INSERT/UPDATE in a SAVEPOINT before checking isUniqueViolation, so a name
// collision leaves the surrounding Postgres transaction aborted (25P02):
// any further statement in the SAME UserTx fails, and even doing nothing
// else and letting UserTx commit fails ("commit unexpectedly resulted in
// rollback"). That looks like a genuine bug in labels.go (reported, not
// fixed here) — each collision below is therefore propagated straight back
// out of its own UserTx (a clean ROLLBACK, not a doomed COMMIT) so this test
// can assert the documented sentinel without tripping over it.
func TestLabelNameCollision(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)

	takenName := "Taken-" + NewULID().String()
	err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		_, err := tx.CreateLabel(ctx, LabelRow{Name: takenName})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	err = s.UserTx(ctx, aID, func(tx *UserTx) error {
		_, err := tx.CreateLabel(ctx, LabelRow{Name: takenName})
		return err
	})
	if !errors.Is(err, ErrLabelNameTaken) {
		t.Errorf("create collision err = %v, want ErrLabelNameTaken", err)
	}

	var otherID ulid.ULID
	err = s.UserTx(ctx, aID, func(tx *UserTx) error {
		var err error
		otherID, err = tx.CreateLabel(ctx, LabelRow{Name: "Other-" + NewULID().String()})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	var found bool
	err = s.UserTx(ctx, aID, func(tx *UserTx) error {
		var err error
		found, err = tx.RenameLabel(ctx, otherID, takenName)
		return err
	})
	if !errors.Is(err, ErrLabelNameTaken) {
		t.Errorf("rename collision err = %v, want ErrLabelNameTaken", err)
	}
	// found is true: the row exists (we located and locked it) even though
	// the write itself failed on the unique constraint.
	if !found {
		t.Errorf("rename collision found = %v, want true", found)
	}
}

// DeleteLabel detaches the label from every message and cascades the
// keyword mapping via FK, appending exactly one destroyed change_log row.
func TestDeleteLabelDetachesAndCascades(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	msgID := mustULID(t, a.MessageID)

	var id ulid.ULID
	err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		var err error
		if id, err = tx.CreateLabel(ctx, LabelRow{Name: "ToDelete-" + NewULID().String(), MirrorToIMAP: true}); err != nil {
			return err
		}
		if _, err := tx.tx.Exec(ctx, `
			INSERT INTO app.message_labels (user_id, message_id, label_id) VALUES ($1, $2, $3)`,
			tx.userID.String(), msgID.String(), id.String()); err != nil {
			return err
		}

		found, err := tx.DeleteLabel(ctx, id)
		if err != nil {
			return err
		}
		if !found {
			t.Fatal("DeleteLabel found = false, want true")
		}

		var labelCount int
		if err := tx.tx.QueryRow(ctx,
			`SELECT count(*) FROM app.labels WHERE user_id = $1 AND id = $2`,
			tx.userID.String(), id.String()).Scan(&labelCount); err != nil {
			return err
		}
		if labelCount != 0 {
			t.Errorf("app.labels rows for deleted label = %d, want 0", labelCount)
		}

		var messageLabelCount int
		if err := tx.tx.QueryRow(ctx,
			`SELECT count(*) FROM app.message_labels WHERE user_id = $1 AND label_id = $2`,
			tx.userID.String(), id.String()).Scan(&messageLabelCount); err != nil {
			return err
		}
		if messageLabelCount != 0 {
			t.Errorf("app.message_labels rows for deleted label = %d, want 0", messageLabelCount)
		}

		var mapCount int
		if err := tx.tx.QueryRow(ctx,
			`SELECT count(*) FROM app.label_keyword_map WHERE user_id = $1 AND label_id = $2`,
			tx.userID.String(), id.String()).Scan(&mapCount); err != nil {
			return err
		}
		if mapCount != 0 {
			t.Errorf("app.label_keyword_map rows for deleted label = %d, want 0 (FK cascade)", mapCount)
		}

		var destroyedCount int
		if err := tx.tx.QueryRow(ctx, `
			SELECT count(*) FROM app.change_log
			 WHERE user_id = $1 AND object_type = 'Label' AND object_id = $2 AND op = 'destroyed'`,
			tx.userID.String(), id.String()).Scan(&destroyedCount); err != nil {
			return err
		}
		if destroyedCount != 1 {
			t.Errorf("destroyed change_log rows = %d, want exactly 1", destroyedCount)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// RLS isolation: user B cannot see, update, or delete user A's label —
// another user's id is indistinguishable from a missing one (found == false,
// not an error).
func TestLabelRLSIsolation(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, b := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID, bID := mustULID(t, a.ID), mustULID(t, b.ID)

	var id ulid.ULID
	err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		var err error
		id, err = tx.CreateLabel(ctx, LabelRow{Name: "Private-" + NewULID().String(), MirrorToIMAP: true})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	err = s.UserTx(ctx, bID, func(tx *UserTx) error {
		views, err := tx.GetLabels(ctx, []ulid.ULID{id})
		if err != nil {
			return err
		}
		if len(views) != 0 {
			t.Errorf("B's GetLabels(A's id) returned %d views, want 0", len(views))
		}

		all, err := tx.ListLabels(ctx)
		if err != nil {
			return err
		}
		for _, v := range all {
			if v.ID == id {
				t.Error("B's ListLabels includes A's label id")
			}
		}

		newName := "Hijacked"
		found, err := tx.UpdateLabel(ctx, id, LabelPatch{Name: &newName})
		if err != nil {
			t.Errorf("B's UpdateLabel(A's id) err = %v, want nil", err)
		}
		if found {
			t.Error("B's UpdateLabel(A's id) found = true, want false")
		}

		found, err = tx.DeleteLabel(ctx, id)
		if err != nil {
			t.Errorf("B's DeleteLabel(A's id) err = %v, want nil", err)
		}
		if found {
			t.Error("B's DeleteLabel(A's id) found = true, want false")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// A's label is untouched by B's attempts.
	err = s.UserTx(ctx, aID, func(tx *UserTx) error {
		views, err := tx.GetLabels(ctx, []ulid.ULID{id})
		if err != nil {
			return err
		}
		lv := findLabel(t, views, id)
		if lv.Name != "" && lv.Name == "Hijacked" {
			t.Error("B's isolated UpdateLabel somehow renamed A's label")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// MailboxAcceptsKeywords reads the recorded PERMANENTFLAGS probe (default
// false at seed time), reflects a later flip to true, and reports found ==
// false for a mailbox id that is not the caller's — including another user's
// real mailbox id.
func TestMailboxAcceptsKeywords(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, b := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID, bID := mustULID(t, a.ID), mustULID(t, b.ID)

	var aMailboxID ulid.ULID
	err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		var idStr string
		if err := tx.tx.QueryRow(ctx,
			`SELECT id FROM app.mailboxes WHERE user_id = $1 LIMIT 1`, tx.userID.String()).Scan(&idStr); err != nil {
			return err
		}
		aMailboxID = mustULID(t, idStr)

		accepts, found, err := tx.MailboxAcceptsKeywords(ctx, aMailboxID)
		if err != nil {
			return err
		}
		if !found {
			t.Fatal("MailboxAcceptsKeywords(own mailbox) found = false, want true")
		}
		if accepts {
			t.Errorf("accepts_keywords = true at seed time, want false default")
		}

		if _, err := tx.tx.Exec(ctx,
			`UPDATE app.mailboxes SET accepts_keywords = true WHERE user_id = $1 AND id = $2`,
			tx.userID.String(), aMailboxID.String()); err != nil {
			return err
		}
		accepts, found, err = tx.MailboxAcceptsKeywords(ctx, aMailboxID)
		if err != nil {
			return err
		}
		if !found || !accepts {
			t.Errorf("after flip: found=%v accepts=%v, want true/true", found, accepts)
		}

		_, found, err = tx.MailboxAcceptsKeywords(ctx, NewULID())
		if err != nil {
			return err
		}
		if found {
			t.Error("MailboxAcceptsKeywords(random id) found = true, want false")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	var bMailboxIDStr string
	if err := h.super.QueryRow(ctx,
		`SELECT id FROM app.mailboxes WHERE user_id = $1 LIMIT 1`, b.ID).Scan(&bMailboxIDStr); err != nil {
		t.Fatal(err)
	}
	bMailboxID := mustULID(t, bMailboxIDStr)

	err = s.UserTx(ctx, aID, func(tx *UserTx) error {
		_, found, err := tx.MailboxAcceptsKeywords(ctx, bMailboxID)
		if err != nil {
			return err
		}
		if found {
			t.Error("MailboxAcceptsKeywords(B's mailbox id, from A) found = true, want false")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = bID
}

// A name collision must be a RECOVERABLE, per-object outcome, not a transaction
// killer. Label/set reports a duplicate name in notCreated and keeps processing
// the rest of the batch in the SAME UserTx, so the failed INSERT must not abort
// that transaction (SQLSTATE 25P02) — the SAVEPOINT in CreateLabel/UpdateLabel
// is what makes this hold. Without it, one duplicate name silently discards
// every other create in the call and the commit itself fails.
func TestLabelNameCollisionDoesNotPoisonTx(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)

	dup := "Dup-" + NewULID().String()
	survivor := "Survivor-" + NewULID().String()
	renamed := "Renamed-" + NewULID().String()

	var survivorID, firstID ulid.ULID
	err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		var err error
		if firstID, err = tx.CreateLabel(ctx, LabelRow{Name: dup, MirrorToIMAP: true}); err != nil {
			return err
		}

		// The colliding create: recoverable, and the tx must stay usable.
		if _, err = tx.CreateLabel(ctx, LabelRow{Name: dup}); !errors.Is(err, ErrLabelNameTaken) {
			return fmt.Errorf("duplicate create err = %v, want ErrLabelNameTaken", err)
		}

		// A colliding RENAME must be recoverable in the same way.
		if _, err = tx.RenameLabel(ctx, firstID, dup); err != nil {
			return fmt.Errorf("self-rename to same name should be a no-op, got %v", err)
		}

		// The tx survived: further work in it still succeeds...
		if survivorID, err = tx.CreateLabel(ctx, LabelRow{Name: survivor, MirrorToIMAP: true}); err != nil {
			return fmt.Errorf("create after collision failed (tx was poisoned): %w", err)
		}
		if _, err = tx.UpdateLabel(ctx, survivorID, LabelPatch{Name: &renamed}); err != nil {
			return fmt.Errorf("update after collision failed (tx was poisoned): %w", err)
		}
		return nil
	})
	// ...and the COMMIT succeeds (a poisoned tx fails here even if nothing else did).
	if err != nil {
		t.Fatal(err)
	}

	// Both the pre-collision and post-collision labels are durably committed.
	err = s.UserTx(ctx, aID, func(tx *UserTx) error {
		views, err := tx.ListLabels(ctx)
		if err != nil {
			return err
		}
		if got := findLabel(t, views, firstID).Name; got != dup {
			t.Errorf("first label name = %q, want %q", got, dup)
		}
		if got := findLabel(t, views, survivorID).Name; got != renamed {
			t.Errorf("survivor label name = %q, want %q", got, renamed)
		}
		if atom, ok := labelKeywordMapRow(t, tx, survivorID); !ok || atom == "" {
			t.Errorf("survivor keyword atom = %q, ok = %v; want a persisted atom", atom, ok)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// ⚠ WHAT THE EXISTING DeleteLabel TEST COULD NOT SEE (#351 B4).
//
// TestDeleteLabelDetachesAndCascades asserts the rows are gone, and it passed
// throughout the entire life of this defect — because rows being gone was never
// the problem. The problem was what the deletion did NOT do.
//
// A label's keyword atom IS an app.message_labels row (jmap_email.go derives
// $-less keywords from them), so detaching one mutates JMAP-visible Email state.
// The old DeleteLabel issued a bare DELETE and appended a single Label/destroyed
// row. So:
//
//   - Email/changes reported NOTHING, and a conforming third-party JMAP client
//     kept rendering the keyword on every cached message indefinitely. Label is
//     a Mailroom extension type, so such a client got no signal at all.
//   - No writeback intent was enqueued, so the mirrored MailroomLabel-<slug>
//     atom stayed on the IMAP server forever.
//
// Both assertions below fail against the old implementation. Neither is
// observable from the tables the older test inspects, which is the whole reason
// this one exists rather than an extra line there.
func TestDeleteLabelAppendsAnEmailChangeRowAndEnqueuesWriteback(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	msgID := mustULID(t, a.MessageID)

	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		id, err := tx.CreateLabel(ctx, LabelRow{
			Name: "Detach-" + NewULID().String(), MirrorToIMAP: true})
		if err != nil {
			return err
		}
		if _, err := tx.tx.Exec(ctx, `
			INSERT INTO app.message_labels (user_id, message_id, label_id) VALUES ($1, $2, $3)`,
			tx.userID.String(), msgID.String(), id.String()); err != nil {
			return err
		}

		// Baselines taken AFTER the attach, so neither number includes setup.
		emailBefore, err := tx.ChangeState(ctx, ObjectEmail)
		if err != nil {
			return err
		}
		var intentsBefore int
		if err := tx.tx.QueryRow(ctx,
			`SELECT count(*) FROM app.writeback_intents WHERE user_id = $1`,
			tx.userID.String()).Scan(&intentsBefore); err != nil {
			return err
		}

		if _, err := tx.DeleteLabel(ctx, id); err != nil {
			return err
		}

		// (1) The message must be visible as CHANGED. Without this, /changes says
		// nothing happened and every cached client keeps showing the keyword.
		emailAfter, err := tx.ChangeState(ctx, ObjectEmail)
		if err != nil {
			return err
		}
		if emailAfter <= emailBefore {
			t.Errorf("Email change state did not advance (%d -> %d): deleting a label "+
				"mutated the message's keywords with no Email change row, so /changes "+
				"reports nothing and clients keep rendering the keyword",
				emailBefore, emailAfter)
		}

		// (2) And the mirrored IMAP atom must be scheduled for removal, or it
		// outlives the label on the server forever.
		var intentsAfter int
		if err := tx.tx.QueryRow(ctx,
			`SELECT count(*) FROM app.writeback_intents WHERE user_id = $1`,
			tx.userID.String()).Scan(&intentsAfter); err != nil {
			return err
		}
		if intentsAfter <= intentsBefore {
			t.Errorf("writeback intents did not increase (%d -> %d): the mirrored "+
				"MailroomLabel atom is never removed from the IMAP server",
				intentsBefore, intentsAfter)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// Deleting a label is the user disposing of the label, NOT the user disagreeing
// with a rule about each message it touched. Routing the detach through the
// correction path would manufacture one rule correction per message and make a
// perfectly good rule look catastrophic — the case RemovalIsUndo exists for.
//
// This is the control on the fix above: it proves the fan-out reuses
// ApplyEmailUpdate WITHOUT also inheriting the behaviour that would be wrong here.
func TestDeleteLabelDoesNotManufactureRuleCorrections(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	msgID := mustULID(t, a.MessageID)

	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		id, err := tx.CreateLabel(ctx, LabelRow{
			Name: "RuleApplied-" + NewULID().String(), MirrorToIMAP: true})
		if err != nil {
			return err
		}
		ruleID, err := tx.CreateRule(ctx, RuleRow{
			Name: "R-" + NewULID().String(), Priority: 100, Enabled: true,
			Conditions: fromEquals("someone@example.com"), Actions: addLabelAction("Whatever"),
		})
		if err != nil {
			return err
		}
		// Attached BY THE RULE — the only shape that can produce a correction.
		if _, err := tx.tx.Exec(ctx, `
			INSERT INTO app.message_labels (user_id, message_id, label_id, applied_by, applied_by_rule_id)
			VALUES ($1, $2, $3, $4, $5)`,
			tx.userID.String(), msgID.String(), id.String(), LabelByRule, ruleID.String()); err != nil {
			return err
		}

		var before int
		if err := tx.tx.QueryRow(ctx,
			`SELECT count(*) FROM app.rule_corrections WHERE user_id = $1`,
			tx.userID.String()).Scan(&before); err != nil {
			return err
		}
		if _, err := tx.DeleteLabel(ctx, id); err != nil {
			return err
		}
		var after int
		if err := tx.tx.QueryRow(ctx,
			`SELECT count(*) FROM app.rule_corrections WHERE user_id = $1`,
			tx.userID.String()).Scan(&after); err != nil {
			return err
		}
		if after != before {
			t.Errorf("rule corrections %d -> %d: deleting a label recorded a correction "+
				"against the rule that applied it; disposing of a label is not "+
				"disagreeing with the rule about each message", before, after)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
