// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

import (
	"context"
	"testing"

	"github.com/oklog/ulid/v2"
)

// MessageFromRefsAfter (#754): the walk carries LearnedPlaced only for a label
// learned placement applied, and never another user's messages.
func TestMessageFromRefsAfter(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, b := seedTwoUsers(t, h.super)
	s := h.appStore()

	walk := func(user string) map[string]MessageFromRef {
		t.Helper()
		out := map[string]MessageFromRef{}
		after := ulid.ULID{}
		for {
			var page []MessageFromRef
			if err := s.UserTx(ctx, mustULID(t, user), func(tx *UserTx) (err error) {
				page, err = tx.MessageFromRefsAfter(ctx, after, 2)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			for _, r := range page {
				out[r.ID.String()] = r
				after = r.ID
			}
			if len(page) < 2 {
				return out
			}
		}
	}

	// CONTROL: the fixture's label was applied by the user, not learned.
	got := walk(a.ID)
	if r, ok := got[a.MessageID]; !ok || r.LearnedPlaced {
		t.Fatalf("A's seeded message: %+v (present %v), want present and not learned", r, ok)
	}
	if _, ok := got[b.MessageID]; ok {
		t.Fatal("A's walk returned B's message")
	}

	if _, err := h.super.Exec(ctx,
		`UPDATE app.message_labels SET applied_by = 'learned' WHERE user_id = $1 AND message_id = $2`,
		a.ID, a.MessageID); err != nil {
		t.Fatal(err)
	}
	if r := walk(a.ID)[a.MessageID]; !r.LearnedPlaced {
		t.Fatalf("a learned label is not reported: %+v", r)
	}
	// B's walk sees B's message and nothing of A's.
	gb := walk(b.ID)
	if _, ok := gb[a.MessageID]; ok {
		t.Fatal("B's walk returned A's message")
	}
	if _, ok := gb[b.MessageID]; !ok {
		t.Fatal("CONTROL: B's walk missed B's own message")
	}
}
