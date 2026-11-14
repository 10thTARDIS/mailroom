// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// The enqueue/mode-switch race (ADR-028 amendment A8).
// Shares ledgerFx with writeback_ledger_integration_test.go.

import (
	"context"
	"testing"
	"time"
)

// TestEnqueueRacingTheModeSwitchLandsSuppressed is the interleaving ADR-028 §4
// described as "tracked rather than done": an enqueue that starts before the mode
// is switched on, and is held up by the switch.
//
// The switching transaction does what Account/set does: UpdateAccount (which takes
// the account row FOR UPDATE) then SuppressAccountIntents, and is held open at that
// point. An enqueue begins meanwhile. It must then BLOCK (witnessed in
// pg_stat_activity, not assumed), and once the switch commits it must land
// 'suppressed', because it read the new flag.
//
// ⚠ WITHOUT THE WITNESS THIS TEST CAN PASS FOR THE WRONG REASON. If the enqueue
// simply ran to completion before the switch started, 'pending' is the correct
// answer and the assertion at the end would be about a different race. The wait
// loop fails the test when no backend is ever seen waiting on a lock.
//
// NEGATIVE CONTROL: remove `FOR KEY SHARE OF a` from EnqueueIntent and the row
// lands 'pending' on a shadowed account while the witness still sees the block.
func TestEnqueueRacingTheModeSwitchLandsSuppressed(t *testing.T) {
	f := newLedgerFx(t)
	ctx := context.Background()
	s := f.h.appStore()
	off := false

	holding := make(chan struct{})
	release := make(chan struct{})
	switchDone := make(chan error, 1)
	go func() {
		switchDone <- s.UserTx(ctx, f.uid, func(tx *UserTx) error {
			if _, err := tx.UpdateAccount(ctx, f.acct, AccountPatch{WritebackEnabled: &off}); err != nil {
				return err
			}
			close(holding) // the account row is now locked FOR UPDATE
			<-release
			_, err := tx.SuppressAccountIntents(ctx, f.acct)
			return err
		})
	}()
	select {
	case <-holding:
	case err := <-switchDone:
		t.Fatalf("the switching transaction ended before holding the lock: %v", err)
	case <-time.After(20 * time.Second):
		t.Fatal("the switching transaction never took the account lock")
	}

	key := "race-" + NewULID().String()
	enqueued := make(chan error, 1)
	go func() {
		enqueued <- s.UserTx(ctx, f.uid, func(tx *UserTx) error {
			_, err := tx.EnqueueIntent(ctx, IntentRow{
				MessageID: &f.msg, AccountID: f.acct, Op: IntentAddKeyword,
				Payload: []byte(`{"keyword":"race"}`), IdempotencyKey: key, Source: IntentByUser,
			})
			return err
		})
	}()

	// THE WITNESS: some backend is waiting on a lock inside the enqueue.
	deadline := time.Now().Add(20 * time.Second)
	for {
		var waiting int
		if err := f.h.super.QueryRow(ctx, `
			SELECT count(*) FROM pg_stat_activity
			 WHERE wait_event_type = 'Lock'
			   AND query LIKE '%INSERT INTO app.writeback_intents%'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			break
		}
		select {
		case err := <-enqueued:
			t.Fatalf("the enqueue finished (%v) without ever blocking on the switch: the interleaving "+
				"this test is about did not happen, so its result would prove nothing", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("no backend was seen waiting on a lock for the enqueue within 20s")
		}
		time.Sleep(25 * time.Millisecond)
	}

	close(release) // the switch suppresses (nothing to see yet) and commits
	if err := <-switchDone; err != nil {
		t.Fatalf("switch: %v", err)
	}
	if err := <-enqueued; err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	var state string
	if err := f.h.super.QueryRow(ctx,
		`SELECT state::text FROM app.writeback_intents WHERE user_id = $1 AND idempotency_key = $2`,
		f.a.ID, key).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "suppressed" {
		t.Fatalf("the racing enqueue landed %q on an account that is now shadowed, want suppressed: "+
			"it would drain, unreviewed, the moment the mode is lifted", state)
	}
}
