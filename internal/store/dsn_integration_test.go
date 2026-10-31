// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// Integration suite for the DSN / bounce pipeline (spec §4, Forgejo #30):
// linking a parsed bounce back to the message and submission it reports on, the
// EmailSubmission change_log row a linked bounce owes (and the absence of one
// when it links to nothing), and — the load-bearing safety property — that a
// per-recipient failure is contained under a SAVEPOINT and never poisons the
// surrounding (ingest) transaction, exactly like TestLabelNameCollision.

import (
	"context"
	"crypto/sha256"
	"testing"

	"github.com/oklog/ulid/v2"
)

// seedDSNMessage inserts a bare message to stand in for the ingested DSN itself
// (dsn_message_id must reference a real message), in the seeded user's thread.
func seedDSNMessage(t *testing.T, ctx context.Context, h *harness, u fixtureUser) ulid.ULID {
	t.Helper()
	var threadID string
	if err := h.super.QueryRow(ctx,
		`SELECT thread_id FROM app.messages WHERE id = $1`, u.MessageID).Scan(&threadID); err != nil {
		t.Fatalf("read seeded thread: %v", err)
	}
	id := NewULID()
	hash := sha256.Sum256([]byte("dsn-" + id.String()))
	// messages.content_hash references app.blobs (user_id, content_hash), so the
	// backing blob must exist first.
	if _, err := h.super.Exec(ctx, `
		INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id)
		VALUES ($1, $2, 500, 540, $3)`,
		u.ID, hash[:], NewULID().String()); err != nil {
		t.Fatalf("seed dsn blob: %v", err)
	}
	if _, err := h.super.Exec(ctx, `
		INSERT INTO app.messages (id, user_id, content_hash, thread_id, subject,
		    from_address, received_at, size_bytes)
		VALUES ($1, $2, $3, $4, 'Delivery Status Notification',
		        'mailer-daemon@example.com', now(), 500)`,
		id.String(), u.ID, hash[:], threadID); err != nil {
		t.Fatalf("seed dsn message: %v", err)
	}
	return id
}

// seededMessageID returns the Message-ID header stored for the fixture user's
// seeded message (bracketed, as fixtures_test.go stores it) and the bracket-
// stripped form the DSN would carry.
func seededMessageID(tag string) (stripped string) {
	return "seed-" + tag + "@example.com"
}

func TestRecordDSNLinksMessageSubmissionAndChangelog(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	dsnMsg := seedDSNMessage(t, ctx, h, a)

	err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		before, err := tx.ChangeState(ctx, ObjectEmailSubmission)
		if err != nil {
			return err
		}

		// The fixture seeds one bare dsn_events row for this message already, so
		// assert against the DELTA our recording adds rather than an absolute count.
		msgID := mustULID(t, a.MessageID)
		beforeEvents, err := tx.DSNEventsForMessage(ctx, msgID)
		if err != nil {
			return err
		}

		recorded, err := tx.RecordDSN(ctx, dsnMsg, seededMessageID(a.Tag), []DSNRecipient{{
			Recipient:  "friend@example.com",
			Action:     "failed",
			Status:     "5.1.1",
			Diagnostic: "550 5.1.1 user unknown",
		}})
		if err != nil {
			t.Fatalf("RecordDSN: %v", err)
		}
		if recorded != 1 {
			t.Fatalf("recorded = %d, want 1", recorded)
		}

		// A linked bounce owes exactly one EmailSubmission `updated` (deliveryStatus
		// changed) — the invariant, in this same transaction.
		after, err := tx.ChangeState(ctx, ObjectEmailSubmission)
		if err != nil {
			return err
		}
		if after != before+1 {
			t.Errorf("EmailSubmission change delta = %d, want 1", after-before)
		}

		// The event links to the sent message and its submission. Find OUR event
		// (the one carrying dsnMsg) among those on the message.
		events, err := tx.DSNEventsForMessage(ctx, msgID)
		if err != nil {
			return err
		}
		if len(events) != len(beforeEvents)+1 {
			t.Fatalf("DSNEventsForMessage grew by %d, want 1", len(events)-len(beforeEvents))
		}
		var e *DSNEvent
		for i := range events {
			if events[i].DSNMessageID != nil && *events[i].DSNMessageID == dsnMsg {
				e = &events[i]
			}
		}
		if e == nil {
			t.Fatalf("recorded event carrying dsn_message_id %s not found", dsnMsg)
		}
		if e.Action != "failed" || e.Status != "5.1.1" {
			t.Errorf("event = %s/%s, want failed/5.1.1", e.Action, e.Status)
		}
		if e.OriginSubmissionID == nil {
			t.Fatal("origin_submission_id was not resolved")
		}

		// And it is reachable from the submission side (the deliveryStatus feed).
		subEvents, err := tx.DSNEventsForSubmission(ctx, *e.OriginSubmissionID)
		if err != nil {
			return err
		}
		if len(subEvents) != 1 {
			t.Errorf("DSNEventsForSubmission = %d rows, want 1", len(subEvents))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("tx: %v", err)
	}
}

func TestRecordDSNUnlinkedRecordsWithoutChangelog(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	dsnMsg := seedDSNMessage(t, ctx, h, a)

	err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		before, err := tx.ChangeState(ctx, ObjectEmailSubmission)
		if err != nil {
			return err
		}
		// A bounce whose original Message-ID matches nothing we sent: still recorded
		// (queryable delivery health), but it mutates no JMAP-visible object, so it
		// owes NO change_log row.
		recorded, err := tx.RecordDSN(ctx, dsnMsg, "unknown-nothing@example.com", []DSNRecipient{{
			Recipient: "someone@example.net", Action: "failed", Status: "5.0.0",
		}})
		if err != nil {
			t.Fatalf("RecordDSN: %v", err)
		}
		if recorded != 1 {
			t.Fatalf("recorded = %d, want 1", recorded)
		}
		after, err := tx.ChangeState(ctx, ObjectEmailSubmission)
		if err != nil {
			return err
		}
		if after != before {
			t.Errorf("unlinked bounce appended %d change rows, want 0", after-before)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("tx: %v", err)
	}

	// The row exists with NULL origins (verify via superuser, bypassing the
	// message/submission links the read methods require).
	var n int
	if err := h.super.QueryRow(ctx, `
		SELECT count(*) FROM app.dsn_events
		 WHERE dsn_message_id = $1 AND origin_message_id IS NULL
		   AND origin_submission_id IS NULL AND action = 'failed'`,
		dsnMsg.String()).Scan(&n); err != nil {
		t.Fatalf("count unlinked: %v", err)
	}
	if n != 1 {
		t.Errorf("unlinked dsn_events rows = %d, want 1", n)
	}
}

func TestRecordDSNFailureDoesNotPoisonTx(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)

	err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		// A DSN message id that references no app.messages row makes every recipient
		// insert fail the dsn_message_id foreign key. Each failure must be caught at
		// its SAVEPOINT so the surrounding transaction stays usable (SQLSTATE 25P02
		// otherwise). RecordDSN reports nothing recorded, without error.
		recorded, err := tx.RecordDSN(ctx, NewULID(), seededMessageID(a.Tag), []DSNRecipient{{
			Recipient: "x@example.net", Action: "failed", Status: "5.1.1",
		}})
		if err != nil {
			t.Fatalf("RecordDSN returned an error instead of containing the failure: %v", err)
		}
		if recorded != 0 {
			t.Fatalf("recorded = %d, want 0 (all inserts should have failed the FK)", recorded)
		}

		// The transaction must still be usable — the whole point of the savepoints.
		if _, err := tx.CreateLabel(ctx, LabelRow{Name: "post-dsn-" + NewULID().String()}); err != nil {
			t.Fatalf("transaction was poisoned by the DSN failure: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("tx: %v", err)
	}
}
