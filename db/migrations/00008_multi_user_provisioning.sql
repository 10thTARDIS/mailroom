-- SPDX-License-Identifier: AGPL-3.0-only

-- OIDC multi-user provisioning (mailroom#98).
--
-- Additive: one UPDATE policy for the definer's owner plus two SECURITY DEFINER
-- functions over the existing app.users table, following the same owner-pinned
-- narrow-surface pattern as app.provision_user (mailroom#24) and
-- app.user_id_for_subject (mailroom#19). No table or column changes.
--
-- The access model changes from an allowlist-of-one to app-binding-is-the-gate:
-- ANY subject that completes the OIDC flow for this application is provisioned on
-- first login, and its role is re-evaluated on EVERY login from the token's group
-- membership. app.upsert_login is that get-or-create-and-refresh; app.provision_user
-- (boot-time, get-or-create only) stays for the dev-auth operator.
--
-- app.upsert_login FAILS CLOSED on a disabled or soft-deleted subject: it returns
-- NULL and mutates nothing, so such a user cannot log back in (the same rule
-- app.user_id_for_subject enforces). Its role argument is the ONE place role is
-- set from a verified token; that is deliberate (app-binding decided it), and it
-- is not an RLS escalation — mailroom_app can already assume any user id it knows
-- via SET mailroom.user_id, and the admin role governs the app plane, never RLS.
--
-- app.active_user_ids enumerates enabled users for the per-user fan-out of the
-- background loops (sync, indexer, Trash purge). RLS blocks mailroom_app from
-- listing app.users at all (users_self scopes it to one row; a SystemTx has no
-- identity), so the list comes through this definer, exposing user IDS only.
--
-- db/schema.sql carries the same definitions in its users section;
-- scripts/migrate-check.sh proves the two agree.

-- +goose Up
CREATE POLICY users_login_update ON app.users
  FOR UPDATE TO mailroom_owner
  USING (true) WITH CHECK (true);

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app.upsert_login(
  p_id app.ulid, p_subject text, p_email text, p_display_name text, p_role text
) RETURNS app.ulid
LANGUAGE plpgsql SECURITY DEFINER SET search_path = app AS $$
DECLARE
  v_id       app.ulid;
  v_disabled timestamptz;
  v_deleted  timestamptz;
BEGIN
  SELECT id, disabled_at, deleted_at
    INTO v_id, v_disabled, v_deleted
    FROM app.users
   WHERE oidc_subject = p_subject;

  IF v_id IS NOT NULL THEN
    -- A disabled or soft-deleted user may not log in: refuse (NULL) and leave
    -- the row untouched.
    IF v_disabled IS NOT NULL OR v_deleted IS NOT NULL THEN
      RETURN NULL;
    END IF;
    -- Existing, enabled user: refresh the identity fields and re-apply the role
    -- so group changes take effect on this login.
    UPDATE app.users
       SET email        = p_email,
           display_name = p_display_name,
           role         = p_role::app.user_role
     WHERE id = v_id;
    RETURN v_id;
  END IF;

  INSERT INTO app.users (id, oidc_subject, email, display_name, role)
       VALUES (p_id, p_subject, p_email, p_display_name, p_role::app.user_role)
  ON CONFLICT (oidc_subject) DO NOTHING
    RETURNING id INTO v_id;

  -- Lost a race with a concurrent first login of the same subject: the row now
  -- exists and its id — not our candidate — is the user's identity. Honor the
  -- disabled/deleted rule on that row too.
  IF v_id IS NULL THEN
    SELECT id INTO v_id FROM app.users
     WHERE oidc_subject = p_subject
       AND disabled_at IS NULL
       AND deleted_at IS NULL;
  END IF;

  RETURN v_id;
END;
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION app.upsert_login(app.ulid, text, text, text, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.upsert_login(app.ulid, text, text, text, text) TO mailroom_app;
ALTER FUNCTION app.upsert_login(app.ulid, text, text, text, text) OWNER TO mailroom_owner;
GRANT UPDATE ON app.users TO mailroom_owner;

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app.active_user_ids()
RETURNS SETOF app.ulid
LANGUAGE sql SECURITY DEFINER SET search_path = app AS $$
  SELECT id FROM app.users
   WHERE disabled_at IS NULL
     AND deleted_at IS NULL
   ORDER BY id
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION app.active_user_ids() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.active_user_ids() TO mailroom_app;
ALTER FUNCTION app.active_user_ids() OWNER TO mailroom_owner;

-- +goose Down
DROP FUNCTION IF EXISTS app.active_user_ids();
DROP FUNCTION IF EXISTS app.upsert_login(app.ulid, text, text, text, text);
DROP POLICY IF EXISTS users_login_update ON app.users;
