-- SPDX-License-Identifier: AGPL-3.0-only
--
-- MAIL-PROVIDER OAUTH: GRANTS AND PENDING FLOWS (#52, ADR-046).
--
-- EXPAND-ONLY (ADR-008). Two new tables; nothing existing changes.
-- app.accounts already has auth_kind ('password' | 'oauth2') and the sealed
-- credentials_ct, which is where the refresh token itself lives.
--
-- 1. app.oauth_grants: the QUERYABLE half of an OAuth account's grant, 1:1
--    with the account. Which provider, whose subject, which scopes were
--    actually granted, and the grant's state: 'active', 'needs_reauth' (the
--    provider said invalid_grant: only the user reconnecting fixes it) or
--    'operator_error' (the provider refused THIS SERVER's client credentials:
--    only the operator fixes it). Columns, not JSONB: this is queryable state.
--    `generation` is the compare-and-swap counter for refresh-token rotation:
--    the transaction that stores a rotated token in accounts.credentials_ct
--    bumps it WHERE generation = the value it read, so two workers refreshing
--    at once cannot overwrite a newer token with an older one. Rotation is not
--    a visible change and appends no change_log row; a STATE transition does
--    (ADR-046 §4).
--
-- 2. app.oauth_flows: a pending connect, single-use (consumed with DELETE ...
--    RETURNING), bound to the user who started it, expiring after minutes.
--    The OAuth `state` is stored only as its SHA-256; the PKCE verifier only
--    sealed. Never updated, so no UPDATE grant.
--
-- Both carry FORCE ROW LEVEL SECURITY with the usual deny-by-default user
-- policy, every index leads with user_id, and both are in the RLS negative
-- suite (internal/store/rls_test.go via fixtures_test.go's appTables).

-- +goose Up
CREATE TABLE app.oauth_grants (
  account_id       app.ulid PRIMARY KEY REFERENCES app.accounts(id) ON DELETE CASCADE,
  user_id          app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  provider         text NOT NULL CHECK (provider IN ('google', 'microsoft')),
  subject          text NOT NULL CHECK (subject <> ''),
  address          citext NOT NULL,
  scopes           text[] NOT NULL DEFAULT '{}',
  state            text NOT NULL DEFAULT 'active'
                     CHECK (state IN ('active', 'needs_reauth', 'operator_error')),
  state_reason     text NOT NULL DEFAULT '',
  state_changed_at timestamptz NOT NULL DEFAULT now(),
  generation       bigint NOT NULL DEFAULT 1 CHECK (generation > 0),
  last_refresh_at  timestamptz,
  created_at       timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT oauth_grants_one_per_subject UNIQUE (user_id, provider, subject)
);
CREATE INDEX oauth_grants_user_state ON app.oauth_grants (user_id, state);
ALTER TABLE app.oauth_grants ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.oauth_grants FORCE ROW LEVEL SECURITY;
CREATE POLICY oauth_grants_isolation ON app.oauth_grants
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());
GRANT SELECT, INSERT, UPDATE, DELETE ON app.oauth_grants TO mailroom_app;

CREATE TABLE app.oauth_flows (
  id          app.ulid PRIMARY KEY,
  user_id     app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  state_hash  bytea NOT NULL CHECK (octet_length(state_hash) = 32),
  provider    text NOT NULL CHECK (provider IN ('google', 'microsoft')),
  account_id  app.ulid REFERENCES app.accounts(id) ON DELETE CASCADE,
  verifier_ct bytea NOT NULL,
  nonce       text NOT NULL CHECK (nonce <> ''),
  created_at  timestamptz NOT NULL DEFAULT now(),
  expires_at  timestamptz NOT NULL,
  CONSTRAINT oauth_flows_expiry_after_creation CHECK (expires_at > created_at),
  CONSTRAINT oauth_flows_state_unique UNIQUE (user_id, state_hash)
);
CREATE INDEX oauth_flows_user_expiry ON app.oauth_flows (user_id, expires_at);
ALTER TABLE app.oauth_flows ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.oauth_flows FORCE ROW LEVEL SECURITY;
CREATE POLICY oauth_flows_isolation ON app.oauth_flows
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());
GRANT SELECT, INSERT, DELETE ON app.oauth_flows TO mailroom_app;

-- +goose Down
DROP TABLE IF EXISTS app.oauth_flows;
DROP TABLE IF EXISTS app.oauth_grants;
