// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

import (
	"context"
	"errors"
	"testing"

	"github.com/oklog/ulid/v2"
)

// TestOutOfOfficeCannotReachAnotherUsersAccount: the row's account_id is a
// foreign key, and a foreign-key check is made by the table owner, outside
// RLS. Without the store's own ownership check, user B could create a row
// "for" user A's account (it would sit under B's user_id, invisible to A, but
// tied to A's account). PutOutOfOffice must refuse it exactly as an account
// that does not exist, and append no change row.
func TestOutOfOfficeCannotReachAnotherUsersAccount(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a := seedUser(t, h.super, "oof-a-"+NewULID().String())
	b := seedUser(t, h.super, "oof-b-"+NewULID().String())
	st := NewFromPool(h.app)
	bID := ulid.MustParse(b.ID)
	aAccount := ulid.MustParse(a.AccountID)

	var before int
	if err := h.super.QueryRow(ctx, `SELECT count(*) FROM app.out_of_office WHERE account_id = $1`, a.AccountID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	err := st.UserTx(ctx, bID, func(tx *UserTx) error {
		o := DefaultOutOfOffice(aAccount)
		o.Subject = "hijack"
		_, e := tx.PutOutOfOffice(ctx, o)
		return e
	})
	if !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("B writing A's account: err = %v, want ErrAccountNotFound", err)
	}
	var after, changes int
	_ = h.super.QueryRow(ctx, `SELECT count(*) FROM app.out_of_office WHERE account_id = $1`, a.AccountID).Scan(&after)
	_ = h.super.QueryRow(ctx, `SELECT count(*) FROM app.change_log WHERE user_id = $1 AND object_type = 'OutOfOffice'`, b.ID).Scan(&changes)
	if after != before || changes != 0 {
		t.Fatalf("rows for A's account %d -> %d, B's change rows %d", before, after, changes)
	}
	// And B cannot read A's seeded row either.
	err = st.UserTx(ctx, bID, func(tx *UserTx) error {
		_, e := tx.GetOutOfOffice(ctx, aAccount)
		return e
	})
	if !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("B reading A's account: %v", err)
	}
}

// TestOutOfOfficeChangeRows: a first save is one `created`, a change is one
// `updated`, and an identical save appends nothing.
func TestOutOfOfficeChangeRows(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	u := seedUser(t, h.super, "oof-c-"+NewULID().String())
	st := NewFromPool(h.app)
	uid, acct := ulid.MustParse(u.ID), ulid.MustParse(u.AccountID)
	// The fixture seeds a row; start from a clean slate for this account.
	if _, err := h.super.Exec(ctx, `DELETE FROM app.out_of_office WHERE account_id = $1`, u.AccountID); err != nil {
		t.Fatal(err)
	}
	put := func(o OutOfOffice) bool {
		var changed bool
		if err := st.UserTx(ctx, uid, func(tx *UserTx) error {
			var e error
			changed, e = tx.PutOutOfOffice(ctx, o)
			return e
		}); err != nil {
			t.Fatal(err)
		}
		return changed
	}
	ops := func() []string {
		rows, err := h.super.Query(ctx, `SELECT op::text FROM app.change_log WHERE user_id = $1 AND object_type = 'OutOfOffice' ORDER BY seq`, u.ID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var s string
			_ = rows.Scan(&s)
			out = append(out, s)
		}
		return out
	}
	o := DefaultOutOfOffice(acct)
	o.Subject = "Away"
	if !put(o) {
		t.Fatal("first save reported no change")
	}
	if put(o) {
		t.Fatal("identical save reported a change")
	}
	o.Body = "back Monday"
	if !put(o) {
		t.Fatal("a real change reported none")
	}
	got := ops()
	if len(got) != 2 || got[0] != "created" || got[1] != "updated" {
		t.Fatalf("change rows = %v, want [created updated]", got)
	}
}
