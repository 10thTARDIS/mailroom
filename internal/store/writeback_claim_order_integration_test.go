// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// The claim order through UPDATE ... RETURNING (ADR-028 amendment A9).
// Shares ledgerFx with writeback_ledger_integration_test.go.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
)

// TestClaimOrderFollowsCreatedAtNotInsertionOrder answers the question ADR-028's
// review left open: does ClaimPendingIntents' ORDER BY survive `UPDATE ...
// RETURNING`? Two safety arguments (a keyword STORE addresses its copy by the UID
// a MOVE in the same action would have removed; a restore's MOVE must follow the
// purge it cancels) lean on the answer.
//
// TestClaimOrdersTiesByID covers rows that SHARE a created_at. This is the other
// half: rows whose created_at order differs from BOTH their insertion order (heap
// order) and their id order, which is what a long transaction that started early
// and committed late produces. Thirty rows, with created_at a fixed permutation of
// the insertion order, so that an order taken from the heap, from the id, or from
// whichever order the UPDATE happened to touch the rows cannot coincide with the
// right one.
//
// Two properties, because they are different clauses:
//   - which rows a LIMIT takes (the inner ORDER BY): the ten earliest by
//     created_at, not the ten first inserted;
//   - the order they come back in (the outer ORDER BY): created_at.
//
// PROBES (run, red, reverted): remove the OUTER `ORDER BY created_at, id` and the
// order assertion fails; remove the INNER one and the membership assertion fails.
func TestClaimOrderFollowsCreatedAtNotInsertionOrder(t *testing.T) {
	f := newLedgerFx(t)
	ctx := context.Background()
	base := time.Now().Add(-48 * time.Hour).Truncate(time.Second)
	const n = 30
	type row struct {
		id  ulid.ULID
		min int
	}
	var rows []row
	for i := 0; i < n; i++ {
		min := (i * 7) % n // a permutation of 0..n-1 (7 and 30 are coprime)
		id := f.intent("+kw", "pending", "user", base.Add(time.Duration(min)*time.Minute),
			fmt.Sprintf(`{"keyword":"o%d"}`, i))
		rows = append(rows, row{id, min})
	}
	// ARMING CONTROL: insertion order and created_at order really do differ, or
	// this test is the tie-break test again.
	inOrder := true
	for i := 1; i < n; i++ {
		if rows[i].min < rows[i-1].min {
			inOrder = false
		}
	}
	if inOrder {
		t.Fatal("the fixture's created_at order equals its insertion order: nothing is being tested")
	}

	var got []ulid.ULID
	if err := f.h.appStore().UserTx(ctx, f.uid, func(tx *UserTx) error {
		ins, err := tx.ClaimPendingIntents(ctx, f.acct, 10)
		for _, in := range ins {
			got = append(got, in.ID)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}

	byMin := map[int]ulid.ULID{}
	for _, r := range rows {
		byMin[r.min] = r.id
	}
	var want []ulid.ULID
	for m := 0; m < 10; m++ {
		want = append(want, byMin[m])
	}
	if len(got) != 10 {
		t.Fatalf("claimed %d rows, want exactly the LIMIT of 10: ClaimPendingIntents over-claimed and "+
			"the extra rows are now inflight with an attempt spent", len(got))
	}
	var inflight int
	if err := f.h.super.QueryRow(ctx,
		`SELECT count(*) FROM app.writeback_intents WHERE user_id = $1 AND state = 'inflight'`,
		f.a.ID).Scan(&inflight); err != nil || inflight != 10 {
		t.Fatalf("%d rows are inflight after a claim of 10 (err %v), want 10", inflight, err)
	}
	gotSet := map[ulid.ULID]bool{}
	for _, id := range got {
		gotSet[id] = true
	}
	for _, id := range want {
		if !gotSet[id] {
			t.Errorf("the ten earliest by created_at must be the ten claimed; %s is missing", id)
		}
	}
	for i := range want {
		if i < len(got) && got[i] != want[i] {
			t.Fatalf("claim order[%d] = %s, want %s: the engine applies a batch in the order this returns, "+
				"and it is not created_at order", i, got[i], want[i])
		}
	}
}
