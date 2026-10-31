// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// End-to-end proof that change_log compaction (mailroom#526,
// internal/store/changelog_compact.go) is exactly what a JMAP client
// experiences as "cannotCalculateChanges once, then a clean resync" — the
// designed recovery path ADR-003 promises, exercised through the real method
// handlers rather than the store directly.

import (
	"context"
	"testing"
	"time"

	"mailroom/internal/store"
)

func TestChangeLogCompactionForcesResyncThenRecovers(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)
	ctx := context.Background()

	// S0: the state a long-offline client last synced to. seedAPIUser always
	// leaves Email at seq 1 (one change_log row, the seeded message's
	// `created`) — the fixture's own documented shape.
	s0 := "s:1"

	// While that client was away, a second mutation happens (state 1 -> 2).
	res := call(t, h.api.emailSet, a.UserID, map[string]any{
		"update": map[string]any{a.MessageID.String(): map[string]any{"keywords/$seen": true}},
	})
	if res["newState"] != "s:2" {
		t.Fatalf("newState after mark-seen = %v, want s:2", res["newState"])
	}

	// Compact with a cutoff in the future: every row that exists right now
	// (seq 1 and 2) is eligible, so the boundary is the current state and the
	// floor advances past S0.
	cutoff := time.Now().UTC().Add(time.Hour)
	if err := h.store.UserTx(ctx, a.UserID, func(tx *store.UserTx) error {
		results, err := tx.CompactChangeLog(ctx, cutoff, 0, true)
		if err != nil {
			return err
		}
		for _, r := range results {
			if r.ObjectType == store.ObjectEmail && r.NewFloor < 2 {
				t.Fatalf("Email floor after compaction = %d, want >= 2 (S0=1 must fall below it)", r.NewFloor)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("compact: %v", err)
	}

	// The long-offline client's sinceState=S0 is now below the floor —
	// method-level cannotCalculateChanges, the designed fallback.
	_, mErr := h.api.emailChanges(ctx, a.UserID, mustJSON(t, map[string]any{"sinceState": s0}))
	if mErr == nil || mErr.Type != errCannotCalculateChanges {
		t.Fatalf("Email/changes(sinceState=%s) after compaction = %+v, want cannotCalculateChanges", s0, mErr)
	}

	// The client's designed reaction: refetch via Email/get, and adopt the
	// state it reports as the new baseline (S1).
	getRes := call(t, h.api.emailGet, a.UserID, map[string]any{"ids": []string{a.MessageID.String()}})
	s1, ok := getRes["state"].(string)
	if !ok || s1 == "" {
		t.Fatalf("Email/get returned no state: %v", getRes)
	}
	if s1 != "s:2" {
		t.Fatalf("S1 = %v, want s:2 (the floor, since nothing has changed since compaction)", s1)
	}

	// A genuinely new mutation after the resync (state 2 -> 3).
	res = call(t, h.api.emailSet, a.UserID, map[string]any{
		"update": map[string]any{a.MessageID.String(): map[string]any{"keywords/$flagged": true}},
	})
	if res["newState"] != "s:3" {
		t.Fatalf("newState after star = %v, want s:3", res["newState"])
	}

	// Email/changes from S1 must return EXACTLY that one change, never
	// cannotCalculateChanges again and never the change compaction already
	// erased history for.
	changes := call(t, h.api.emailChanges, a.UserID, map[string]any{"sinceState": s1})
	if changes["newState"] != "s:3" {
		t.Errorf("newState = %v, want s:3", changes["newState"])
	}
	updated := changes["updated"].([]any)
	if len(updated) != 1 || updated[0] != a.MessageID.String() {
		t.Errorf("updated = %v, want [%s]", updated, a.MessageID)
	}
	if created := changes["created"].([]any); len(created) != 0 {
		t.Errorf("created = %v, want empty (that history was compacted away, correctly, since S1 already covers it)", created)
	}
}
