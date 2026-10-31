-- SPDX-License-Identifier: AGPL-3.0-only

-- Login-time OIDC subject → mailroom user id lookup (mailroom#19).
-- Additive: one SELECT policy for the definer's owner plus one SECURITY DEFINER
-- function over the existing app.users table, following the same owner-pinned
-- narrow-surface pattern as app.dequeue_due_jobs (mailroom#16). No table or
-- column changes. db/schema.sql carries the same definitions in its users
-- section; scripts/migrate-check.sh proves the two agree.

-- +goose Up
CREATE POLICY users_subject_lookup ON app.users
  FOR SELECT TO mailroom_owner
  USING (true);

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app.user_id_for_subject(p_subject text)
RETURNS app.ulid
LANGUAGE sql SECURITY DEFINER SET search_path = app AS $$
  SELECT id FROM app.users
   WHERE oidc_subject = p_subject
     AND disabled_at IS NULL
     AND deleted_at IS NULL
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION app.user_id_for_subject(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.user_id_for_subject(text) TO mailroom_app;
ALTER FUNCTION app.user_id_for_subject(text) OWNER TO mailroom_owner;
GRANT SELECT ON app.users TO mailroom_owner;

-- +goose Down
DROP FUNCTION IF EXISTS app.user_id_for_subject(text);
DROP POLICY IF EXISTS users_subject_lookup ON app.users;
