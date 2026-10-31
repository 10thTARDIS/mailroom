// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

import (
	"context"
	"crypto/sha256"
	"testing"

	"github.com/oklog/ulid/v2"
)

// LabelTrainingRows is the classifier's entire view of the evidence, so its
// sampling contract is pinned against a real database (#169):
//
//   - positives are hand-applied ONLY;
//   - negatives never include the label itself (any provenance) or trash;
//   - the negative sample interleaves the two pools — mail hand-labelled with
//     some OTHER label (hard, uncontaminated by construction) and unlabelled
//     mail (soft) — half-and-half up to the bound;
//   - the whole result is deterministic, row order included, because fold
//     assignment and the naive-split diagnostic read that order.
//
// The old query took the N most recent unlabelled messages, which both skewed
// negatives toward the present (positives span all time) and maximised
// contamination (recent unlabelled mail is where the not-yet-labelled
// positives live).
func TestLabelTrainingRowsSampling(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	u := seedUser(t, h.super, "ltr-"+NewULID().String())
	st := h.appStore()

	// Message identity is (user_id, content_hash) with an FK onto blobs, so
	// every message needs a distinct hash and its own blob row.
	newMsg := func(from string, trashed bool) string {
		id := NewULID().String()
		hash := sha256.Sum256([]byte("ltr-msg-" + id))
		if _, err := h.super.Exec(ctx, `
			INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id)
			VALUES ($1, $2, 10, 42, $3)`, u.ID, hash[:], NewULID().String()); err != nil {
			t.Fatalf("insert blob: %v", err)
		}
		trash := "NULL"
		if trashed {
			trash = "now()"
		}
		if _, err := h.super.Exec(ctx, `
			INSERT INTO app.messages (id, user_id, content_hash, subject, from_address,
			       received_at, size_bytes, trashed_at)
			VALUES ($1, $2, $3, 'ltr', $4, now(), 10, `+trash+`)`,
			id, u.ID, hash[:], from); err != nil {
			t.Fatalf("insert message: %v", err)
		}
		return id
	}
	label := func(msgID, labelID, by string) {
		if _, err := h.super.Exec(ctx, `
			INSERT INTO app.message_labels (user_id, message_id, label_id, applied_by)
			VALUES ($1, $2, $3, $4)`, u.ID, msgID, labelID, by); err != nil {
			t.Fatalf("insert message_label: %v", err)
		}
	}
	labelX, labelY := NewULID().String(), NewULID().String()
	for _, l := range []struct{ id, name string }{{labelX, "X"}, {labelY, "Y"}} {
		if _, err := h.super.Exec(ctx, `
			INSERT INTO app.labels (id, user_id, name) VALUES ($1, $2, $3)`,
			l.id, u.ID, "ltr-"+l.name+"-"+u.Tag); err != nil {
			t.Fatalf("insert label: %v", err)
		}
	}

	// 3 hand-applied positives for X.
	positives := map[string]bool{}
	for i := 0; i < 3; i++ {
		id := newMsg("deals@shop.example", false)
		label(id, labelX, LabelByUser)
		positives[id] = true
	}
	// One X applied by the model: NOT a positive (self-confirmation loop) and
	// NOT a negative (it carries the label) — it must appear nowhere.
	mlApplied := newMsg("deals@shop.example", false)
	label(mlApplied, labelX, LabelByML)
	// 4 hand-labelled with Y: the hard-negative pool (the seeded fixture
	// message carries a hand label too, making 5 hard candidates).
	hard := map[string]bool{u.MessageID: true}
	for i := 0; i < 4; i++ {
		id := newMsg("news@other.example", false)
		label(id, labelY, LabelByUser)
		hard[id] = true
	}
	// 6 unlabelled: the soft pool. Plus one trashed, which must never appear.
	soft := map[string]bool{}
	for i := 0; i < 6; i++ {
		soft[newMsg("misc@plain.example", false)] = true
	}
	trashed := newMsg("misc@plain.example", true)

	var rows []LabelTrainingRow
	lid := ulid.MustParse(labelX)
	if err := st.UserTx(ctx, ulid.MustParse(u.ID), func(tx *UserTx) error {
		var e error
		rows, e = tx.LabelTrainingRows(ctx, lid, 6)
		return e
	}); err != nil {
		t.Fatalf("LabelTrainingRows: %v", err)
	}

	var gotPos, gotHard, gotSoft int
	for _, r := range rows {
		id := r.MessageID.String()
		if id == mlApplied {
			t.Errorf("the model-applied X message %s came back (positive=%v)", id, r.Positive)
		}
		if id == trashed {
			t.Errorf("a trashed message came back as evidence")
		}
		switch {
		case r.Positive:
			gotPos++
			if !positives[id] {
				t.Errorf("message %s reported positive but was not hand-labelled X", id)
			}
			if r.SenderDomain != "shop.example" {
				t.Errorf("positive sender domain = %q, want shop.example", r.SenderDomain)
			}
		case hard[id]:
			gotHard++
		case soft[id]:
			gotSoft++
		default:
			t.Errorf("negative %s is in neither pool", id)
		}
	}
	if gotPos != 3 {
		t.Errorf("positives = %d, want 3 (hand-applied only)", gotPos)
	}
	// Both pools exceed half the bound, so the interleave takes exactly half
	// from each: 3 hard + 3 soft of the limit 6.
	if gotHard != 3 || gotSoft != 3 {
		t.Errorf("negatives = %d hard + %d soft, want 3 + 3 — the sample must mix uncontaminated and corpus-shaped negatives", gotHard, gotSoft)
	}

	// Determinism, order included: the same evidence must give the same rows.
	var again []LabelTrainingRow
	if err := st.UserTx(ctx, ulid.MustParse(u.ID), func(tx *UserTx) error {
		var e error
		again, e = tx.LabelTrainingRows(ctx, lid, 6)
		return e
	}); err != nil {
		t.Fatalf("LabelTrainingRows again: %v", err)
	}
	if len(again) != len(rows) {
		t.Fatalf("second call returned %d rows, first %d", len(again), len(rows))
	}
	for i := range rows {
		if rows[i] != again[i] {
			t.Errorf("row %d differs between calls: %+v vs %+v — the sample is not reproducible from the evidence", i, rows[i], again[i])
		}
	}
}
