// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

import (
	"context"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
)

// MessageAuthRefsAfter carries received_at for `auth-report --authserv`'s
// month buckets (v1.90.1), and stays scoped to the caller.
func TestMessageAuthRefsAfterCarriesReceivedAt(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, b := seedTwoUsers(t, h.super)
	when := time.Date(2025, 3, 14, 9, 0, 0, 0, time.UTC)
	if _, err := h.super.Exec(ctx, `UPDATE app.messages SET received_at = $2 WHERE id = $1`, a.MessageID, when); err != nil {
		t.Fatal(err)
	}
	var page []MessageAuthRef
	if err := h.appStore().UserTx(ctx, mustULID(t, a.ID), func(tx *UserTx) (err error) {
		page, err = tx.MessageAuthRefsAfter(ctx, ulid.ULID{}, 100)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range page {
		if r.ID.String() == b.MessageID {
			t.Fatal("A's walk returned B's message")
		}
		if r.ID.String() == a.MessageID {
			found = true
			if !r.ReceivedAt.Equal(when) {
				t.Fatalf("ReceivedAt = %v, want %v", r.ReceivedAt, when)
			}
		}
	}
	if !found {
		t.Fatal("CONTROL: A's own message is missing from A's walk")
	}
}
