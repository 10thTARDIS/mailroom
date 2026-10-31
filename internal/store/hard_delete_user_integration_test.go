// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// The destructive half of #54, tested on real rows.
//
// ⚠ THESE TESTS COMMIT. Unlike delete_policy_integration_test.go, which rolls
// everything back, a delete that is never committed cannot show that the CASCADE
// reached anything — and "the cascade reaches everything" is the property the
// whole design rests on. They delete users the test itself seeded, in a database
// that is thrown away with the container.

import (
	"context"
	"strings"
	"testing"
)

// TestHardDeleteUserRemovesEveryUserScopedRow is the property #54 depends on:
// one DELETE, and the database removes the rest.
//
// ⚠ IT ASSERTS PER TABLE, NOT ON A TOTAL. "23,645 rows became 0" is satisfied by
// a cascade that missed one table holding 12 rows if another table happened to
// shrink by 12 — and more importantly, a single total cannot say WHICH table
// survived. The census is catalog-driven, so this checks every table that exists
// rather than the ones someone remembered.
func TestHardDeleteUserRemovesEveryUserScopedRow(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, b := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID, bID := mustULID(t, a.ID), mustULID(t, b.ID)

	before, err := censusFor(ctx, s, aID)
	if err != nil {
		t.Fatalf("census before: %v", err)
	}
	if before.TotalRows == 0 {
		t.Fatal("the seeded user owns no rows, so this test cannot observe a cascade")
	}
	// The fixture asserts at least one row in every app table, so a cascade that
	// missed one is observable here.
	if len(before.Tables) < 20 {
		t.Fatalf("only %d tables in the census; the cascade check would be shallow",
			len(before.Tables))
	}

	// B is the CONTROL: an untouched user whose rows must be identical
	// afterwards. Without it, a delete that truncated everything would pass.
	bBefore, err := censusFor(ctx, s, bID)
	if err != nil {
		t.Fatalf("census B before: %v", err)
	}

	if err := s.SystemTx(ctx, func(tx *SystemTx) error {
		return tx.HardDeleteUser(ctx, aID, "test:cascade")
	}); err != nil {
		t.Fatalf("HardDeleteUser: %v", err)
	}

	// Per table, by name.
	after, err := censusFor(ctx, s, aID)
	if err != nil {
		t.Fatalf("census after: %v", err)
	}
	for _, tc := range after.Tables {
		if tc.Rows != 0 {
			t.Errorf("app.%s still holds %d row(s) for the deleted user — the cascade "+
				"did not reach it", tc.Table, tc.Rows)
		}
	}
	if after.RootRows != 0 {
		t.Errorf("the app.users row itself survives (%d)", after.RootRows)
	}

	// ⚠ THE CONTROL. One user's deletion must not touch another's.
	bAfter, err := censusFor(ctx, s, bID)
	if err != nil {
		t.Fatalf("census B after: %v", err)
	}
	if bAfter.TotalRows != bBefore.TotalRows {
		t.Errorf("the OTHER user's rows changed %d -> %d — this delete reached beyond "+
			"the user it named", bBefore.TotalRows, bAfter.TotalRows)
	}
}

// TestHardDeleteUserLeavesATombstoneThatSurvivesTheCascade.
//
// ⚠ THE TRAP THIS GUARDS IS SUBTLE AND SILENT. app.audit_log.user_id REFERENCES
// app.users ON DELETE SET NULL, so the tombstone's user_id is blanked by the very
// cascade it is recording — microseconds after it is written. The id therefore
// has to survive somewhere without a foreign key, and object_id is where. Get
// that wrong and the audit row still exists, still says "user.hard_delete", and
// no longer says WHOSE data was destroyed: the one fact it is for.
func TestHardDeleteUserLeavesATombstoneThatSurvivesTheCascade(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	aID := mustULID(t, a.ID)

	if err := h.appStore().SystemTx(ctx, func(tx *SystemTx) error {
		return tx.HardDeleteUser(ctx, aID, "test:tombstone")
	}); err != nil {
		t.Fatalf("HardDeleteUser: %v", err)
	}

	var actor, objectID, detail string
	var userIDIsNull bool
	if err := h.super.QueryRow(ctx, `
		SELECT actor, object_id, detail::text, user_id IS NULL
		  FROM app.audit_log
		 WHERE action = 'user.hard_delete' AND object_id = $1`,
		a.ID).Scan(&actor, &objectID, &detail, &userIDIsNull); err != nil {
		t.Fatalf("the tombstone is not there at all: %v", err)
	}
	if actor != "test:tombstone" {
		t.Errorf("tombstone actor = %q, want the caller's", actor)
	}
	if objectID != a.ID {
		t.Errorf("tombstone object_id = %q, want the deleted user's id", objectID)
	}
	// ⚠ The FK column is NULL BY CONSTRUCTION, not by cascade — the definer writes
	// NULL because the cascade would blank it anyway and because setting it breaks
	// the nonexistent-id path outright. Asserting it documents
	// why object_id carries the id too. If this ever comes back non-null, something
	// is writing the column again and the duplication should be revisited.
	if !userIDIsNull {
		t.Error("audit_log.user_id survived the cascade — ON DELETE SET NULL no longer " +
			"applies, so the object_id duplication may no longer be needed")
	}
	if !contains(detail, a.ID) {
		t.Errorf("tombstone detail %q does not carry the user id", detail)
	}
}

// TestHardDeleteUserRefusesToReportSuccessForAnIdThatIsNotThere.
//
// ⚠ ZERO ROWS IS THE FAILURE THIS WHOLE ISSUE IS ABOUT. It is what the missing
// policy produced for the project's whole life, and a caller handed a quiet
// "0" would reproduce it exactly. The store returns an error instead.
func TestHardDeleteUserRefusesToReportSuccessForAnIdThatIsNotThere(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	err := h.appStore().SystemTx(ctx, func(tx *SystemTx) error {
		return tx.HardDeleteUser(ctx, NewULID(), "test:ghost")
	})
	if err == nil {
		t.Fatal("deleting a nonexistent user reported success — which is precisely " +
			"what the missing policy used to do")
	}
	if !contains(err.Error(), "removed no rows") {
		t.Errorf("error %q does not say the delete removed nothing", err)
	}
}

func contains(haystack, needle string) bool {
	return strings.Contains(haystack, needle)
}
