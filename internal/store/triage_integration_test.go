// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// Integration suite for the T1 triage store surface (spec §8, mailroom#33):
// UpsertTriageScore insert-then-overwrite on the (user, message) PK,
// ContactByAddress hit/miss, the priorityScore projection on the JMAP Email
// view, and cross-user isolation on all of it. Runs against the throwaway
// Postgres from scripts/integration-test.sh; skips when the env is absent
// (harness_test.go). RLS-table isolation is additionally covered generically by
// the negative RLS suite (triage_scores is in appTables).

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/oklog/ulid/v2"
)

// triageRow reads a triage_scores row directly as the superuser (an independent
// check of what the store wrote).
func triageRow(t *testing.T, h *harness, userID, msgID string) (score float64, version, topFeatures string, found bool) {
	t.Helper()
	err := h.super.QueryRow(context.Background(), `
		SELECT score, model_version, top_features::text
		  FROM app.triage_scores
		 WHERE user_id = $1 AND message_id = $2`,
		userID, msgID).Scan(&score, &version, &topFeatures)
	if err != nil {
		if isNoRows(err) {
			return 0, "", "", false
		}
		t.Fatalf("read triage_scores: %v", err)
	}
	return score, version, topFeatures, true
}

func approxEqual(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 1e-5
}

// viewScore reads a message through the JMAP Email view (GetEmails' triage
// LEFT JOIN) and returns its PriorityScore, plus whether the message was
// returned to this user at all.
func viewScore(t *testing.T, s *Store, userID, msgID ulid.ULID) (score *float64, present bool) {
	t.Helper()
	if err := s.UserTx(context.Background(), userID, func(tx *UserTx) error {
		views, err := tx.GetEmails(context.Background(), []ulid.ULID{msgID})
		if err != nil {
			return err
		}
		for _, v := range views {
			if v.ID == msgID {
				score = v.PriorityScore
				present = true
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("get emails: %v", err)
	}
	return score, present
}

// UpsertTriageScore inserts on first call and OVERWRITES on the (user, message)
// PK on the second — one row, latest values win — and the score surfaces on the
// JMAP Email view. A nil/empty top_features is stored as the empty array.
func TestUpsertTriageScoreInsertThenOverwrite(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	msgID := mustULID(t, a.MessageID)

	// seedUser pre-seeds a score for this message; clear it so we exercise a
	// genuine INSERT, then an overwrite.
	if _, err := h.super.Exec(ctx,
		`DELETE FROM app.triage_scores WHERE user_id = $1 AND message_id = $2`, a.ID, a.MessageID); err != nil {
		t.Fatalf("clear seeded score: %v", err)
	}

	// INSERT (nil top_features -> stored as []).
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		return tx.UpsertTriageScore(ctx, msgID, 0.8, "priority:0", nil)
	}); err != nil {
		t.Fatalf("insert score: %v", err)
	}
	score, version, top, found := triageRow(t, h, a.ID, a.MessageID)
	if !found {
		t.Fatal("no triage_scores row after insert")
	}
	if !approxEqual(score, 0.8) {
		t.Errorf("score = %v, want 0.8", score)
	}
	if version != "priority:0" {
		t.Errorf("model_version = %q, want priority:0", version)
	}
	if top != "[]" {
		t.Errorf("top_features = %q, want []", top)
	}

	// The score surfaces on the JMAP Email view.
	if sc, present := viewScore(t, s, aID, msgID); !present || sc == nil || !approxEqual(*sc, 0.8) {
		t.Errorf("Email view PriorityScore = %v (present=%v), want 0.8", sc, present)
	}

	// OVERWRITE the same PK.
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		return tx.UpsertTriageScore(ctx, msgID, 0.2, "priority:1", []byte(`[{"k":"known:1"}]`))
	}); err != nil {
		t.Fatalf("overwrite score: %v", err)
	}
	score, version, _, _ = triageRow(t, h, a.ID, a.MessageID)
	if !approxEqual(score, 0.2) {
		t.Errorf("score after overwrite = %v, want 0.2", score)
	}
	if version != "priority:1" {
		t.Errorf("model_version after overwrite = %q, want priority:1", version)
	}

	// Exactly one row for the PK (overwrite, not a second insert).
	var n int
	if err := h.super.QueryRow(ctx,
		`SELECT count(*) FROM app.triage_scores WHERE user_id = $1 AND message_id = $2`,
		a.ID, a.MessageID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("triage rows for PK = %d, want 1", n)
	}
}

// A scored message carries its stored top_features JSON (the #34 explainability
// payload) verbatim onto the JMAP Email view, round-tripping to the same
// per-feature contributions; an unscored message carries nil.
func TestTriageTopFeaturesOnView(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	msgID := mustULID(t, a.MessageID)

	payload := []byte(`[{"key":"known:1","weight":1.6},{"key":"list:1","weight":-1.3}]`)
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		return tx.UpsertTriageScore(ctx, msgID, 0.7, "priority:0", payload)
	}); err != nil {
		t.Fatalf("score message: %v", err)
	}

	var top []byte
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		views, err := tx.GetEmails(ctx, []ulid.ULID{msgID})
		if err != nil {
			return err
		}
		for _, v := range views {
			if v.ID == msgID {
				top = v.TopFeatures
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("get emails: %v", err)
	}

	if len(top) == 0 {
		t.Fatal("Email view TopFeatures empty; want the stored top_features array")
	}
	var got []struct {
		Key    string  `json:"key"`
		Weight float64 `json:"weight"`
	}
	if err := json.Unmarshal(top, &got); err != nil {
		t.Fatalf("decode view TopFeatures %q: %v", top, err)
	}
	if len(got) != 2 || got[0].Key != "known:1" || got[1].Key != "list:1" {
		t.Errorf("view TopFeatures = %+v, want known:1 then list:1", got)
	}
	if !approxEqual(got[0].Weight, 1.6) || !approxEqual(got[1].Weight, -1.3) {
		t.Errorf("view TopFeatures weights = %v / %v, want 1.6 / -1.3", got[0].Weight, got[1].Weight)
	}
}

// A message user b cannot see (RLS) carries no triage score into b's Email view,
// even after a scores it — the score is isolated with the message.
func TestTriageScoreIsolation(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	a, b := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	bID := mustULID(t, b.ID)
	aMsg := mustULID(t, a.MessageID)

	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		return tx.UpsertTriageScore(ctx, aMsg, 0.9, "priority:0", nil)
	}); err != nil {
		t.Fatalf("score a's message: %v", err)
	}

	// a sees the score; b cannot even see a's message.
	if sc, present := viewScore(t, s, aID, aMsg); !present || sc == nil {
		t.Errorf("a should see its own scored message; got present=%v score=%v", present, sc)
	}
	if _, present := viewScore(t, s, bID, aMsg); present {
		t.Error("b's Email view returned a's message — RLS isolation breach")
	}
}

// ContactByAddress reports the sender's contact stats (auto-derived address
// book), is case/whitespace insensitive, misses cleanly for an unknown address,
// and is RLS-scoped to the caller.
func TestContactByAddress(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	a, b := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)

	// seedUser gives every user a contact 'friend@example.com' with seen_count 1.
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		found, seen, err := tx.ContactByAddress(ctx, "  Friend@Example.com ")
		if err != nil {
			return err
		}
		if !found {
			t.Error("known contact not found (case/space-insensitive lookup failed)")
		}
		if seen != 1 {
			t.Errorf("seen_count = %d, want 1", seen)
		}
		// A never-seen address misses.
		found, _, err = tx.ContactByAddress(ctx, "stranger-"+a.ID+"@nowhere.test")
		if err != nil {
			return err
		}
		if found {
			t.Error("unknown address reported as a known contact")
		}
		return nil
	}); err != nil {
		t.Fatalf("contact lookup: %v", err)
	}

	// Isolation: a b-only correspondent is invisible to a.
	bOnly := "bonly-" + b.ID + "@corp.test"
	if _, err := h.super.Exec(ctx, `
		INSERT INTO app.contact_addresses (id, user_id, contact_id, address, seen_count)
		SELECT $1, $2::app.ulid, id, $3, 7 FROM app.contacts WHERE user_id = $2::app.ulid LIMIT 1`,
		NewULID().String(), b.ID, bOnly); err != nil {
		t.Fatalf("seed b-only contact: %v", err)
	}
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		found, _, err := tx.ContactByAddress(ctx, bOnly)
		if err != nil {
			return err
		}
		if found {
			t.Error("a saw b's contact address — RLS isolation breach")
		}
		return nil
	}); err != nil {
		t.Fatalf("isolation lookup: %v", err)
	}
}
