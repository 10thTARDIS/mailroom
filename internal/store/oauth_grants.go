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

// The store surface for mail-provider OAuth (#52, ADR-046, migration 00078).
//
// app.oauth_grants is the QUERYABLE half of an OAuth account's grant; the
// refresh token itself is sealed in app.accounts.credentials_ct, and access
// tokens are never stored. app.oauth_flows holds pending connects.
//
// CHANGE-LOG DISCIPLINE (the load-bearing invariant, and ADR-046 §4, written in
// the style of ADR-014):
//
//   - A grant's STATE is synced Account state (it surfaces as Account.authState),
//     so creating a grant and every state TRANSITION append one ObjectAccount
//     'updated' row in the same transaction.
//   - Refresh-token ROTATION and the last_refresh_at stamp change nothing a
//     client can see — the token is never exposed, and a refresh happens about
//     hourly per account. They append NO change row; otherwise every OAuth
//     account would push an Account change to every client every hour for
//     nothing. If a future feature exposes either, that feature owes the row.
//   - Flows are not JMAP-visible objects at all: no change rows.
//
// Every method runs on a UserTx, so RLS scopes every statement: user B can
// neither see, consume, rotate nor re-state user A's grant or flow, and with no
// identity there are no rows (rls_test.go, oauth_grants_integration_test.go).

// OAuth grant states (app.oauth_grants.state).
const (
	OAuthStateActive        = "active"
	OAuthStateNeedsReauth   = "needs_reauth"
	OAuthStateOperatorError = "operator_error"
)

// MaxOAuthStateReason caps state_reason. It holds an OAuth error code such as
// "invalid_grant", never provider prose (which can be long and, from a hostile
// or broken server, could carry anything).
const MaxOAuthStateReason = 64

var (
	// ErrOAuthGrantNotFound: the caller has no grant for that account.
	ErrOAuthGrantNotFound = errors.New("store: oauth grant not found")
	// ErrOAuthGrantExists: the account already has a grant, or the user already
	// connected that provider account (UNIQUE (user_id, provider, subject)).
	ErrOAuthGrantExists = errors.New("store: oauth grant already exists")
	// ErrOAuthFlowNotFound: no pending flow with that state for this user —
	// never started, already consumed, or someone else's.
	ErrOAuthFlowNotFound = errors.New("store: oauth flow not found")
	// ErrOAuthFlowExpired: the flow existed but had expired. It has been
	// consumed (deleted) all the same, so it cannot be retried.
	ErrOAuthFlowExpired = errors.New("store: oauth flow expired")
)

// OAuthGrant is one account's grant.
type OAuthGrant struct {
	AccountID      ulid.ULID
	Provider       string
	Subject        string
	Address        string
	Scopes         []string
	State          string
	StateReason    string
	StateChangedAt time.Time
	Generation     int64
	LastRefreshAt  *time.Time
	CreatedAt      time.Time
}

// OAuthGrantRow is the input to CreateOAuthGrant.
type OAuthGrantRow struct {
	AccountID ulid.ULID
	Provider  string
	Subject   string
	Address   string
	Scopes    []string
	// Generation is the grant's starting rotation generation; <= 0 takes the
	// schema default (1). The connect flow draws it at random so a recreated
	// grant cannot share a generation with the one it replaces (see
	// ReconnectOAuthAccount).
	Generation int64
}

const oauthGrantColumns = `account_id, provider, subject, address::text, scopes, state,
	state_reason, state_changed_at, generation, last_refresh_at, created_at`

func scanOAuthGrant(row interface{ Scan(...any) error }) (OAuthGrant, error) {
	var (
		g    OAuthGrant
		acct string
	)
	if err := row.Scan(&acct, &g.Provider, &g.Subject, &g.Address, &g.Scopes, &g.State,
		&g.StateReason, &g.StateChangedAt, &g.Generation, &g.LastRefreshAt, &g.CreatedAt); err != nil {
		return OAuthGrant{}, err
	}
	id, err := ulid.Parse(acct)
	if err != nil {
		return OAuthGrant{}, fmt.Errorf("store: oauth grant account id %q: %w", acct, err)
	}
	g.AccountID = id
	return g, nil
}

// CreateOAuthGrant records the grant for an existing OAuth account of the
// caller's (auth_kind 'oauth2') and appends the Account change row. The INSERT
// selects from app.accounts, so a grant can only ever be attached to an account
// the caller can see — not to another user's account id, which the foreign key
// alone (checked without RLS) would allow. ErrAccountNotFound when there is no
// such OAuth account; ErrOAuthGrantExists on a duplicate.
//
// The INSERT runs under a SAVEPOINT so a duplicate is a recoverable outcome.
func (tx *UserTx) CreateOAuthGrant(ctx context.Context, g OAuthGrantRow) error {
	switch {
	case g.Provider == "":
		return errors.New("store: create oauth grant: provider is required")
	case g.Subject == "":
		return errors.New("store: create oauth grant: subject is required")
	case g.Address == "":
		return errors.New("store: create oauth grant: address is required")
	}
	scopes := g.Scopes
	if scopes == nil {
		scopes = []string{}
	}
	sp, err := tx.tx.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: create oauth grant (savepoint): %w", err)
	}
	tag, err := sp.Exec(ctx, `
		INSERT INTO app.oauth_grants (account_id, user_id, provider, subject, address, scopes, generation)
		SELECT a.id, a.user_id, $3, $4, $5, $6, CASE WHEN $7::bigint > 0 THEN $7::bigint ELSE 1 END
		  FROM app.accounts a
		 WHERE a.user_id = $1 AND a.id = $2 AND a.auth_kind = 'oauth2'`,
		tx.userID.String(), g.AccountID.String(), g.Provider, g.Subject, g.Address, scopes, g.Generation)
	if err != nil {
		_ = sp.Rollback(ctx)
		if isUniqueViolation(err) {
			return ErrOAuthGrantExists
		}
		return fmt.Errorf("store: create oauth grant: %w", err)
	}
	if err := sp.Commit(ctx); err != nil {
		return fmt.Errorf("store: create oauth grant (release): %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrAccountNotFound
	}
	if _, err := tx.AppendChange(ctx, ObjectAccount, g.AccountID, OpUpdated); err != nil {
		return err
	}
	return nil
}

// OAuthGrant returns the caller's grant for accountID.
func (tx *UserTx) OAuthGrant(ctx context.Context, accountID ulid.ULID) (OAuthGrant, error) {
	g, err := scanOAuthGrant(tx.tx.QueryRow(ctx, `
		SELECT `+oauthGrantColumns+` FROM app.oauth_grants
		 WHERE user_id = $1 AND account_id = $2`,
		tx.userID.String(), accountID.String()))
	if err != nil {
		if isNoRows(err) {
			return OAuthGrant{}, ErrOAuthGrantNotFound
		}
		return OAuthGrant{}, fmt.Errorf("store: oauth grant: %w", err)
	}
	return g, nil
}

// OAuthGrantsFor returns the caller's grants for the given accounts, keyed by
// account id; an account with no grant (a password account, or a disconnected
// OAuth one) is simply absent. Account/getHealth's auth section reads it.
func (tx *UserTx) OAuthGrantsFor(ctx context.Context, accountIDs []ulid.ULID) (map[ulid.ULID]OAuthGrant, error) {
	out := make(map[ulid.ULID]OAuthGrant, len(accountIDs))
	if len(accountIDs) == 0 {
		return out, nil
	}
	rows, err := tx.tx.Query(ctx, `
		SELECT `+oauthGrantColumns+` FROM app.oauth_grants
		 WHERE user_id = $1 AND account_id = ANY($2)`,
		tx.userID.String(), ulidStrings(accountIDs))
	if err != nil {
		return nil, fmt.Errorf("store: oauth grants: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		g, err := scanOAuthGrant(rows)
		if err != nil {
			return nil, fmt.Errorf("store: oauth grants: %w", err)
		}
		out[g.AccountID] = g
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: oauth grants: %w", err)
	}
	return out, nil
}

// OAuthGrantWithSecret returns the grant together with the account's sealed
// credentials, read in ONE statement so the generation and the ciphertext the
// token source sees belong to the same rotation. The caller opens the envelope;
// the store never sees plaintext.
func (tx *UserTx) OAuthGrantWithSecret(ctx context.Context, accountID ulid.ULID) (OAuthGrant, []byte, error) {
	var ct []byte
	var (
		g    OAuthGrant
		acct string
	)
	err := tx.tx.QueryRow(ctx, `
		SELECT g.account_id, g.provider, g.subject, g.address::text, g.scopes, g.state,
		       g.state_reason, g.state_changed_at, g.generation, g.last_refresh_at, g.created_at,
		       a.credentials_ct
		  FROM app.oauth_grants g
		  JOIN app.accounts a ON a.id = g.account_id AND a.user_id = g.user_id
		 WHERE g.user_id = $1 AND g.account_id = $2`,
		tx.userID.String(), accountID.String()).Scan(&acct, &g.Provider, &g.Subject, &g.Address,
		&g.Scopes, &g.State, &g.StateReason, &g.StateChangedAt, &g.Generation, &g.LastRefreshAt,
		&g.CreatedAt, &ct)
	if err != nil {
		if isNoRows(err) {
			return OAuthGrant{}, nil, ErrOAuthGrantNotFound
		}
		return OAuthGrant{}, nil, fmt.Errorf("store: oauth grant with secret: %w", err)
	}
	if g.AccountID, err = ulid.Parse(acct); err != nil {
		return OAuthGrant{}, nil, fmt.Errorf("store: oauth grant account id %q: %w", acct, err)
	}
	return g, ct, nil
}

// RotateOAuthRefreshToken stores a rotated refresh token — newCT, the account's
// re-sealed credentials — IF AND ONLY IF the grant's generation is still
// seenGeneration, bumping the generation and stamping last_refresh_at in the
// same statement. won is false (and nothing is written) when another worker
// rotated first: the caller re-reads and uses the winner's token. Appends NO
// change_log row (see the file comment).
//
// One statement, a data-modifying CTE: the credentials UPDATE joins the grant
// UPDATE's RETURNING, so the ciphertext is written exactly when the CAS wins.
func (tx *UserTx) RotateOAuthRefreshToken(ctx context.Context, accountID ulid.ULID, seenGeneration int64, newCT []byte, at time.Time) (newGeneration int64, won bool, err error) {
	if len(newCT) == 0 {
		return 0, false, errors.New("store: rotate oauth refresh token: empty credentials")
	}
	err = tx.tx.QueryRow(ctx, `
		WITH g AS (
		  UPDATE app.oauth_grants
		     SET generation = generation + 1, last_refresh_at = $4
		   WHERE user_id = $1 AND account_id = $2 AND generation = $3
		  RETURNING account_id, generation
		)
		UPDATE app.accounts a
		   SET credentials_ct = $5
		  FROM g
		 WHERE a.user_id = $1 AND a.id = g.account_id
		RETURNING g.generation`,
		tx.userID.String(), accountID.String(), seenGeneration, at.UTC(), newCT).Scan(&newGeneration)
	if err != nil {
		if isNoRows(err) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("store: rotate oauth refresh token: %w", err)
	}
	return newGeneration, true, nil
}

// RecordOAuthRefresh stamps last_refresh_at after a refresh that did not rotate
// the refresh token. Operational; NO change_log row.
func (tx *UserTx) RecordOAuthRefresh(ctx context.Context, accountID ulid.ULID, at time.Time) error {
	if _, err := tx.tx.Exec(ctx, `
		UPDATE app.oauth_grants SET last_refresh_at = $3
		 WHERE user_id = $1 AND account_id = $2`,
		tx.userID.String(), accountID.String(), at.UTC()); err != nil {
		return fmt.Errorf("store: record oauth refresh: %w", err)
	}
	return nil
}

// SetOAuthGrantState moves the grant to state with reason (an OAuth error code,
// capped at MaxOAuthStateReason). Only a real TRANSITION writes: it appends the
// Account change row and an `oauth.state_changed` audit row (provider, from,
// to, reason — never a token) in this transaction, and changed is true. Setting
// the state the grant is already in writes nothing, so a refresh that fails the
// same way every hour does not flood /changes or the audit log.
func (tx *UserTx) SetOAuthGrantState(ctx context.Context, accountID ulid.ULID, state, reason, actor string, at time.Time) (changed bool, err error) {
	switch state {
	case OAuthStateActive, OAuthStateNeedsReauth, OAuthStateOperatorError:
	default:
		return false, fmt.Errorf("store: set oauth grant state: unknown state %q", state)
	}
	if actor == "" {
		actor = "system"
	}
	if len(reason) > MaxOAuthStateReason {
		reason = reason[:MaxOAuthStateReason]
	}
	var from, provider string
	if err := tx.tx.QueryRow(ctx, `
		SELECT state, provider FROM app.oauth_grants
		 WHERE user_id = $1 AND account_id = $2
		 FOR UPDATE`,
		tx.userID.String(), accountID.String()).Scan(&from, &provider); err != nil {
		if isNoRows(err) {
			return false, ErrOAuthGrantNotFound
		}
		return false, fmt.Errorf("store: set oauth grant state (read): %w", err)
	}
	if from == state {
		return false, nil
	}
	if _, err := tx.tx.Exec(ctx, `
		UPDATE app.oauth_grants
		   SET state = $3, state_reason = $4, state_changed_at = $5
		 WHERE user_id = $1 AND account_id = $2`,
		tx.userID.String(), accountID.String(), state, reason, at.UTC()); err != nil {
		return false, fmt.Errorf("store: set oauth grant state: %w", err)
	}
	if _, err := tx.AppendChange(ctx, ObjectAccount, accountID, OpUpdated); err != nil {
		return false, err
	}
	detail, err := json.Marshal(map[string]string{
		"provider": provider, "from": from, "to": state, "reason": reason,
	})
	if err != nil {
		return false, fmt.Errorf("store: audit oauth state: %w", err)
	}
	if _, err := tx.tx.Exec(ctx, `
		INSERT INTO app.audit_log (user_id, actor, action, object_type, object_id, detail)
		VALUES ($1, $2, 'oauth.state_changed', 'Account', $3, $4)`,
		tx.userID.String(), actor, accountID.String(), detail); err != nil {
		return false, fmt.Errorf("store: audit oauth state: %w", err)
	}
	return true, nil
}

// OAuthFlowRow is the input to CreateOAuthFlow. StateHash is SHA-256 of the
// OAuth `state` parameter (the raw state is never stored); VerifierCT is the
// sealed PKCE verifier. AccountID is the re-auth target, nil for a new account.
type OAuthFlowRow struct {
	ID         ulid.ULID
	StateHash  [32]byte
	Provider   string
	AccountID  *ulid.ULID
	VerifierCT []byte
	Nonce      string
	ExpiresAt  time.Time
}

// OAuthFlow is a consumed pending flow.
type OAuthFlow struct {
	ID         ulid.ULID
	Provider   string
	AccountID  *ulid.ULID
	VerifierCT []byte
	Nonce      string
	CreatedAt  time.Time
	ExpiresAt  time.Time
}

// CreateOAuthFlow records a pending connect for the caller. A re-auth target
// must be one of the caller's own accounts (ErrAccountNotFound otherwise), for
// the same reason CreateOAuthGrant selects from app.accounts.
func (tx *UserTx) CreateOAuthFlow(ctx context.Context, f OAuthFlowRow) error {
	switch {
	case f.ID == (ulid.ULID{}):
		return errors.New("store: create oauth flow: id is required")
	case f.Provider == "":
		return errors.New("store: create oauth flow: provider is required")
	case len(f.VerifierCT) == 0:
		return errors.New("store: create oauth flow: sealed verifier is required")
	case f.Nonce == "":
		return errors.New("store: create oauth flow: nonce is required")
	}
	var target *string
	if f.AccountID != nil {
		s := f.AccountID.String()
		target = &s
	}
	tag, err := tx.tx.Exec(ctx, `
		INSERT INTO app.oauth_flows (id, user_id, state_hash, provider, account_id, verifier_ct, nonce, expires_at)
		SELECT $2::text, $1::text, $3::bytea, $4::text, $5::text, $6::bytea, $7::text, $8::timestamptz
		 WHERE $5::text IS NULL
		    OR EXISTS (SELECT 1 FROM app.accounts WHERE user_id = $1::text AND id = $5::text)`,
		tx.userID.String(), f.ID.String(), f.StateHash[:], f.Provider, target,
		f.VerifierCT, f.Nonce, f.ExpiresAt.UTC())
	if err != nil {
		return fmt.Errorf("store: create oauth flow: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrAccountNotFound
	}
	return nil
}

// ConsumeOAuthFlow deletes and returns the caller's pending flow whose state
// hashes to stateHash. Single use by construction: the DELETE is the read, so
// a replayed state finds nothing (ErrOAuthFlowNotFound), and so does another
// user's state, which RLS hides. An expired flow is reported as
// ErrOAuthFlowExpired; if the caller rolls back on that error the row survives,
// is refused as expired again on any replay, and DeleteExpiredOAuthFlows
// removes it.
func (tx *UserTx) ConsumeOAuthFlow(ctx context.Context, stateHash [32]byte, now time.Time) (OAuthFlow, error) {
	var (
		f      OAuthFlow
		id     string
		target *string
	)
	err := tx.tx.QueryRow(ctx, `
		DELETE FROM app.oauth_flows
		 WHERE user_id = $1 AND state_hash = $2
		RETURNING id, provider, account_id, verifier_ct, nonce, created_at, expires_at`,
		tx.userID.String(), stateHash[:]).Scan(&id, &f.Provider, &target, &f.VerifierCT,
		&f.Nonce, &f.CreatedAt, &f.ExpiresAt)
	if err != nil {
		if isNoRows(err) {
			return OAuthFlow{}, ErrOAuthFlowNotFound
		}
		return OAuthFlow{}, fmt.Errorf("store: consume oauth flow: %w", err)
	}
	if f.ID, err = ulid.Parse(id); err != nil {
		return OAuthFlow{}, fmt.Errorf("store: oauth flow id %q: %w", id, err)
	}
	if target != nil {
		a, err := ulid.Parse(*target)
		if err != nil {
			return OAuthFlow{}, fmt.Errorf("store: oauth flow account id %q: %w", *target, err)
		}
		f.AccountID = &a
	}
	if !now.Before(f.ExpiresAt) {
		return OAuthFlow{}, ErrOAuthFlowExpired
	}
	return f, nil
}

// DeleteExpiredOAuthFlows removes the caller's expired pending flows and
// returns how many.
func (tx *UserTx) DeleteExpiredOAuthFlows(ctx context.Context, now time.Time) (int64, error) {
	tag, err := tx.tx.Exec(ctx, `
		DELETE FROM app.oauth_flows WHERE user_id = $1 AND expires_at <= $2`,
		tx.userID.String(), now.UTC())
	if err != nil {
		return 0, fmt.Errorf("store: delete expired oauth flows: %w", err)
	}
	return tag.RowsAffected(), nil
}
