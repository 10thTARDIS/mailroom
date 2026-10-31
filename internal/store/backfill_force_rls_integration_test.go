// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// TestBackfillUnderForceRLS proves mailroom#505 end to end against a real
// PostgreSQL 18: migrations run as mailroom_owner, which — like any role
// without BYPASSRLS — is subject to FORCE ROW LEVEL SECURITY on every app
// table, and mailroom_owner holds no policy of its own. See
// db/migrations/00051_backfills_under_force_rls.sql and docs/migrations.md
// "Backfilling a FORCE-RLS table from a migration".
//
// Structure: POSITIVE CONTROL first — reproduce the defect exactly as
// 00050 shipped it (a plain UPDATE, no NO FORCE/FORCE) and require it to
// affect zero rows, so this test documents the failure it guards rather than
// merely asserting the fix works in isolation (CLAUDE.md "negative-control
// every safety or security assertion" — the same principle applied to a
// correctness defect). Then the fix sequence, in one transaction, affecting
// exactly one row. Then relforcerowsecurity is re-asserted true from
// pg_class, not assumed from "the ALTER didn't error".
import (
	"context"
	"testing"
)

func TestBackfillUnderForceRLS(t *testing.T) {
	h := newHarness(t)
	owner := h.ownerPool(t)
	ctx := context.Background()

	// seedTwoUsers's fixture inserts app.desk_preferences (user_id) with no
	// setup_completed_at column given, so it is NULL here — the same "fresh
	// row" state 00050's inline backfill exists to fix for an established
	// desk (fixtures_test.go: `INSERT INTO app.desk_preferences (user_id)
	// VALUES ($1)`).
	a, _ := seedTwoUsers(t, h.super)

	var before *string
	if err := h.super.QueryRow(ctx,
		`SELECT setup_completed_at::text FROM app.desk_preferences WHERE user_id = $1`, a.ID).Scan(&before); err != nil {
		t.Fatalf("read fixture desk_preferences: %v", err)
	}
	if before != nil {
		t.Fatalf("fixture precondition: app.desk_preferences.setup_completed_at = %q, want NULL", *before)
	}

	// POSITIVE CONTROL: the defect, reproduced exactly as 00050 shipped it —
	// a plain UPDATE as mailroom_owner, with FORCE ROW LEVEL SECURITY still
	// on and no policy granted to mailroom_owner. This must affect zero
	// rows: that silent zero is #505 itself, on the deployed instance.
	tag, err := owner.Exec(ctx,
		`UPDATE app.desk_preferences SET setup_completed_at = now() WHERE user_id = $1`, a.ID)
	if err != nil {
		t.Fatalf("plain UPDATE as mailroom_owner errored (expected: no error, zero rows affected — "+
			"that silence is the defect): %v", err)
	}
	if got := tag.RowsAffected(); got != 0 {
		t.Fatalf("plain UPDATE as mailroom_owner affected %d rows, want 0 — "+
			"the positive control did not reproduce the defect: has mailroom_owner been "+
			"granted BYPASSRLS or a policy on app.desk_preferences since #505?", got)
	}

	// THE FIX: NO FORCE / UPDATE / FORCE, in one transaction, exactly
	// 00051's shape.
	fixTx, err := owner.Begin(ctx)
	if err != nil {
		t.Fatalf("begin fix transaction: %v", err)
	}
	defer func() { _ = fixTx.Rollback(ctx) }() // no-op after Commit; the error is deliberately irrelevant (errcheck)

	if _, err := fixTx.Exec(ctx, `ALTER TABLE app.desk_preferences NO FORCE ROW LEVEL SECURITY`); err != nil {
		t.Fatalf("NO FORCE ROW LEVEL SECURITY: %v", err)
	}
	tag, err = fixTx.Exec(ctx,
		`UPDATE app.desk_preferences SET setup_completed_at = now() WHERE user_id = $1 AND setup_completed_at IS NULL`, a.ID)
	if err != nil {
		t.Fatalf("UPDATE under NO FORCE: %v", err)
	}
	if got := tag.RowsAffected(); got != 1 {
		t.Fatalf("UPDATE under NO FORCE affected %d rows, want 1", got)
	}
	if _, err := fixTx.Exec(ctx, `ALTER TABLE app.desk_preferences FORCE ROW LEVEL SECURITY`); err != nil {
		t.Fatalf("FORCE ROW LEVEL SECURITY: %v", err)
	}
	if err := fixTx.Commit(ctx); err != nil {
		t.Fatalf("commit fix transaction: %v", err)
	}

	// FORCE must be back on afterward — read from pg_class, not assumed from
	// the ALTER not having errored (CLAUDE.md "the absence of an error
	// proves nothing on its own").
	var forced bool
	if err := h.super.QueryRow(ctx,
		`SELECT relforcerowsecurity FROM pg_class WHERE oid = 'app.desk_preferences'::regclass`).Scan(&forced); err != nil {
		t.Fatalf("query relforcerowsecurity: %v", err)
	}
	if !forced {
		t.Fatal("app.desk_preferences.relforcerowsecurity = false after the fix sequence committed, want true")
	}

	// And the row now actually holds the backfilled value, visible even
	// under FORCE because we're reading as superuser.
	var after *string
	if err := h.super.QueryRow(ctx,
		`SELECT setup_completed_at::text FROM app.desk_preferences WHERE user_id = $1`, a.ID).Scan(&after); err != nil {
		t.Fatalf("read post-fix desk_preferences: %v", err)
	}
	if after == nil {
		t.Fatal("app.desk_preferences.setup_completed_at is still NULL after the fix sequence, want set")
	}
}
