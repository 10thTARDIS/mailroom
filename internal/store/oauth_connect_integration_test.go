// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// The connect flow's store paths (#52, ADR-046 §9, increment 3): re-auth
// (UpdateOAuthGrant), disconnect, reconnect, OAuthGrantBySubject, and the
// UpdateAccount guard on OAuth accounts — their change rows and audit rows,
// obligation (c)'s generation bump, and user B's inability to reach any of them
// on A's account.

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

func TestOAuthReauthDisconnectReconnect(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a := seedUser(t, h.super, "oconn-"+NewULID().String())
	s := h.appStore()
	aID := mustULID(t, a.ID)
	acct := seedOAuthAccount(t, h, a, []byte("sealed-v1"))
	at := time.Now().UTC()
	tx := func(fn func(tx *UserTx) error) error { return s.UserTx(ctx, aID, fn) }

	if err := tx(func(tx *UserTx) error {
		return tx.CreateOAuthGrant(ctx, OAuthGrantRow{AccountID: acct, Provider: "google", Subject: "sub-1",
			Address: "a@example.com", Generation: 5_000_000_000})
	}); err != nil {
		t.Fatal(err)
	}
	var g OAuthGrant
	_ = tx(func(tx *UserTx) error {
		var err error
		g, err = tx.OAuthGrantBySubject(ctx, "google", "sub-1")
		return err
	})
	if g.AccountID != acct || g.Generation != 5_000_000_000 {
		t.Fatalf("by subject = %+v (explicit starting generation honoured?)", g)
	}
	if err := tx(func(tx *UserTx) error { _, err := tx.OAuthGrantBySubject(ctx, "microsoft", "sub-1"); return err }); !errors.Is(err, ErrOAuthGrantNotFound) {
		t.Errorf("by subject, other provider = %v", err)
	}

	// Re-auth with another subject: refused, nothing written.
	if err := tx(func(tx *UserTx) error {
		return tx.UpdateOAuthGrant(ctx, OAuthGrantUpdate{AccountID: acct, Provider: "google", Subject: "sub-OTHER",
			Address: "a@example.com", CredentialsCT: []byte("sealed-evil")}, at)
	}); !errors.Is(err, ErrOAuthSubjectMismatch) {
		t.Fatalf("re-auth as another subject = %v", err)
	}
	if !bytes.Equal(storedCT(t, h, acct), []byte("sealed-v1")) {
		t.Fatal("a refused re-auth wrote credentials")
	}
	// Re-auth: state back to active, generation +1 with the credentials, one
	// change row, one audit row.
	if _, err := h.super.Exec(ctx, `UPDATE app.oauth_grants SET state = 'needs_reauth' WHERE account_id = $1`, acct.String()); err != nil {
		t.Fatal(err)
	}
	before := accountChangeRows(t, h, a.ID, acct)
	if err := tx(func(tx *UserTx) error {
		return tx.UpdateOAuthGrant(ctx, OAuthGrantUpdate{AccountID: acct, Provider: "google", Subject: "sub-1",
			Address: "a2@example.com", Scopes: []string{"https://mail.google.com/"}, CredentialsCT: []byte("sealed-v2")}, at)
	}); err != nil {
		t.Fatalf("re-auth: %v", err)
	}
	_ = tx(func(tx *UserTx) error { var err error; g, err = tx.OAuthGrant(ctx, acct); return err })
	if g.State != OAuthStateActive || g.Generation != 5_000_000_001 || g.Address != "a2@example.com" ||
		!bytes.Equal(storedCT(t, h, acct), []byte("sealed-v2")) {
		t.Errorf("after re-auth: %+v", g)
	}
	if n := accountChangeRows(t, h, a.ID, acct) - before; n != 1 {
		t.Errorf("re-auth appended %d change rows, want 1", n)
	}
	// Obligation (c): a rotation still holding the old generation loses.
	if err := tx(func(tx *UserTx) error {
		_, won, err := tx.RotateOAuthRefreshToken(ctx, acct, 5_000_000_000, []byte("sealed-stale"), at)
		if won {
			t.Error("a stale rotation overwrote the re-authenticated token")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}

	// The UpdateAccount guard: no connection change on an OAuth account,
	// and the transaction stays usable.
	if err := tx(func(tx *UserTx) error {
		_, err := tx.UpdateAccount(ctx, acct, AccountPatch{Connection: &AccountConnection{
			IMAPHost: "imap.attacker.test", SMTPHost: "smtp.attacker.test", CredentialsCT: []byte("pw"), CredentialsDEK: NewULID()}})
		if !errors.Is(err, ErrOAuthConnectionImmutable) {
			t.Errorf("connection change on an OAuth account = %v", err)
		}
		name := "renamed"
		_, err = tx.UpdateAccount(ctx, acct, AccountPatch{Name: &name}) // metadata still fine
		return err
	}); err != nil {
		t.Fatalf("metadata update after the refusal: %v", err)
	}
	if !bytes.Equal(storedCT(t, h, acct), []byte("sealed-v2")) {
		t.Fatal("the refused connection change wrote credentials")
	}

	// Disconnect: grant gone, credentials replaced, sync off, one change row.
	before = accountChangeRows(t, h, a.ID, acct)
	var provider string
	if err := tx(func(tx *UserTx) error {
		var err error
		provider, err = tx.DisconnectOAuthGrant(ctx, acct, []byte("sealed-no-token"), "revoked")
		return err
	}); err != nil || provider != "google" {
		t.Fatalf("disconnect = %q, %v", provider, err)
	}
	var syncOn bool
	_ = h.super.QueryRow(ctx, `SELECT sync_enabled FROM app.accounts WHERE id = $1`, acct.String()).Scan(&syncOn)
	if syncOn || !bytes.Equal(storedCT(t, h, acct), []byte("sealed-no-token")) {
		t.Errorf("after disconnect: sync=%v", syncOn)
	}
	if n := accountChangeRows(t, h, a.ID, acct) - before; n != 1 {
		t.Errorf("disconnect appended %d change rows, want 1", n)
	}
	if err := tx(func(tx *UserTx) error { _, err := tx.DisconnectOAuthGrant(ctx, acct, []byte("x"), "x"); return err }); !errors.Is(err, ErrOAuthGrantNotFound) {
		t.Errorf("second disconnect = %v", err)
	}
	if err := tx(func(tx *UserTx) error {
		return tx.UpdateOAuthGrant(ctx, OAuthGrantUpdate{AccountID: acct, Provider: "google", Subject: "sub-1",
			Address: "a@example.com", CredentialsCT: []byte("x")}, at)
	}); !errors.Is(err, ErrOAuthGrantNotFound) {
		t.Errorf("re-auth with no grant = %v, want ErrOAuthGrantNotFound", err)
	}

	// Reconnect: grant back, sync on.
	if err := tx(func(tx *UserTx) error {
		return tx.ReconnectOAuthAccount(ctx, OAuthGrantRow{AccountID: acct, Provider: "google", Subject: "sub-1",
			Address: "a@example.com", Generation: 7_000_000_000}, []byte("sealed-v3"), true)
	}); err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	_ = h.super.QueryRow(ctx, `SELECT sync_enabled FROM app.accounts WHERE id = $1`, acct.String()).Scan(&syncOn)
	if !syncOn || !bytes.Equal(storedCT(t, h, acct), []byte("sealed-v3")) {
		t.Errorf("after reconnect: sync=%v", syncOn)
	}
	var audits int
	_ = h.super.QueryRow(ctx, `SELECT count(*) FROM app.audit_log WHERE user_id = $1 AND object_id = $2
	       AND action IN ('oauth.reauth', 'oauth.disconnect', 'oauth.connect')`, a.ID, acct.String()).Scan(&audits)
	if audits != 3 {
		t.Errorf("audit rows = %d, want 3 (reauth, disconnect, connect)", audits)
	}
	// Reconnect onto an account that has a grant is a duplicate.
	if err := tx(func(tx *UserTx) error {
		return tx.ReconnectOAuthAccount(ctx, OAuthGrantRow{AccountID: acct, Provider: "google", Subject: "sub-1",
			Address: "a@example.com"}, []byte("x"), true)
	}); !errors.Is(err, ErrOAuthGrantExists) {
		t.Errorf("reconnect over a live grant = %v", err)
	}
}

// User B, through the new methods, against A's OAuth account.
func TestOAuthConnectCrossUserIsolation(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, b := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID, bID := mustULID(t, a.ID), mustULID(t, b.ID)
	acctA := seedOAuthAccount(t, h, a, []byte("sealed-A"))
	acctA2 := seedOAuthAccount(t, h, a, []byte("sealed-A2")) // grantless (disconnected)
	at := time.Now().UTC()
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		return tx.CreateOAuthGrant(ctx, OAuthGrantRow{AccountID: acctA, Provider: "google", Subject: "sub-A", Address: "a@example.com"})
	}); err != nil {
		t.Fatal(err)
	}
	// Positive control: A sees its grant by subject.
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error { _, err := tx.OAuthGrantBySubject(ctx, "google", "sub-A"); return err }); err != nil {
		t.Fatalf("control: %v", err)
	}
	_ = s.UserTx(ctx, bID, func(tx *UserTx) error {
		if _, err := tx.OAuthGrantBySubject(ctx, "google", "sub-A"); !errors.Is(err, ErrOAuthGrantNotFound) {
			t.Errorf("B found A's grant by subject: %v", err)
		}
		return nil
	})
	if err := s.UserTx(ctx, bID, func(tx *UserTx) error {
		return tx.UpdateOAuthGrant(ctx, OAuthGrantUpdate{AccountID: acctA, Provider: "google", Subject: "sub-A",
			Address: "b@example.com", CredentialsCT: []byte("sealed-B")}, at)
	}); !errors.Is(err, ErrOAuthGrantNotFound) {
		t.Errorf("B re-authenticated A's grant: %v", err)
	}
	if err := s.UserTx(ctx, bID, func(tx *UserTx) error {
		_, err := tx.DisconnectOAuthGrant(ctx, acctA, []byte("sealed-B"), "revoked")
		return err
	}); !errors.Is(err, ErrOAuthGrantNotFound) {
		t.Errorf("B disconnected A's grant: %v", err)
	}
	if err := s.UserTx(ctx, bID, func(tx *UserTx) error {
		return tx.ReconnectOAuthAccount(ctx, OAuthGrantRow{AccountID: acctA2, Provider: "google", Subject: "sub-B",
			Address: "b@example.com"}, []byte("sealed-B"), true)
	}); !errors.Is(err, ErrAccountNotFound) {
		t.Errorf("B reconnected A's disconnected account: %v", err)
	}
	if !bytes.Equal(storedCT(t, h, acctA), []byte("sealed-A")) || !bytes.Equal(storedCT(t, h, acctA2), []byte("sealed-A2")) {
		t.Error("A's credentials changed")
	}
	var grants int
	_ = h.super.QueryRow(ctx, `SELECT count(*) FROM app.oauth_grants WHERE account_id IN ($1, $2)`,
		acctA.String(), acctA2.String()).Scan(&grants)
	if grants != 1 {
		t.Errorf("grants on A's two accounts = %d, want 1 (B neither deleted nor attached one)", grants)
	}
}
