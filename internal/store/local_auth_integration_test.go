// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

import (
	"context"
	"testing"

	"github.com/oklog/ulid/v2"
)

// #776: a user reads and writes only their own local DKIM verdicts. The generic
// suite (rls_test.go, via appTables) covers the table; this proves the store
// API, which is what Email/get calls.
func TestLocalDKIMIsolationAndRoundTrip(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a := seedUser(t, h.super, "lda-"+NewULID().String())
	b := seedUser(t, h.super, "ldb-"+NewULID().String())
	st := NewFromPool(h.app)
	aID, bID := ulid.MustParse(a.ID), ulid.MustParse(b.ID)
	aMsg, bMsg := ulid.MustParse(a.MessageID), ulid.MustParse(b.MessageID)

	// seedUser's fixtures may already hold a row for the message; replace it.
	if _, err := h.super.Exec(ctx, `DELETE FROM app.message_local_auth WHERE message_id = ANY($1)`, []string{a.MessageID, b.MessageID}); err != nil {
		t.Fatal(err)
	}
	// B cannot plant a verdict against A's message. This runs BEFORE A writes its
	// own: the primary key is (message_id, method), so a planted row would
	// squat the key and make A's later insert a silent no-op (ON CONFLICT DO
	// NOTHING). Probed: with the EXISTS guard removed, this goes red only in this
	// order; after A's write the conflict hides the plant.
	_ = st.UserTx(ctx, bID, func(tx *UserTx) error {
		return tx.SetLocalDKIM(ctx, aMsg, LocalDKIM{Result: "fail_body"})
	})
	var n int
	if err := h.super.QueryRow(ctx, `SELECT count(*) FROM app.message_local_auth WHERE message_id = $1 AND user_id = $2`, a.MessageID, b.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("B wrote %d verdict rows for A's message under its own id", n)
	}
	if err := st.UserTx(ctx, aID, func(tx *UserTx) error {
		return tx.SetLocalDKIM(ctx, aMsg, LocalDKIM{Result: "pass", Domain: "example.com", Aligned: true})
	}); err != nil {
		t.Fatal(err)
	}
	// Positive control: A reads its own row.
	if err := st.UserTx(ctx, aID, func(tx *UserTx) error {
		m, e := tx.LocalDKIMFor(ctx, []ulid.ULID{aMsg, bMsg})
		if v, ok := m[aMsg]; !ok || !v.LocalDKIMPass() {
			t.Errorf("A cannot read its own verdict: %+v", m)
		}
		return e
	}); err != nil {
		t.Fatal(err)
	}
	// B asking for A's message id gets nothing.
	if err := st.UserTx(ctx, bID, func(tx *UserTx) error {
		m, e := tx.LocalDKIMFor(ctx, []ulid.ULID{aMsg})
		if len(m) != 0 {
			t.Errorf("B read A's verdict: %+v", m)
		}
		return e
	}); err != nil {
		t.Fatal(err)
	}
	// The change_log gets nothing from SetLocalDKIM.
	var before, after int
	_ = h.super.QueryRow(ctx, `SELECT count(*) FROM app.change_log WHERE user_id = $1`, a.ID).Scan(&before)
	_ = st.UserTx(ctx, aID, func(tx *UserTx) error {
		return tx.SetLocalDKIM(ctx, aMsg, LocalDKIM{Result: "fail_body"})
	})
	_ = h.super.QueryRow(ctx, `SELECT count(*) FROM app.change_log WHERE user_id = $1`, a.ID).Scan(&after)
	if before != after {
		t.Errorf("SetLocalDKIM appended %d change_log rows", after-before)
	}
}
