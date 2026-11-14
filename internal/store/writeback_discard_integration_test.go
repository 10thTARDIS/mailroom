// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// Discarding the ingest-only ledger (ADR-028 amendment A3).
// Shares ledgerFx with writeback_ledger_integration_test.go.

import (
	"context"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
)

func (f *ledgerFx) discard() []SuppressedIntentRow {
	f.t.Helper()
	var rows []SuppressedIntentRow
	if err := f.h.appStore().UserTx(context.Background(), f.uid, func(tx *UserTx) error {
		var e error
		rows, e = tx.DiscardSuppressedIntents(context.Background(), f.acct)
		return e
	}); err != nil {
		f.t.Fatal(err)
	}
	return rows
}

// TestDiscardIsTerminalAndCounted: discarding moves exactly the suppressed rows
// to 'discarded', says how many per op and source, and leaves every other state
// alone. A discarded row is then outside the ledger count, outside replay and
// outside the claim.
//
// NEGATIVE CONTROL: drop `AND state = 'suppressed'` from DiscardSuppressedIntents
// and the pending and applied rows are swept up too; replace the state with
// 'pending' and the claim at the end hands the rows to the engine.
func TestDiscardIsTerminalAndCounted(t *testing.T) {
	f := newLedgerFx(t)
	now := time.Now()
	f.dateProvenance(now.Add(-10 * time.Hour))
	a1 := f.intent("move", "suppressed", "user", now.Add(-3*time.Hour), `{"mailbox":"Archive"}`)
	a2 := f.intent("+kw", "suppressed", "rule", now.Add(-3*time.Hour), `{"keyword":"x"}`)
	a3 := f.intent("+kw", "suppressed", "rule", now.Add(-3*time.Hour), `{"keyword":"y"}`)
	a4 := f.intent("+kw", "suppressed", "", now.Add(-3*time.Hour), `{"keyword":"z"}`)
	keep := map[ulid.ULID]string{
		f.intent("+flag", "pending", "user", now.Add(-time.Hour), `{"flag":"\\Seen"}`):  "pending",
		f.intent("+flag", "applied", "user", now.Add(-time.Hour), `{"flag":"\\Seen"}`):  "applied",
		f.intent("-flag", "dead", "user", now.Add(-time.Hour), `{"flag":"\\Seen"}`):     "dead",
		f.intent("-flag", "inflight", "user", now.Add(-time.Hour), `{"flag":"\\Seen"}`): "inflight",
	}

	got := f.discard()
	per := map[string]int64{}
	var total int64
	for _, r := range got {
		per[r.Op+"/"+r.Source] += r.Count
		total += r.Count
	}
	if total != 4 || per["move/user"] != 1 || per["+kw/rule"] != 2 || per["+kw/"] != 1 {
		t.Fatalf("discard reported %v (total %d), want move/user 1, +kw/rule 2, +kw/(unknown) 1, total 4", per, total)
	}
	for _, id := range []ulid.ULID{a1, a2, a3, a4} {
		if s := f.state(id); s != "discarded" {
			t.Errorf("row %s is %q, want discarded", id, s)
		}
	}
	for id, want := range keep {
		if s := f.state(id); s != want {
			t.Errorf("a %s row came out %q: discard must touch only the ledger", want, s)
		}
	}

	// Outside the ledger count, so Account/getHealth stops reporting it.
	var n int64
	if err := f.h.appStore().UserTx(context.Background(), f.uid, func(tx *UserTx) error {
		var e error
		n, e = tx.SuppressedIntentCount(context.Background(), f.acct)
		return e
	}); err != nil || n != 0 {
		t.Fatalf("suppressed count after discard = %d (err %v), want 0", n, err)
	}
	// A second discard finds nothing: the first was terminal.
	if again := f.discard(); len(again) != 0 {
		t.Errorf("a second discard moved %v, want nothing", again)
	}
	// Replay finds nothing to revive.
	var res ReplayResult
	if err := f.h.appStore().UserTx(context.Background(), f.uid, func(tx *UserTx) error {
		var e error
		res, e = tx.ReplaySuppressedIntents(context.Background(), f.acct)
		return e
	}); err != nil || res != (ReplayResult{}) {
		t.Fatalf("replay after discard = %+v (err %v), want all zero", res, err)
	}
	// And a discarded row is never claimed: only the one pending row is.
	var claimed []Intent
	if err := f.h.appStore().UserTx(context.Background(), f.uid, func(tx *UserTx) error {
		var e error
		claimed, e = tx.ClaimPendingIntents(context.Background(), f.acct, 50)
		return e
	}); err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 1 {
		t.Fatalf("claimed %d intents, want only the 1 pending row (a discarded row was handed to the engine)", len(claimed))
	}
}

// TestDiscardCannotReachAnotherUsersLedger: scoped by the caller's identity.
func TestDiscardCannotReachAnotherUsersLedger(t *testing.T) {
	f := newLedgerFx(t)
	id := f.intent("move", "suppressed", "user", time.Now().Add(-time.Hour), `{"mailbox":"Archive"}`)
	var rows []SuppressedIntentRow
	if err := f.h.appStore().UserTx(context.Background(), mustULID(t, f.b.ID), func(tx *UserTx) error {
		var e error
		rows, e = tx.DiscardSuppressedIntents(context.Background(), f.acct)
		return e
	}); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 || f.state(id) != "suppressed" {
		t.Fatalf("another user's discard moved %v; the row is %q", rows, f.state(id))
	}
}

// TestAnEnqueueAfterADiscardIsNotSwallowed: a discarded row must not eat the same
// deterministic key arriving later, or the purge sweep's re-enqueue (which uses
// one key per message and account on every pass) would stop expunging that
// message for ever.
//
// Both states the new request can be born in are covered, with the discarded row
// replaced rather than duplicated (one row per key).
//
// NEGATIVE CONTROL: remove `OR w.state = 'discarded'` from EnqueueIntent's ON
// CONFLICT and the row stays 'discarded' in both subtests.
func TestAnEnqueueAfterADiscardIsNotSwallowed(t *testing.T) {
	for _, tc := range []struct {
		name      string
		writeback bool
		want      string
	}{
		{"live account: born pending", true, "pending"},
		{"still shadowed: born suppressed", false, "suppressed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLedgerFx(t)
			f.exec(`UPDATE app.accounts SET writeback_enabled = $3 WHERE user_id = $1 AND id = $2`,
				f.a.ID, f.a.AccountID, tc.writeback)
			key := "discard-key-" + NewULID().String()
			enqueue := func() ulid.ULID {
				var id ulid.ULID
				if err := f.h.appStore().UserTx(context.Background(), f.uid, func(tx *UserTx) error {
					var e error
					id, e = tx.EnqueueIntent(context.Background(), IntentRow{
						MessageID: &f.msg, AccountID: f.acct, Op: IntentAppend,
						Payload: []byte(`{"mailbox":"Drafts","content_hash":"ff"}`), IdempotencyKey: key,
						Source: IntentByDraft,
					})
					return e
				}); err != nil {
					t.Fatal(err)
				}
				return id
			}
			first := enqueue()
			// Park it as a discarded ledger row, whatever state it was born in.
			f.exec(`UPDATE app.writeback_intents SET state = 'discarded' WHERE id = $1`, first.String())

			second := enqueue()
			if second != first {
				t.Fatalf("the second enqueue returned %s, want the same row %s (one row per key)", second, first)
			}
			if got := f.state(first); got != tc.want {
				t.Fatalf("the re-enqueued row is %q, want %q: a discarded row swallowed the request", got, tc.want)
			}
			var rows int
			if err := f.h.super.QueryRow(context.Background(),
				`SELECT count(*) FROM app.writeback_intents WHERE user_id = $1 AND idempotency_key = $2`,
				f.a.ID, key).Scan(&rows); err != nil || rows != 1 {
				t.Fatalf("%d rows for the key (err %v), want 1", rows, err)
			}
		})
	}
}
