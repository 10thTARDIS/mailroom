// SPDX-License-Identifier: AGPL-3.0-only

package oauthmail

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/crypto"
	"mailroom/internal/onboarding"
	"mailroom/internal/store"
	"mailroom/internal/xoauth2"
)

// StoreGrants is the GrantStore over the database: the grant row through the
// store's UserTx (RLS-scoped, fail closed), the refresh token through the
// sealed account credentials (onboarding.SealCredentials / OpenCredentials,
// the ONE credentials-key path). The store never sees a plaintext token.
type StoreGrants struct {
	Store *store.Store
	Keys  crypto.KeyProvider
}

var _ GrantStore = StoreGrants{}

// LoadGrant implements GrantStore.
func (s StoreGrants) LoadGrant(ctx context.Context, userID, accountID ulid.ULID) (Grant, error) {
	var (
		g  store.OAuthGrant
		ct []byte
	)
	if err := s.Store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var err error
		g, ct, err = tx.OAuthGrantWithSecret(ctx, accountID)
		return err
	}); err != nil {
		return Grant{}, err
	}
	creds, err := s.open(ctx, userID, ct)
	if err != nil {
		return Grant{}, err
	}
	if creds.OAuth == nil {
		return Grant{}, fmt.Errorf("oauthmail: account %s has a grant but its credentials hold no OAuth secret", accountID)
	}
	if creds.OAuth.Provider != g.Provider {
		return Grant{}, fmt.Errorf("oauthmail: account %s: sealed provider %q does not match the grant's %q",
			accountID, creds.OAuth.Provider, g.Provider)
	}
	return Grant{
		Provider:     g.Provider,
		Address:      creds.IMAPUsername,
		State:        g.State,
		Generation:   g.Generation,
		RefreshToken: xoauth2.Secret(creds.OAuth.RefreshToken),
	}, nil
}

// RotateRefreshToken implements GrantStore: in ONE transaction, re-read the
// grant, give up early if the generation already moved, re-seal the
// credentials with the new refresh token, and store them under the
// compare-and-swap (store.RotateOAuthRefreshToken). No change_log row.
func (s StoreGrants) RotateRefreshToken(ctx context.Context, userID, accountID ulid.ULID, seenGeneration int64, newToken xoauth2.Secret, at time.Time) (bool, error) {
	if newToken.Empty() {
		return false, errors.New("oauthmail: rotate: empty refresh token")
	}
	master, err := s.Keys.MasterKey(ctx)
	if err != nil {
		return false, fmt.Errorf("oauthmail: master key: %w", err)
	}
	defer crypto.Zero(master)

	won := false
	err = s.Store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		g, ct, err := tx.OAuthGrantWithSecret(ctx, accountID)
		if err != nil {
			return err
		}
		if g.Generation != seenGeneration {
			return nil // lost before we started: nothing to write
		}
		creds, err := onboarding.OpenCredentials(master, userID, ct)
		if err != nil {
			return fmt.Errorf("oauthmail: account %s: %w", accountID, err)
		}
		if creds.OAuth == nil {
			return fmt.Errorf("oauthmail: account %s: credentials hold no OAuth secret", accountID)
		}
		creds.OAuth.RefreshToken = newToken.Reveal()
		newCT, err := onboarding.SealCredentials(master, userID, creds)
		if err != nil {
			return err
		}
		_, won, err = tx.RotateOAuthRefreshToken(ctx, accountID, seenGeneration, newCT, at)
		return err
	})
	if err != nil {
		return false, err
	}
	return won, nil
}

// RecordRefresh implements GrantStore.
func (s StoreGrants) RecordRefresh(ctx context.Context, userID, accountID ulid.ULID, at time.Time) error {
	return s.Store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		return tx.RecordOAuthRefresh(ctx, accountID, at)
	})
}

// SetState implements GrantStore. A transition appends the Account change row
// and the audit row in the same transaction (store.SetOAuthGrantState), and a
// transition INTO needs_reauth also queues its one Web Push there (notify.go):
// the notice exists exactly when the transition does.
func (s StoreGrants) SetState(ctx context.Context, userID, accountID ulid.ULID, state, reason string, at time.Time) error {
	return s.Store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		changed, err := tx.SetOAuthGrantState(ctx, accountID, state, reason, "system", at)
		if err != nil || !changed || state != store.OAuthStateNeedsReauth {
			return err
		}
		return enqueueReauthNotice(ctx, tx, accountID, at)
	})
}

func (s StoreGrants) open(ctx context.Context, userID ulid.ULID, ct []byte) (onboarding.Credentials, error) {
	master, err := s.Keys.MasterKey(ctx)
	if err != nil {
		return onboarding.Credentials{}, fmt.Errorf("oauthmail: master key: %w", err)
	}
	defer crypto.Zero(master)
	return onboarding.OpenCredentials(master, userID, ct)
}

// FlowVerifierInfoV1 is the HKDF label for the key that seals a pending
// flow's PKCE verifier (app.oauth_flows.verifier_ct).
const FlowVerifierInfoV1 = "mailroom-oauth-flow-v1"

// SealFlowVerifier seals a PKCE verifier for app.oauth_flows under a per-user
// key derived from the master key, with the user id AND the flow id as
// associated data, so a sealed verifier cannot be replayed into another user's
// flow or another of the same user's flows.
func SealFlowVerifier(master []byte, userID, flowID ulid.ULID, verifier xoauth2.Secret) ([]byte, error) {
	key, err := crypto.DeriveKey(master, userID[:], FlowVerifierInfoV1)
	if err != nil {
		return nil, fmt.Errorf("oauthmail: derive flow key: %w", err)
	}
	defer crypto.Zero(key)
	return crypto.Seal(key, nil, []byte(verifier.Reveal()), flowAAD(userID, flowID))
}

// OpenFlowVerifier opens what SealFlowVerifier sealed.
func OpenFlowVerifier(master []byte, userID, flowID ulid.ULID, ct []byte) (xoauth2.Secret, error) {
	key, err := crypto.DeriveKey(master, userID[:], FlowVerifierInfoV1)
	if err != nil {
		return "", fmt.Errorf("oauthmail: derive flow key: %w", err)
	}
	defer crypto.Zero(key)
	pt, err := crypto.Open(key, ct, flowAAD(userID, flowID))
	if err != nil {
		return "", fmt.Errorf("oauthmail: open flow verifier: %w", err)
	}
	defer crypto.Zero(pt)
	return xoauth2.Secret(pt), nil
}

func flowAAD(userID, flowID ulid.ULID) []byte {
	aad := make([]byte, 0, 32)
	aad = append(aad, userID[:]...)
	return append(aad, flowID[:]...)
}
