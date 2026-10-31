-- SPDX-License-Identifier: AGPL-3.0-only
--
-- SERVER-SIDE BROWSER SESSIONS (#937, ADR-048, increment 1).
--
-- EXPAND-ONLY (ADR-008). One new table; nothing existing changes. A browser
-- session was a stateless HMAC-signed cookie that could not be revoked, so
-- disabling a user or signing out ended nothing server-side. A row here is one
-- signed-in browser: the cookie carries only a random secret, this table holds
-- its SHA-256 (a database read yields no usable cookie, ADR-042's rule), and
-- the per-request check is one indexed lookup through app.session_for_token.
--
-- Differences from ADR-048 section 2's column list: there is NO refresh_dek.
-- refresh_ct and id_token_ct are sealed under a key derived from the master key
-- per user (HKDF, as oauthmail.SealFlowVerifier does), with the user id and
-- session id as associated data, so no per-row key needs storing.
--
-- No change_log rows: a session is not a JMAP-visible object (same class as
-- push_subscriptions and api_keys). RLS is FORCEd with the usual user policy;
-- the lookup function and its owner policy mirror api_keys_token_lookup /
-- app.api_key_for_token exactly. Sessions are Withheld from the user export
-- (they are credentials).

-- +goose Up
CREATE TABLE app.sessions (
  id              app.ulid PRIMARY KEY,
  user_id         app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  token_hash      bytea NOT NULL UNIQUE CHECK (octet_length(token_hash) = 32),
  refresh_ct      bytea,
  id_token_ct     bytea,
  generation      bigint NOT NULL DEFAULT 0,
  created_at      timestamptz NOT NULL DEFAULT now(),
  last_seen_at    timestamptz NOT NULL DEFAULT now(),
  last_checked_at timestamptz NOT NULL DEFAULT now(),
  user_agent      text NOT NULL DEFAULT '' CHECK (octet_length(user_agent) <= 512),
  last_addr       text NOT NULL DEFAULT '',
  revoked_at      timestamptz,
  revoked_reason  text CHECK (revoked_reason IN
                    ('signed_out', 'signed_out_elsewhere', 'replaced', 'refused', 'expired')),
  CHECK ((revoked_at IS NULL) = (revoked_reason IS NULL))
);
CREATE INDEX sessions_user_created ON app.sessions (user_id, created_at);
ALTER TABLE app.sessions ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.sessions FORCE ROW LEVEL SECURITY;
CREATE POLICY sessions_isolation ON app.sessions
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());
GRANT SELECT, INSERT, UPDATE ON app.sessions TO mailroom_app;

-- The per-request lookup (ADR-048 §3). The ONLY cross-user read of
-- app.sessions: by the hash of the presented cookie, returning the session id,
-- its user and the last provider confirmation, for a session not revoked whose
-- user can still sign in. Everything else is read under the user's own UserTx.
CREATE POLICY sessions_token_lookup ON app.sessions
  FOR SELECT TO mailroom_owner
  USING (revoked_at IS NULL);

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app.session_for_token(p_hash bytea)
RETURNS TABLE (session_id app.ulid, user_id app.ulid, last_checked_at timestamptz)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = app, pg_temp AS $$
  SELECT s.id, s.user_id, s.last_checked_at
    FROM app.sessions s
    JOIN app.users u ON u.id = s.user_id
   WHERE s.token_hash = p_hash
     AND s.revoked_at IS NULL
     AND u.disabled_at IS NULL
     AND u.deleted_at IS NULL
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION app.session_for_token(bytea) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.session_for_token(bytea) TO mailroom_app;
ALTER FUNCTION app.session_for_token(bytea) OWNER TO mailroom_owner;
GRANT SELECT ON app.sessions TO mailroom_owner;

-- +goose Down
DROP FUNCTION IF EXISTS app.session_for_token(bytea);
DROP TABLE IF EXISTS app.sessions;
