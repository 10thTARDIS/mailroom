// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// Store-layer tests for the ingest-only ledger (ADR-028 and its amendments):
// the index behind the ledger count, the stale/superseded findings, replay,
// discard, and the enqueue lock.

import (
	"context"
	"strings"
	"testing"
)

// TestSuppressedLedgerHasAnIndex pins ADR-028 A6: the partial index must exist
// AND be one the ledger queries can use. A partial index whose predicate drifts
// from the query's text is created, costs writes, and is never chosen, and
// nothing would notice.
//
// The plan is read with sequential scans disabled, because on a table this small
// the planner would pick a scan whether or not an index could serve the query;
// what is being asked is whether the index is ELIGIBLE.
//
// NEGATIVE CONTROL: drop writeback_suppressed (or write its predicate as
// `state::text = 'suppressed'`) and the plan below falls back to the table.
func TestSuppressedLedgerHasAnIndex(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)

	tx, err := h.super.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SET LOCAL enable_seqscan = off`); err != nil {
		t.Fatal(err)
	}
	// The statement SuppressedIntentCount runs, verbatim apart from the count.
	rows, err := tx.Query(ctx, `
		EXPLAIN SELECT count(*) FROM app.writeback_intents
		 WHERE user_id = $1 AND account_id = $2 AND state = 'suppressed'`,
		a.ID, a.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, line)
	}
	if !strings.Contains(strings.Join(plan, "\n"), "writeback_suppressed") {
		t.Fatalf("the ledger count does not use writeback_suppressed:\n%s", strings.Join(plan, "\n"))
	}
}
