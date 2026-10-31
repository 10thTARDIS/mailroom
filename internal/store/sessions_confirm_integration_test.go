// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

import (
	"context"
	"testing"
	"time"
)

// ConfirmSession is the last step of a renewal: it stamps only the generation
// the rotation produced, never a revoked session, and never another user's.
func TestConfirmSession(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, b := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID, bID := mustULID(t, a.ID), mustULID(t, b.ID)

	stale := func(id string) {
		t.Helper()
		if _, err := h.super.Exec(ctx, `UPDATE app.sessions SET last_checked_at = now() - interval '1 hour' WHERE id = $1`, id); err != nil {
			t.Fatal(err)
		}
	}
	age := func(id string) float64 {
		t.Helper()
		var v float64
		if err := h.super.QueryRow(ctx, `SELECT extract(epoch FROM now() - last_checked_at) FROM app.sessions WHERE id = $1`, id).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	var lastRevoked bool
	confirm := func(user, sess interface{ String() string }, gen int64) (time.Time, bool) {
		t.Helper()
		var at time.Time
		var ok bool
		if err := s.UserTx(ctx, mustULID(t, user.String()), func(tx *UserTx) error {
			var e error
			at, ok, lastRevoked, e = tx.ConfirmSession(ctx, mustULID(t, sess.String()), gen)
			return e
		}); err != nil {
			t.Fatal(err)
		}
		return at, ok
	}

	id := createSession(t, s, aID, "confirm-1", nil)
	stale(id.String())

	// Rotate (generation 0 -> 1), then confirm with the generation it returned.
	var gen int64
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		st, _, e := tx.RenewSessionLocked(ctx, id, time.Now().Add(-15*time.Minute), func(SessionRenewState) (SessionRenewWrite, error) {
			return SessionRenewWrite{Action: SessionRenewRotate}, nil
		})
		gen = st.Generation
		return e
	}); err != nil || gen != 1 {
		t.Fatalf("rotate: gen=%d err=%v", gen, err)
	}
	if age(id.String()) < 3000 {
		t.Fatal("a rotation stamped last_checked_at")
	}

	// A stale generation (another process rotated since) writes nothing.
	if _, ok := confirm(aID, id, gen-1); ok || age(id.String()) < 3000 {
		t.Fatal("a confirm with an old generation stamped the session")
	}
	if lastRevoked {
		t.Fatal("a stale generation was reported as revoked; the caller would sign the user out")
	}
	// Another user cannot confirm it.
	if _, ok := confirm(bID, id, gen); ok || age(id.String()) < 3000 {
		t.Fatal("another user's confirm stamped the session")
	}
	// The right generation stamps it, and returns the stamp.
	if at, ok := confirm(aID, id, gen); !ok || time.Since(at) > time.Minute || age(id.String()) > 60 {
		t.Fatalf("confirm = %v, %v; want the session stamped now", at, ok)
	}

	// A revoked session is left alone.
	stale(id.String())
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		_, e := tx.RevokeSession(ctx, id, SessionRevokedSignedOut)
		return e
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := confirm(aID, id, gen); ok || age(id.String()) < 3000 {
		t.Fatal("a revoked session was confirmed")
	}
	if !lastRevoked {
		t.Fatal("a revoked session was not reported as revoked; a sign-out racing a renewal would be confirmed")
	}
}
