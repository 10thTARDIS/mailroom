// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// Store-layer integration tests for the writeback intent queue (spec §4a):
// the pending->inflight->applied/dead state machine, idempotent enqueue, and
// the provenance-target read the engine uses for conflict detection. Runs
// against the throwaway Postgres provisioned by scripts/integration-test.sh;
// skips when the env is absent (see harness_test.go).

import (
	"context"
	"crypto/sha256"
	"testing"

	"github.com/oklog/ulid/v2"
)

// findIntent returns the claimed intent with id, or fails.
func findIntent(t *testing.T, intents []Intent, id ulid.ULID) Intent {
	t.Helper()
	for _, in := range intents {
		if in.ID == id {
			return in
		}
	}
	t.Fatalf("intent %s not found in claimed batch", id)
	return Intent{}
}

// EnqueueIntent is idempotent on (user_id, idempotency_key): a repeat returns
// the existing id and inserts no second row.
func TestEnqueueIntentIdempotent(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	acctID := mustULID(t, a.AccountID)
	msgID := mustULID(t, a.MessageID)

	key := "idem-enqueue-" + NewULID().String()
	var first, second ulid.ULID
	err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		var err error
		first, err = tx.EnqueueIntent(ctx, IntentRow{
			MessageID: &msgID, AccountID: acctID, Op: IntentAddFlag,
			Payload: []byte(`{"flag":"\\Seen"}`), IdempotencyKey: key,
		})
		if err != nil {
			return err
		}
		second, err = tx.EnqueueIntent(ctx, IntentRow{
			MessageID: &msgID, AccountID: acctID, Op: IntentAddFlag,
			Payload: []byte(`{"flag":"\\Seen"}`), IdempotencyKey: key,
		})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Errorf("duplicate enqueue returned %s, want the existing %s", second, first)
	}

	var n int
	if err := h.super.QueryRow(ctx,
		`SELECT count(*) FROM app.writeback_intents WHERE user_id = $1 AND idempotency_key = $2`,
		a.ID, key).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("rows for idempotency_key = %d, want exactly 1", n)
	}
}

// The full state machine: enqueue -> claim (pending->inflight, attempts+1) ->
// a claim does not re-claim an inflight row -> MarkIntentApplied lands it.
func TestClaimAndApply(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	acctID := mustULID(t, a.AccountID)
	msgID := mustULID(t, a.MessageID)

	var id ulid.ULID
	err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		var err error
		id, err = tx.EnqueueIntent(ctx, IntentRow{
			MessageID: &msgID, AccountID: acctID, Op: IntentAddFlag,
			Payload: []byte(`{"flag":"\\Seen"}`), IdempotencyKey: "claim-" + NewULID().String(),
		})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	// Claim: our intent flips to inflight with attempts == 1.
	err = s.UserTx(ctx, aID, func(tx *UserTx) error {
		claimed, err := tx.ClaimPendingIntents(ctx, acctID, 50)
		if err != nil {
			return err
		}
		in := findIntent(t, claimed, id)
		if in.State != IntentInflight {
			t.Errorf("claimed state = %q, want inflight", in.State)
		}
		if in.Attempts != 1 {
			t.Errorf("attempts = %d, want 1", in.Attempts)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// A second claim must not return the now-inflight intent.
	err = s.UserTx(ctx, aID, func(tx *UserTx) error {
		claimed, err := tx.ClaimPendingIntents(ctx, acctID, 50)
		if err != nil {
			return err
		}
		for _, in := range claimed {
			if in.ID == id {
				t.Error("inflight intent was claimed a second time")
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// Apply it.
	err = s.UserTx(ctx, aID, func(tx *UserTx) error {
		_, err := tx.MarkIntentApplied(ctx, id)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	var state string
	var appliedAt *string
	if err := h.super.QueryRow(ctx,
		`SELECT state, applied_at::text FROM app.writeback_intents WHERE id = $1`,
		id.String()).Scan(&state, &appliedAt); err != nil {
		t.Fatal(err)
	}
	if state != "applied" || appliedAt == nil {
		t.Errorf("state=%q applied_at=%v, want applied + timestamp", state, appliedAt)
	}
}

// MarkIntentFailed: an empty deadReason returns the intent to pending (and it
// can be re-claimed, incrementing attempts); a non-empty deadReason moves it to
// the surfaced dead state, which ListDeadIntents reports.
func TestFailRetryThenDead(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	acctID := mustULID(t, a.AccountID)
	msgID := mustULID(t, a.MessageID)

	var id ulid.ULID
	claim := func() Intent {
		var in Intent
		if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
			claimed, err := tx.ClaimPendingIntents(ctx, acctID, 50)
			if err != nil {
				return err
			}
			in = findIntent(t, claimed, id)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return in
	}

	err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		var err error
		id, err = tx.EnqueueIntent(ctx, IntentRow{
			MessageID: &msgID, AccountID: acctID, Op: IntentAddFlag,
			Payload: []byte(`{"flag":"\\Seen"}`), IdempotencyKey: "fail-" + NewULID().String(),
		})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	// Claim #1, transient fail -> back to pending with last_error.
	if in := claim(); in.Attempts != 1 {
		t.Fatalf("first claim attempts = %d, want 1", in.Attempts)
	}
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		_, err := tx.MarkIntentFailed(ctx, id, "connection reset", "")
		return err
	}); err != nil {
		t.Fatal(err)
	}

	// Claim #2 sees the retry: pending again, attempts now 2, last_error kept.
	in := claim()
	if in.State != IntentInflight || in.Attempts != 2 {
		t.Errorf("second claim state=%q attempts=%d, want inflight/2", in.State, in.Attempts)
	}
	if in.LastError != "connection reset" {
		t.Errorf("last_error = %q, want %q", in.LastError, "connection reset")
	}

	// Dead-letter it.
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		_, err := tx.MarkIntentFailed(ctx, id, "connection reset", "gave up after 2 attempts")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	err = s.UserTx(ctx, aID, func(tx *UserTx) error {
		dead, err := tx.ListDeadIntents(ctx, acctID)
		if err != nil {
			return err
		}
		found := findIntent(t, dead, id)
		if found.State != IntentDead {
			t.Errorf("state = %q, want dead", found.State)
		}
		if found.DeadReason != "gave up after 2 attempts" {
			t.Errorf("dead_reason = %q, want surfaced reason", found.DeadReason)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// MessageProvenanceTargetsForAccount returns the fixture's live location, and
// an EMPTY set for a message with no provenance (the engine's conflict signal).
func TestMessageProvenanceTargetsForAccount(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	acctID := mustULID(t, a.AccountID)
	msgID := mustULID(t, a.MessageID)

	err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		targets, err := tx.MessageProvenanceTargetsForAccount(ctx, msgID, acctID)
		if err != nil {
			return err
		}
		if len(targets) != 1 {
			t.Fatalf("targets = %d, want the fixture's single live location", len(targets))
		}
		target := targets[0]
		if target.IMAPName != "INBOX" || target.UID != 1 || target.UIDValidity != 1 {
			t.Errorf("target = %s/uid %d/uidv %d, want INBOX/1/1",
				target.IMAPName, target.UID, target.UIDValidity)
		}
		if target.State != ProvenanceLive {
			t.Errorf("state = %q, want live", target.State)
		}

		// A message with no provenance reads as an empty set (conflict signal).
		if gone, err := tx.MessageProvenanceTargetsForAccount(ctx, NewULID(), acctID); err != nil {
			return err
		} else if len(gone) != 0 {
			t.Errorf("targets for a message with no provenance = %d, want 0", len(gone))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// ⚠ THE REGRESSION TEST FOR #296, store half.
//
// A message can live at SEVERAL live coordinates inside ONE account — that is
// what #206's re-delivered-mail work records, and in practice it affects a
// small share of messages, with 2–4 live copies. The resolver
// used to end in `LIMIT 1`, so the writeback engine mirrored a user's mutation
// to exactly one of them while ingest merged the flags of all of them with OR.
// Adds converged; removals came straight back on the next poll.
//
// This asserts the store half: every live copy is returned. The engine half —
// that the engine then WRITES to every one of them — is
// TestFlagRemovalReachesEveryLiveCopyInTheAccount in internal/writeback, which
// cannot live here because internal/writeback imports internal/store and a test
// in package store importing it back is an import cycle.
func TestMessageProvenanceTargetsReturnsEveryLiveCopy(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	acctID := mustULID(t, a.AccountID)
	msgID := mustULID(t, a.MessageID)

	// A SECOND live row for the same (message, account): the same INBOX, a
	// different UID, exactly as a re-delivery of the same bytes produces. The
	// fixture already seeded uid 1 / uidvalidity 1 in this mailbox.
	if _, err := h.super.Exec(ctx,
		`INSERT INTO app.message_provenance (id, user_id, message_id, account_id,
		        mailbox_id, uid, uidvalidity, state)
		 VALUES ($1, $2, $3, $4, $5, 2, 1, 'live')`,
		NewULID().String(), a.ID, a.MessageID, a.AccountID, a.MailboxID); err != nil {
		t.Fatal(err)
	}

	err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		targets, err := tx.MessageProvenanceTargetsForAccount(ctx, msgID, acctID)
		if err != nil {
			return err
		}
		if len(targets) != 2 {
			t.Fatalf("targets = %d, want 2 — every live copy in the account, not just the newest", len(targets))
		}
		uids := map[int64]bool{}
		for _, tg := range targets {
			if tg.State != ProvenanceLive {
				t.Errorf("target %s state = %q, want live", tg.IMAPName, tg.State)
			}
			uids[tg.UID] = true
		}
		if !uids[1] || !uids[2] {
			t.Errorf("returned UIDs = %v, want both 1 and 2", uids)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A live copy and a stale one for the same (message, account): the stale row is
// NOT returned alongside the live one. It is not a writable coordinate — its UID
// belongs to a UIDVALIDITY generation the server threw away and may now name a
// different message — so handing it to a caller whose job is to write to every
// element of the set would be a mail-destroying foot-gun (#296).
func TestMessageProvenanceTargetsExcludesStaleWhenALiveCopyExists(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	acctID := mustULID(t, a.AccountID)
	msgID := mustULID(t, a.MessageID)

	if _, err := h.super.Exec(ctx,
		`INSERT INTO app.message_provenance (id, user_id, message_id, account_id,
		        mailbox_id, uid, uidvalidity, state)
		 VALUES ($1, $2, $3, $4, $5, 7, 99, 'stale_uidvalidity')`,
		NewULID().String(), a.ID, a.MessageID, a.AccountID, a.MailboxID); err != nil {
		t.Fatal(err)
	}

	err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		targets, err := tx.MessageProvenanceTargetsForAccount(ctx, msgID, acctID)
		if err != nil {
			return err
		}
		if len(targets) != 1 {
			t.Fatalf("targets = %d, want only the live copy", len(targets))
		}
		if targets[0].State != ProvenanceLive || targets[0].UID != 1 {
			t.Errorf("target = uid %d state %q, want the live uid 1", targets[0].UID, targets[0].State)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A message whose ONLY provenance row is 'stale_uidvalidity' (the mailbox was
// recreated on the server — UIDVALIDITY rolled, mailroom#88 — and the
// re-fetch under the new generation has not, yet or ever, re-attached a live
// row to this message) must read as found=true with that state, NOT as
// not-found. Before mailroom#91 the store filtered state='live' unconditionally,
// so this case was indistinguishable from "no provenance at all", and the
// engine dead-lettered it with the wrong user-facing reason ("moved or
// expunged remotely" instead of "mailbox recreated on the server").
func TestMessageProvenanceTargetsDistinguishStaleFromGone(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	acctID := mustULID(t, a.AccountID)

	// A mailbox belonging to the same account, and a message whose only
	// provenance row in that mailbox is explicitly 'stale_uidvalidity' — no
	// live row exists anywhere for this message.
	mailboxID := NewULID().String()
	if _, err := h.super.Exec(ctx,
		`INSERT INTO app.mailboxes (id, user_id, account_id, imap_name, display_name)
		 VALUES ($1, $2, $3, 'Archive', 'Archive')`,
		mailboxID, a.ID, a.AccountID); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte("stale-provenance-message"))
	if _, err := h.super.Exec(ctx,
		`INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id)
		 VALUES ($1, $2, 10, 20, $3)`,
		a.ID, hash[:], NewULID().String()); err != nil {
		t.Fatal(err)
	}
	msgID := NewULID().String()
	if _, err := h.super.Exec(ctx,
		`INSERT INTO app.messages (id, user_id, content_hash, subject, from_address,
		        received_at, size_bytes, message_id_hint)
		 VALUES ($1, $2, $3, 'stale test', 'friend@example.com', now(), 10, $4)`,
		msgID, a.ID, hash[:], "<stale-"+msgID+"@example.com>"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.super.Exec(ctx,
		`INSERT INTO app.message_provenance (id, user_id, message_id, account_id,
		        mailbox_id, uid, uidvalidity, state)
		 VALUES ($1, $2, $3, $4, $5, 1, 2, 'stale_uidvalidity')`,
		NewULID().String(), a.ID, msgID, a.AccountID, mailboxID); err != nil {
		t.Fatal(err)
	}

	err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		mID := mustULID(t, msgID)
		targets, err := tx.MessageProvenanceTargetsForAccount(ctx, mID, acctID)
		if err != nil {
			return err
		}
		if len(targets) == 0 {
			t.Fatal("expected the stale-only provenance to be found, not reported as absent")
		}
		if targets[0].State != ProvenanceStaleUIDValidity {
			t.Errorf("state = %q, want stale_uidvalidity", targets[0].State)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// The marks report whether a row moved (#691). An intent that is gone by the
// time the pass records it — deleting a message cascades its intents away —
// must come back false from every mark, so the engine never counts it as
// applied, retried or dead-lettered. The true side is checked too: a mark that
// could only ever return false would pass the first half on its own.
func TestMarkIntentReportsWhetherARowMoved(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	acctID := mustULID(t, a.AccountID)
	msgID := mustULID(t, a.MessageID)

	claimNew := func() ulid.ULID {
		var id ulid.ULID
		if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
			var err error
			id, err = tx.EnqueueIntent(ctx, IntentRow{
				MessageID: &msgID, AccountID: acctID, Op: IntentAddFlag,
				Payload: []byte(`{"flag":"\\Seen"}`), IdempotencyKey: "moved-" + NewULID().String(),
			})
			if err != nil {
				return err
			}
			claimed, err := tx.ClaimPendingIntents(ctx, acctID, 50)
			if err != nil {
				return err
			}
			findIntent(t, claimed, id)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	marks := []struct {
		name string
		mark func(tx *UserTx, id ulid.ULID) (bool, error)
	}{
		{"applied", func(tx *UserTx, id ulid.ULID) (bool, error) { return tx.MarkIntentApplied(ctx, id) }},
		{"retry", func(tx *UserTx, id ulid.ULID) (bool, error) {
			return tx.MarkIntentFailed(ctx, id, "connection reset", "")
		}},
		{"dead", func(tx *UserTx, id ulid.ULID) (bool, error) {
			return tx.MarkIntentFailed(ctx, id, "connection reset", "gave up")
		}},
	}
	for _, m := range marks {
		t.Run(m.name, func(t *testing.T) {
			run := func(id ulid.ULID) bool {
				var moved bool
				if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
					var err error
					moved, err = m.mark(tx, id)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				return moved
			}

			if id := claimNew(); !run(id) {
				t.Error("mark of an inflight intent reported no row moved")
			}

			id := claimNew()
			if _, err := h.super.Exec(ctx,
				`DELETE FROM app.writeback_intents WHERE id = $1`, id.String()); err != nil {
				t.Fatal(err)
			}
			if run(id) {
				t.Error("mark of a deleted intent reported a row moved")
			}
		})
	}
}
