-- SPDX-License-Identifier: AGPL-3.0-only

-- Boot-time operator provisioning (mailroom#24).
--
-- Additive: one INSERT policy for the definer's owner plus one SECURITY DEFINER
-- get-or-create function over the existing app.users table, following the same
-- owner-pinned narrow-surface pattern as app.user_id_for_subject (mailroom#19)
-- and app.dequeue_due_jobs (mailroom#16). No table or column changes.
--
-- Why a definer at all: app.users grants mailroom_app SELECT only, scoped by the
-- users_self policy to the caller's own row — the app role literally cannot
-- INSERT a user, and at boot there is no identity to scope anything to anyway.
-- The server therefore provisions its single configured operator through this
-- one function, run inside a SystemTx.
--
-- The candidate id is generated APP-side (ADR-007: ULIDs are ours, not the
-- database's) and passed in; the function returns the pre-existing id when the
-- subject is already known, so calling it every boot is idempotent and never
-- duplicates or renames a user. An existing row is never mutated (no email
-- rewrite, no un-disabling) — provisioning is get-or-create, nothing more.
--
-- Privilege note: this lets mailroom_app create a users row (role defaults to
-- 'user'; the signature exposes no way to ask for 'admin') and learn one id. It
-- is not an escalation — mailroom_app can already assume any user id it knows
-- via SET mailroom.user_id — and it exposes no mail content.
--
-- db/schema.sql carries the same definitions in its users section;
-- scripts/migrate-check.sh proves the two agree.

-- +goose Up
CREATE POLICY users_provision_insert ON app.users
  FOR INSERT TO mailroom_owner
  WITH CHECK (true);

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app.provision_user(p_id app.ulid, p_subject text, p_email text)
RETURNS app.ulid
LANGUAGE plpgsql SECURITY DEFINER SET search_path = app AS $$
DECLARE
  v_id app.ulid;
BEGIN
  SELECT id INTO v_id FROM app.users WHERE oidc_subject = p_subject;
  IF v_id IS NOT NULL THEN
    RETURN v_id;
  END IF;

  INSERT INTO app.users (id, oidc_subject, email)
       VALUES (p_id, p_subject, p_email)
  ON CONFLICT (oidc_subject) DO NOTHING
    RETURNING id INTO v_id;

  -- Lost a race with a concurrent provision of the same subject: the row now
  -- exists and its id — not our candidate — is the user's identity.
  IF v_id IS NULL THEN
    SELECT id INTO v_id FROM app.users WHERE oidc_subject = p_subject;
  END IF;

  RETURN v_id;
END;
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION app.provision_user(app.ulid, text, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.provision_user(app.ulid, text, text) TO mailroom_app;
ALTER FUNCTION app.provision_user(app.ulid, text, text) OWNER TO mailroom_owner;
GRANT INSERT ON app.users TO mailroom_owner;

-- +goose Down
DROP FUNCTION IF EXISTS app.provision_user(app.ulid, text, text);
DROP POLICY IF EXISTS users_provision_insert ON app.users;
