// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

import (
	"context"
	"testing"
)

// ⚠ THIS IS THE TEST THAT JUSTIFIES A TENTH SECURITY DEFINER (#524).
//
// A new cross-user surface has to earn itself. The argument for
// app.count_due_jobs is that the obvious alternative — counting app.jobs
// directly in the SystemTx the dispatcher already runs in — does not merely
// fail, it returns a PLAUSIBLE WRONG NUMBER. In a SystemTx
// app.current_user_id() is NULL, and the app.jobs policy is
//
//	USING (user_id = app.current_user_id() OR
//	       (user_id IS NULL AND app.current_user_id() IS NULL))
//
// so it matches SYSTEM rows only. Every per-user job — purge_trash, ml_train,
// snooze_wake, submit — is silently invisible, and a gauge fed from that count
// would under-report backlog without ever erroring.
//
// So the two halves below are both required. The definer seeing all three jobs
// proves the mechanism works; the raw count seeing only ONE proves the definer
// was necessary rather than ceremonial. Without the second half this test would
// pass just as happily against a plain count, and the extra surface would be
// unjustified.
func TestCountDueJobsSeesUserJobsThatARawCountCannot(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, b := seedTwoUsers(t, h.super)
	s := h.appStore()

	// ⚠ THIS TEST MUST LEAVE NO ROWS BEHIND, and the reason is specific rather
	// than tidiness. It inserts a SYSTEM job (user_id NULL), which a SystemTx can
	// legitimately see by policy — and the isolation suite asserts that a
	// SystemTx sees ZERO rows of app.jobs directly (store_integration_test.go).
	// Leaving one behind fails that invariant in every test that runs afterwards,
	// while this test and that one both pass when run alone. Caught exactly that
	// way: green in a filtered run, two failures in the full suite.
	t.Cleanup(func() {
		if _, err := h.super.Exec(context.Background(),
			`DELETE FROM app.jobs WHERE idempotency_key LIKE 'count-%'`); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})

	// Deltas, not absolutes: the harness database is shared with whatever else
	// this package seeded, so an absolute count would be asserting about rows
	// this test did not create.
	before, err := countDue(ctx, t, s)
	if err != nil {
		t.Fatal(err)
	}
	rawBefore := rawSystemVisibleDue(ctx, t, s)

	// Three DUE pending jobs: one for each user, one system-owned.
	for _, j := range []struct{ owner, key string }{
		{a.ID, "count-due-a"},
		{b.ID, "count-due-b"},
	} {
		if _, err := h.super.Exec(ctx, `
			INSERT INTO app.jobs (id, user_id, kind, fire_at, payload, idempotency_key)
			VALUES ($1, $2, 'purge_trash', now() - interval '1 minute', '{}'::jsonb, $3)`,
			NewULID().String(), j.owner, j.key+"-"+NewULID().String()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.super.Exec(ctx, `
		INSERT INTO app.jobs (id, user_id, kind, fire_at, payload, idempotency_key)
		VALUES ($1, NULL, 'purge_trash', now() - interval '1 minute', '{}'::jsonb, $2)`,
		NewULID().String(), "count-due-system-"+NewULID().String()); err != nil {
		t.Fatal(err)
	}

	// Not backlog, and each for its own reason: one is scheduled for the future
	// (a snooze for next week is not work that is owed now), one is already
	// running (migration 00041's lease returns it to the claimable set by
	// itself, so counting it here would double-count it while it is reclaimed).
	if _, err := h.super.Exec(ctx, `
		INSERT INTO app.jobs (id, user_id, kind, fire_at, payload, idempotency_key)
		VALUES ($1, $2, 'snooze_wake', now() + interval '7 days', '{}'::jsonb, $3),
		       ($4, $2, 'ml_train', now() - interval '1 minute', '{}'::jsonb, $5)`,
		NewULID().String(), a.ID, "count-future-"+NewULID().String(),
		NewULID().String(), "count-running-"+NewULID().String()); err != nil {
		t.Fatal(err)
	}
	if _, err := h.super.Exec(ctx,
		`UPDATE app.jobs SET state = 'running', locked_at = now()
		  WHERE idempotency_key LIKE 'count-running-%'`); err != nil {
		t.Fatal(err)
	}

	after, err := countDue(ctx, t, s)
	if err != nil {
		t.Fatal(err)
	}
	if got := after - before; got != 3 {
		t.Errorf("app.count_due_jobs counted %d new due jobs, want 3 "+
			"(one per user plus one system job; the future-dated and running rows "+
			"must not count)", got)
	}

	// THE NEGATIVE CONTROL. The same transaction, counting the table directly,
	// must see only the SYSTEM row — which is exactly why the definer exists.
	if got := rawSystemVisibleDue(ctx, t, s) - rawBefore; got != 1 {
		t.Errorf("a raw count in a SystemTx saw %d of the 3 new due jobs, want 1. "+
			"If this is now 3, RLS on app.jobs has changed and the definer may no "+
			"longer be necessary; if it is 0, the system job is not visible either "+
			"and this test is not measuring what it claims", got)
	}
}

func countDue(ctx context.Context, t *testing.T, s *Store) (int64, error) {
	t.Helper()
	var n int64
	err := s.SystemTx(ctx, func(tx *SystemTx) error {
		var e error
		n, e = tx.CountDueJobs(ctx)
		return e
	})
	return n, err
}

// rawSystemVisibleDue counts app.jobs directly inside a SystemTx — i.e. through
// RLS, with no definer — which is the wrong answer the definer exists to avoid.
func rawSystemVisibleDue(ctx context.Context, t *testing.T, s *Store) int64 {
	t.Helper()
	var n int64
	if err := s.SystemTx(ctx, func(tx *SystemTx) error {
		return tx.tx.QueryRow(ctx,
			`SELECT count(*) FROM app.jobs WHERE state = 'pending' AND fire_at <= now()`).Scan(&n)
	}); err != nil {
		t.Fatal(err)
	}
	return n
}
