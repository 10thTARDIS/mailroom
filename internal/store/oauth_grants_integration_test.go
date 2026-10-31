// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// Mail-provider OAuth grants and flows (#52, ADR-046, migration 00078), driven
// through the store methods as mailroom_app. Three things are proven here:
//
//  1. The change-log rule ADR-046 §4 states: a grant's creation and every
//     state TRANSITION append exactly one Account row; rotation, the refresh
//     stamp and a same-state "transition" append none.
//  2. Rotation is a compare-and-swap: the stale generation writes nothing,
//     ciphertext included.
//  3. Isolation through the real methods, not just raw SQL (rls_test.go covers
//     the raw-SQL half via fixtures_test.go): user B can neither read, rotate,
//     re-state nor consume A's grant or flow, nor attach either to A's account.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
)

// seedOAuthAccount adds an auth_kind 'oauth2' account for u and returns its id.
func seedOAuthAccount(t *testing.T, h *harness, u fixtureUser, ct []byte) ulid.ULID {
	t.Helper()
	id := NewULID()
	if _, err := h.super.Exec(context.Background(), `
		INSERT INTO app.accounts (id, user_id, name, provider_profile, imap_host, smtp_host, smtp_port,
		                          auth_kind, credentials_ct, credentials_dek)
		VALUES ($1, $2, $3, 'gmail', 'imap.gmail.com', 'smtp.gmail.com', 465, 'oauth2', $4, $5)`,
		id.String(), u.ID, "gmail-"+id.String(), ct, NewULID().String()); err != nil {
		t.Fatalf("seed oauth account: %v", err)
	}
	return id
}

func accountChangeRows(t *testing.T, h *harness, userID string, acct ulid.ULID) int {
	t.Helper()
	var n int
	if err := h.super.QueryRow(context.Background(), `
		SELECT count(*) FROM app.change_log
		 WHERE user_id = $1 AND object_type = 'Account' AND object_id = $2`,
		userID, acct.String()).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func auditRows(t *testing.T, h *harness, userID string, acct ulid.ULID) int {
	t.Helper()
	var n int
	if err := h.super.QueryRow(context.Background(), `
		SELECT count(*) FROM app.audit_log
		 WHERE user_id = $1 AND action = 'oauth.state_changed' AND object_id = $2`,
		userID, acct.String()).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func storedCT(t *testing.T, h *harness, acct ulid.ULID) []byte {
	t.Helper()
	var ct []byte
	if err := h.super.QueryRow(context.Background(),
		`SELECT credentials_ct FROM app.accounts WHERE id = $1`, acct.String()).Scan(&ct); err != nil {
		t.Fatal(err)
	}
	return ct
}

func TestOAuthGrantLifecycle(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a := seedUser(t, h.super, "oauth-"+NewULID().String())
	s := h.appStore()
	aID := mustULID(t, a.ID)
	acct := seedOAuthAccount(t, h, a, []byte("sealed-v1"))
	at := time.Now().UTC().Truncate(time.Microsecond)

	// A grant cannot be attached to a PASSWORD account (the seeded one).
	err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		return tx.CreateOAuthGrant(ctx, OAuthGrantRow{AccountID: mustULID(t, a.AccountID),
			Provider: "google", Subject: "sub-x", Address: "x@example.com"})
	})
	if !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("grant on a password account = %v, want ErrAccountNotFound", err)
	}

	// Create: one Account change row.
	before := accountChangeRows(t, h, a.ID, acct)
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		return tx.CreateOAuthGrant(ctx, OAuthGrantRow{AccountID: acct, Provider: "google",
			Subject: "1234567890", Address: "You@Example.com", Scopes: []string{"openid", "https://mail.google.com/"}})
	}); err != nil {
		t.Fatalf("create grant: %v", err)
	}
	if got := accountChangeRows(t, h, a.ID, acct) - before; got != 1 {
		t.Errorf("grant creation appended %d Account change rows, want 1", got)
	}
	// A duplicate is reported, and leaves the transaction usable.
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		if err := tx.CreateOAuthGrant(ctx, OAuthGrantRow{AccountID: acct, Provider: "google",
			Subject: "1234567890", Address: "you@example.com"}); !errors.Is(err, ErrOAuthGrantExists) {
			t.Errorf("duplicate grant = %v, want ErrOAuthGrantExists", err)
		}
		_, err := tx.OAuthGrant(ctx, acct)
		return err
	}); err != nil {
		t.Fatalf("transaction unusable after a duplicate: %v", err)
	}

	var g OAuthGrant
	var ct []byte
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		var err error
		g, ct, err = tx.OAuthGrantWithSecret(ctx, acct)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if g.State != OAuthStateActive || g.Generation != 1 || g.Provider != "google" ||
		len(g.Scopes) != 2 || g.LastRefreshAt != nil || !bytes.Equal(ct, []byte("sealed-v1")) {
		t.Fatalf("fresh grant = %+v ct=%q", g, ct)
	}

	// Rotation: wins at generation 1, writes the ciphertext, bumps to 2,
	// stamps last_refresh_at, appends NO change row.
	before = accountChangeRows(t, h, a.ID, acct)
	var won bool
	var gen int64
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		var err error
		gen, won, err = tx.RotateOAuthRefreshToken(ctx, acct, 1, []byte("sealed-v2"), at)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !won || gen != 2 || !bytes.Equal(storedCT(t, h, acct), []byte("sealed-v2")) {
		t.Fatalf("rotation: won=%v gen=%d ct=%q", won, gen, storedCT(t, h, acct))
	}
	// A STALE generation (the loser of a race) writes nothing at all.
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		var err error
		gen, won, err = tx.RotateOAuthRefreshToken(ctx, acct, 1, []byte("sealed-STALE"), at)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if won || !bytes.Equal(storedCT(t, h, acct), []byte("sealed-v2")) {
		t.Fatalf("stale rotation: won=%v ct=%q — an older token overwrote a newer one", won, storedCT(t, h, acct))
	}
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		return tx.RecordOAuthRefresh(ctx, acct, at.Add(time.Minute))
	}); err != nil {
		t.Fatal(err)
	}
	if got := accountChangeRows(t, h, a.ID, acct) - before; got != 0 {
		t.Errorf("rotation and refresh stamps appended %d change rows, want 0 (ADR-046 §4)", got)
	}

	// State: a transition appends one change row and one audit row; the same
	// state again appends neither.
	before = accountChangeRows(t, h, a.ID, acct)
	auditBefore := auditRows(t, h, a.ID, acct)
	setState := func(state, reason string) bool {
		t.Helper()
		var changed bool
		if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
			var err error
			changed, err = tx.SetOAuthGrantState(ctx, acct, state, reason, "system", at)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return changed
	}
	if !setState(OAuthStateNeedsReauth, "invalid_grant") {
		t.Fatal("first transition reported no change")
	}
	if setState(OAuthStateNeedsReauth, "invalid_grant") {
		t.Error("same-state call reported a change")
	}
	if got := accountChangeRows(t, h, a.ID, acct) - before; got != 1 {
		t.Errorf("one transition appended %d change rows, want 1", got)
	}
	if got := auditRows(t, h, a.ID, acct) - auditBefore; got != 1 {
		t.Errorf("one transition appended %d audit rows, want 1", got)
	}
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		_, err := tx.SetOAuthGrantState(ctx, acct, "bogus", "", "system", at)
		return err
	}); err == nil {
		t.Error("an unknown state was accepted")
	}
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		var err error
		g, err = tx.OAuthGrant(ctx, acct)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if g.State != OAuthStateNeedsReauth || g.StateReason != "invalid_grant" || g.Generation != 2 || g.LastRefreshAt == nil {
		t.Errorf("grant after transitions = %+v", g)
	}
	// Deleting the account takes the grant with it.
	if _, err := h.super.Exec(ctx, `DELETE FROM app.accounts WHERE id = $1`, acct.String()); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = h.super.QueryRow(ctx, `SELECT count(*) FROM app.oauth_grants WHERE account_id = $1`, acct.String()).Scan(&n)
	if n != 0 {
		t.Error("grant survived its account")
	}
}

func TestOAuthFlowSingleUse(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a := seedUser(t, h.super, "oflow-"+NewULID().String())
	s := h.appStore()
	aID := mustULID(t, a.ID)
	now := time.Now().UTC()

	state := sha256.Sum256([]byte("state-" + a.ID))
	flowID := NewULID()
	acct := mustULID(t, a.AccountID)
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		return tx.CreateOAuthFlow(ctx, OAuthFlowRow{ID: flowID, StateHash: state, Provider: "microsoft",
			AccountID: &acct, VerifierCT: []byte("sealed-verifier"), Nonce: "n-1", ExpiresAt: now.Add(10 * time.Minute)})
	}); err != nil {
		t.Fatalf("create flow: %v", err)
	}
	consume := func(st [32]byte, at time.Time) (OAuthFlow, error) {
		var f OAuthFlow
		err := s.UserTx(ctx, aID, func(tx *UserTx) error {
			var err error
			f, err = tx.ConsumeOAuthFlow(ctx, st, at)
			if errors.Is(err, ErrOAuthFlowExpired) {
				return nil // commit the consumption, as the callback will
			}
			return err
		})
		return f, err
	}
	f, err := consume(state, now)
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if f.ID != flowID || f.Provider != "microsoft" || f.AccountID == nil || *f.AccountID != acct ||
		!bytes.Equal(f.VerifierCT, []byte("sealed-verifier")) || f.Nonce != "n-1" {
		t.Errorf("consumed flow = %+v", f)
	}
	// Replay finds nothing.
	if _, err := consume(state, now); !errors.Is(err, ErrOAuthFlowNotFound) {
		t.Errorf("replayed state = %v, want ErrOAuthFlowNotFound", err)
	}

	// Expired: refused, and gone afterwards.
	state2 := sha256.Sum256([]byte("state2-" + a.ID))
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		return tx.CreateOAuthFlow(ctx, OAuthFlowRow{ID: NewULID(), StateHash: state2, Provider: "google",
			VerifierCT: []byte("v"), Nonce: "n-2", ExpiresAt: now.Add(time.Minute)})
	}); err != nil {
		t.Fatal(err)
	}
	var expiredErr error
	_ = s.UserTx(ctx, aID, func(tx *UserTx) error {
		_, expiredErr = tx.ConsumeOAuthFlow(ctx, state2, now.Add(2*time.Minute))
		return nil
	})
	if !errors.Is(expiredErr, ErrOAuthFlowExpired) {
		t.Errorf("expired flow = %v, want ErrOAuthFlowExpired", expiredErr)
	}
	if _, err := consume(state2, now); !errors.Is(err, ErrOAuthFlowNotFound) {
		t.Errorf("expired flow consumable again: %v", err)
	}

	// The purge removes only expired flows.
	state3 := sha256.Sum256([]byte("state3-" + a.ID))
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		if err := tx.CreateOAuthFlow(ctx, OAuthFlowRow{ID: NewULID(), StateHash: state3, Provider: "google",
			VerifierCT: []byte("v"), Nonce: "n-3", ExpiresAt: now.Add(time.Minute)}); err != nil {
			return err
		}
		n, err := tx.DeleteExpiredOAuthFlows(ctx, now.Add(30*time.Second))
		if err != nil {
			return err
		}
		// The fixture flow (expires in 10 minutes) and state3 survive.
		if n != 0 {
			t.Errorf("purge removed %d unexpired flows", n)
		}
		n, err = tx.DeleteExpiredOAuthFlows(ctx, now.Add(time.Hour))
		if err != nil {
			return err
		}
		if n != 2 {
			t.Errorf("purge removed %d flows, want 2 (state3 and the fixture's)", n)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// User B, through the store's own methods, against A's grant and flow.
func TestOAuthCrossUserIsolation(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, b := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID, bID := mustULID(t, a.ID), mustULID(t, b.ID)
	acctA := seedOAuthAccount(t, h, a, []byte("sealed-A"))
	at := time.Now().UTC()

	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		return tx.CreateOAuthGrant(ctx, OAuthGrantRow{AccountID: acctA, Provider: "google", Subject: "sub-A-only", Address: "a@example.com"})
	}); err != nil {
		t.Fatal(err)
	}
	stateA := sha256.Sum256([]byte("oauth-state-" + a.Tag)) // the fixture flow's state

	// Positive control: A can read its grant.
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error { _, err := tx.OAuthGrant(ctx, acctA); return err }); err != nil {
		t.Fatalf("control: A cannot read its own grant: %v", err)
	}

	_ = s.UserTx(ctx, bID, func(tx *UserTx) error {
		if _, err := tx.OAuthGrant(ctx, acctA); !errors.Is(err, ErrOAuthGrantNotFound) {
			t.Errorf("B read A's grant: %v", err)
		}
		if _, _, err := tx.OAuthGrantWithSecret(ctx, acctA); !errors.Is(err, ErrOAuthGrantNotFound) {
			t.Errorf("B read A's grant with secret: %v", err)
		}
		if _, won, err := tx.RotateOAuthRefreshToken(ctx, acctA, 1, []byte("sealed-B"), at); won || err != nil {
			t.Errorf("B rotated A's grant: won=%v err=%v", won, err)
		}
		if _, err := tx.SetOAuthGrantState(ctx, acctA, OAuthStateNeedsReauth, "x", "system", at); !errors.Is(err, ErrOAuthGrantNotFound) {
			t.Errorf("B re-stated A's grant: %v", err)
		}
		if _, err := tx.ConsumeOAuthFlow(ctx, stateA, at); !errors.Is(err, ErrOAuthFlowNotFound) {
			t.Errorf("B consumed A's flow: %v", err)
		}
		return nil
	})
	// B cannot attach a grant or a flow to A's account, even knowing its id.
	if err := s.UserTx(ctx, bID, func(tx *UserTx) error {
		return tx.CreateOAuthGrant(ctx, OAuthGrantRow{AccountID: acctA, Provider: "microsoft", Subject: "sub-B", Address: "b@example.com"})
	}); !errors.Is(err, ErrAccountNotFound) {
		t.Errorf("B attached a grant to A's account: %v", err)
	}
	if err := s.UserTx(ctx, bID, func(tx *UserTx) error {
		return tx.CreateOAuthFlow(ctx, OAuthFlowRow{ID: NewULID(), StateHash: sha256.Sum256([]byte("b")), Provider: "google",
			AccountID: &acctA, VerifierCT: []byte("v"), Nonce: "n", ExpiresAt: at.Add(time.Minute)})
	}); !errors.Is(err, ErrAccountNotFound) {
		t.Errorf("B started a re-auth flow on A's account: %v", err)
	}

	// Nothing of A's changed.
	if !bytes.Equal(storedCT(t, h, acctA), []byte("sealed-A")) {
		t.Error("A's credentials changed")
	}
	var state string
	var flows int
	_ = h.super.QueryRow(ctx, `SELECT state FROM app.oauth_grants WHERE account_id = $1`, acctA.String()).Scan(&state)
	_ = h.super.QueryRow(ctx, `SELECT count(*) FROM app.oauth_flows WHERE user_id = $1 AND state_hash = $2`, a.ID, stateA[:]).Scan(&flows)
	if state != OAuthStateActive || flows != 1 {
		t.Errorf("A's grant state = %q, A's flow present = %d; want active and 1", state, flows)
	}
}
