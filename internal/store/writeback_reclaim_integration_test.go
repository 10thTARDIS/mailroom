// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

import (
	"context"
	"testing"

	"github.com/oklog/ulid/v2"
)

// ⚠ WHAT THIS PINS, AND WHY IT COULD NOT HAVE EXISTED BEFORE (#520).
//
// ClaimPendingIntents matched `state = 'pending'` only, and MarkIntentApplied /
// MarkIntentFailed are BOTH guarded on `state = 'inflight'`. So an intent left
// inflight by a process that died between claim and completion was read by
// nothing, ever again. app.writeback_intents did not even record WHEN a row was
// claimed, so there was nothing to age against — unlike app.jobs, where the
// column existed and only the logic was missing (#351 B3, migration 00041).
//
// It is fail-SAFE — nothing re-fires, so no duplicate IMAP operation — and that
// is exactly what made it dangerous: the user's flag change, move or expunge
// silently never reached IMAP, no error was raised, and account health surfaces
// only 'dead'. This is the one subsystem where the symptom is Mailroom and the
// real mailbox quietly disagreeing.
//
// THE PAIR BELOW IS DELIBERATE, and it is the whole point of the test:
//
//   - reclaiming a STALE claim proves the mechanism exists;
//   - NOT reclaiming a FRESH claim proves it is a lease, and not "the claim
//     query ignores state" — which a single positive test would also accept.
//
// A third assertion covers the way this could pass while doing nothing:
// claimed_at must actually be STAMPED. If the UPDATE failed to set it, every
// row would have claimed_at NULL, the reclaim arm (which requires NOT NULL)
// would match nothing forever, and a test that only checked the stale case by
// writing claimed_at itself would never notice.
func TestAWritebackIntentStrandedByADeadWorkerIsReclaimed(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	acctID := mustULID(t, a.AccountID)
	msgID := mustULID(t, a.MessageID)

	var intentID ulid.ULID
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		var err error
		intentID, err = tx.EnqueueIntent(ctx, IntentRow{
			MessageID: &msgID, AccountID: acctID, Op: IntentAddFlag,
			Payload:        []byte(`{"flag":"\\Seen"}`),
			IdempotencyKey: "reclaim-" + NewULID().String(),
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}

	// A worker claims it and then dies. The row keeps 'inflight' and a
	// claimed_at that no live pass will ever complete.
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		claimed, err := tx.ClaimPendingIntents(ctx, acctID, 50)
		if err != nil {
			return err
		}
		in := findIntent(t, claimed, intentID)
		if in.State != IntentInflight {
			t.Errorf("after claim state = %q, want %q", in.State, IntentInflight)
		}
		if in.Attempts != 1 {
			t.Errorf("after first claim attempts = %d, want 1", in.Attempts)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// The claim must have stamped claimed_at. A NULL here makes the reclaim arm
	// unreachable for the life of the row, and every later assertion in this
	// test would still pass because it backdates the column by hand.
	var stamped bool
	if err := h.super.QueryRow(ctx,
		`SELECT claimed_at IS NOT NULL FROM app.writeback_intents WHERE id = $1`,
		intentID.String()).Scan(&stamped); err != nil {
		t.Fatal(err)
	}
	if !stamped {
		t.Fatal("claimed_at is NULL after a claim; the reclaim arm can never match this row")
	}

	// NEGATIVE CONTROL: a FRESH claim is still someone's work. Re-claiming here
	// would mean the query had simply stopped caring about state.
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		claimed, err := tx.ClaimPendingIntents(ctx, acctID, 50)
		if err != nil {
			return err
		}
		for _, in := range claimed {
			if in.ID == intentID {
				t.Errorf("intent %s was re-claimed while its lease was still fresh; "+
					"the reclaim is not a lease", intentID)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// Backdating claimed_at past the lease is how "the process died 16 minutes
	// ago" looks to the next pass.
	if _, err := h.super.Exec(ctx,
		`UPDATE app.writeback_intents SET claimed_at = now() - interval '16 minutes' WHERE id = $1`,
		intentID.String()); err != nil {
		t.Fatal(err)
	}

	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		claimed, err := tx.ClaimPendingIntents(ctx, acctID, 50)
		if err != nil {
			return err
		}
		in := findIntent(t, claimed, intentID)
		// attempts must keep climbing on a reclaim: the intent WAS attempted,
		// and burning retries is what stops an intent that reliably kills its
		// process from being retried forever instead of dead-lettering.
		if in.Attempts != 2 {
			t.Errorf("after reclaim attempts = %d, want 2 (the reclaim must burn a retry)", in.Attempts)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// A terminal intent is never reclaimed however old its claim is. 'applied' and
// 'dead' are end states; the lease exists to rescue work still owed, not to
// re-run work that finished or was deliberately given up on. Without this, a
// lease predicate written as "claimed_at is old" rather than "inflight AND
// claimed_at is old" would re-apply every historical intent in the mailbox --
// which on a real instance is thousands of rows, including many moves and
// expunges.
func TestATerminalWritebackIntentIsNeverReclaimed(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	acctID := mustULID(t, a.AccountID)
	msgID := mustULID(t, a.MessageID)

	for _, tc := range []struct {
		name  string
		apply func(tx *UserTx, id ulid.ULID) error
	}{
		{"applied", func(tx *UserTx, id ulid.ULID) error {
			_, err := tx.MarkIntentApplied(ctx, id)
			return err
		}},
		{"dead", func(tx *UserTx, id ulid.ULID) error {
			_, err := tx.MarkIntentFailed(ctx, id, "boom", "gave up")
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var id ulid.ULID
			if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
				var err error
				id, err = tx.EnqueueIntent(ctx, IntentRow{
					MessageID: &msgID, AccountID: acctID, Op: IntentAddFlag,
					Payload:        []byte(`{"flag":"\\Seen"}`),
					IdempotencyKey: "terminal-" + NewULID().String(),
				})
				if err != nil {
					return err
				}
				if _, err := tx.ClaimPendingIntents(ctx, acctID, 50); err != nil {
					return err
				}
				return tc.apply(tx, id)
			}); err != nil {
				t.Fatal(err)
			}

			if _, err := h.super.Exec(ctx,
				`UPDATE app.writeback_intents SET claimed_at = now() - interval '30 days' WHERE id = $1`,
				id.String()); err != nil {
				t.Fatal(err)
			}

			if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
				claimed, err := tx.ClaimPendingIntents(ctx, acctID, 50)
				if err != nil {
					return err
				}
				for _, in := range claimed {
					if in.ID == id {
						t.Errorf("a %q intent with a 30-day-old claim was reclaimed", tc.name)
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
