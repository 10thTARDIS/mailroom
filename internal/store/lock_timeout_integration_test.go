// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

import (
	"context"
	"os"
	"testing"
)

// The integration lane's database must carry the timeouts scripts/integration-
// test.sh sets (#855). Without them a lock wait is an unbounded hang again, and
// nothing else in the suite would notice: a hang that never happens looks
// exactly like a setting that works. Checked on the mailroom_app pool, because
// that is the role whose connections the suite's stalls came from.
func TestIntegrationDatabaseHasStallTimeouts(t *testing.T) {
	h := newHarness(t)
	var lock, stmt string
	if err := h.app.QueryRow(context.Background(), `SELECT current_setting('lock_timeout'), current_setting('statement_timeout')`).Scan(&lock, &stmt); err != nil {
		t.Fatal(err)
	}
	if lock != "30s" || stmt != "2min" {
		t.Fatalf("lock_timeout=%q statement_timeout=%q, want 30s and 2min (set by scripts/integration-test.sh)", lock, stmt)
	}
}

// TestLockTimeoutProof deliberately blocks one transaction behind another to
// show what a lock wait now looks like: a bounded failure naming the cause, and
// the blocker dump a failed test produces. It takes the full lock_timeout
// (30s) and ends in a DELIBERATE failure, so it only runs when asked:
//
//	MAILROOM_PROVE_LOCK_TIMEOUT=1 go test -tags integration -run TestLockTimeoutProof ./internal/store
func TestLockTimeoutProof(t *testing.T) {
	if os.Getenv("MAILROOM_PROVE_LOCK_TIMEOUT") == "" {
		t.Skip("deliberately failing demonstration; set MAILROOM_PROVE_LOCK_TIMEOUT=1")
	}
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.super.Exec(ctx, `CREATE TABLE IF NOT EXISTS public.lock_proof (id int PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = h.super.Exec(context.Background(), `DROP TABLE IF EXISTS public.lock_proof`) })
	if _, err := h.super.Exec(ctx, `INSERT INTO public.lock_proof VALUES (1) ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}

	holder, err := h.super.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.Rollback(context.Background()) })
	if _, err := holder.Exec(ctx, `UPDATE public.lock_proof SET id = 1 WHERE id = 1`); err != nil {
		t.Fatal(err)
	}

	// A second session now waits for the row the first holds.
	_, err = h.super.Exec(ctx, `UPDATE public.lock_proof SET id = 1 WHERE id = 1`)
	if err == nil {
		t.Fatal("second UPDATE did not block")
	}
	t.Logf("blocked statement failed with: %v", err)
	dumpBlockers(t, h.super)
	t.Fail() // the cleanup dump runs on failure, as it would in a real stall
}
