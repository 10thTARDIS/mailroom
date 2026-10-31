// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

import (
	"context"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
)

// Receive-only state (ADR-046 §12): marking keeps the first time, marking
// again is a no-op, a rename leaves it, a connection change clears it, and no
// step appends a change_log row (operational state).
func TestSendBlockedLifecycle(t *testing.T) {
	h := newHarness(t)
	u := seedUser(t, h.super, "send-blocked-"+NewULID().String())
	uid := mustParseULID(t, u.ID)
	st := h.appStore()
	ctx := context.Background()
	var id ulid.ULID
	if err := st.UserTx(ctx, uid, func(tx *UserTx) error {
		var e error
		row := sampleAccountRow("ms")
		row.OutboundDailyQuota = 42
		id, e = tx.CreateAccount(ctx, row)
		return e
	}); err != nil {
		t.Fatal(err)
	}
	var quota int
	if err := h.super.QueryRow(ctx, `SELECT outbound_daily_quota FROM app.accounts WHERE id = $1`, id.String()).Scan(&quota); err != nil || quota != 42 {
		t.Errorf("outbound_daily_quota = %d (%v), want 42", quota, err)
	}
	changes := func() int {
		var n int
		_ = h.super.QueryRow(ctx, `SELECT count(*) FROM app.change_log WHERE user_id = $1`, u.ID).Scan(&n)
		return n
	}
	read := func() (code string, at *time.Time) {
		_ = h.super.QueryRow(ctx, `SELECT COALESCE(send_blocked_code, ''), send_blocked_at FROM app.account_sync_status WHERE account_id = $1`,
			id.String()).Scan(&code, &at)
		return code, at
	}
	tx := func(f func(tx *UserTx) error) {
		t.Helper()
		if err := st.UserTx(ctx, uid, f); err != nil {
			t.Fatal(err)
		}
	}
	c0 := changes()
	t1 := time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)
	tx(func(tx *UserTx) error { return tx.MarkSendBlocked(ctx, id, SendBlockedSMTPAuthDisabled, t1) })
	tx(func(tx *UserTx) error {
		return tx.MarkSendBlocked(ctx, id, SendBlockedSMTPAuthDisabled, t1.Add(time.Hour))
	})
	if code, at := read(); code != SendBlockedSMTPAuthDisabled || at == nil || !at.Equal(t1) {
		t.Fatalf("after two marks: %q %v, want the first time kept", code, at)
	}
	tx(func(tx *UserTx) error {
		code, err := tx.SendBlocked(ctx, id)
		if err == nil && code != SendBlockedSMTPAuthDisabled {
			t.Errorf("SendBlocked = %q", code)
		}
		return err
	})
	name := "ms-renamed"
	tx(func(tx *UserTx) error { _, err := tx.UpdateAccount(ctx, id, AccountPatch{Name: &name}); return err })
	if code, _ := read(); code == "" {
		t.Error("a rename cleared the receive-only mark")
	}
	tx(func(tx *UserTx) error {
		_, err := tx.UpdateAccount(ctx, id, AccountPatch{Connection: &AccountConnection{
			IMAPHost: "imap2.example.com", SMTPHost: "smtp2.example.com", CredentialsCT: []byte{1}, CredentialsDEK: NewULID(),
		}})
		return err
	})
	if code, at := read(); code != "" || at != nil {
		t.Errorf("a connection change left the mark: %q %v", code, at)
	}
	// Two account updates appended their own rows; the marks and the clear none.
	if n := changes() - c0; n != 2 {
		t.Errorf("change_log rows = %d, want 2 (the two account updates only)", n)
	}
}
