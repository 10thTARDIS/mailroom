// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// The JMAP surface of the priority inbox (spec §6/§8, mailroom#154): the
// isImportant filter condition, the `priority` sort that groups the buckets, and
// the Email properties a client reads to render and explain a ranking.
//
// The store suite covers the SQL. What is tested here is the contract a client
// sees — including the two answers that are easy to get wrong: an unscored
// message is not important (but still exists), and a priority-sorted query can
// never claim to compute a delta.

import (
	"context"
	"crypto/sha256"
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// scoreMessage writes a triage score as the owner, the way ingest would.
func (h *apiHarness) scoreMessage(t *testing.T, userID, msgID ulid.ULID, score float64, topFeatures string) {
	t.Helper()
	ctx := context.Background()
	if err := h.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		return tx.UpsertTriageScore(ctx, msgID, score, "priority:1", []byte(topFeatures))
	}); err != nil {
		t.Fatalf("score message: %v", err)
	}
}

func TestEmailQueryIsImportantFilter(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	// The seeded message scores above the cutoff.
	h.scoreMessage(t, a.UserID, a.MessageID, 0.92, "[]")

	yes, no := true, false
	important := call(t, h.api.emailQuery, a.UserID, map[string]any{
		"filter": map[string]any{"isImportant": yes},
	})
	ids := important["ids"].([]any)
	if len(ids) != 1 || ids[0] != a.MessageID.String() {
		t.Fatalf("isImportant:true = %v, want [%s]", ids, a.MessageID)
	}

	rest := call(t, h.api.emailQuery, a.UserID, map[string]any{
		"filter": map[string]any{"isImportant": no},
	})
	if n := len(rest["ids"].([]any)); n != 0 {
		t.Errorf("isImportant:false returned %d ids, want 0 — the only message is important", n)
	}

	// A priority-filtered query cannot compute a delta: the model rescores, and a
	// message crosses the threshold with no Email change_log row to replay.
	if important["canCalculateChanges"] != false {
		t.Errorf("canCalculateChanges = %v for an isImportant filter, want false",
			important["canCalculateChanges"])
	}
}

// AN UNSCORED MESSAGE IS NOT IMPORTANT — and must still be somewhere. Mail
// ingested before scoring was wired, or whose scoring failed, would otherwise be
// invisible in both halves of the inbox.
func TestEmailQueryUnscoredMailIsNotImportantButStillVisible(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)
	// Deliberately NOT scored.

	yes, no := true, false
	if n := len(call(t, h.api.emailQuery, a.UserID, map[string]any{
		"filter": map[string]any{"isImportant": yes},
	})["ids"].([]any)); n != 0 {
		t.Errorf("an unscored message was reported important (%d ids)", n)
	}
	ids := call(t, h.api.emailQuery, a.UserID, map[string]any{
		"filter": map[string]any{"isImportant": no},
	})["ids"].([]any)
	if len(ids) != 1 || ids[0] != a.MessageID.String() {
		t.Fatalf("unscored mail vanished from the not-important half: %v", ids)
	}
}

// The Priority view's order: the important bucket first, then everything else.
// The grouping is server-side because a client can only regroup the page it
// already holds.
func TestEmailQueryPrioritySortGroupsBuckets(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	// A second message, NEWER than the seeded one, scored below the cutoff — so a
	// plain date sort would put it first and only the bucket sort puts it second.
	// Its own content hash: that pair is the dedup identity (ADR-001), so a copy
	// of the first message's would be the same message.
	ctx := context.Background()
	newer := store.NewULID()
	hash := sha256.Sum256([]byte("newer-but-dull-" + newer.String()))
	if _, err := h.super.Exec(ctx, `
		INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id)
		VALUES ($1,$2,10,20,$3)`,
		a.UserID.String(), hash[:], store.NewULID().String()); err != nil {
		t.Fatalf("seed blob: %v", err)
	}
	if _, err := h.super.Exec(ctx, `
		INSERT INTO app.messages (id, user_id, content_hash, thread_id, subject, snippet,
		        from_name, from_address, received_at, size_bytes)
		VALUES ($1,$2,$3,$4,'newer but dull','x','Friend','friend@example.com',
		        now() + interval '1 hour', 100)`,
		newer.String(), a.UserID.String(), hash[:], a.ThreadID.String()); err != nil {
		t.Fatalf("seed newer message: %v", err)
	}
	h.scoreMessage(t, a.UserID, a.MessageID, 0.95, "[]") // older, important
	h.scoreMessage(t, a.UserID, newer, 0.10, "[]")       // newer, not

	res := call(t, h.api.emailQuery, a.UserID, map[string]any{
		"sort": []any{map[string]any{"property": "priority", "isAscending": false}},
	})
	ids := res["ids"].([]any)
	if len(ids) != 2 || ids[0] != a.MessageID.String() || ids[1] != newer.String() {
		t.Fatalf("priority order = %v, want the important-but-older message first", ids)
	}
	// Date order is the opposite, which is what makes the assertion above mean
	// something.
	byDate := call(t, h.api.emailQuery, a.UserID, map[string]any{})["ids"].([]any)
	if len(byDate) != 2 || byDate[0] != newer.String() {
		t.Fatalf("default order = %v, want the newer message first", byDate)
	}
	if res["canCalculateChanges"] != false {
		t.Errorf("canCalculateChanges = %v for a priority sort, want false", res["canCalculateChanges"])
	}
}

// The properties a client renders the ranking and its explanation from. They are
// opt-in: a client that does not name them does not pay for them.
func TestEmailGetPriorityProperties(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)
	h.scoreMessage(t, a.UserID, a.MessageID, 0.83,
		`[{"key":"corr:1","weight":1.8},{"key":"unsub:1","weight":-1.6}]`)

	res := call(t, h.api.emailGet, a.UserID, map[string]any{
		"ids":        []string{a.MessageID.String()},
		"properties": []string{"id", "priorityScore", "isImportant", "priorityReasons"},
	})
	obj := res["list"].([]any)[0].(map[string]any)
	if obj["isImportant"] != true {
		t.Errorf("isImportant = %v, want true for 0.83", obj["isImportant"])
	}
	if s, ok := obj["priorityScore"].(float64); !ok || s < 0.82 || s > 0.84 {
		t.Errorf("priorityScore = %v, want ~0.83", obj["priorityScore"])
	}
	reasons, ok := obj["priorityReasons"].([]any)
	if !ok || len(reasons) != 2 {
		t.Fatalf("priorityReasons = %v, want 2 entries", obj["priorityReasons"])
	}
	// Humanised, not raw feature keys — the panel renders these verbatim.
	first := reasons[0].(map[string]any)
	if first["label"] == "corr:1" {
		t.Error("priorityReasons leaked a raw feature key instead of a label")
	}
	if first["polarity"] != "up" {
		t.Errorf("first reason polarity = %v, want up (weight 1.8)", first["polarity"])
	}

	// Not requested ⇒ not present: the default property set stays cheap.
	plain := call(t, h.api.emailGet, a.UserID, map[string]any{
		"ids": []string{a.MessageID.String()},
	})
	if _, present := plain["list"].([]any)[0].(map[string]any)["priorityScore"]; present {
		t.Error("priorityScore returned without being requested")
	}
}
