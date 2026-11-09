-- SPDX-License-Identifier: AGPL-3.0-only
--
-- Disable and re-enable a user (soft, reversible).
--
-- app.users.disabled_at has existed since the multi-user work and sign-in, the
-- session lookup, the API-key and OAuth lookups and the per-user fan-out already
-- refuse a user who has it set. Two things were missing:
--
--   1. A way to SET it that is not a hand-written UPDATE: app.set_user_disabled,
--      which refuses to disable the last enabled user and is safe to run twice.
--   2. The job dispatcher still ran a disabled user's due jobs (scheduled sends,
--      snooze wake-ups, the Trash purge). app.dequeue_due_jobs and
--      app.count_due_jobs now leave those rows pending, untouched, so enabling
--      the user again resumes them and nothing is lost.
--   3. app.oauth_refresh_consume used a refresh token up before checking the
--      user; see below.
--
-- EXPAND-ONLY (ADR-008): one new function and three replaced bodies with unchanged
-- signatures. No table or column changes.

-- +goose Up

-- The outcome is a closed set the caller prints verbatim:
--   not_found, disabled, already_disabled, enabled, already_enabled, last_enabled.
-- p_ref is a user id or an email address. A soft-deleted user is not found: there
-- is nothing to switch. The advisory lock serialises concurrent calls so two
-- operators disabling the last two users cannot both pass the "someone else is
-- still enabled" check.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app.set_user_disabled(p_ref text, p_disabled boolean)
RETURNS TABLE (out_id app.ulid, out_email text, outcome text)
LANGUAGE plpgsql SECURITY DEFINER SET search_path = app, pg_temp AS $$
DECLARE
  v_id       app.ulid;
  v_email    text;
  v_disabled timestamptz;
  v_others   int;
BEGIN
  PERFORM pg_advisory_xact_lock(hashtext('mailroom.set_user_disabled'));

  SELECT u.id, u.email::text, u.disabled_at
    INTO v_id, v_email, v_disabled
    FROM app.users u
   -- An id is matched as an id (any case, as ulid.Parse accepts), anything
   -- else as an email; each side is unique, so at most one row can match.
   WHERE (CASE WHEN upper(btrim(p_ref)) ~ '^[0-9A-HJKMNP-TV-Z]{26}$'
               THEN u.id::text = upper(btrim(p_ref))
               ELSE lower(u.email::text) = lower(btrim(p_ref)) END)
     AND u.deleted_at IS NULL;

  IF v_id IS NULL THEN
    RETURN QUERY SELECT NULL::app.ulid, NULL::text, 'not_found'::text;
    RETURN;
  END IF;

  IF p_disabled THEN
    IF v_disabled IS NOT NULL THEN
      RETURN QUERY SELECT v_id, v_email, 'already_disabled'::text;
      RETURN;
    END IF;
    SELECT count(*) INTO v_others FROM app.users u
     WHERE u.id <> v_id AND u.disabled_at IS NULL AND u.deleted_at IS NULL;
    IF v_others = 0 THEN
      RETURN QUERY SELECT v_id, v_email, 'last_enabled'::text;
      RETURN;
    END IF;
    UPDATE app.users u SET disabled_at = now() WHERE u.id = v_id;
    RETURN QUERY SELECT v_id, v_email, 'disabled'::text;
  ELSE
    IF v_disabled IS NULL THEN
      RETURN QUERY SELECT v_id, v_email, 'already_enabled'::text;
      RETURN;
    END IF;
    UPDATE app.users u SET disabled_at = NULL WHERE u.id = v_id;
    RETURN QUERY SELECT v_id, v_email, 'enabled'::text;
  END IF;
END;
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION app.set_user_disabled(text, boolean) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.set_user_disabled(text, boolean) TO mailroom_app;
ALTER FUNCTION app.set_user_disabled(text, boolean) OWNER TO mailroom_owner;

-- The dispatcher skips a disabled (or soft-deleted) user's jobs. System jobs
-- (user_id IS NULL) are unaffected.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app.dequeue_due_jobs(p_worker text, p_limit int)
RETURNS SETOF app.jobs
LANGUAGE sql SECURITY DEFINER SET search_path = app, pg_temp AS $$
  UPDATE app.jobs j SET state = 'running', locked_by = p_worker, locked_at = now(),
                        attempts = attempts + 1
  WHERE j.id IN (
    SELECT jj.id FROM app.jobs jj
    WHERE ((jj.state = 'pending' AND jj.fire_at <= now())
       -- Stranded by a process that died holding the lock (#351 B3); see
       -- db/migrations/00041 for the lease trade-off.
       OR (jj.state = 'running' AND jj.locked_at IS NOT NULL
           AND jj.locked_at < now() - interval '15 minutes'))
      AND (jj.user_id IS NULL OR EXISTS (
            SELECT 1 FROM app.users u
             WHERE u.id = jj.user_id AND u.disabled_at IS NULL AND u.deleted_at IS NULL))
    ORDER BY jj.fire_at
    FOR UPDATE OF jj SKIP LOCKED
    LIMIT p_limit
  )
  RETURNING j.*;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app.count_due_jobs()
RETURNS bigint
LANGUAGE sql SECURITY DEFINER STABLE SET search_path = app, pg_temp AS $$
  SELECT count(*) FROM app.jobs jj
   WHERE jj.state = 'pending' AND jj.fire_at <= now()
     AND (jj.user_id IS NULL OR EXISTS (
           SELECT 1 FROM app.users u
            WHERE u.id = jj.user_id AND u.disabled_at IS NULL AND u.deleted_at IS NULL));
$$;
-- +goose StatementEnd

-- A refresh token presented for a DISABLED user is refused without being used up.
-- It used to be marked used first and checked second, so a client retrying during
-- the disabled period burned its token, and after the user was enabled again its
-- next try read as a reuse and ended the whole grant family.
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
  SELECT (u.disabled_at IS NULL AND u.deleted_at IS NULL) INTO v_ok FROM app.users u WHERE u.id = r.user_id;
  -- A refusal for a disabled user must NOT use the token up: re-enabling the user
  -- restores the grant, and a token burned here would read as a REUSE on the
  -- client's next try and end the whole family.
  IF COALESCE(v_ok, false) AND r.revoked_at IS NULL AND r.used_at IS NULL AND r.expires_at > now() THEN
    UPDATE app.oauth_tokens t SET used_at = now() WHERE t.id = r.id;
  END IF;
  RETURN QUERY SELECT r.id, r.user_id, r.api_key_id, r.family_id, r.resource, r.scopes, r.expires_at,
                      r.revoked_at IS NOT NULL, r.used_at IS NOT NULL, COALESCE(v_ok, false);
END
$$;
-- +goose StatementEnd

-- The AI-tool approval sweep skips a disabled (or deleted) user too, so all of
-- a disabled user's background work stops.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app.api_key_pending_due_users()
RETURNS SETOF app.ulid
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = app, pg_temp AS $$
  SELECT DISTINCT p.user_id
    FROM app.api_key_pending p
   WHERE ((p.state = 'pending' AND p.expires_at <= now())
       OR (p.state = 'approved' AND p.ran_at IS NULL AND p.decided_at <= now() - interval '10 minutes'))
     -- A disabled (or deleted) user's background work stops (migration 00091).
     AND EXISTS (SELECT 1 FROM app.users u
                  WHERE u.id = p.user_id AND u.disabled_at IS NULL AND u.deleted_at IS NULL)
$$;
-- +goose StatementEnd

-- +goose Down
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

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app.count_due_jobs()
RETURNS bigint
LANGUAGE sql SECURITY DEFINER STABLE SET search_path = app, pg_temp AS $$
  SELECT count(*) FROM app.jobs
   WHERE state = 'pending' AND fire_at <= now();
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app.dequeue_due_jobs(p_worker text, p_limit int)
RETURNS SETOF app.jobs
LANGUAGE sql SECURITY DEFINER SET search_path = app, pg_temp AS $$
  UPDATE app.jobs j SET state = 'running', locked_by = p_worker, locked_at = now(),
                        attempts = attempts + 1
  WHERE j.id IN (
    SELECT id FROM app.jobs
    WHERE (state = 'pending' AND fire_at <= now())
       OR (state = 'running' AND locked_at IS NOT NULL
           AND locked_at < now() - interval '15 minutes')
    ORDER BY fire_at
    FOR UPDATE SKIP LOCKED
    LIMIT p_limit
  )
  RETURNING j.*;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app.api_key_pending_due_users()
RETURNS SETOF app.ulid
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = app, pg_temp AS $$
  SELECT DISTINCT p.user_id
    FROM app.api_key_pending p
   WHERE (p.state = 'pending' AND p.expires_at <= now())
      OR (p.state = 'approved' AND p.ran_at IS NULL AND p.decided_at <= now() - interval '10 minutes')
$$;
-- +goose StatementEnd

DROP FUNCTION IF EXISTS app.set_user_disabled(text, boolean);
