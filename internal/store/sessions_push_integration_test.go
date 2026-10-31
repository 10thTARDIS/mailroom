// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

import (
	"context"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
)

// A renewal the provider refuses (invalid_grant) revokes the session through
// RenewSessionLocked's own branch, which must delete that session's push
// subscriptions in the same transaction as every other way a session ends. Only
// that session's subscription goes: another session's and a NULL-session one stay.
func TestRenewSessionLockedRefusalDeletesItsPush(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)

	refused := createSession(t, s, aID, "rp-refused", func(n *NewSession) { n.RefreshCT = []byte{0x01} })
	other := createSession(t, s, aID, "rp-other", nil)
	pRefused := pushSub(t, s, aID, "https://push.example.com/rp-refused", &refused)
	pOther := pushSub(t, s, aID, "https://push.example.com/rp-other", &other)
	pNone := pushSub(t, s, aID, "https://push.example.com/rp-none", nil)
	if _, err := h.super.Exec(ctx, `UPDATE app.sessions SET last_checked_at = now() - interval '1 hour' WHERE id = $1`, refused.String()); err != nil {
		t.Fatal(err)
	}

	// Positive control: a renewal that does NOT refuse leaves the subscription.
	var ran bool
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		var err error
		_, ran, err = tx.RenewSessionLocked(ctx, refused, time.Now().Add(-15*time.Minute),
			func(SessionRenewState) (SessionRenewWrite, error) {
				return SessionRenewWrite{Action: SessionRenewLeave}, nil
			})
		return err
	}); err != nil || !ran {
		t.Fatalf("leave: ran=%v err=%v", ran, err)
	}
	if ok, _ := pushSessionOf(t, h, pRefused.ID); !ok {
		t.Fatal("a renewal that wrote nothing deleted the subscription")
	}

	var st SessionRenewState
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		var err error
		st, ran, err = tx.RenewSessionLocked(ctx, refused, time.Now().Add(-15*time.Minute),
			func(SessionRenewState) (SessionRenewWrite, error) {
				return SessionRenewWrite{Action: SessionRenewRefuse}, nil
			})
		return err
	}); err != nil || !ran || !st.Revoked {
		t.Fatalf("refuse: ran=%v revoked=%v err=%v", ran, st.Revoked, err)
	}
	var reason *string
	if err := h.super.QueryRow(ctx, `SELECT revoked_reason FROM app.sessions WHERE id = $1`, refused.String()).Scan(&reason); err != nil || reason == nil || *reason != SessionRevokedRefused {
		t.Fatalf("revoked_reason = %v, %v", reason, err)
	}
	if ok, _ := pushSessionOf(t, h, pRefused.ID); ok {
		t.Error("the refused session's push subscription survived the refusal")
	}
	for name, id := range map[string]ulid.ULID{"another session's": pOther.ID, "the NULL-session": pNone.ID} {
		if ok, _ := pushSessionOf(t, h, id); !ok {
			t.Errorf("%s push subscription was deleted by the refusal", name)
		}
	}
}
