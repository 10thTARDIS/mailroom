-- SPDX-License-Identifier: AGPL-3.0-only
--
-- app.oauth_code_consume checks the user BEFORE it marks an authorization code
-- used, as app.oauth_refresh_consume has done since migration 00091.
--
-- The code was marked used first and the user checked afterwards, so a refused
-- exchange for a disabled user burned the code. Codes are single-use and short
-- lived, and a disabled user cannot reach the authorization screen, so the harm
-- was small, but the two consume functions should behave the same way: a refusal
-- for a disabled user leaves the code unused, so a retry after re-enabling reads
-- as the first use and not as a replay (which would revoke the family).
--
-- EXPAND-ONLY (ADR-008): one replaced body, unchanged signature, return columns
-- and grants. The SECURITY DEFINER count is unchanged.

-- +goose Up
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
  SELECT (u.disabled_at IS NULL AND u.deleted_at IS NULL) INTO v_ok FROM app.users u WHERE u.id = r.user_id;
  -- A refusal for a disabled user must NOT use the code up (migration 00097).
  IF COALESCE(v_ok, false) AND NOT v_replay AND r.expires_at > now() THEN
    UPDATE app.oauth_codes c SET used_at = now() WHERE c.id = r.id;
  END IF;
  RETURN QUERY SELECT r.id, r.user_id, r.api_key_id, r.client_id, r.redirect_uri, r.code_challenge,
                      r.resource, r.scopes, r.expires_at, v_replay, COALESCE(v_ok, false);
END
$$;
-- +goose StatementEnd

-- +goose Down
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
