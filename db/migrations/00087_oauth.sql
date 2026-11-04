-- SPDX-License-Identifier: AGPL-3.0-only
--
-- OAUTH AUTHORIZATION SERVER FOR /mcp (ADR-050, increment 1).
--
-- EXPAND-ONLY (ADR-008). Nothing existing changes meaning: an API key is still a
-- row with kind 'key' and a token_hash.
--
--   1. app.api_keys gains `kind` ('key' | 'oauth', default 'key') and the OAuth
--      client columns. token_hash becomes nullable, with
--      CHECK ((kind = 'key') = (token_hash IS NOT NULL)): an OAuth grant has no
--      token of its own. A grant carries every client field, a key none. One
--      grant per user and client (partial unique index).
--   2. app.api_key_events.anomaly admits 'refresh_reuse' and 'code_replay'.
--   3. app.oauth_tokens, app.oauth_requests, app.oauth_codes: user_id-led, FORCE
--      RLS, isolation policies, like every user table. Not JMAP-synced: no
--      change_log rows, as for app.api_keys.
--   4. app.oauth_clients (a user's manual clients; DCR entries later) and
--      app.oauth_cimd_cache. The cache holds no user's data and has NO policy
--      for mailroom_app; clients have an owner-scoped policy for manual rows
--      only. Registered clients and the cache are reached through definers.
--   5. Nine SECURITY DEFINER functions, each returning the minimum its caller
--      needs: the bearer lookup of an access token, client lookup and creation
--      (ids are minted server-side), CIMD cache get and put, code and
--      refresh redemption, RFC 7009 revocation, and the daily sweep. They run as
--      mailroom_owner, which FORCE binds too, hence one owner policy per table.
--
-- Down removes every OAuth grant and the new anomaly rows.

-- +goose Up
ALTER TABLE app.api_keys
  ADD COLUMN kind text NOT NULL DEFAULT 'key',
  ADD COLUMN oauth_client_id text,
  ADD COLUMN oauth_client_name text,
  ADD COLUMN oauth_client_kind text,
  ADD COLUMN oauth_redirect_host text;
ALTER TABLE app.api_keys ALTER COLUMN token_hash DROP NOT NULL;
ALTER TABLE app.api_keys
  ADD CONSTRAINT api_keys_kind_known CHECK (kind IN ('key', 'oauth')),
  ADD CONSTRAINT api_keys_oauth_client_kind_known CHECK (oauth_client_kind IN ('cimd', 'registered', 'manual')),
  ADD CONSTRAINT api_keys_token_by_kind CHECK ((kind = 'key') = (token_hash IS NOT NULL)),
  ADD CONSTRAINT api_keys_oauth_fields CHECK (
    (kind = 'oauth' AND oauth_client_id IS NOT NULL AND oauth_client_name IS NOT NULL
       AND oauth_client_kind IS NOT NULL AND oauth_redirect_host IS NOT NULL)
    OR (kind = 'key' AND oauth_client_id IS NULL AND oauth_client_name IS NULL
       AND oauth_client_kind IS NULL AND oauth_redirect_host IS NULL));
-- Revoked grants are kept for their audit and must not block the same client
-- being authorized again, so the uniqueness is over live grants only.
CREATE UNIQUE INDEX api_keys_oauth_client ON app.api_keys (user_id, oauth_client_id)
  WHERE kind = 'oauth' AND revoked_at IS NULL;

ALTER TABLE app.api_key_events DROP CONSTRAINT api_key_events_anomaly_check;
ALTER TABLE app.api_key_events ADD CONSTRAINT api_key_events_anomaly_check
  CHECK (anomaly IN ('mass_delete', 'mass_send', 'off_hours', 'critical_refusals', 'refresh_reuse', 'code_replay'));

-- OAuth tokens (migration 00087, ADR-050 section 3): the opaque bearer
-- credentials a grant has been issued. Only SHA-256 is stored. `family_id` is
-- the authorization code's id: every token minted from one authorization (and
-- by refreshing it) shares it, so one family can be revoked on its own. `scopes`
-- is a snapshot for display; the gate reads the grant's CURRENT scopes. Not a
-- JMAP-synced object: no change_log rows, like app.api_keys.
CREATE TABLE app.oauth_tokens (
  id            app.ulid PRIMARY KEY,
  user_id       app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  api_key_id    app.ulid NOT NULL REFERENCES app.api_keys(id) ON DELETE CASCADE,
  kind          text NOT NULL CHECK (kind IN ('access', 'refresh')),
  token_hash    bytea NOT NULL UNIQUE,
  family_id     app.ulid NOT NULL,
  resource      text NOT NULL,
  scopes        text[] NOT NULL,
  expires_at    timestamptz NOT NULL,
  used_at       timestamptz,
  revoked_at    timestamptz,
  created_at    timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT oauth_tokens_hash_is_sha256 CHECK (octet_length(token_hash) = 32),
  CONSTRAINT oauth_tokens_used_is_refresh CHECK (used_at IS NULL OR kind = 'refresh')
);
CREATE INDEX oauth_tokens_by_grant ON app.oauth_tokens (user_id, api_key_id, family_id);
-- The daily sweep scans by age across users; it is not a request-path query,
-- which is why this one index does not lead with user_id.
CREATE INDEX oauth_tokens_by_expiry ON app.oauth_tokens (expires_at);
ALTER TABLE app.oauth_tokens ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.oauth_tokens FORCE ROW LEVEL SECURITY;
CREATE POLICY oauth_tokens_isolation ON app.oauth_tokens
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());
-- The definers below run as mailroom_owner, which FORCE binds too. The rows hold
-- hashes of 256-bit random values and no mail content; what may be done with
-- them is the narrowness of those functions.
CREATE POLICY oauth_tokens_owner ON app.oauth_tokens
  FOR ALL TO mailroom_owner
  USING (true) WITH CHECK (true);
GRANT SELECT, INSERT ON app.oauth_tokens TO mailroom_app;
GRANT UPDATE (revoked_at) ON app.oauth_tokens TO mailroom_app;

-- A validated authorization request, held server-side (ADR-050 section 5). It
-- exists only for a signed-in user, created after the client and redirect URI
-- were validated; the consent page names it by an unguessable id (stored
-- hashed) so the form cannot change the client, redirect URI or PKCE challenge.
-- A user holds at most 20 live ones (the application evicts that user's oldest).
CREATE TABLE app.oauth_requests (
  id            app.ulid PRIMARY KEY,
  request_hash  bytea NOT NULL UNIQUE,
  user_id       app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  client_id     text NOT NULL,
  client_name   text NOT NULL,
  client_kind   text NOT NULL CHECK (client_kind IN ('cimd', 'registered', 'manual')),
  redirect_uri  text NOT NULL,
  redirect_host text NOT NULL,
  all_loopback  boolean NOT NULL,
  code_challenge text NOT NULL,
  resource      text NOT NULL,
  scopes        text[] NOT NULL,
  state         text,
  expires_at    timestamptz NOT NULL,
  decided_at    timestamptz,
  created_at    timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT oauth_requests_hash_is_sha256 CHECK (octet_length(request_hash) = 32),
  CONSTRAINT oauth_requests_state_bounded CHECK (state IS NULL OR char_length(state) <= 2048)
);
CREATE INDEX oauth_requests_by_user ON app.oauth_requests (user_id, expires_at);
CREATE INDEX oauth_requests_by_expiry ON app.oauth_requests (expires_at);
ALTER TABLE app.oauth_requests ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.oauth_requests FORCE ROW LEVEL SECURITY;
CREATE POLICY oauth_requests_isolation ON app.oauth_requests
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());
CREATE POLICY oauth_requests_owner ON app.oauth_requests
  FOR ALL TO mailroom_owner
  USING (true) WITH CHECK (true);
GRANT SELECT, INSERT, DELETE ON app.oauth_requests TO mailroom_app;
GRANT UPDATE (decided_at) ON app.oauth_requests TO mailroom_app;

-- An authorization code (ADR-050 sections 5 and 6): hashed, bound to the client,
-- redirect URI, PKCE challenge, resource, grant and user, valid 60 seconds,
-- single use. Its id is the family id of the tokens it produces, so a replayed
-- code can find and revoke the family it already created.
CREATE TABLE app.oauth_codes (
  id            app.ulid PRIMARY KEY,
  user_id       app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  api_key_id    app.ulid NOT NULL REFERENCES app.api_keys(id) ON DELETE CASCADE,
  code_hash     bytea NOT NULL UNIQUE,
  client_id     text NOT NULL,
  redirect_uri  text NOT NULL,
  code_challenge text NOT NULL,
  resource      text NOT NULL,
  scopes        text[] NOT NULL,
  expires_at    timestamptz NOT NULL,
  used_at       timestamptz,
  created_at    timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT oauth_codes_hash_is_sha256 CHECK (octet_length(code_hash) = 32)
);
CREATE INDEX oauth_codes_by_grant ON app.oauth_codes (user_id, api_key_id);
CREATE INDEX oauth_codes_by_expiry ON app.oauth_codes (expires_at);
ALTER TABLE app.oauth_codes ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.oauth_codes FORCE ROW LEVEL SECURITY;
CREATE POLICY oauth_codes_isolation ON app.oauth_codes
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());
CREATE POLICY oauth_codes_owner ON app.oauth_codes
  FOR ALL TO mailroom_owner
  USING (true) WITH CHECK (true);
GRANT SELECT, INSERT ON app.oauth_codes TO mailroom_app;

-- Registered OAuth clients (ADR-050 section 4): a user's MANUAL clients now,
-- and Dynamic Client Registration entries (kind 'registered', no owner) in a
-- later increment. One table so a client_id resolves through one function. A
-- manual client belongs to its creator: the app role sees and manages only its
-- own manual rows (policy below); registered rows have no policy for the app
-- role at all, so they are reachable only through the definers.
CREATE TABLE app.oauth_clients (
  id            text PRIMARY KEY,
  kind          text NOT NULL CHECK (kind IN ('manual', 'registered')),
  user_id       app.ulid REFERENCES app.users(id) ON DELETE CASCADE,
  name          text NOT NULL,
  redirect_uris text[] NOT NULL,
  created_at    timestamptz NOT NULL DEFAULT now(),
  last_used_at  timestamptz,
  -- The id is minted by the server (app.oauth_client_create) and its shape
  -- follows its kind, so a stored id can never be URL-shaped: a client_id that
  -- is an https URL is only ever a Client ID Metadata Document client.
  CONSTRAINT oauth_clients_id_shape CHECK (
    (kind = 'manual' AND id ~ '^mc_[0-9a-f]{32}$') OR (kind = 'registered' AND id ~ '^rc_[0-9a-f]{32}$')),
  CONSTRAINT oauth_clients_owner CHECK ((kind = 'manual') = (user_id IS NOT NULL)),
  CONSTRAINT oauth_clients_name_bounded CHECK (char_length(name) BETWEEN 1 AND 200),
  CONSTRAINT oauth_clients_uris_bounded CHECK (cardinality(redirect_uris) BETWEEN 1 AND 10)
);
CREATE INDEX oauth_clients_by_user ON app.oauth_clients (user_id, created_at);
ALTER TABLE app.oauth_clients ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.oauth_clients FORCE ROW LEVEL SECURITY;
CREATE POLICY oauth_clients_manual_isolation ON app.oauth_clients
  FOR ALL TO mailroom_app
  USING (kind = 'manual' AND user_id = app.current_user_id())
  WITH CHECK (kind = 'manual' AND user_id = app.current_user_id());
CREATE POLICY oauth_clients_owner ON app.oauth_clients
  FOR ALL TO mailroom_owner
  USING (true) WITH CHECK (true);
-- No INSERT: ids are minted by app.oauth_client_create only.
GRANT SELECT, DELETE ON app.oauth_clients TO mailroom_app;

-- The cache of fetched Client ID Metadata Documents (ADR-050 section 4). No
-- user's data: FORCE RLS with NO policy for mailroom_app and no grant, so the
-- application role cannot read or write it except through the two definers
-- below. A cached payload, hence JSONB.
CREATE TABLE app.oauth_cimd_cache (
  client_id     text PRIMARY KEY,
  document      jsonb NOT NULL,
  fetched_at    timestamptz NOT NULL DEFAULT now(),
  expires_at    timestamptz NOT NULL
);
CREATE INDEX oauth_cimd_cache_by_expiry ON app.oauth_cimd_cache (expires_at);
ALTER TABLE app.oauth_cimd_cache ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.oauth_cimd_cache FORCE ROW LEVEL SECURITY;
CREATE POLICY oauth_cimd_cache_owner ON app.oauth_cimd_cache
  FOR ALL TO mailroom_owner
  USING (true) WITH CHECK (true);

-- Bearer authentication for an OAuth access token (ADR-050 section 3). The
-- api_key_for_token shape: before any identity exists, the SHA-256 of a
-- presented token maps to (user_id, key_id), and nothing else leaves. Beyond
-- api_key_for_token's own predicate (grant neither revoked nor suspended, user
-- able to sign in) the token must be an unexpired, unrevoked ACCESS token, and
-- its grant must be an OAuth grant: an API key's row can never answer here.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app.oauth_access_for_token(p_hash bytea)
RETURNS TABLE (user_id app.ulid, key_id app.ulid)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = app, pg_temp AS $$
  SELECT t.user_id, t.api_key_id
    FROM app.oauth_tokens t
    JOIN app.api_keys k ON k.id = t.api_key_id AND k.user_id = t.user_id
    JOIN app.users u ON u.id = t.user_id
   WHERE t.token_hash = p_hash
     AND t.kind = 'access'
     AND t.revoked_at IS NULL
     AND t.expires_at > now()
     AND k.kind = 'oauth'
     AND k.revoked_at IS NULL
     AND k.suspended_at IS NULL
     AND u.disabled_at IS NULL
     AND u.deleted_at IS NULL
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION app.oauth_access_for_token(bytea) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.oauth_access_for_token(bytea) TO mailroom_app;
ALTER FUNCTION app.oauth_access_for_token(bytea) OWNER TO mailroom_owner;

-- A client by its client_id, for the authorization endpoint, which has no user
-- yet. Returns the registered or manual client's name and redirect URIs, and the
-- owner of a manual one (the consent step refuses another user's).
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app.oauth_client_resolve(p_id text)
RETURNS TABLE (id text, kind text, user_id app.ulid, name text, redirect_uris text[])
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = app, pg_temp AS $$
  SELECT c.id, c.kind, c.user_id, c.name, c.redirect_uris
    FROM app.oauth_clients c
   WHERE c.id = p_id
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION app.oauth_client_resolve(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.oauth_client_resolve(text) TO mailroom_app;
ALTER FUNCTION app.oauth_client_resolve(text) OWNER TO mailroom_owner;

-- The cached Client ID Metadata Document of a client_id, while fresh.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app.oauth_cimd_get(p_client_id text)
RETURNS TABLE (document jsonb, expires_at timestamptz)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = app, pg_temp AS $$
  SELECT c.document, c.expires_at
    FROM app.oauth_cimd_cache c
   WHERE c.client_id = p_client_id AND c.expires_at > now()
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION app.oauth_cimd_get(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.oauth_cimd_get(text) TO mailroom_app;
ALTER FUNCTION app.oauth_cimd_get(text) OWNER TO mailroom_owner;

-- Cache a fetched document. Any signed-in user can make the server fetch a URL of
-- their choosing (the authorize route is session-gated; an anonymous visitor
-- cannot), so the cache is bounded: past 5000 live rows a NEW client_id is simply
-- not cached (the caller still has the document it just fetched).
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app.oauth_cimd_put(p_client_id text, p_document jsonb, p_expires timestamptz)
RETURNS boolean
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = app, pg_temp AS $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM app.oauth_cimd_cache c WHERE c.client_id = p_client_id)
     AND (SELECT count(*) FROM app.oauth_cimd_cache c WHERE c.expires_at > now()) >= 5000 THEN
    RETURN false;
  END IF;
  INSERT INTO app.oauth_cimd_cache (client_id, document, fetched_at, expires_at)
  VALUES (p_client_id, p_document, now(), p_expires)
  ON CONFLICT (client_id) DO UPDATE
    SET document = EXCLUDED.document, fetched_at = EXCLUDED.fetched_at, expires_at = EXCLUDED.expires_at;
  RETURN true;
END
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION app.oauth_cimd_put(text, jsonb, timestamptz) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.oauth_cimd_put(text, jsonb, timestamptz) TO mailroom_app;
ALTER FUNCTION app.oauth_cimd_put(text, jsonb, timestamptz) OWNER TO mailroom_owner;

-- Create one of the CALLER's manual clients (the user whose transaction this
-- is, never a parameter). The client_id is minted here, so the application role
-- cannot choose it (it has no INSERT on the table) and a manual client can never
-- take the id of another client or of a URL.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app.oauth_client_create(p_name text, p_redirect_uris text[])
RETURNS text
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = app, pg_temp AS $$
DECLARE
  v_user app.ulid := app.current_user_id();
  v_id text := 'mc_' || replace(gen_random_uuid()::text, '-', '');
BEGIN
  IF v_user IS NULL THEN
    RAISE EXCEPTION 'oauth_client_create needs a user identity';
  END IF;
  INSERT INTO app.oauth_clients (id, kind, user_id, name, redirect_uris)
  VALUES (v_id, 'manual', v_user, p_name, p_redirect_uris);
  RETURN v_id;
END
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION app.oauth_client_create(text, text[]) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.oauth_client_create(text, text[]) TO mailroom_app;
ALTER FUNCTION app.oauth_client_create(text, text[]) OWNER TO mailroom_owner;

-- Redeem an authorization code at the token endpoint, which has no user. The
-- row is locked and marked used in one step, so two concurrent redemptions
-- cannot both succeed. `replay` is true when the code was ALREADY used (the
-- caller revokes the family it produced, OAuth 2.1 4.1.3); an expired code is
-- returned unmarked for the caller to refuse. user_ok is false for a user who
-- can no longer sign in.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app.oauth_code_consume(p_hash bytea)
RETURNS TABLE (id app.ulid, user_id app.ulid, api_key_id app.ulid, client_id text, redirect_uri text,
               code_challenge text, resource text, scopes text[], expires_at timestamptz,
               replay boolean, user_ok boolean)
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = app, pg_temp AS $$
DECLARE
  r app.oauth_codes%ROWTYPE;
  v_replay boolean;
  v_ok boolean;
BEGIN
  SELECT * INTO r FROM app.oauth_codes c WHERE c.code_hash = p_hash FOR UPDATE;
  IF NOT FOUND THEN
    RETURN;
  END IF;
  v_replay := r.used_at IS NOT NULL;
  IF NOT v_replay AND r.expires_at > now() THEN
    UPDATE app.oauth_codes c SET used_at = now() WHERE c.id = r.id;
  END IF;
  SELECT (u.disabled_at IS NULL AND u.deleted_at IS NULL) INTO v_ok FROM app.users u WHERE u.id = r.user_id;
  RETURN QUERY SELECT r.id, r.user_id, r.api_key_id, r.client_id, r.redirect_uri, r.code_challenge,
                      r.resource, r.scopes, r.expires_at, v_replay, COALESCE(v_ok, false);
END
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION app.oauth_code_consume(bytea) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.oauth_code_consume(bytea) TO mailroom_app;
ALTER FUNCTION app.oauth_code_consume(bytea) OWNER TO mailroom_owner;

-- Use a refresh token at the token endpoint (rotation). Locks the row and marks
-- a live token used in one step. The caller reads the flags: `revoked` (refuse,
-- quietly), `reuse` (the token was already used: revoke the family and raise the
-- anomaly), otherwise refuse if expires_at has passed, else issue the next pair.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app.oauth_refresh_consume(p_hash bytea)
RETURNS TABLE (id app.ulid, user_id app.ulid, api_key_id app.ulid, family_id app.ulid, resource text,
               scopes text[], expires_at timestamptz, revoked boolean, reuse boolean, user_ok boolean)
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = app, pg_temp AS $$
DECLARE
  r app.oauth_tokens%ROWTYPE;
  v_ok boolean;
BEGIN
  SELECT * INTO r FROM app.oauth_tokens t WHERE t.token_hash = p_hash AND t.kind = 'refresh' FOR UPDATE;
  IF NOT FOUND THEN
    RETURN;
  END IF;
  IF r.revoked_at IS NULL AND r.used_at IS NULL AND r.expires_at > now() THEN
    UPDATE app.oauth_tokens t SET used_at = now() WHERE t.id = r.id;
  END IF;
  SELECT (u.disabled_at IS NULL AND u.deleted_at IS NULL) INTO v_ok FROM app.users u WHERE u.id = r.user_id;
  RETURN QUERY SELECT r.id, r.user_id, r.api_key_id, r.family_id, r.resource, r.scopes, r.expires_at,
                      r.revoked_at IS NOT NULL, r.used_at IS NOT NULL, COALESCE(v_ok, false);
END
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION app.oauth_refresh_consume(bytea) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.oauth_refresh_consume(bytea) TO mailroom_app;
ALTER FUNCTION app.oauth_refresh_consume(bytea) OWNER TO mailroom_owner;

-- RFC 7009 revocation by the token itself, with no user: a refresh token revokes
-- its whole family, an access token only itself. Returns who and what was
-- revoked so the caller can audit it in that user's own transaction; no row for
-- an unknown token (the endpoint answers 200 either way).
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app.oauth_token_revoke(p_hash bytea)
RETURNS TABLE (user_id app.ulid, api_key_id app.ulid, family_id app.ulid, kind text, revoked bigint)
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = app, pg_temp AS $$
DECLARE
  r app.oauth_tokens%ROWTYPE;
  v_n bigint;
BEGIN
  SELECT * INTO r FROM app.oauth_tokens t WHERE t.token_hash = p_hash;
  IF NOT FOUND THEN
    RETURN;
  END IF;
  IF r.kind = 'refresh' THEN
    UPDATE app.oauth_tokens t SET revoked_at = now()
     WHERE t.user_id = r.user_id AND t.family_id = r.family_id AND t.revoked_at IS NULL;
  ELSE
    UPDATE app.oauth_tokens t SET revoked_at = now() WHERE t.id = r.id AND t.revoked_at IS NULL;
  END IF;
  GET DIAGNOSTICS v_n = ROW_COUNT;
  RETURN QUERY SELECT r.user_id, r.api_key_id, r.family_id, r.kind, v_n;
END
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION app.oauth_token_revoke(bytea) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.oauth_token_revoke(bytea) TO mailroom_app;
ALTER FUNCTION app.oauth_token_revoke(bytea) OWNER TO mailroom_owner;

-- The daily housekeeping: tokens expired or revoked more than a grace period
-- ago, codes and requests expired more than a grace period ago, and cached
-- client documents past their expiry. At most p_limit of each per call. One row
-- per kind, ZERO INCLUDED, so a run that deleted nothing reads differently from
-- a report that cannot see a kind at all.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app.sweep_oauth(p_now timestamptz, p_grace interval, p_limit int)
RETURNS TABLE (what text, deleted bigint)
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = app, pg_temp AS $$
DECLARE
  v_tokens   bigint;
  v_codes    bigint;
  v_requests bigint;
  v_cimd     bigint;
BEGIN
  WITH gone AS (
    DELETE FROM app.oauth_tokens t
     WHERE t.id IN (SELECT x.id FROM app.oauth_tokens x
                     WHERE x.expires_at < p_now - p_grace OR x.revoked_at < p_now - p_grace
                     LIMIT p_limit FOR UPDATE SKIP LOCKED)
    RETURNING 1)
  SELECT count(*) INTO v_tokens FROM gone;

  WITH gone AS (
    DELETE FROM app.oauth_codes c
     WHERE c.id IN (SELECT x.id FROM app.oauth_codes x
                     WHERE x.expires_at < p_now - p_grace
                     LIMIT p_limit FOR UPDATE SKIP LOCKED)
    RETURNING 1)
  SELECT count(*) INTO v_codes FROM gone;

  WITH gone AS (
    DELETE FROM app.oauth_requests q
     WHERE q.id IN (SELECT x.id FROM app.oauth_requests x
                     WHERE x.expires_at < p_now - p_grace
                     LIMIT p_limit FOR UPDATE SKIP LOCKED)
    RETURNING 1)
  SELECT count(*) INTO v_requests FROM gone;

  WITH gone AS (
    DELETE FROM app.oauth_cimd_cache m
     WHERE m.client_id IN (SELECT x.client_id FROM app.oauth_cimd_cache x
                            WHERE x.expires_at < p_now
                            LIMIT p_limit FOR UPDATE SKIP LOCKED)
    RETURNING 1)
  SELECT count(*) INTO v_cimd FROM gone;

  RETURN QUERY VALUES ('tokens'::text, v_tokens), ('codes'::text, v_codes),
                      ('requests'::text, v_requests), ('cimd'::text, v_cimd);
END
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION app.sweep_oauth(timestamptz, interval, int) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.sweep_oauth(timestamptz, interval, int) TO mailroom_app;
ALTER FUNCTION app.sweep_oauth(timestamptz, interval, int) OWNER TO mailroom_owner;

-- +goose Down
DROP FUNCTION IF EXISTS app.sweep_oauth(timestamptz, interval, int);
DROP FUNCTION IF EXISTS app.oauth_token_revoke(bytea);
DROP FUNCTION IF EXISTS app.oauth_refresh_consume(bytea);
DROP FUNCTION IF EXISTS app.oauth_code_consume(bytea);
DROP FUNCTION IF EXISTS app.oauth_client_create(text, text[]);
DROP FUNCTION IF EXISTS app.oauth_cimd_put(text, jsonb, timestamptz);
DROP FUNCTION IF EXISTS app.oauth_cimd_get(text);
DROP FUNCTION IF EXISTS app.oauth_client_resolve(text);
DROP FUNCTION IF EXISTS app.oauth_access_for_token(bytea);
DROP TABLE IF EXISTS app.oauth_cimd_cache;
DROP TABLE IF EXISTS app.oauth_clients;
DROP TABLE IF EXISTS app.oauth_codes;
DROP TABLE IF EXISTS app.oauth_requests;
DROP TABLE IF EXISTS app.oauth_tokens;
-- The rows that carry the new values are removed so the narrower constraints can
-- be restored. The tables are FORCE RLS and migrations run as mailroom_owner, so
-- the DML runs with FORCE lifted for the statement (docs/migrations.md).
ALTER TABLE app.api_key_events NO FORCE ROW LEVEL SECURITY;
DELETE FROM app.api_key_events WHERE anomaly IN ('refresh_reuse', 'code_replay');
ALTER TABLE app.api_key_events FORCE ROW LEVEL SECURITY;
ALTER TABLE app.api_key_events DROP CONSTRAINT api_key_events_anomaly_check;
ALTER TABLE app.api_key_events ADD CONSTRAINT api_key_events_anomaly_check
  CHECK (anomaly IN ('mass_delete', 'mass_send', 'off_hours', 'critical_refusals'));
ALTER TABLE app.api_keys NO FORCE ROW LEVEL SECURITY;
DELETE FROM app.api_keys WHERE kind = 'oauth';
ALTER TABLE app.api_keys FORCE ROW LEVEL SECURITY;
DROP INDEX IF EXISTS app.api_keys_oauth_client;
ALTER TABLE app.api_keys
  DROP CONSTRAINT IF EXISTS api_keys_oauth_fields,
  DROP CONSTRAINT IF EXISTS api_keys_token_by_kind,
  DROP CONSTRAINT IF EXISTS api_keys_oauth_client_kind_known,
  DROP CONSTRAINT IF EXISTS api_keys_kind_known;
ALTER TABLE app.api_keys ALTER COLUMN token_hash SET NOT NULL;
ALTER TABLE app.api_keys
  DROP COLUMN IF EXISTS oauth_redirect_host,
  DROP COLUMN IF EXISTS oauth_client_kind,
  DROP COLUMN IF EXISTS oauth_client_name,
  DROP COLUMN IF EXISTS oauth_client_id,
  DROP COLUMN IF EXISTS kind;
