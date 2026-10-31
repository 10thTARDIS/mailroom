// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// Server-side browser sessions (#937, ADR-048, migration 00082), driven through
// the store methods as mailroom_app. What is proven:
//
//  1. The per-request lookup finds a live session by its hash and ONLY by the
//     whole hash, and refuses a revoked session and a disabled or deleted user.
//  2. A touch writes at most once a minute.
//  3. Isolation through the real methods: user B can neither read, touch nor
//     revoke user A's session (rls_test.go covers the raw-SQL half through the
//     seeded row in fixtures_test.go).
//  4. Writes append no change_log row.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
)

func hashOf(secret string) [32]byte { return sha256.Sum256([]byte(secret)) }

func createSession(t *testing.T, s *Store, user ulid.ULID, secret string, mutate func(*NewSession)) ulid.ULID {
	t.Helper()
	n := NewSession{
		ID: NewULID(), TokenHash: hashOf(secret),
		RefreshCT: []byte{0x01, 0x02}, IDTokenCT: []byte{0x03, 0x04},
		UserAgent: "Test Browser", Addr: "192.0.2.1",
	}
	if mutate != nil {
		mutate(&n)
	}
	if err := s.UserTx(context.Background(), user, func(tx *UserTx) error {
		return tx.CreateSession(context.Background(), n)
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	return n.ID
}

func TestSessionLookup(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, b := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID, bID := mustULID(t, a.ID), mustULID(t, b.ID)

	sa := createSession(t, s, aID, "secret-of-a", nil)
	sb := createSession(t, s, bID, "secret-of-b", nil)

	lookup := func(secret string) (SessionRef, bool) {
		t.Helper()
		ref, ok, err := s.LookupSession(ctx, hashOf(secret))
		if err != nil {
			t.Fatalf("lookup %q: %v", secret, err)
		}
		return ref, ok
	}

	// Positive controls first: without them every "not found" below would pass
	// against a lookup that finds nothing at all.
	if ref, ok := lookup("secret-of-a"); !ok || ref.ID != sa || ref.UserID != aID || ref.LastCheckedAt.IsZero() {
		t.Fatalf("A's live session did not resolve: ok=%v ref=%+v", ok, ref)
	}
	if ref, ok := lookup("secret-of-b"); !ok || ref.ID != sb || ref.UserID != bID {
		t.Fatalf("B's live session did not resolve to B: ok=%v ref=%+v", ok, ref)
	}
	if _, ok := lookup("not-a-secret"); ok {
		t.Fatal("an unknown secret resolved")
	}

	// A hash differing in ONE byte resolves nothing: an exact match only, no
	// prefix or fuzzy matching, so the function cannot enumerate.
	near := hashOf("secret-of-a")
	near[31] ^= 0x01
	if _, ok, err := s.LookupSession(ctx, near); err != nil || ok {
		t.Fatalf("a hash one byte off resolved: ok=%v err=%v", ok, err)
	}
	near = hashOf("secret-of-a")
	near[0] ^= 0x80
	if _, ok, err := s.LookupSession(ctx, near); err != nil || ok {
		t.Fatalf("a hash one byte off (first byte) resolved: ok=%v err=%v", ok, err)
	}

	// The stored hash is the hash, never the secret.
	var stored []byte
	if err := h.super.QueryRow(ctx, `SELECT token_hash FROM app.sessions WHERE id = $1`, sa.String()).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if want := hashOf("secret-of-a"); !bytes.Equal(stored, want[:]) {
		t.Fatal("the stored token_hash is not the SHA-256 of the secret")
	}

	// Revoked: refused (and the row is kept, so it is the revocation, not loss).
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		ok, err := tx.RevokeSession(ctx, sa, SessionRevokedSignedOut)
		if err == nil && !ok {
			t.Error("RevokeSession reported nothing revoked for a live session")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := lookup("secret-of-a"); ok {
		t.Fatal("a revoked session resolved")
	}
	var reason string
	if err := h.super.QueryRow(ctx, `SELECT revoked_reason FROM app.sessions WHERE id = $1`, sa.String()).Scan(&reason); err != nil || reason != SessionRevokedSignedOut {
		t.Fatalf("revoked_reason = %q, %v", reason, err)
	}
	// Revoking twice reports false rather than rewriting the reason.
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		ok, err := tx.RevokeSession(ctx, sa, SessionRevokedExpired)
		if err == nil && ok {
			t.Error("a second RevokeSession reported a revocation")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}

	// A disabled user's session stops resolving on the next lookup, and resumes
	// when the user is re-enabled (so it is the disabling, not damage).
	if _, err := h.super.Exec(ctx, `UPDATE app.users SET disabled_at = now() WHERE id = $1`, b.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := lookup("secret-of-b"); ok {
		t.Fatal("a disabled user's session resolved")
	}
	if _, err := h.super.Exec(ctx, `UPDATE app.users SET disabled_at = NULL WHERE id = $1`, b.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := lookup("secret-of-b"); !ok {
		t.Fatal("re-enabling the user did not restore the session: the disabled test proved nothing")
	}
	if _, err := h.super.Exec(ctx, `UPDATE app.users SET deleted_at = now() WHERE id = $1`, b.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := lookup("secret-of-b"); ok {
		t.Fatal("a deleted user's session resolved")
	}
}

func TestSessionStoresOnlyWhatItWasGiven(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)

	id := createSession(t, s, aID, "secret-1", func(n *NewSession) {
		n.RefreshCT = nil
		n.UserAgent = string(bytes.Repeat([]byte("x"), 900))
	})
	var refresh, idt []byte
	var ua string
	if err := h.super.QueryRow(ctx, `SELECT refresh_ct, id_token_ct, user_agent FROM app.sessions WHERE id = $1`, id.String()).Scan(&refresh, &idt, &ua); err != nil {
		t.Fatal(err)
	}
	if refresh != nil {
		t.Errorf("refresh_ct = %v, want NULL when none was issued", refresh)
	}
	if !bytes.Equal(idt, []byte{0x03, 0x04}) {
		t.Errorf("id_token_ct = %v", idt)
	}
	if len(ua) != 512 {
		t.Errorf("user_agent length = %d, want it capped at 512", len(ua))
	}

	var got []byte
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		var err error
		got, err = tx.SessionIDToken(ctx, id)
		return err
	}); err != nil || !bytes.Equal(got, []byte{0x03, 0x04}) {
		t.Fatalf("SessionIDToken = %v, %v", got, err)
	}

	// A session creates no change_log row (ADR-048 section 2).
	var n int
	if err := h.super.QueryRow(ctx, `SELECT count(*) FROM app.change_log WHERE user_id = $1 AND object_id = $2`, a.ID, id.String()).Scan(&n); err != nil || n != 0 {
		t.Fatalf("change_log rows for the session = %d, %v; want 0", n, err)
	}
}

// A touch writes at most once a minute: the first moves last_seen_at, one
// inside the minute does not, one past it does.
func TestSessionTouchWritesAtMostOnceAMinute(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	id := createSession(t, s, aID, "secret-touch", nil)

	// Pin last_seen_at to a known instant so the arithmetic is explicit.
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	if _, err := h.super.Exec(ctx, `UPDATE app.sessions SET last_seen_at = $2, last_addr = 'start' WHERE id = $1`, id.String(), base); err != nil {
		t.Fatal(err)
	}
	seen := func() (time.Time, string) {
		t.Helper()
		var at time.Time
		var addr string
		if err := h.super.QueryRow(ctx, `SELECT last_seen_at, last_addr FROM app.sessions WHERE id = $1`, id.String()).Scan(&at, &addr); err != nil {
			t.Fatal(err)
		}
		return at.UTC(), addr
	}
	touch := func(addr string, at time.Time) {
		t.Helper()
		if err := s.UserTx(ctx, aID, func(tx *UserTx) error { return tx.TouchSession(ctx, id, addr, at) }); err != nil {
			t.Fatal(err)
		}
	}

	touch("first", base.Add(2*time.Minute))
	if at, addr := seen(); !at.Equal(base.Add(2*time.Minute)) || addr != "first" {
		t.Fatalf("the first touch did not write: %v %q", at, addr)
	}
	touch("second", base.Add(2*time.Minute+30*time.Second))
	if at, addr := seen(); !at.Equal(base.Add(2*time.Minute)) || addr != "first" {
		t.Fatalf("a touch inside the minute wrote: %v %q", at, addr)
	}
	touch("third", base.Add(4*time.Minute))
	if at, addr := seen(); !at.Equal(base.Add(4*time.Minute)) || addr != "third" {
		t.Fatalf("a touch past the minute did not write: %v %q", at, addr)
	}
}

// Through the real methods, user B can neither read, touch nor revoke A's
// session, and with no identity there is nothing to act on.
func TestSessionIsolation(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, b := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID, bID := mustULID(t, a.ID), mustULID(t, b.ID)
	sa := createSession(t, s, aID, "secret-iso-a", nil)

	// Positive control: A can read and touch its own session.
	var tok []byte
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		var err error
		tok, err = tx.SessionIDToken(ctx, sa)
		return err
	}); err != nil || len(tok) == 0 {
		t.Fatalf("A cannot read its own session's ID token: %v %v", tok, err)
	}

	// Read.
	if err := s.UserTx(ctx, bID, func(tx *UserTx) error {
		got, err := tx.SessionIDToken(ctx, sa)
		if err != nil {
			return err
		}
		if got != nil {
			t.Errorf("B read A's sealed ID token: %v", got)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// Touch.
	var before time.Time
	if err := h.super.QueryRow(ctx, `SELECT last_seen_at FROM app.sessions WHERE id = $1`, sa.String()).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := s.UserTx(ctx, bID, func(tx *UserTx) error {
		return tx.TouchSession(ctx, sa, "attacker", time.Now().Add(time.Hour))
	}); err != nil {
		t.Fatal(err)
	}
	var after time.Time
	var addr string
	if err := h.super.QueryRow(ctx, `SELECT last_seen_at, last_addr FROM app.sessions WHERE id = $1`, sa.String()).Scan(&after, &addr); err != nil {
		t.Fatal(err)
	}
	if !after.Equal(before) || addr == "attacker" {
		t.Fatalf("B touched A's session: %v -> %v, addr %q", before, after, addr)
	}

	// Revoke.
	if err := s.UserTx(ctx, bID, func(tx *UserTx) error {
		ok, err := tx.RevokeSession(ctx, sa, SessionRevokedSignedOutElsewhere)
		if err == nil && ok {
			t.Error("B revoked A's session")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.LookupSession(ctx, hashOf("secret-iso-a")); err != nil || !ok {
		t.Fatalf("A's session was damaged by B's attempts: ok=%v err=%v", ok, err)
	}

	// No identity: no UserTx at all.
	if err := s.UserTx(ctx, ulid.ULID{}, func(tx *UserTx) error { return nil }); err == nil {
		t.Fatal("UserTx without an identity succeeded")
	}
}

// The owner policy that backs the lookup function is confined to live
// sessions, and the app role cannot read another user's rows at all (the
// raw-SQL half, complementing rls_test.go's seeded-row checks).
func TestSessionLookupFunctionIsTheOnlyCrossUserRead(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	createSession(t, s, aID, "secret-xuser", nil)

	// As the app role with NO identity set, a direct SELECT sees nothing.
	var n int
	if err := h.app.QueryRow(ctx, `SELECT count(*) FROM app.sessions`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("the app role with no identity saw %d session rows; want 0", n)
	}
	// ...while the definer function still finds the live one.
	if _, ok, err := s.LookupSession(ctx, hashOf("secret-xuser")); err != nil || !ok {
		t.Fatalf("the lookup function did not find the live session: ok=%v err=%v", ok, err)
	}
}
