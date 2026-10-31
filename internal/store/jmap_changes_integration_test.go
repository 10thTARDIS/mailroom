// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// Integration coverage for Changes() itself (mailroom-data-model.md §3.2,
// mailroom#526 Part 1). jmap_changes_test.go already exhausts the pure
// collapseChanges bucketing with no database; what only a real Postgres can
// prove is the floor boundary and the one-statement shape.

import (
	"context"
	"errors"
	"testing"
)

// TestChangesAtFloorSucceedsBelowFloorFails pins §3.2 step 1 exactly at the
// boundary: a cursor AT the floor is one the floor promises is still
// satisfiable (compaction's floor_seq is the highest DELETED seq, not
// highest-deleted+1 — see changelog_compact.go), so it must succeed; one seq
// below it is exactly what the floor exists to reject.
func TestChangesAtFloorSucceedsBelowFloorFails(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)

	// The fixture leaves (A, Email) floor at 0, last_seq at 2, with two log rows
	// (seq 1, 2). Raise the floor to 1 directly by SQL: a floor is only ever
	// moved by compaction (CompactChangeLog), never by Changes() itself, so
	// setting it up this way is the fixture's job, not a call through the store
	// under test.
	if _, err := h.super.Exec(ctx,
		`UPDATE app.change_floor SET floor_seq = 1 WHERE user_id = $1 AND object_type = 'Email'`,
		a.ID); err != nil {
		t.Fatalf("raise floor: %v", err)
	}

	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		// AT the floor: succeeds. Only seq 2 (> 1) is in the delta.
		page, err := tx.Changes(ctx, ObjectEmail, 1, 0)
		if err != nil {
			t.Fatalf("Changes(since=floor=1) = %v, want success", err)
		}
		if page.OldState != 1 || page.NewState != 2 {
			t.Errorf("Changes(1) = %+v, want OldState=1 NewState=2", page)
		}

		// BELOW the floor: rejected.
		if _, err := tx.Changes(ctx, ObjectEmail, 0, 0); !errors.Is(err, ErrCannotCalculateChanges) {
			t.Errorf("Changes(since=0, floor=1) = %v, want ErrCannotCalculateChanges", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// ⚠ WHY THERE IS NO CONCURRENCY TEST HERE, AND WHY THAT IS THE POINT
// (mailroom#526 Part 1, test item 7).
//
// The race this rewrite closes is: a compaction committing BETWEEN the old
// floor-read statement and the old change_log-scan statement, so the scan runs
// against a floor that is already stale. Proving that race is gone by actually
// racing it would mean pausing Changes() between two statements and landing a
// second connection's commit in the gap — but after this change there IS no
// gap: tx.tx.Query issues exactly one round trip for the whole floor+state+scan
// read, with no point between "the query is sent" and "the query returns" that
// application code ever observes or could inject a pause into. There is no
// query-count or statement-tracing hook in this codebase's pgx wiring
// (grepped: no QueryTracer/pgx.Tracer implementation exists) to instrument a
// black-box "exactly one statement" assertion either.
//
// So the guard here is structural, not a passing test that could go stale
// silently: Changes() has exactly one `tx.tx.Query` call in its body (see the
// function above), full stop. TestChangesAtFloorSucceedsBelowFloorFails is the
// behavioural half — it fails if that one query's floor/state logic ever
// regresses — and the doc comment on Changes() is the other half, aimed at the
// next person tempted to "simplify" it back into three round trips. Writing a
// concurrency test that could not actually observe the old race (because
// nothing here can pause mid-statement) would be exactly the kind of fabricated
// evidence CLAUDE.md's measurement discipline warns against, so this comment
// is offered instead, honestly, as what it is.
