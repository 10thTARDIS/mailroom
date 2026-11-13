// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// Pausing sync is not ingest-only mode (ADR-028 amendment A10).
// Shares ledgerFx with writeback_ledger_integration_test.go.

import (
	"context"
	"testing"

	"github.com/oklog/ulid/v2"
)

// TestPausingSyncKeepsQueueingRealWork pins ADR-028 amendment A10: pausing an
// account (sync_enabled=false) is NOT ingest-only mode. Intents made while paused
// are born 'pending' (real work), the account is not listed for the sync pass that
// would drain them, and resuming sync makes the whole backlog claimable at once.
//
// That is a decision, not an accident: the user acted on mail they could see and
// expects it to reach the server when sync resumes, which is the opposite of the
// shadow mode that exists because they do not. The test exists so that changing
// it is a visible edit to a test with this comment, not a side effect of touching
// the enqueue or the listing.
//
// NEGATIVE CONTROL: the shadowed account in the same test queues 'suppressed', so
// "pending" below is not simply what every account does.
func TestPausingSyncKeepsQueueingRealWork(t *testing.T) {
	f := newLedgerFx(t)
	ctx := context.Background()
	s := f.h.appStore()
	enqueue := func(key string) ulid.ULID {
		var id ulid.ULID
		if err := s.UserTx(ctx, f.uid, func(tx *UserTx) error {
			var e error
			id, e = tx.EnqueueIntent(ctx, IntentRow{
				MessageID: &f.msg, AccountID: f.acct, Op: IntentAddKeyword,
				Payload: []byte(`{"keyword":"paused"}`), IdempotencyKey: key, Source: IntentByUser,
			})
			return e
		}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	listed := func() bool {
		var found bool
		if err := s.UserTx(ctx, f.uid, func(tx *UserTx) error {
			accts, err := tx.ListSyncableAccounts(ctx)
			for _, a := range accts {
				found = found || a.ID == f.acct
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return found
	}

	f.exec(`UPDATE app.accounts SET sync_enabled = false WHERE user_id = $1 AND id = $2`, f.a.ID, f.a.AccountID)
	paused := enqueue("paused-" + NewULID().String())
	if got := f.state(paused); got != "pending" {
		t.Fatalf("an intent made while sync is paused is %q, want pending", got)
	}
	if listed() {
		t.Fatal("a paused account is listed for the sync pass; the premise of the test is wrong")
	}

	// Resuming makes the backlog claimable.
	f.exec(`UPDATE app.accounts SET sync_enabled = true WHERE user_id = $1 AND id = $2`, f.a.ID, f.a.AccountID)
	if !listed() {
		t.Fatal("a resumed account is not listed")
	}
	var claimed []Intent
	if err := s.UserTx(ctx, f.uid, func(tx *UserTx) error {
		var e error
		claimed, e = tx.ClaimPendingIntents(ctx, f.acct, 50)
		return e
	}); err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 1 || claimed[0].ID != paused {
		t.Fatalf("resuming claimed %v, want exactly the intent made while paused", claimed)
	}

	// The control: a SHADOWED account queues the same mutation as ledger.
	f.exec(`UPDATE app.accounts SET writeback_enabled = false WHERE user_id = $1 AND id = $2`, f.a.ID, f.a.AccountID)
	if got := f.state(enqueue("shadowed-" + NewULID().String())); got != "suppressed" {
		t.Fatalf("an intent made on a shadowed account is %q, want suppressed", got)
	}
}
