-- SPDX-License-Identifier: AGPL-3.0-only
--
-- DYNAMIC CLIENT REGISTRATION (ADR-050 section 4, increment 3).
--
-- EXPAND-ONLY (ADR-008). Nothing existing changes meaning.
--
--   1. app.oauth_client_register: the ONLY way a 'registered' client row is
--      written. The endpoint is unauthenticated, so the table is bounded here:
--      past p_max_unused registrations that no live grant uses, a new one is
--      refused (it returns no row); nothing is evicted. The id is minted here
--      (rc_ + 32 hex), serialised by an advisory lock so two concurrent
--      registrations cannot both pass the count.
--   2. A registration is "used" while a live (unrevoked) OAuth grant names it.
--      Reading that is a cross-user read of app.api_keys by the definer, so
--      the owner role gains one narrow SELECT policy (OAuth grants, unrevoked)
--      and a client-led partial index. Only the definers run as that role.
--   3. app.sweep_oauth also deletes a registered client older than 24 hours
--      that no live grant uses, and reports it as 'clients'.
--
-- Down restores the previous sweep and removes the new objects; registered
-- clients stay (they are only rows).

-- +goose Up
CREATE POLICY api_keys_oauth_client_use ON app.api_keys
  FOR SELECT TO mailroom_owner
  USING (kind = 'oauth' AND revoked_at IS NULL);
-- Definer-only lookup ("does a live grant use this client?"), so it does not
-- lead with user_id: no application query plan reaches it.
CREATE INDEX api_keys_by_oauth_client ON app.api_keys (oauth_client_id)
  WHERE kind = 'oauth' AND revoked_at IS NULL;

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app.oauth_client_register(p_name text, p_redirect_uris text[], p_max_unused int)
RETURNS TABLE (client_id text, registered_at timestamptz)
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = app, pg_temp AS $$
DECLARE
  v_unused bigint;
  v_id     text := 'rc_' || replace(gen_random_uuid()::text, '-', '');
  v_at     timestamptz;
BEGIN
  PERFORM pg_advisory_xact_lock(hashtextextended('oauth_client_register', 0));
  SELECT count(*) INTO v_unused
    FROM app.oauth_clients c
   WHERE c.kind = 'registered'
     AND NOT EXISTS (SELECT 1 FROM app.api_keys k
                      WHERE k.kind = 'oauth' AND k.revoked_at IS NULL AND k.oauth_client_id = c.id);
  IF v_unused >= p_max_unused THEN
    RETURN;
  END IF;
  INSERT INTO app.oauth_clients (id, kind, user_id, name, redirect_uris)
  VALUES (v_id, 'registered', NULL, p_name, p_redirect_uris)
  RETURNING oauth_clients.created_at INTO v_at;
  RETURN QUERY SELECT v_id, v_at;
END
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION app.oauth_client_register(text, text[], int) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.oauth_client_register(text, text[], int) TO mailroom_app;
ALTER FUNCTION app.oauth_client_register(text, text[], int) OWNER TO mailroom_owner;

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app.sweep_oauth(p_now timestamptz, p_grace interval, p_limit int)
RETURNS TABLE (what text, deleted bigint)
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = app, pg_temp AS $$
DECLARE
  v_tokens   bigint;
  v_codes    bigint;
  v_requests bigint;
  v_cimd     bigint;
  v_clients  bigint;
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

  -- A registration (unauthenticated, ADR-050 section 4) that no live grant used
  -- within 24 hours. One a live grant names is kept for as long as it lives.
  WITH gone AS (
    DELETE FROM app.oauth_clients c
     WHERE c.id IN (SELECT x.id FROM app.oauth_clients x
                     WHERE x.kind = 'registered'
                       AND x.created_at < p_now - interval '24 hours'
                       AND NOT EXISTS (SELECT 1 FROM app.api_keys k
                                        WHERE k.kind = 'oauth' AND k.revoked_at IS NULL AND k.oauth_client_id = x.id)
                     LIMIT p_limit FOR UPDATE SKIP LOCKED)
    RETURNING 1)
  SELECT count(*) INTO v_clients FROM gone;

  RETURN QUERY VALUES ('tokens'::text, v_tokens), ('codes'::text, v_codes),
                      ('requests'::text, v_requests), ('cimd'::text, v_cimd),
                      ('clients'::text, v_clients);
END
$$;
-- +goose StatementEnd

-- +goose Down
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
DROP FUNCTION IF EXISTS app.oauth_client_register(text, text[], int);
DROP INDEX IF EXISTS app.api_keys_by_oauth_client;
DROP POLICY IF EXISTS api_keys_oauth_client_use ON app.api_keys;
