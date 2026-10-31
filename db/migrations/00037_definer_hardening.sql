-- SPDX-License-Identifier: AGPL-3.0-only

-- SECURITY DEFINER HARDENING: TWO PROPERTIES THAT WERE TRUE BY REVIEW BECOME
-- TRUE BY CONSTRUCTION (Forgejo #309, items 2 and 4).
--
-- ⚠ NOTHING HERE FIXES A LIVE VULNERABILITY, and saying so plainly matters: the
-- definer layer audited clean. All eight functions already pin a search_path,
-- already REVOKE EXECUTE from PUBLIC, already reference every relation
-- schema-qualified, and each was verified to genuinely need DEFINER. This
-- migration removes the two ways a FUTURE change could quietly undo that.
--
-- (There are exactly EIGHT definers. `grep -c 'SECURITY DEFINER' db/schema.sql`
-- says 15 because seven matches are prose; `SELECT count(*) FROM pg_proc WHERE
-- prosecdef` says 8, and the schema and the live instance reconcile exactly.)
--
-- ─────────────────────────────────────────────────────────────────────────────
-- 1. `SET search_path = app` -> `SET search_path = app, pg_temp`
--
-- PostgreSQL searches pg_temp FIRST for any relation that is not
-- schema-qualified — before anything in search_path, and regardless of what
-- search_path says — UNLESS pg_temp is named in the path explicitly, in which
-- case it is searched exactly where it is written. Naming it LAST demotes it
-- behind app.
--
-- That matters because mailroom_app CAN create temporary objects: PUBLIC holds
-- TEMP on the database (`SELECT datacl FROM pg_database WHERE datname='mailroom'`
-- includes `=Tc/postgres`). So a definer body with one unqualified `FROM users`
-- would resolve to the CALLER'S `pg_temp.users` — a table the caller controls the
-- contents of, read with the definer's privileges. That is the classic definer
-- hijack, and today the only thing preventing it is that every relation
-- reference in all eight bodies happens to be qualified.
--
-- The unqualified FUNCTION and OPERATOR calls in those bodies (`now()`,
-- `lower()`, `split_part()`) were never exposed: pg_temp is never implicitly
-- searched for functions or operators, only for relations.
--
-- ALTER FUNCTION rather than CREATE OR REPLACE, deliberately: it changes the
-- proconfig and NOTHING else, so a body that has drifted from db/schema.sql
-- cannot be silently overwritten by a migration whose subject is a GUC. The
-- resulting proconfig is identical either way, which is what scripts/
-- migrate-check.sh diffs (pg_dump renders proconfig as part of the function
-- definition, so this one IS visible to the drift check — see item 2, which is
-- not).
--
-- ─────────────────────────────────────────────────────────────────────────────
-- 2. A default-privilege revoke, so a NINTH definer cannot be born
--    PUBLIC-executable
--
-- `SELECT * FROM pg_default_acl` was EMPTY. PostgreSQL grants EXECUTE to PUBLIC
-- on every new function, so each of the eight relies on its own explicit
-- `REVOKE ALL ON FUNCTION … FROM PUBLIC` — all present, all verified. A ninth
-- function added without that boilerplate would be executable by anyone who can
-- connect, and no test in the repo would notice.
--
-- Exposure today is ~0 (the only login roles are postgres, mailroom_owner and
-- mailroom_app), so this is purely about the next function.
--
-- ⚠ THE OBVIOUS SPELLING OF THIS STATEMENT DOES NOTHING, AND IT WAS WRITTEN THAT
-- WAY FIRST. `ALTER DEFAULT PRIVILEGES FOR ROLE mailroom_owner IN SCHEMA app
-- REVOKE EXECUTE ON FUNCTIONS FROM PUBLIC` is a SILENT NO-OP on PostgreSQL 16.
-- Measured on 16.14 rather than reasoned about, because the first version of the
-- test for this went red and the statement was the thing that was wrong:
--
--   ALTER DEFAULT PRIVILEGES  -- reported success
--   SELECT count(*) FROM pg_default_acl;                   -> 0
--   CREATE FUNCTION app.f() …;                             -- as mailroom_owner
--   SELECT proacl, has_function_privilege('public', …);    -> NULL, true
--
-- A SCHEMA-SCOPED default ACL is purely ADDITIVE: it is merged on top of
-- acldefault(), which for functions always contains PUBLIC EXECUTE, so a revoke
-- has nothing to subtract from and no row is even recorded. Adding a GRANT to
-- the same entry proves the merge direction — `{mailroom_app=X/owner}` is stored
-- and the created function comes out `{=X/owner, owner=X/owner,
-- mailroom_app=X/owner}`, PUBLIC still there.
--
-- The GLOBAL form (no `IN SCHEMA`) — the spelling PostgreSQL's own documentation
-- shows — is the one that works: pg_default_acl gets a `defaclnamespace = 0` row
-- holding `{mailroom_owner=X/mailroom_owner}`, and the next function is born
-- with exactly that ACL and `has_function_privilege('public', …) = false`, while
-- an explicit GRANT to mailroom_app still lands normally.
--
-- Global means "every schema mailroom_owner creates functions in", not just app.
-- That is wider than the issue asked for and it is the right width: migrations
-- are the only thing that runs as this role and app is the only schema they
-- write to, so the extra reach costs nothing and the narrower spelling costs
-- everything.
--
-- ⚠ IT IS INVISIBLE TO migrate-check, AND THAT IS A LIMIT WORTH RECORDING RATHER
-- THAN DISCOVERING. scripts/migrate-check.sh diffs `pg_dump --schema-only
-- --no-owner --no-privileges`, and --no-privileges suppresses ALTER DEFAULT
-- PRIVILEGES along with every GRANT. So the drift check can neither confirm this
-- landed nor catch it drifting from db/schema.sql; the integration lane is what
-- exercises it, because scripts/integration-test.sh applies db/schema.sql AS
-- mailroom_owner and therefore actually creates functions under this default.
--
-- FOR ROLE mailroom_owner, not the implicit "current role": migrations run as
-- mailroom_owner in production, the integration harness applies the schema as
-- mailroom_owner, and migrate-check applies both paths as the bootstrap
-- superuser. Naming the role makes all three mean the same thing.
--
-- ⚠ THE ONE EXCEPTION, AND WHY IT IS NOT OPTIONAL. app.current_user_id() is
-- called by almost every RLS policy in the schema, and a policy expression runs
-- with the privileges of the role executing the query. In db/schema.sql the
-- default-privilege revoke is placed BEFORE that function is created, so on a
-- fresh owner-applied build PUBLIC never gets EXECUTE on it — and without an
-- explicit grant every policy would fail with `permission denied for function
-- current_user_id` and the application could not read a single row. The grants
-- below give the two databases the same ACL: on a migrated database PUBLIC held
-- EXECUTE from before this migration, so it is revoked here too.
--
-- Failure mode if a role were missed: total and immediate (no rows anywhere for
-- that role), never subtle. That is the right shape for this control.

-- +goose Up

ALTER FUNCTION app.user_id_for_subject(text)                        SET search_path = app, pg_temp;
ALTER FUNCTION app.provision_user(app.ulid, text, text)             SET search_path = app, pg_temp;
ALTER FUNCTION app.upsert_login(app.ulid, text, text, text, text)   SET search_path = app, pg_temp;
ALTER FUNCTION app.active_user_ids()                                SET search_path = app, pg_temp;
ALTER FUNCTION app.dequeue_due_jobs(text, int)                      SET search_path = app, pg_temp;
ALTER FUNCTION app.complete_job(app.ulid, text)                     SET search_path = app, pg_temp;
ALTER FUNCTION app.fail_job(app.ulid, text, text, timestamptz)      SET search_path = app, pg_temp;
ALTER FUNCTION app.wkd_public_key(text, text)                       SET search_path = app, pg_temp;

ALTER DEFAULT PRIVILEGES FOR ROLE mailroom_owner
  REVOKE EXECUTE ON FUNCTIONS FROM PUBLIC;

REVOKE ALL ON FUNCTION app.current_user_id() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.current_user_id()
  TO mailroom_owner, mailroom_app, mailroom_admin, mailroom_wkd;

-- +goose Down

-- Back to `search_path = app` (no pg_temp terminator) and to PUBLIC-executable
-- new functions. The current_user_id grant is restored to PUBLIC rather than
-- merely dropped: rolling back the revoke without restoring the grant would
-- leave every RLS policy unable to call it, which is a broken database rather
-- than an earlier one.
ALTER FUNCTION app.user_id_for_subject(text)                        SET search_path = app;
ALTER FUNCTION app.provision_user(app.ulid, text, text)             SET search_path = app;
ALTER FUNCTION app.upsert_login(app.ulid, text, text, text, text)   SET search_path = app;
ALTER FUNCTION app.active_user_ids()                                SET search_path = app;
ALTER FUNCTION app.dequeue_due_jobs(text, int)                      SET search_path = app;
ALTER FUNCTION app.complete_job(app.ulid, text)                     SET search_path = app;
ALTER FUNCTION app.fail_job(app.ulid, text, text, timestamptz)      SET search_path = app;
ALTER FUNCTION app.wkd_public_key(text, text)                       SET search_path = app;

ALTER DEFAULT PRIVILEGES FOR ROLE mailroom_owner
  GRANT EXECUTE ON FUNCTIONS TO PUBLIC;

GRANT EXECUTE ON FUNCTION app.current_user_id() TO PUBLIC;
