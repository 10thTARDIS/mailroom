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

// The OAuth authorization server's storage (migration 00087, ADR-050).
//
// A grant to an OAuth client is a row in app.api_keys with kind 'oauth' (see
// APIKey), so every rule an agent obeys applies to it through the one key gate.
// This file holds the rest: app.oauth_tokens, app.oauth_requests,
// app.oauth_codes, the user's manual clients, and the cross-user paths the
// token endpoint and the bearer gate need (each a narrow SECURITY DEFINER
// function, the only way those callers reach the data).
//
// None of these is a JMAP-synced object: a write here appends NO change_log
// row, exactly like app.api_keys.

// Errors the OAuth surface distinguishes.
var (
	// ErrOAuthClientNotFound: not one of the caller's manual clients.
	ErrOAuthClientNotFound = errors.New("store: OAuth client not found")
	// ErrOAuthClientGrantExists: a live grant for this client already exists (a
	// concurrent authorization won the race).
	ErrOAuthClientGrantExists = errors.New("store: an OAuth grant for this client already exists")
)

// OAuth token kinds (app.oauth_tokens.kind).
const (
	OAuthTokenAccess  = "access"
	OAuthTokenRefresh = "refresh"
)

// OAuth client kinds, on a grant and on a request.
const (
	OAuthClientCIMD       = "cimd"
	OAuthClientRegistered = "registered"
	OAuthClientManual     = "manual"
)

// ---- bearer resolution -----------------------------------------------------

// ResolveOAuthAccessToken maps the SHA-256 of a presented access token to its
// user and grant, through app.oauth_access_for_token (ADR-050 section 3) run in
// a SystemTx because no identity exists yet. ok is false for anything that must
// be refused (unknown, expired, revoked, a refresh token, a revoked or suspended
// grant, a user who cannot sign in); the caller must not distinguish them.
func (s *Store) ResolveOAuthAccessToken(ctx context.Context, hash [32]byte) (userID, keyID ulid.ULID, ok bool, err error) {
	err = s.SystemTx(ctx, func(tx *SystemTx) error {
		var u, k string
		qerr := tx.tx.QueryRow(ctx, `SELECT user_id, key_id FROM app.oauth_access_for_token($1)`, hash[:]).Scan(&u, &k)
		if qerr != nil {
			if isNoRows(qerr) {
				return nil
			}
			return fmt.Errorf("store: resolve OAuth access token: %w", qerr)
		}
		var perr error
		if userID, perr = ulid.Parse(u); perr != nil {
			return fmt.Errorf("store: parse user id: %w", perr)
		}
		if keyID, perr = ulid.Parse(k); perr != nil {
			return fmt.Errorf("store: parse key id: %w", perr)
		}
		ok = true
		return nil
	})
	if err != nil {
		return ulid.ULID{}, ulid.ULID{}, false, err
	}
	return userID, keyID, ok, nil
}

// ---- clients ---------------------------------------------------------------

// OAuthClient is a registered client as the server holds it: a user's manual
// client, or (in a later increment) a Dynamic Client Registration entry.
// OwnerID is set for a manual client only.
type OAuthClient struct {
	ID           string
	Kind         string
	OwnerID      *ulid.ULID
	Name         string
	RedirectURIs []string
	CreatedAt    time.Time
}

// ResolveOAuthClient reads a registered client by client_id without an
// identity, through app.oauth_client_resolve. found is false for an id that is
// not a registered or manual client (a CIMD client_id is a URL and is never
// here).
func (s *Store) ResolveOAuthClient(ctx context.Context, id string) (c OAuthClient, found bool, err error) {
	err = s.SystemTx(ctx, func(tx *SystemTx) error {
		var (
			owner *string
			uris  []string
		)
		qerr := tx.tx.QueryRow(ctx, `SELECT id, kind, user_id, name, redirect_uris FROM app.oauth_client_resolve($1)`, id).
			Scan(&c.ID, &c.Kind, &owner, &c.Name, &uris)
		if qerr != nil {
			if isNoRows(qerr) {
				return nil
			}
			return fmt.Errorf("store: resolve OAuth client: %w", qerr)
		}
		if owner != nil {
			o, perr := ulid.Parse(*owner)
			if perr != nil {
				return fmt.Errorf("store: parse client owner: %w", perr)
			}
			c.OwnerID = &o
		}
		c.RedirectURIs = nonNilStrings(uris)
		found = true
		return nil
	})
	if err != nil {
		return OAuthClient{}, false, err
	}
	return c, found, nil
}

// MaxOAuthRequestsPerUser bounds a user's live authorization requests; creating
// one past it evicts that user's oldest.
const MaxOAuthRequestsPerUser = 20

// CreateOAuthClient creates one of the caller's manual clients through
// app.oauth_client_create. The client_id is minted by the database: the caller
// cannot choose it, and the application role cannot INSERT into the table.
func (tx *UserTx) CreateOAuthClient(ctx context.Context, name string, redirectURIs []string) (OAuthClient, error) {
	c := OAuthClient{Kind: OAuthClientManual, Name: name, RedirectURIs: redirectURIs}
	owner := tx.userID
	c.OwnerID = &owner
	if err := tx.tx.QueryRow(ctx, `SELECT app.oauth_client_create($1, $2)`, name, redirectURIs).Scan(&c.ID); err != nil {
		return OAuthClient{}, fmt.Errorf("store: create OAuth client: %w", err)
	}
	if err := tx.tx.QueryRow(ctx, `SELECT created_at FROM app.oauth_clients WHERE user_id = $1 AND id = $2`,
		tx.userID.String(), c.ID).Scan(&c.CreatedAt); err != nil {
		return OAuthClient{}, fmt.Errorf("store: read OAuth client: %w", err)
	}
	return c, nil
}

// ListOAuthClients returns the caller's manual clients, oldest first.
func (tx *UserTx) ListOAuthClients(ctx context.Context) ([]OAuthClient, error) {
	rows, err := tx.tx.Query(ctx, `
		SELECT id, kind, name, redirect_uris, created_at
		  FROM app.oauth_clients WHERE user_id = $1 ORDER BY created_at, id`, tx.userID.String())
	if err != nil {
		return nil, fmt.Errorf("store: list OAuth clients: %w", err)
	}
	defer rows.Close()
	var out []OAuthClient
	for rows.Next() {
		var c OAuthClient
		owner := tx.userID
		c.OwnerID = &owner
		if err := rows.Scan(&c.ID, &c.Kind, &c.Name, &c.RedirectURIs, &c.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: scan OAuth client: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CountOAuthClients counts the caller's manual clients, holding a per-user lock
// so two concurrent creates cannot both pass a cap checked against the count.
func (tx *UserTx) CountOAuthClients(ctx context.Context) (int, error) {
	if _, err := tx.tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		"oauth_client_create:"+tx.userID.String()); err != nil {
		return 0, fmt.Errorf("store: lock OAuth client creation: %w", err)
	}
	var n int
	if err := tx.tx.QueryRow(ctx, `SELECT count(*) FROM app.oauth_clients WHERE user_id = $1`,
		tx.userID.String()).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count OAuth clients: %w", err)
	}
	return n, nil
}

// DeleteOAuthClient removes one of the caller's manual clients. Grants already
// made through it stay (they are revoked separately).
func (tx *UserTx) DeleteOAuthClient(ctx context.Context, id string) error {
	tag, err := tx.tx.Exec(ctx, `DELETE FROM app.oauth_clients WHERE user_id = $1 AND id = $2`,
		tx.userID.String(), id)
	if err != nil {
		return fmt.Errorf("store: delete OAuth client: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrOAuthClientNotFound
	}
	return nil
}

// GetCIMDDocument returns the cached Client ID Metadata Document of a client_id
// while it is fresh, through app.oauth_cimd_get.
func (s *Store) GetCIMDDocument(ctx context.Context, clientID string) (doc []byte, expires time.Time, found bool, err error) {
	err = s.SystemTx(ctx, func(tx *SystemTx) error {
		qerr := tx.tx.QueryRow(ctx, `SELECT document, expires_at FROM app.oauth_cimd_get($1)`, clientID).Scan(&doc, &expires)
		if qerr != nil {
			if isNoRows(qerr) {
				return nil
			}
			return fmt.Errorf("store: get CIMD document: %w", qerr)
		}
		found = true
		return nil
	})
	if err != nil {
		return nil, time.Time{}, false, err
	}
	return doc, expires, found, nil
}

// PutCIMDDocument caches a validated document until expires, through
// app.oauth_cimd_put. It reports false when the cache is full and the document
// was not stored (the caller still has it).
func (s *Store) PutCIMDDocument(ctx context.Context, clientID string, doc []byte, expires time.Time) (stored bool, err error) {
	err = s.SystemTx(ctx, func(tx *SystemTx) error {
		if qerr := tx.tx.QueryRow(ctx, `SELECT app.oauth_cimd_put($1, $2::jsonb, $3)`,
			clientID, string(doc), expires.UTC()).Scan(&stored); qerr != nil {
			return fmt.Errorf("store: put CIMD document: %w", qerr)
		}
		return nil
	})
	return stored, err
}

// ---- authorization requests ------------------------------------------------

// OAuthRequest is a validated authorization request held server-side.
type OAuthRequest struct {
	ID            ulid.ULID
	UserID        ulid.ULID
	ClientID      string
	ClientName    string
	ClientKind    string
	RedirectURI   string
	RedirectHost  string
	AllLoopback   bool
	CodeChallenge string
	Resource      string
	Scopes        []string
	State         string
	ExpiresAt     time.Time
	DecidedAt     *time.Time
}

// NewOAuthRequest is a request to store. Hash is SHA-256 of the unguessable id
// the browser carries; the raw id is never stored.
type NewOAuthRequest struct {
	ID            ulid.ULID
	Hash          [32]byte
	ClientID      string
	ClientName    string
	ClientKind    string
	RedirectURI   string
	RedirectHost  string
	AllLoopback   bool
	CodeChallenge string
	Resource      string
	Scopes        []string
	State         string
}

// CreateOAuthRequest stores a validated authorization request for the caller.
// A user holds at most MaxOAuthRequestsPerUser live ones: the oldest beyond
// that are evicted first (and expired ones are cleared), so the cap is per user
// and one user cannot crowd out another.
func (tx *UserTx) CreateOAuthRequest(ctx context.Context, n NewOAuthRequest) error {
	var state *string
	if n.State != "" {
		state = &n.State
	}
	if _, err := tx.tx.Exec(ctx, `
		DELETE FROM app.oauth_requests
		 WHERE user_id = $1 AND (expires_at <= now() OR id IN (
		       SELECT id FROM app.oauth_requests WHERE user_id = $1 AND expires_at > now()
		        ORDER BY created_at DESC, id DESC OFFSET $2))`,
		tx.userID.String(), MaxOAuthRequestsPerUser-1); err != nil {
		return fmt.Errorf("store: evict OAuth requests: %w", err)
	}
	if _, err := tx.tx.Exec(ctx, `
		INSERT INTO app.oauth_requests (id, request_hash, user_id, client_id, client_name, client_kind,
		       redirect_uri, redirect_host, all_loopback, code_challenge, resource, scopes, state, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, now() + interval '10 minutes')`,
		n.ID.String(), n.Hash[:], tx.userID.String(), n.ClientID, n.ClientName, n.ClientKind, n.RedirectURI,
		n.RedirectHost, n.AllLoopback, n.CodeChallenge, n.Resource, nonNilStrings(n.Scopes), state); err != nil {
		return fmt.Errorf("store: create OAuth request: %w", err)
	}
	return nil
}

const oauthRequestColumns = `id, user_id, client_id, client_name, client_kind, redirect_uri, redirect_host,
	all_loopback, code_challenge, resource, scopes, COALESCE(state, ''), expires_at, decided_at`

func scanOAuthRequest(row interface{ Scan(...any) error }) (OAuthRequest, error) {
	var (
		r        OAuthRequest
		id, user string
	)
	if err := row.Scan(&id, &user, &r.ClientID, &r.ClientName, &r.ClientKind, &r.RedirectURI, &r.RedirectHost,
		&r.AllLoopback, &r.CodeChallenge, &r.Resource, &r.Scopes, &r.State, &r.ExpiresAt, &r.DecidedAt); err != nil {
		return OAuthRequest{}, err
	}
	var err error
	if r.ID, err = ulid.Parse(id); err != nil {
		return OAuthRequest{}, fmt.Errorf("store: parse request id: %w", err)
	}
	if r.UserID, err = ulid.Parse(user); err != nil {
		return OAuthRequest{}, fmt.Errorf("store: parse request user: %w", err)
	}
	r.Scopes = nonNilStrings(r.Scopes)
	return r, nil
}

// GetOAuthRequest reads the caller's own unexpired request by hash. Another
// user's request is invisible under RLS, indistinguishable from an unknown one.
func (tx *UserTx) GetOAuthRequest(ctx context.Context, hash [32]byte) (OAuthRequest, bool, error) {
	r, err := scanOAuthRequest(tx.tx.QueryRow(ctx, `SELECT `+oauthRequestColumns+`
		  FROM app.oauth_requests WHERE user_id = $1 AND request_hash = $2 AND expires_at > now()`,
		tx.userID.String(), hash[:]))
	if err != nil {
		if isNoRows(err) {
			return OAuthRequest{}, false, nil
		}
		return OAuthRequest{}, false, fmt.Errorf("store: get OAuth request: %w", err)
	}
	return r, true, nil
}

// DecideOAuthRequest marks the caller's request decided, once: it reports false
// for a request that is not the caller's, has expired, or was already decided
// (approved or denied), so a replayed or forged decision does nothing.
func (tx *UserTx) DecideOAuthRequest(ctx context.Context, id ulid.ULID) (bool, error) {
	tag, err := tx.tx.Exec(ctx, `
		UPDATE app.oauth_requests SET decided_at = now()
		 WHERE user_id = $1 AND id = $2 AND decided_at IS NULL AND expires_at > now()`,
		tx.userID.String(), id.String())
	if err != nil {
		return false, fmt.Errorf("store: decide OAuth request: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// ---- grants ----------------------------------------------------------------

// NewClientGrant is an OAuth grant to create: an api_keys row of kind 'oauth'.
type NewClientGrant struct {
	Name             string
	ClientID         string
	ClientName       string
	ClientKind       string
	RedirectHost     string
	Scopes           []string
	AccountIDs       []ulid.ULID
	ConfirmScopes    []string
	RatePerMin       int
	DailyBudget      int
	SuspendOnAnomaly bool
}

// CreateClientGrant inserts a grant. It reports ErrAPIKeyNameTaken for a name in
// use and ErrOAuthClientGrantExists when a live grant for the client already exists.
func (tx *UserTx) CreateClientGrant(ctx context.Context, n NewClientGrant) (APIKey, error) {
	id := NewULID()
	row := tx.tx.QueryRow(ctx, `
		INSERT INTO app.api_keys (id, user_id, name, token_hash, token_hint, scopes, account_ids,
		                          confirm_scopes, rate_per_min, daily_budget, suspend_on_anomaly,
		                          kind, oauth_client_id, oauth_client_name, oauth_client_kind, oauth_redirect_host)
		VALUES ($1, $2, $3, NULL, '', $4, $5, $6, $7, $8, $9, 'oauth', $10, $11, $12, $13)
		RETURNING `+apiKeyColumns,
		id.String(), tx.userID.String(), n.Name, n.Scopes, nullableULIDs(n.AccountIDs),
		nonNilStrings(n.ConfirmScopes), n.RatePerMin, n.DailyBudget, n.SuspendOnAnomaly,
		n.ClientID, n.ClientName, n.ClientKind, n.RedirectHost)
	k, err := scanAPIKey(row)
	if err != nil {
		if name, ok := uniqueViolationConstraint(err); ok {
			if name == "api_keys_oauth_client" {
				return APIKey{}, ErrOAuthClientGrantExists
			}
			return APIKey{}, ErrAPIKeyNameTaken
		}
		return APIKey{}, fmt.Errorf("store: create OAuth grant: %w", err)
	}
	return k, nil
}

// LockOAuthGrantForClient returns the caller's live (unrevoked) grant for the
// client, locked FOR UPDATE, or found=false.
func (tx *UserTx) LockOAuthGrantForClient(ctx context.Context, clientID string) (APIKey, bool, error) {
	k, err := scanAPIKey(tx.tx.QueryRow(ctx, `SELECT `+apiKeyColumns+`
		  FROM app.api_keys
		 WHERE user_id = $1 AND kind = 'oauth' AND oauth_client_id = $2 AND revoked_at IS NULL
		   FOR UPDATE`, tx.userID.String(), clientID))
	if err != nil {
		if isNoRows(err) {
			return APIKey{}, false, nil
		}
		return APIKey{}, false, fmt.Errorf("store: lock OAuth grant: %w", err)
	}
	return k, true, nil
}

// UpdateOAuthGrantClient refreshes the client's display fields on a re-
// authorization (a CIMD client may have renamed itself or moved hosts).
func (tx *UserTx) UpdateOAuthGrantClient(ctx context.Context, id ulid.ULID, name, kind, redirectHost string) error {
	if _, err := tx.tx.Exec(ctx, `
		UPDATE app.api_keys SET oauth_client_name = $3, oauth_client_kind = $4, oauth_redirect_host = $5
		 WHERE user_id = $1 AND id = $2 AND kind = 'oauth' AND revoked_at IS NULL`,
		tx.userID.String(), id.String(), name, kind, redirectHost); err != nil {
		return fmt.Errorf("store: update OAuth grant client: %w", err)
	}
	return nil
}

// APIKeyNames returns the names of every key of the caller, revoked ones
// included (a name stays taken after revocation, for the audit).
func (tx *UserTx) APIKeyNames(ctx context.Context) (map[string]bool, error) {
	rows, err := tx.tx.Query(ctx, `SELECT name FROM app.api_keys WHERE user_id = $1`, tx.userID.String())
	if err != nil {
		return nil, fmt.Errorf("store: list key names: %w", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, fmt.Errorf("store: scan key name: %w", err)
		}
		out[n] = true
	}
	return out, rows.Err()
}

// AuditAPIKeySystem records a system-initiated event on a key or grant in
// app.audit_log (actor 'system'): a replayed code, a reused refresh token.
func (tx *UserTx) AuditAPIKeySystem(ctx context.Context, action string, keyID ulid.ULID, detail map[string]any) error {
	b, err := json.Marshal(detail)
	if err != nil {
		return fmt.Errorf("store: audit API key: %w", err)
	}
	if _, err := tx.tx.Exec(ctx, `
		INSERT INTO app.audit_log (user_id, actor, action, object_type, object_id, detail)
		VALUES ($1, 'system', $2, 'ApiKey', $3, $4)`,
		tx.userID.String(), action, keyID.String(), b); err != nil {
		return fmt.Errorf("store: audit API key: %w", err)
	}
	return nil
}

// ---- codes -----------------------------------------------------------------

// NewOAuthCode is an authorization code to store. ID doubles as the family id
// of the tokens it produces.
type NewOAuthCode struct {
	ID            ulid.ULID
	Hash          [32]byte
	APIKeyID      ulid.ULID
	ClientID      string
	RedirectURI   string
	CodeChallenge string
	Resource      string
	Scopes        []string
	TTL           time.Duration
}

// InsertOAuthCode stores an authorization code for the caller's grant.
func (tx *UserTx) InsertOAuthCode(ctx context.Context, n NewOAuthCode) error {
	if _, err := tx.tx.Exec(ctx, `
		INSERT INTO app.oauth_codes (id, user_id, api_key_id, code_hash, client_id, redirect_uri,
		                             code_challenge, resource, scopes, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, now() + make_interval(secs => $10::float8))`,
		n.ID.String(), tx.userID.String(), n.APIKeyID.String(), n.Hash[:], n.ClientID, n.RedirectURI,
		n.CodeChallenge, n.Resource, nonNilStrings(n.Scopes), n.TTL.Seconds()); err != nil {
		return fmt.Errorf("store: insert OAuth code: %w", err)
	}
	return nil
}

// OAuthCode is a redeemed authorization code.
type OAuthCode struct {
	ID            ulid.ULID // the family id
	UserID        ulid.ULID
	APIKeyID      ulid.ULID
	ClientID      string
	RedirectURI   string
	CodeChallenge string
	Resource      string
	Scopes        []string
	ExpiresAt     time.Time
	// Replay: the code had ALREADY been used. The caller revokes the family.
	Replay bool
	// UserOK: the user can still sign in.
	UserOK bool
}

// ConsumeOAuthCode redeems a code by hash through app.oauth_code_consume: it
// marks a live code used in the same step it reads it, so two concurrent
// redemptions cannot both succeed. found is false for an unknown code.
func (s *Store) ConsumeOAuthCode(ctx context.Context, hash [32]byte) (c OAuthCode, found bool, err error) {
	err = s.SystemTx(ctx, func(tx *SystemTx) error {
		var id, user, key string
		qerr := tx.tx.QueryRow(ctx, `
			SELECT id, user_id, api_key_id, client_id, redirect_uri, code_challenge, resource, scopes,
			       expires_at, replay, user_ok FROM app.oauth_code_consume($1)`, hash[:]).
			Scan(&id, &user, &key, &c.ClientID, &c.RedirectURI, &c.CodeChallenge, &c.Resource, &c.Scopes,
				&c.ExpiresAt, &c.Replay, &c.UserOK)
		if qerr != nil {
			if isNoRows(qerr) {
				return nil
			}
			return fmt.Errorf("store: consume OAuth code: %w", qerr)
		}
		var perr error
		if c.ID, perr = ulid.Parse(id); perr != nil {
			return fmt.Errorf("store: parse code id: %w", perr)
		}
		if c.UserID, perr = ulid.Parse(user); perr != nil {
			return fmt.Errorf("store: parse code user: %w", perr)
		}
		if c.APIKeyID, perr = ulid.Parse(key); perr != nil {
			return fmt.Errorf("store: parse code key: %w", perr)
		}
		c.Scopes = nonNilStrings(c.Scopes)
		found = true
		return nil
	})
	if err != nil {
		return OAuthCode{}, false, err
	}
	return c, found, nil
}

// ---- tokens ----------------------------------------------------------------

// NewOAuthToken is a token to store (its hash only).
type NewOAuthToken struct {
	Kind      string
	Hash      [32]byte
	APIKeyID  ulid.ULID
	FamilyID  ulid.ULID
	Resource  string
	Scopes    []string
	ExpiresAt time.Time
}

// InsertOAuthTokens stores tokens for the caller's grant.
func (tx *UserTx) InsertOAuthTokens(ctx context.Context, tokens ...NewOAuthToken) error {
	for _, n := range tokens {
		if _, err := tx.tx.Exec(ctx, `
			INSERT INTO app.oauth_tokens (id, user_id, api_key_id, kind, token_hash, family_id, resource, scopes, expires_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			NewULID().String(), tx.userID.String(), n.APIKeyID.String(), n.Kind, n.Hash[:], n.FamilyID.String(),
			n.Resource, nonNilStrings(n.Scopes), n.ExpiresAt.UTC()); err != nil {
			return fmt.Errorf("store: insert OAuth token: %w", err)
		}
	}
	return nil
}

// RevokeOAuthFamily revokes every unrevoked token of a family of the caller's
// and reports how many. A family of another user revokes nothing (RLS).
func (tx *UserTx) RevokeOAuthFamily(ctx context.Context, familyID ulid.ULID) (int64, error) {
	tag, err := tx.tx.Exec(ctx, `
		UPDATE app.oauth_tokens SET revoked_at = now()
		 WHERE user_id = $1 AND family_id = $2 AND revoked_at IS NULL`,
		tx.userID.String(), familyID.String())
	if err != nil {
		return 0, fmt.Errorf("store: revoke OAuth family: %w", err)
	}
	return tag.RowsAffected(), nil
}

// OAuthFamily is one authorization's tokens: one per device that connected.
type OAuthFamily struct {
	ID         ulid.ULID
	APIKeyID   ulid.ULID
	CreatedAt  time.Time
	LastUsedAt time.Time // the latest token issue or refresh
	// ExpiresAt is when the family's newest refresh token lapses unused.
	ExpiresAt time.Time
	Revoked   bool
}

// ListOAuthFamilies returns the caller's token families that are not fully
// revoked, newest first, with at most limit per grant.
func (tx *UserTx) ListOAuthFamilies(ctx context.Context, limit int) ([]OAuthFamily, error) {
	rows, err := tx.tx.Query(ctx, `
		SELECT family_id, api_key_id, min(created_at), max(created_at),
		       COALESCE(max(expires_at) FILTER (WHERE kind = 'refresh'), max(expires_at)),
		       bool_and(revoked_at IS NOT NULL)
		  FROM app.oauth_tokens
		 WHERE user_id = $1
		 GROUP BY family_id, api_key_id
		HAVING NOT bool_and(revoked_at IS NOT NULL)
		 ORDER BY max(created_at) DESC
		 LIMIT $2`, tx.userID.String(), limit)
	if err != nil {
		return nil, fmt.Errorf("store: list OAuth families: %w", err)
	}
	defer rows.Close()
	var out []OAuthFamily
	for rows.Next() {
		var (
			f       OAuthFamily
			id, key string
		)
		if err := rows.Scan(&id, &key, &f.CreatedAt, &f.LastUsedAt, &f.ExpiresAt, &f.Revoked); err != nil {
			return nil, fmt.Errorf("store: scan OAuth family: %w", err)
		}
		var perr error
		if f.ID, perr = ulid.Parse(id); perr != nil {
			return nil, perr
		}
		if f.APIKeyID, perr = ulid.Parse(key); perr != nil {
			return nil, perr
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// OAuthRefresh is a refresh token presented at the token endpoint.
type OAuthRefresh struct {
	ID        ulid.ULID
	UserID    ulid.ULID
	APIKeyID  ulid.ULID
	FamilyID  ulid.ULID
	Resource  string
	Scopes    []string
	ExpiresAt time.Time
	// Revoked: the token (or its family) was revoked. Refuse, quietly.
	Revoked bool
	// Reuse: the token had ALREADY been used. Revoke the family and raise the
	// anomaly.
	Reuse  bool
	UserOK bool
}

// ConsumeOAuthRefresh uses a refresh token by hash through
// app.oauth_refresh_consume: a live token is marked used in the same step it is
// read. found is false for an unknown token.
func (s *Store) ConsumeOAuthRefresh(ctx context.Context, hash [32]byte) (r OAuthRefresh, found bool, err error) {
	err = s.SystemTx(ctx, func(tx *SystemTx) error {
		var id, user, key, fam string
		qerr := tx.tx.QueryRow(ctx, `
			SELECT id, user_id, api_key_id, family_id, resource, scopes, expires_at, revoked, reuse, user_ok
			  FROM app.oauth_refresh_consume($1)`, hash[:]).
			Scan(&id, &user, &key, &fam, &r.Resource, &r.Scopes, &r.ExpiresAt, &r.Revoked, &r.Reuse, &r.UserOK)
		if qerr != nil {
			if isNoRows(qerr) {
				return nil
			}
			return fmt.Errorf("store: consume OAuth refresh token: %w", qerr)
		}
		var perr error
		if r.ID, perr = ulid.Parse(id); perr != nil {
			return perr
		}
		if r.UserID, perr = ulid.Parse(user); perr != nil {
			return perr
		}
		if r.APIKeyID, perr = ulid.Parse(key); perr != nil {
			return perr
		}
		if r.FamilyID, perr = ulid.Parse(fam); perr != nil {
			return perr
		}
		r.Scopes = nonNilStrings(r.Scopes)
		found = true
		return nil
	})
	if err != nil {
		return OAuthRefresh{}, false, err
	}
	return r, found, nil
}

// OAuthRevocation is what an RFC 7009 revocation touched.
type OAuthRevocation struct {
	UserID   ulid.ULID
	APIKeyID ulid.ULID
	FamilyID ulid.ULID
	Kind     string
	Revoked  int64
}

// RevokeOAuthToken revokes a presented token by hash through
// app.oauth_token_revoke: a refresh token's whole family, or one access token.
// found is false for an unknown token.
func (s *Store) RevokeOAuthToken(ctx context.Context, hash [32]byte) (rev OAuthRevocation, found bool, err error) {
	err = s.SystemTx(ctx, func(tx *SystemTx) error {
		var user, key, fam string
		qerr := tx.tx.QueryRow(ctx, `SELECT user_id, api_key_id, family_id, kind, revoked FROM app.oauth_token_revoke($1)`, hash[:]).
			Scan(&user, &key, &fam, &rev.Kind, &rev.Revoked)
		if qerr != nil {
			if isNoRows(qerr) {
				return nil
			}
			return fmt.Errorf("store: revoke OAuth token: %w", qerr)
		}
		var perr error
		if rev.UserID, perr = ulid.Parse(user); perr != nil {
			return perr
		}
		if rev.APIKeyID, perr = ulid.Parse(key); perr != nil {
			return perr
		}
		if rev.FamilyID, perr = ulid.Parse(fam); perr != nil {
			return perr
		}
		found = true
		return nil
	})
	if err != nil {
		return OAuthRevocation{}, false, err
	}
	return rev, found, nil
}

// ---- sweep -----------------------------------------------------------------

// OAuthSweepKinds is every key SweepOAuth reports, in order.
var OAuthSweepKinds = []string{"cimd", "codes", "requests", "tokens"}

// DefaultOAuthSweepBatch bounds one call's deletes per kind.
const DefaultOAuthSweepBatch = 5000

// OAuthSweepGrace is how long an expired or revoked token, code or request is
// kept before the sweep deletes it.
const OAuthSweepGrace = 7 * 24 * time.Hour

// SweepOAuth deletes expired OAuth tokens, codes, requests and cached client
// documents past the grace period through app.sweep_oauth, and returns the
// count per kind, every kind present even at zero.
func (s *Store) SweepOAuth(ctx context.Context, now time.Time, grace time.Duration) (map[string]int64, error) {
	if grace <= 0 {
		return nil, fmt.Errorf("store: sweep OAuth: the grace period must be positive (%s)", grace)
	}
	out := make(map[string]int64, len(OAuthSweepKinds))
	for _, k := range OAuthSweepKinds {
		out[k] = 0
	}
	for i := 0; i < 100; i++ {
		var full bool
		err := s.SystemTx(ctx, func(tx *SystemTx) error {
			rows, err := tx.tx.Query(ctx, `SELECT what, deleted FROM app.sweep_oauth($1, $2, $3)`,
				now.UTC(), grace, DefaultOAuthSweepBatch)
			if err != nil {
				return fmt.Errorf("store: sweep OAuth: %w", err)
			}
			defer rows.Close()
			for rows.Next() {
				var (
					what string
					n    int64
				)
				if err := rows.Scan(&what, &n); err != nil {
					return fmt.Errorf("store: scan OAuth sweep: %w", err)
				}
				out[what] += n
				if n >= DefaultOAuthSweepBatch {
					full = true
				}
			}
			return rows.Err()
		})
		if err != nil {
			return out, err
		}
		if !full {
			break
		}
	}
	return out, nil
}
