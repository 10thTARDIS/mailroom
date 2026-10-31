// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// The negative control the hard-delete `--apply` increment will be judged
// against (#54), measured ON app.users ITSELF.

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ⚠ THE ARRANGEMENT THIS FILE MEASURES DOES REPRODUCE HERE, AND AN EARLIER
// VERSION OF THIS TEST SAID IT DID NOT.
//
// The claim that had to be corrected: "schema-apply runs as postgres, so in CI
// app.users is owned by postgres and the dangerous arrangement never arises."
// That is true of scripts/schema-apply.sh — and irrelevant, because the store
// suite does not run there. scripts/integration-test.sh applies db/schema.sql
// as **mailroom_owner**, deliberately (its own header explains why: applying it
// as the superuser made CI structurally blind to every privilege property that
// depends on ownership, and shipped two defects that way, #48 and #257).
//
// Measured in this harness:
//
//	users_owner               = mailroom_owner
//	rls_forced                = t
//	delete_policies_for_owner = 0
//
// which is byte-for-byte the deployed instance's arrangement. So the earlier
// scratch-table model of app.users was not merely unnecessary, it was weaker
// than the real thing that was available all along.
//
// # Why this is safe to run against the real table
//
// Every case below runs inside ONE transaction that is ALWAYS rolled back:
// PostgreSQL is transactional for DDL too, so a CREATE POLICY / DROP POLICY
// disappears on rollback exactly as a DELETE does. Nothing here survives the
// test.
//
// ⚠ IT DOES NOT `ALTER TABLE app.users OWNER TO ...`, AND THAT IS THE POINT.
// A previous attempt did, with a cleanup that restored the owner, and broke
// four unrelated provision tests with `permission denied for table users` —
// changing a table's owner REWRITES ITS ACL, and restoring the owner does not
// restore the grants. There is nothing to alter: the table already has the
// ownership the test needs.

// asOwnerRolledBack runs fn inside a transaction as mailroom_owner and always
// rolls it back. ddl statements run FIRST, as the superuser, before the role
// switch — so a policy can be added or removed for the duration of one case.
//
// SET LOCAL ROLE, not SET ROLE: the connection goes back to a pool afterwards,
// and a session-level role change would leak into whatever ran next.
func asOwnerRolledBack(t *testing.T, super *pgxpool.Pool, ddl []string, fn func(pgx.Tx)) {
	t.Helper()
	ctx := context.Background()
	tx, err := super.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	// Rollback is the ONLY exit. No commit path exists in this helper.
	defer func() { _ = tx.Rollback(context.Background()) }()

	for _, stmt := range ddl {
		if _, err := tx.Exec(ctx, stmt); err != nil {
			t.Fatalf("case ddl (%s): %v", stmt, err)
		}
	}
	if _, err := tx.Exec(ctx, `SET LOCAL ROLE mailroom_owner`); err != nil {
		t.Fatalf("set local role: %v", err)
	}
	fn(tx)
}

// deleteUserAsOwner issues the delete a SECURITY DEFINER hard-delete would
// issue, and returns the rows it claims to have removed.
//
// ⚠ An ERROR here would be the SAFE outcome — loud and catchable. The entire
// finding is that there is no error to catch, so an error means the premise
// has changed and #54's prerequisite must be re-derived, not assumed.
func deleteUserAsOwner(t *testing.T, tx pgx.Tx, id string) int64 {
	t.Helper()
	tag, err := tx.Exec(context.Background(), `DELETE FROM app.users WHERE id = $1`, id)
	if err != nil {
		t.Fatalf("the delete raised an error (%v). That is SAFER than the silent no-op "+
			"this test exists to demonstrate, so the premise has changed and #54's "+
			"prerequisite should be re-derived rather than assumed", err)
	}
	return tag.RowsAffected()
}

// TestHardDeleteOfAUserActuallyRemovesTheRow.
//
// ⚠ THIS TEST USED TO ASSERT THE OPPOSITE, AND THE FLIP IS THE POINT. Until
// migration 00040 it was TestHardDeleteOfAUserSilentlyRemovesNothingToday: it
// asserted `DELETE FROM app.users` removed ZERO rows and reported success,
// because the owner held DELETE implicitly while FORCE ROW LEVEL SECURITY had
// no policy to admit it. Its failure message said, in as many words, that a red
// result meant the prerequisite had been met and the test should be updated
// deliberately. Migration 00040 is that, and this is the deliberate update.
//
// It now asserts the property #54's --apply depends on: the delete reaches the
// row. The sibling below still proves WHY — remove the owner's SELECT policy and
// it silently stops working again.
//
// The transaction is still always rolled back: nothing here destroys a user.
func TestHardDeleteOfAUserActuallyRemovesTheRow(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)

	asOwnerRolledBack(t, h.super, nil, func(tx pgx.Tx) {
		if n := deleteUserAsOwner(t, tx, a.ID); n != 1 {
			t.Fatalf("DELETE removed %d rows, want 1. Zero means the users_owner_delete "+
				"policy (migration 00040) is missing or was narrowed — and a hard delete "+
				"is a SILENT no-op again, with no error to catch it", n)
		}
		var still int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM app.users WHERE id = $1`, a.ID).Scan(&still); err != nil {
			t.Fatalf("recount: %v", err)
		}
		if still != 0 {
			t.Fatalf("DELETE reported 1 row affected but %d remain", still)
		}
	})
}

// TestTheDeletePolicyIsWhatMakesTheDifference is the negative control that
// survives the flip above.
//
// ⚠ WITHOUT IT, THE TEST ABOVE PROVES ONLY THAT A DELETE WORKS — not that the
// policy is what makes it work. Dropping users_owner_delete inside a rolled-back
// transaction returns the database to the pre-00040 state, where the same
// statement must silently remove nothing.
func TestTheDeletePolicyIsWhatMakesTheDifference(t *testing.T) {
	h := newHarness(t)
	a, _ := seedTwoUsers(t, h.super)

	ddl := []string{`DROP POLICY users_owner_delete ON app.users`}
	asOwnerRolledBack(t, h.super, ddl, func(tx pgx.Tx) {
		if n := deleteUserAsOwner(t, tx, a.ID); n != 0 {
			t.Fatalf("with users_owner_delete dropped the delete still removed %d rows — "+
				"so that policy is not what admits it, and the migration's rationale is "+
				"wrong", n)
		}
	})
}

// TestTheDeletePolicyIsNotSufficientWithoutTheOwnersSelectPolicy records a
// dependency that is invisible from migration 00040 alone.
//
// ⚠ A `DELETE ... WHERE` NEEDS ITS TARGET ROWS TO BE *SELECTABLE* UNDER RLS, not
// merely deletable. users_owner_delete works only because users_subject_lookup
// already grants mailroom_owner SELECT with USING (true). Narrow or drop that
// policy later — for a perfectly good-looking reason, since it exists for
// subject lookup and not for deletion — and hard delete silently stops working
// again, with no error and nothing to catch it.
//
// The policy is dropped inside the rolled-back transaction, so the real
// users_subject_lookup is untouched.
func TestTheDeletePolicyIsNotSufficientWithoutTheOwnersSelectPolicy(t *testing.T) {
	h := newHarness(t)
	a, _ := seedTwoUsers(t, h.super)

	ddl := []string{`DROP POLICY users_subject_lookup ON app.users`}
	asOwnerRolledBack(t, h.super, ddl, func(tx pgx.Tx) {
		if n := deleteUserAsOwner(t, tx, a.ID); n != 0 {
			t.Fatalf("with users_owner_delete present but NO owner SELECT policy the "+
				"delete removed %d rows, want 0. If a delete no longer needs SELECT "+
				"visibility, the dependency recorded in migration 00040 is gone and the "+
				"pairing can be simplified — verify before relying on it", n)
		}
	})
}
