// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// Store-layer integration tests for Web Push device registration (mailroom#42,
// ADR-016). Proves the upsert-on-(user_id,endpoint) semantics, that the active
// listing excludes expired rows, and that mark-expired and delete behave. Runs
// against the throwaway Postgres from scripts/integration-test.sh; skips absent.

import (
	"context"
	"testing"
)

func TestPushSubscriptionUpsertAndLifecycle(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	u := seedUser(t, h.super, "push-"+NewULID().String())
	s := h.appStore()
	uID := mustULID(t, u.ID)

	// seedUser seeds one push_subscription for the RLS fixture inventory; this
	// test asserts EXACT counts, so start this user from a clean slate.
	if _, err := h.super.Exec(ctx, `DELETE FROM app.push_subscriptions WHERE user_id=$1`, u.ID); err != nil {
		t.Fatal(err)
	}

	const endpoint = "https://push.example.com/send/device-1"

	// First registration inserts one row.
	var firstID string
	if err := s.UserTx(ctx, uID, func(tx *UserTx) error {
		vip := PushModeVIP
		ps, err := tx.CreatePushSubscription(ctx, PushSubscriptionInput{
			Endpoint:   endpoint,
			KeysP256dh: "p256dh-v1",
			KeysAuth:   "auth-v1",
			DeviceName: "Phone",
			NotifyMode: &vip,
		})
		firstID = ps.ID.String()
		return err
	}); err != nil {
		t.Fatal(err)
	}

	// Re-subscribing the SAME endpoint upserts: one row, same id, updated fields.
	var secondID string
	if err := s.UserTx(ctx, uID, func(tx *UserTx) error {
		// No NotifyMode: the browser re-subscribing on "turn on" must not
		// reset the mode the user chose (#59).
		ps, err := tx.CreatePushSubscription(ctx, PushSubscriptionInput{
			Endpoint:   endpoint,
			KeysP256dh: "p256dh-v2",
			KeysAuth:   "auth-v2",
			DeviceName: "Phone (renamed)",
		})
		secondID = ps.ID.String()
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if secondID != firstID {
		t.Errorf("upsert changed the id: %s -> %s (want stable)", firstID, secondID)
	}

	var count int
	if err := h.super.QueryRow(ctx,
		`SELECT count(*) FROM app.push_subscriptions WHERE user_id=$1`, u.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("row count after re-subscribe = %d, want 1 (upsert, not duplicate)", count)
	}

	// The active listing reflects the updated fields.
	if err := s.UserTx(ctx, uID, func(tx *UserTx) error {
		subs, err := tx.ListActivePushSubscriptions(ctx)
		if err != nil {
			return err
		}
		if len(subs) != 1 {
			t.Fatalf("active subs = %d, want 1", len(subs))
		}
		got := subs[0]
		if got.KeysP256dh != "p256dh-v2" || got.KeysAuth != "auth-v2" {
			t.Errorf("keys not updated by upsert: %+v", got)
		}
		if got.DeviceName != "Phone (renamed)" {
			t.Errorf("metadata not updated by upsert: %+v", got)
		}
		if got.NotifyMode != PushModeVIP {
			t.Errorf("re-subscribe without a mode reset it to %q, want the chosen %q", got.NotifyMode, PushModeVIP)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// Mark expired: the row leaves the active listing but survives (ALL listing).
	if err := s.UserTx(ctx, uID, func(tx *UserTx) error {
		return tx.MarkPushSubscriptionExpired(ctx, mustULID(t, firstID))
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.UserTx(ctx, uID, func(tx *UserTx) error {
		active, err := tx.ListActivePushSubscriptions(ctx)
		if err != nil {
			return err
		}
		if len(active) != 0 {
			t.Errorf("active subs after expire = %d, want 0", len(active))
		}
		all, err := tx.ListPushSubscriptions(ctx)
		if err != nil {
			return err
		}
		if len(all) != 1 || all[0].ExpiredAt == nil {
			t.Errorf("all subs = %+v, want one row with expiredAt set", all)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// Delete removes it entirely; a second delete reports not-found.
	if err := s.UserTx(ctx, uID, func(tx *UserTx) error {
		found, err := tx.DeletePushSubscription(ctx, mustULID(t, firstID))
		if err != nil {
			return err
		}
		if !found {
			t.Error("first delete reported not-found")
		}
		found, err = tx.DeletePushSubscription(ctx, mustULID(t, firstID))
		if err != nil {
			return err
		}
		if found {
			t.Error("second delete reported found")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
