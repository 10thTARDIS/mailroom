// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// Integration suite for the feedback WRITE side (spec §8, mailroom#33 PR2):
// InsertFeedbackEvent's plain insert, the SAVEPOINT-guarded EmitFeedbackEvent
// (a forced feedback failure must NOT poison the surrounding mutation),
// MessageSeen, OriginalMessageForSend's reply-target resolution, and cross-user
// isolation. Runs against the throwaway Postgres from scripts/integration-test.sh;
// skips when the env is absent (harness_test.go). feedback_events isolation is
// additionally covered generically by the negative RLS suite (it is in appTables).

import (
	"context"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
)

// seedDraft inserts a draft message (with its own blob, to satisfy the messages
// -> blobs FK and the 32-byte content_hash domain) that replies to inReplyTo.
// Returns the draft's id.
func seedDraft(t *testing.T, h *harness, userID, fromAddr, salt, inReplyTo string) string {
	t.Helper()
	ctx := context.Background()
	hash := sha256.Sum256([]byte("draft-" + salt))
	if _, err := h.super.Exec(ctx,
		`INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id)
		 VALUES ($1, $2, 10, 20, $3)`,
		userID, hash[:], NewULID().String()); err != nil {
		t.Fatalf("seed draft blob: %v", err)
	}
	draftID := NewULID().String()
	if _, err := h.super.Exec(ctx, `
		INSERT INTO app.messages (id, user_id, content_hash, subject, from_address,
		        received_at, size_bytes, is_draft, in_reply_to)
		VALUES ($1, $2, $3, 'Re: seed', $4, now(), 10, true, $5)`,
		draftID, userID, hash[:], fromAddr, inReplyTo); err != nil {
		t.Fatalf("seed draft: %v", err)
	}
	return draftID
}

// feedbackRow reads the latest feedback_events row for a (user, action) directly
// as the superuser — an independent check of what the store wrote.
func feedbackRows(t *testing.T, h *harness, userID, action string) int {
	t.Helper()
	var n int
	if err := h.super.QueryRow(context.Background(), `
		SELECT count(*) FROM app.feedback_events
		 WHERE user_id = $1 AND action = $2`,
		userID, action).Scan(&n); err != nil {
		t.Fatalf("read feedback_events: %v", err)
	}
	return n
}

// InsertFeedbackEvent writes a message-tied row (no change_log), and rejects a
// zero message id (every action Mailroom emits is message-tied).
func TestInsertFeedbackEvent(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	msgID := mustULID(t, a.MessageID)

	// A distinctive action so the count is unambiguous (seedUser writes 'open').
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		return tx.InsertFeedbackEvent(ctx, msgID, "star", nil)
	}); err != nil {
		t.Fatalf("insert feedback: %v", err)
	}
	if got := feedbackRows(t, h, a.ID, "star"); got != 1 {
		t.Errorf("star feedback rows = %d, want 1", got)
	}

	// A zero message id is rejected (nullable in schema, but our actions are all
	// message-tied).
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		return tx.InsertFeedbackEvent(ctx, ulid.ULID{}, "star", nil)
	}); err == nil {
		t.Error("InsertFeedbackEvent with a zero message id should error")
	}

	// No change_log row is emitted by a feedback write: the Email seq is unchanged
	// by the insert above (seedUser left it at 2).
	var seq int64
	if err := h.super.QueryRow(ctx,
		`SELECT last_seq FROM app.change_seq WHERE user_id = $1 AND object_type = 'Email'`,
		a.ID).Scan(&seq); err != nil {
		t.Fatalf("read change_seq: %v", err)
	}
	if seq != 2 {
		t.Errorf("Email change seq = %d, want 2 (a feedback write appends no change_log)", seq)
	}
}

// A forced feedback-insert failure inside a mutation UserTx rolls back only to
// the savepoint: the user's action (the mark-read, and a valid feedback emit
// after the failure) still commits. This is the mail-safety guarantee — a broken
// feedback write can never abort the Email/set it rides along with.
func TestEmitFeedbackEventSavepointGuard(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	msgID := mustULID(t, a.MessageID)
	bogus := ulid.Make() // a message id that does not exist -> FK violation on emit

	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		// 1. The user's real action: mark read.
		if _, _, err := tx.ApplyEmailUpdate(ctx, msgID, EmailUpdate{Seen: boolp(true)}); err != nil {
			return err
		}
		// 2. A POISON emit: a non-existent message id violates the message_id FK.
		//    The savepoint must contain the failure and leave the tx usable.
		if err := tx.EmitFeedbackEvent(ctx, bogus, "trash", nil); err == nil {
			t.Error("EmitFeedbackEvent against a non-existent message should fail")
		}
		// 3. Proof the tx survived the failure: a valid emit still works.
		return tx.EmitFeedbackEvent(ctx, msgID, "mark_read_no_open", nil)
	}); err != nil {
		t.Fatalf("mutation tx failed (savepoint did not contain the feedback failure): %v", err)
	}

	// The mark-read committed (not rolled back by the poison emit)...
	var seen bool
	if err := h.super.QueryRow(ctx,
		`SELECT is_seen FROM app.messages WHERE user_id = $1 AND id = $2`,
		a.ID, a.MessageID).Scan(&seen); err != nil {
		t.Fatalf("read is_seen: %v", err)
	}
	if !seen {
		t.Error("mark-read was rolled back by a feedback-insert failure — savepoint guard broken")
	}
	// ...and the valid post-failure emit committed, while the poison one did not.
	if got := feedbackRows(t, h, a.ID, "mark_read_no_open"); got != 1 {
		t.Errorf("mark_read_no_open feedback rows = %d, want 1 (valid emit after the poison)", got)
	}
	if got := feedbackRows(t, h, a.ID, "trash"); got != 0 {
		t.Errorf("trash feedback rows = %d, want 0 (the poison emit must leave nothing)", got)
	}
}

// MessageSeen reflects the message's read state, and misses cleanly for a message
// the caller does not own.
func TestMessageSeen(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	a, b := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	bID := mustULID(t, b.ID)
	msgID := mustULID(t, a.MessageID)

	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		seen, found, err := tx.MessageSeen(ctx, msgID)
		if err != nil {
			return err
		}
		if !found {
			t.Error("a's own message not found by MessageSeen")
		}
		if seen {
			t.Error("freshly seeded message reported as seen")
		}
		return nil
	}); err != nil {
		t.Fatalf("message seen (a): %v", err)
	}

	// b cannot see a's message: found is false (RLS), not an error.
	if err := s.UserTx(ctx, bID, func(tx *UserTx) error {
		_, found, err := tx.MessageSeen(ctx, msgID)
		if err != nil {
			return err
		}
		if found {
			t.Error("b's MessageSeen found a's message — RLS breach")
		}
		return nil
	}); err != nil {
		t.Fatalf("message seen (b): %v", err)
	}
}

// OriginalMessageForSend resolves the message a reply answers via the draft's
// In-Reply-To header (bracket-insensitive), and returns ok=false when the draft
// references nothing this user has.
func TestOriginalMessageForSend(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)

	// The seeded message is the ORIGINAL; its Message-ID is '<seed-<tag>@example.com>'.
	// Seed a draft that replies to it, storing the BARE form to exercise the
	// bracket-normalizing match.
	bareOriginalID := "seed-" + a.Tag + "@example.com"
	draftID := seedDraft(t, h, a.ID, a.Tag+"@example.com", "reply-"+a.Tag, bareOriginalID)

	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		got, ok, err := tx.OriginalMessageForSend(ctx, mustULID(t, draftID))
		if err != nil {
			return err
		}
		if !ok {
			t.Fatal("reply target not resolved from In-Reply-To")
		}
		if got.String() != a.MessageID {
			t.Errorf("resolved original = %s, want %s", got, a.MessageID)
		}
		return nil
	}); err != nil {
		t.Fatalf("resolve reply target: %v", err)
	}

	// A draft that references nothing resolvable -> ok=false (skip, never guess).
	orphanID := seedDraft(t, h, a.ID, a.Tag+"@example.com", "orphan-"+a.Tag, "no-such@nowhere.test")
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		_, ok, err := tx.OriginalMessageForSend(ctx, mustULID(t, orphanID))
		if err != nil {
			return err
		}
		if ok {
			t.Error("a draft referencing an unknown message must not resolve a target")
		}
		return nil
	}); err != nil {
		t.Fatalf("resolve orphan: %v", err)
	}
}

// ListFeedbackEvents is RLS-scoped: a's distinctive feedback never appears in b's
// window.
func TestFeedbackEventIsolation(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	a, b := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	bID := mustULID(t, b.ID)

	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		return tx.InsertFeedbackEvent(ctx, mustULID(t, a.MessageID), "spam", nil)
	}); err != nil {
		t.Fatalf("a insert feedback: %v", err)
	}

	since := time.Now().Add(-24 * time.Hour)
	until := time.Now().Add(24 * time.Hour)
	if err := s.UserTx(ctx, bID, func(tx *UserTx) error {
		events, err := tx.ListFeedbackEvents(ctx, since, until)
		if err != nil {
			return err
		}
		for _, e := range events {
			if e.Action == "spam" {
				t.Error("b saw a's 'spam' feedback event — RLS isolation breach")
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("b list feedback: %v", err)
	}
}

// boolp is a local *bool helper for building EmailUpdate patches in tests.
func boolp(b bool) *bool { return &b }
