// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

import (
	"context"
	"testing"

	"github.com/oklog/ulid/v2"
)

// TestRLSRemovedKeysAndHistory: a REMOVED key and its history are still the
// user's, and RLS hides them from everyone else exactly as it hides a held key,
// through the history readers too, which are the only ones that look at removed
// rows. Positive control first: A sees A's own.
func TestRLSRemovedKeysAndHistory(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, b := seedTwoUsers(t, h.super)
	s := h.appStore()
	removedOf := func(u fixtureUser) ulid.ULID {
		var id string
		if err := h.super.QueryRow(ctx, `SELECT id FROM app.contact_keys WHERE user_id = $1 AND removed_at IS NOT NULL`, u.ID).Scan(&id); err != nil {
			t.Fatalf("fixture: no removed key for %s: %v", u.Tag, err)
		}
		return ulid.MustParse(id)
	}
	aRemoved, bRemoved := removedOf(a), removedOf(b)
	if err := s.UserTx(ctx, mustULID(t, a.ID), func(tx *UserTx) error {
		all, err := tx.AllContactKeysIncludingRemoved(ctx)
		if err != nil {
			return err
		}
		var mine, theirs int
		for _, k := range all {
			switch k.ID {
			case aRemoved:
				mine++
			case bRemoved:
				theirs++
			}
		}
		if mine != 1 {
			t.Errorf("control: A does not see A's own removed key (%d)", mine)
		}
		if theirs != 0 {
			t.Errorf("A sees B's removed key")
		}
		ev, found, err := tx.ContactKeyHistory(ctx, aRemoved)
		if err != nil {
			return err
		}
		if !found || len(ev) != 1 {
			t.Errorf("control: A's own history = %v found=%v", ev, found)
		}
		if ev, found, err := tx.ContactKeyHistory(ctx, bRemoved); err != nil || found || len(ev) != 0 {
			t.Errorf("A reads B's key history: %v found=%v err=%v", ev, found, err)
		}
		var n int
		if err := tx.tx.QueryRow(ctx, `SELECT count(*) FROM app.contact_key_events WHERE contact_key_id = $1`, bRemoved.String()).Scan(&n); err != nil || n != 0 {
			t.Errorf("A sees %d of B's events (err %v)", n, err)
		}
		// Reviving B's key by naming it is not possible either: A's write lands
		// in A's rows or nowhere.
		tag, err := tx.tx.Exec(ctx, `UPDATE app.contact_keys SET removed_at = NULL WHERE id = $1`, bRemoved.String())
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 0 {
			t.Errorf("A revived B's removed key (%d rows)", tag.RowsAffected())
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
