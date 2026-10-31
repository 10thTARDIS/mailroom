// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"
)

// The store surface of the OAuth CONNECT flow (#52, ADR-046 §3 and §9,
// increment 3): re-authentication, disconnect, reconnecting a disconnected
// account, and the audit rows they owe. Same discipline as oauth_grants.go:
// every method runs on a UserTx (RLS-scoped, fail closed), the store never
// sees a plaintext token (callers pass re-sealed credentials), and every
// mutation of the Account's visible state appends its change_log row in the
// same transaction.

var (
	// ErrOAuthSubjectMismatch: a re-authentication came back signed in as a
	// DIFFERENT provider account (another subject, or another provider) than
	// the one the grant belongs to. Refused, so one mailbox can never be
	// silently swapped for another under an existing account (ADR-046 §3).
	ErrOAuthSubjectMismatch = errors.New("store: oauth re-authentication returned a different account")
	// ErrOAuthConnectionImmutable: a connection change (hosts, ports,
	// credentials) was attempted on an OAuth account. Its hosts are its
	// provider's and its secret is the grant's (ADR-046 §9 obligation (e)).
	ErrOAuthConnectionImmutable = errors.New("store: an OAuth account's connection settings cannot be changed")
)

// OAuthGrantUpdate is the input to UpdateOAuthGrant: what a successful
// re-authentication learned, and the account's credentials re-sealed with the
// new refresh token.
type OAuthGrantUpdate struct {
	AccountID     ulid.ULID
	Provider      string
	Subject       string
	Address       string
	Scopes        []string
	CredentialsCT []byte
}

// OAuthGrantBySubject returns the caller's grant for (provider, subject) —
// the provider account a connect just signed in as — or ErrOAuthGrantNotFound.
// The connect flow asks it first, so a duplicate connect is answered as such
// instead of as a constraint violation.
func (tx *UserTx) OAuthGrantBySubject(ctx context.Context, provider, subject string) (OAuthGrant, error) {
	g, err := scanOAuthGrant(tx.tx.QueryRow(ctx, `
		SELECT `+oauthGrantColumns+` FROM app.oauth_grants
		 WHERE user_id = $1 AND provider = $2 AND subject = $3`,
		tx.userID.String(), provider, subject))
	if err != nil {
		if isNoRows(err) {
			return OAuthGrant{}, ErrOAuthGrantNotFound
		}
		return OAuthGrant{}, fmt.Errorf("store: oauth grant by subject: %w", err)
	}
	return g, nil
}

// UpdateOAuthGrant records a re-authentication (ADR-046 §9 obligation (d)):
// the grant's scopes and address are replaced, its state returns to active,
// and the account's credentials are replaced by u.CredentialsCT (the new
// refresh token, sealed by the caller).
//
// The grant must belong to the same provider account: a different provider or
// subject is ErrOAuthSubjectMismatch and nothing is written. No grant is
// ErrOAuthGrantNotFound.
//
// Obligation (c): the credentials are written in the SAME statement that bumps
// the grant's generation, so a refresh worker still holding the previous
// generation loses its compare-and-swap (RotateOAuthRefreshToken) and can never
// overwrite the new token with one minted under the old grant.
//
// Appends one Account 'updated' change row (the account's visible auth state
// may change, and a reconnect is something a client should re-read for) and
// one `oauth.reauth` audit row (provider, scopes — never a token).
func (tx *UserTx) UpdateOAuthGrant(ctx context.Context, u OAuthGrantUpdate, at time.Time) error {
	switch {
	case u.Provider == "" || u.Subject == "" || u.Address == "":
		return errors.New("store: update oauth grant: provider, subject and address are required")
	case len(u.CredentialsCT) == 0:
		return errors.New("store: update oauth grant: sealed credentials are required")
	}
	scopes := u.Scopes
	if scopes == nil {
		scopes = []string{}
	}
	var provider, subject string
	if err := tx.tx.QueryRow(ctx, `
		SELECT provider, subject FROM app.oauth_grants
		 WHERE user_id = $1 AND account_id = $2
		 FOR UPDATE`,
		tx.userID.String(), u.AccountID.String()).Scan(&provider, &subject); err != nil {
		if isNoRows(err) {
			return ErrOAuthGrantNotFound
		}
		return fmt.Errorf("store: update oauth grant (read): %w", err)
	}
	if provider != u.Provider || subject != u.Subject {
		return ErrOAuthSubjectMismatch
	}
	var gen int64
	if err := tx.tx.QueryRow(ctx, `
		WITH g AS (
		  UPDATE app.oauth_grants
		     SET scopes = $3, address = $4, generation = generation + 1,
		         state_changed_at = CASE WHEN state <> 'active' THEN $5 ELSE state_changed_at END,
		         state = 'active', state_reason = ''
		   WHERE user_id = $1 AND account_id = $2
		  RETURNING account_id, generation
		)
		UPDATE app.accounts a
		   SET credentials_ct = $6
		  FROM g
		 WHERE a.user_id = $1 AND a.id = g.account_id AND a.auth_kind = 'oauth2'
		RETURNING g.generation`,
		tx.userID.String(), u.AccountID.String(), scopes, u.Address, at.UTC(), u.CredentialsCT).Scan(&gen); err != nil {
		if isNoRows(err) {
			return ErrOAuthGrantNotFound
		}
		return fmt.Errorf("store: update oauth grant: %w", err)
	}
	if _, err := tx.AppendChange(ctx, ObjectAccount, u.AccountID, OpUpdated); err != nil {
		return err
	}
	return tx.AuditOAuth(ctx, "oauth.reauth", u.AccountID, map[string]any{
		"provider": u.Provider, "scopes": scopes,
	})
}

// ReconnectOAuthAccount re-attaches a grant to one of the caller's OAuth
// accounts that has none (it was disconnected), replaces its credentials with
// creds (the new refresh token, sealed by the caller) and sets its sync to
// syncEnabled (on, unless the caller's Gmail sync hold says otherwise), in
// this transaction. The caller has already established that the
// provider account is the one the account belonged to.
//
// g.Generation should be unpredictable (the connect flow draws it at random):
// a refresh worker that was mid-rotation when the account was disconnected
// holds the OLD grant's generation, and a recreated grant starting at the same
// small number would let its compare-and-swap win against the new grant
// (obligation (c)).
//
// Appends the Account change rows (CreateOAuthGrant's, UpdateAccount's) and an
// `oauth.connect` audit row. ErrAccountNotFound when there is no such OAuth
// account; ErrOAuthGrantExists when it already has a grant.
func (tx *UserTx) ReconnectOAuthAccount(ctx context.Context, g OAuthGrantRow, creds []byte, syncEnabled bool) error {
	if len(creds) == 0 {
		return errors.New("store: reconnect oauth account: sealed credentials are required")
	}
	if err := tx.CreateOAuthGrant(ctx, g); err != nil {
		return err
	}
	tag, err := tx.tx.Exec(ctx, `
		UPDATE app.accounts SET credentials_ct = $3, sync_enabled = $4
		 WHERE user_id = $1 AND id = $2 AND auth_kind = 'oauth2'`,
		tx.userID.String(), g.AccountID.String(), creds, syncEnabled)
	if err != nil {
		return fmt.Errorf("store: reconnect oauth account: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrAccountNotFound
	}
	if _, err := tx.AppendChange(ctx, ObjectAccount, g.AccountID, OpUpdated); err != nil {
		return err
	}
	return tx.AuditOAuth(ctx, "oauth.connect", g.AccountID, map[string]any{
		"provider": g.Provider, "scopes": g.Scopes, "reconnect": true,
	})
}

// DisconnectOAuthGrant ends an OAuth account's grant and KEEPS the account and
// its mail (ADR-046 §6): the grant row is deleted, the account's credentials
// are replaced by creds (the caller re-sealed them without the refresh token)
// and its sync is switched off, in this transaction. revocation records what
// happened at the provider ("revoked", "failed", "unsupported") in the
// `oauth.disconnect` audit row.
//
// Appends one Account 'updated' change row. Returns the grant's provider;
// ErrOAuthGrantNotFound when the account has no grant.
func (tx *UserTx) DisconnectOAuthGrant(ctx context.Context, accountID ulid.ULID, creds []byte, revocation string) (string, error) {
	if len(creds) == 0 {
		return "", errors.New("store: disconnect oauth grant: sealed credentials are required")
	}
	var provider string
	if err := tx.tx.QueryRow(ctx, `
		DELETE FROM app.oauth_grants WHERE user_id = $1 AND account_id = $2
		RETURNING provider`,
		tx.userID.String(), accountID.String()).Scan(&provider); err != nil {
		if isNoRows(err) {
			return "", ErrOAuthGrantNotFound
		}
		return "", fmt.Errorf("store: disconnect oauth grant: %w", err)
	}
	tag, err := tx.tx.Exec(ctx, `
		UPDATE app.accounts SET credentials_ct = $3, sync_enabled = false
		 WHERE user_id = $1 AND id = $2 AND auth_kind = 'oauth2'`,
		tx.userID.String(), accountID.String(), creds)
	if err != nil {
		return "", fmt.Errorf("store: disconnect oauth grant (account): %w", err)
	}
	if tag.RowsAffected() != 1 {
		return "", ErrAccountNotFound
	}
	if _, err := tx.AppendChange(ctx, ObjectAccount, accountID, OpUpdated); err != nil {
		return "", err
	}
	return provider, tx.AuditOAuth(ctx, "oauth.disconnect", accountID, map[string]any{
		"provider": provider, "revocation": revocation,
	})
}

// AuditOAuth appends one audit_log row for an OAuth lifecycle event on an
// Account (`oauth.connect`, `oauth.reauth`, `oauth.disconnect`). detail
// carries provider, scopes and outcomes only — never a token, never provider
// prose.
func (tx *UserTx) AuditOAuth(ctx context.Context, action string, accountID ulid.ULID, detail map[string]any) error {
	b, err := json.Marshal(detail)
	if err != nil {
		return fmt.Errorf("store: audit %s: %w", action, err)
	}
	if _, err := tx.tx.Exec(ctx, `
		INSERT INTO app.audit_log (user_id, actor, action, object_type, object_id, detail)
		VALUES ($1, 'user', $2, 'Account', $3, $4)`,
		tx.userID.String(), action, accountID.String(), b); err != nil {
		return fmt.Errorf("store: audit %s: %w", action, err)
	}
	return nil
}
