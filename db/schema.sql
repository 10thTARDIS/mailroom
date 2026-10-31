-- SPDX-License-Identifier: AGPL-3.0-only

-- ============================================================================
-- Mailroom — canonical Postgres schema (v0.1, targets spec v0.4)
-- ============================================================================
-- Assumptions:
--   * PostgreSQL 16+ (identity columns, FORCE RLS, jsonb, citext).
--   * IDs are ULIDs generated app-side (26-char Crockford base32), per ADR D7.
--   * All timestamps are timestamptz, UTC.
--   * This file is the reference DDL; the migration tool (ADR D8) owns the
--     real evolution history. CI must apply this file to a clean PG 16 and
--     run the RLS negative-test suite against it.
--   * NOT YET VALIDATED against a live server (authored offline) — the first
--     CI job to write is "psql -f schema.sql on scratch DB".
--
-- Invariants enforced by convention + tests (not expressible in pure DDL):
--   * Every mutation of a JMAP-visible object (messages, mailboxes, labels,
--     threads, identities, contacts, submissions) MUST append a change_log
--     row in the same transaction. Enforced by the store layer + tests;
--     optional hardening later via constraint triggers.
--   * The app obtains connections only through the tx wrapper, which SETs
--     mailroom.user_id (see app.current_user_id below).
-- ============================================================================

BEGIN;

CREATE EXTENSION IF NOT EXISTS citext;

-- ----------------------------------------------------------------------------
-- 0. Roles
-- ----------------------------------------------------------------------------
-- mailroom_owner : owns all objects; used ONLY by migrations. Never the app.
--
--                  ⚠ IT CAN LOG IN, WHATEVER THE `NOLOGIN` BELOW SUGGESTS
--                  (#309). Migrations connect AS this role, so every real
--                  deployment does `ALTER ROLE mailroom_owner LOGIN PASSWORD …`
--                  once, and the guarded CREATE ROLE below never re-applies —
--                  so the attribute sticks forever. Live: rolcanlogin = t.
--                  scripts/integration-test.sh creates it WITH LOGIN for
--                  exactly that reason, which is the honest shape.
--
--                  WHAT ACTUALLY PROTECTS THIS ROLE is therefore NOT the
--                  absence of a login. It is that mailroom_owner is
--                  NOT a superuser and does NOT hold BYPASSRLS, so FORCE ROW
--                  LEVEL SECURITY binds it like anyone else, and the only
--                  policies written FOR it are the two narrow ones the definer
--                  functions need (users_subject_lookup, jobs_dispatcher_owner).
--                  A credential-holder connecting as this role gets those two
--                  surfaces and nothing more — no mail content, because no
--                  policy on any mail table names mailroom_owner.
--
-- mailroom_app   : the application role. NOT the owner, no BYPASSRLS, so
--                  FORCE ROW LEVEL SECURITY applies to it.
-- mailroom_admin : admin-plane role. Gets privileges on user management and
--                  operational tables ONLY — deliberately NO grants on mail
--                  content tables. "Admin can't read mail" (spec §10) is
--                  implemented as the absence of grants + RLS, and audited.
-- mailroom_wkd   : owns ONE function — the anonymous WKD lookup — and NOTHING
--                  else. It exists precisely so that it owns no tables: a
--                  SECURITY DEFINER running as a table OWNER can never be
--                  narrowed, because an owner must keep full privileges or
--                  foreign-key cascades fail. See migration 00035 / #266 for
--                  the three attempts that established this the hard way.
--
-- (CREATE ROLE is cluster-level; guarded so the file is re-runnable.)
DO $$
BEGIN
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'mailroom_owner') THEN
    -- NOLOGIN is the SEED value, not a property of the installation: a fresh
    -- cluster gets a role with no password and no way in, and the deployment
    -- then grants it LOGIN so migrations can connect. See the note above — this
    -- statement runs exactly once, so it never takes LOGIN back.
    CREATE ROLE mailroom_owner NOLOGIN;
  END IF;
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'mailroom_app') THEN
    CREATE ROLE mailroom_app NOLOGIN;
  END IF;
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'mailroom_admin') THEN
    CREATE ROLE mailroom_admin NOLOGIN;
  END IF;
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'mailroom_wkd') THEN
    CREATE ROLE mailroom_wkd NOLOGIN;
  END IF;
  -- ALTER FUNCTION ... OWNER TO requires the current owner to hold SET
  -- membership of the target role (see the WKD block near app.user_keys).
  -- ⚠ Creating the role is NOT enough: in PostgreSQL 16 a CREATEROLE role gets
  -- ADMIN OPTION on what it creates, not SET membership, so this GRANT is
  -- load-bearing and must be attempted every time. Only insufficient_privilege
  -- is swallowed — the case where a superuser pre-created the role, which is
  -- what docs/backup-restore-runbook.md instructs before a pg_restore. See
  -- migration 00035 for both failures this shape prevents.
  BEGIN
    EXECUTE 'GRANT mailroom_wkd TO mailroom_owner';
  EXCEPTION WHEN insufficient_privilege THEN
    NULL;
  END;
END $$;

-- ----------------------------------------------------------------------------
-- 1. Schema, domains, helper functions
-- ----------------------------------------------------------------------------
CREATE SCHEMA IF NOT EXISTS app AUTHORIZATION mailroom_owner;

-- ⚠ NO FUNCTION IN app IS BORN PUBLIC-EXECUTABLE (migration 00037, #309).
--
-- PostgreSQL grants EXECUTE to PUBLIC on every new function by default. All
-- eight SECURITY DEFINERs in this file carry their own `REVOKE ALL ON FUNCTION
-- … FROM PUBLIC` immediately after their definition, and every one of them was
-- verified present — but that is a convention enforced by review. A NINTH
-- definer written without the boilerplate would be executable by anyone who can
-- connect, and nothing would say so.
--
-- This default ACL makes it structural instead: the boilerplate below each
-- function becomes belt-and-braces rather than the only control.
--
-- ⚠ THERE IS NO `IN SCHEMA app` HERE, AND ITS ABSENCE IS THE WHOLE STATEMENT.
-- The schema-scoped spelling — `ALTER DEFAULT PRIVILEGES FOR ROLE … IN SCHEMA
-- app REVOKE EXECUTE ON FUNCTIONS FROM PUBLIC` — is a SILENT NO-OP on
-- PostgreSQL 16. Measured, not assumed: it records nothing in pg_default_acl,
-- and a function created afterwards still comes out with proacl NULL and
-- has_function_privilege('public', …) = true. A schema-scoped default ACL is
-- purely ADDITIVE — it is merged on top of acldefault(), which for functions
-- always contains PUBLIC EXECUTE — so a revoke has nothing to subtract from.
-- The GLOBAL form is the one PostgreSQL's own documentation shows, and it is
-- the one that works: pg_default_acl gets a defaclnamespace = 0 row and the
-- next function is born {mailroom_owner=X/mailroom_owner}, PUBLIC absent.
--
-- FOR ROLE mailroom_owner explicitly, not "the current role": migrations run as
-- mailroom_owner in production and scripts/integration-test.sh applies this file
-- as mailroom_owner, but scripts/migrate-check.sh applies it as the bootstrap
-- superuser. Naming the role pins WHOSE default this is in all three, instead of
-- silently attaching it to whoever happened to run the file. (It follows that in
-- a superuser-applied database the default does not bind, because the functions
-- are then owned by that superuser — the same CI blind spot #266 was about, and
-- the reason the assertion for this lives in the integration lane, which applies
-- this file as mailroom_owner.)
--
-- ⚠ ORDERING IS LOAD-BEARING AND IT IS DELIBERATE THAT THIS COMES FIRST. It
-- applies to functions created AFTER it, so putting it at the end of the file
-- would leave everything above born PUBLIC-executable and a schema.sql-built
-- database would disagree with a migrations-built one — a divergence pg_dump
-- --no-privileges cannot show, so migrate-check would never catch it. Placing it
-- here costs one explicit grant, immediately below.
ALTER DEFAULT PRIVILEGES FOR ROLE mailroom_owner
  REVOKE EXECUTE ON FUNCTIONS FROM PUBLIC;

-- ULID: 26 chars, Crockford base32 (no I, L, O, U).
CREATE DOMAIN app.ulid AS text
  CHECK (VALUE ~ '^[0-9A-HJKMNP-TV-Z]{26}$');

-- SHA-256 content hash, raw bytes.
CREATE DOMAIN app.sha256 AS bytea
  CHECK (octet_length(VALUE) = 32);

-- The RLS anchor. Reads the per-transaction setting installed by the tx
-- wrapper. missing_ok=true => NULL when unset => every policy evaluates to
-- NULL => row invisible. FAIL CLOSED: no identity, no rows.
CREATE OR REPLACE FUNCTION app.current_user_id() RETURNS app.ulid
LANGUAGE sql STABLE PARALLEL SAFE AS $$
  SELECT NULLIF(current_setting('mailroom.user_id', true), '')::app.ulid
$$;
-- ⚠ THE ONE FUNCTION THAT MUST BE EXECUTABLE BY THE ROLES THAT RUN QUERIES, and
-- the reason the default-privilege revoke above needs an explicit exception.
-- Almost every RLS policy in this file calls it, and a policy expression is
-- evaluated with the privileges of the role running the query — so without these
-- grants EVERY policy fails with `permission denied for function
-- current_user_id` and the application cannot read one row. Named roles rather
-- than PUBLIC: a role added later must be granted deliberately, and the failure
-- if it is not is immediate, total and loud rather than subtle.
--
-- It is SECURITY INVOKER (no DEFINER here, correctly): it reads one GUC the
-- caller itself set and fails closed to NULL, so it grants no visibility of its
-- own — the grant is a calling right, not a privilege.
REVOKE ALL ON FUNCTION app.current_user_id() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.current_user_id()
  TO mailroom_owner, mailroom_app, mailroom_admin, mailroom_wkd;

-- ⚠ FOREIGN KEYS AND RLS: A SINGLE-COLUMN FK IS A CROSS-USER EXISTENCE ORACLE.
-- Read this before adding one (Forgejo #311 item 3).
--
-- PostgreSQL runs a foreign key's referential-integrity check with the
-- privileges of the REFERENCED table's owner, and that check BYPASSES RLS. So
-- for an FK that names only `(id)`:
--
--   * user A inserts a child row carrying `user_id = A`, which satisfies the
--     WITH CHECK on the child table, but naming a parent id that belongs to B;
--   * the RI check looks for that parent WITHOUT the RLS predicate, finds B's
--     row, and PERMITS the insert.
--
-- The difference between "insert succeeded" and SQLSTATE 23503 therefore tells
-- the caller whether an id exists in SOME tenant. 42 FKs in this file have that
-- shape today — messages.thread_id, attachments.message_id,
-- message_labels.{message_id,label_id,applied_by_rule_id},
-- message_provenance.{message_id,account_id,mailbox_id},
-- identities.default_key_id, and others.
--
-- WHY THIS IS ACCEPTED RATHER THAN FIXED. It leaks the existence of a SPECIFIC,
-- ALREADY-KNOWN id and never any content. Ids are 128-bit crypto/rand ULIDs
-- (internal/store/store.go), so they cannot be enumerated — an attacker who can
-- already name a foreign id has learned nothing they did not have. Closing it
-- means composite FKs including user_id everywhere, a very wide migration for a
-- leak with no practical reach.
--
-- CONTRAST, and the shape to copy when it matters: app.attachments references
-- app.blobs on the COMPOSITE `(user_id, content_hash)`. That FK cannot be an
-- oracle, because the parent lookup is already scoped to the caller's own rows.
--
-- So: single-column FK = deliberate, and the property above is what you are
-- choosing. If a new table's ids are ever guessable, or the parent's existence
-- is itself sensitive, use the composite form.

-- Enumerations ---------------------------------------------------------------
CREATE TYPE app.user_role        AS ENUM ('user', 'admin');
CREATE TYPE app.identity_kind    AS ENUM ('fixed', 'domain_wildcard');
CREATE TYPE app.object_type      AS ENUM ('Email', 'Mailbox', 'Thread',
                                          'Identity', 'EmailSubmission',
                                          'Contact', 'Label', 'Rule', 'Key',
                                          'Account', 'RemoteAllow',
                                          'ComposeSnippet', 'SavedSearch',
                                          'CategoryTab',
                                          'Area', 'DeskPreferences',
                                          'ClientPreferences', 'SetupRun',
                                          'OutOfOffice');
-- The Gmail-style category tabs (migration 00028, #219). A closed vocabulary:
-- a tab is only useful if something can decide it and the decider is code, so a
-- sixth tab needs a release regardless of how it is stored.
CREATE TYPE app.category_tab   AS ENUM ('primary', 'social', 'promotions',
                                        'updates', 'forums');
-- Which surface asked (migration 00046, #442's client_preferences). A closed
-- vocabulary for the same reason app.category_tab is one.
CREATE TYPE app.client_family AS ENUM ('web', 'phone', 'desktop');
CREATE TYPE app.change_op        AS ENUM ('created', 'updated', 'destroyed');
CREATE TYPE app.job_state        AS ENUM ('pending', 'running', 'done',
                                          'failed', 'dead');
-- 'suppressed' (ADR-028, migration 00056) is TERMINAL and never claimed: it is
-- what an intent becomes on an ingest-only account, forming a countable ledger
-- of every change Mailroom would have made to a mailbox it must not touch.
-- Lifting the mode does NOT drain it; replay is an explicit --apply command.
CREATE TYPE app.intent_state     AS ENUM ('pending', 'inflight', 'applied',
                                          'dead', 'suppressed');
CREATE TYPE app.key_kind         AS ENUM ('pgp', 'smime');
CREATE TYPE app.dsn_action       AS ENUM ('failed', 'delayed', 'delivered',
                                          'relayed', 'expanded');
CREATE TYPE app.provenance_state AS ENUM ('live', 'stale_uidvalidity');

-- ----------------------------------------------------------------------------
-- 2. Users (admin-plane table — the ONE table mailroom_admin can write)
-- ----------------------------------------------------------------------------
CREATE TABLE app.users (
  id            app.ulid PRIMARY KEY,
  oidc_subject  text NOT NULL UNIQUE,          -- Authentik subject claim
  email         citext NOT NULL UNIQUE,        -- login/display email
  display_name  text NOT NULL DEFAULT '',
  role          app.user_role NOT NULL DEFAULT 'user',
  -- quotas: enforced defaults (spec §10); admin UI may lag
  quota_storage_bytes   bigint NOT NULL DEFAULT 107374182400, -- 100 GiB
  quota_sync_connections int   NOT NULL DEFAULT 4,
  quota_llm_daily_tokens bigint NOT NULL DEFAULT 0,           -- 0 = disabled
  created_at    timestamptz NOT NULL DEFAULT now(),
  disabled_at   timestamptz,
  deleted_at    timestamptz                     -- soft; hard delete is a job
);
ALTER TABLE app.users ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.users FORCE ROW LEVEL SECURITY;

-- App plane: a user sees exactly their own row.
CREATE POLICY users_self ON app.users
  FOR SELECT TO mailroom_app
  USING (id = app.current_user_id());
-- Admin plane: full management, but via the admin role only.
CREATE POLICY users_admin_all ON app.users
  FOR ALL TO mailroom_admin
  USING (true) WITH CHECK (true);

-- Login-time subject → user id lookup (mailroom#19). RLS blocks a lookup by
-- oidc_subject as mailroom_app before any identity is set (no identity ⇒ no
-- rows), so login resolves the user through this narrow SECURITY DEFINER — the
-- SAME pattern as app.dequeue_due_jobs (data-model §5): an owner-run function
-- with a tight signature that exposes exactly one id, never mail content.
--
-- The definer runs as its owner mailroom_owner, and FORCE ROW LEVEL SECURITY
-- applies to the owner too, so without a policy for it the function would see
-- zero rows and no one could ever log in (the same subtlety as
-- jobs_dispatcher_owner, mailroom#16). users_subject_lookup IS that narrow
-- surface made explicit: SELECT visibility of app.users for mailroom_owner.
--
-- ⚠ THIS COMMENT USED TO SAY mailroom_owner WAS "a NOLOGIN role reachable only
-- through the definer function", AND THAT IS FALSE OF EVERY DEPLOYMENT (#309):
-- migrations connect as it, so it has LOGIN. The policy below is therefore
-- reachable by anyone holding that credential, not only through this function.
-- That is acceptable, and it is worth saying WHY rather than restating the
-- wrong reassurance: what this policy exposes is app.users — ids, subjects,
-- emails, quotas — and NOT ONE mail-content table names mailroom_owner in any
-- policy, so the role's reach ends here and at jobs_dispatcher_owner. The
-- protection is the narrowness of these two policies plus the role being
-- non-superuser and non-BYPASSRLS, never the absence of a login.
CREATE POLICY users_subject_lookup ON app.users
  FOR SELECT TO mailroom_owner
  USING (true);

-- The DELETE half of that owner surface (migration 00040, #54).
--
-- ⚠ WITHOUT THIS A HARD DELETE IS A SILENT NO-OP. An owner holds DELETE
-- implicitly — which is why information_schema reports a grant this file never
-- writes — so nothing raises a permission error; FORCE ROW LEVEL SECURITY just
-- filters the statement to zero rows and DELETE reports success.
--
-- ⚠ AND IT IS LOAD-BEARING ONLY TOGETHER WITH users_subject_lookup ABOVE. A
-- `DELETE ... WHERE` needs its rows SELECTABLE under RLS, not merely deletable:
-- with this policy and no owner SELECT policy, the delete removes zero rows
-- again. Narrowing users_subject_lookup silently breaks hard delete, which is
-- why internal/store/delete_policy_integration_test.go asserts the pair on
-- app.users itself.
CREATE POLICY users_owner_delete ON app.users
  FOR DELETE TO mailroom_owner
  USING (true);

COMMENT ON POLICY users_owner_delete ON app.users IS
  'Lets the table owner actually delete a user. Without it FORCE ROW LEVEL '
  'SECURITY filters the statement to zero rows and DELETE reports success — a '
  'silent no-op, because an owner holds DELETE implicitly and no permission '
  'error is raised. Works only in combination with users_subject_lookup, which '
  'gives the owner the SELECT visibility a DELETE ... WHERE requires (#54).';

-- ⚠ `SET search_path = app, pg_temp` — THE TERMINATOR IS THE POINT, and it is on
-- all eight definers in this file (migration 00037, #309).
--
-- PostgreSQL searches pg_temp FIRST for any relation that is not
-- schema-qualified, whatever the search_path says, UNLESS pg_temp appears in the
-- path explicitly — in which case it is searched exactly where it is written.
-- Putting it LAST therefore demotes it behind app. mailroom_app can create
-- temporary objects (PUBLIC holds TEMP on the database, `datacl` includes
-- `=Tc/postgres`), so without this a definer body containing one unqualified
-- `FROM users` would read the caller's `pg_temp.users` instead — the classic
-- definer hijack.
--
-- Zero-risk today, and that is exactly why it is worth closing: every relation
-- reference in all eight bodies IS schema-qualified, verified against the live
-- `pg_get_functiondef`. But that is a property enforced by review, and one
-- future unqualified reference reintroduces the whole class. With the terminator
-- it is enforced by the planner instead. (Unqualified FUNCTIONS and OPERATORS —
-- `now()`, `lower()`, `split_part()` — were never at risk: pg_temp is never
-- implicitly searched for those.)
CREATE OR REPLACE FUNCTION app.user_id_for_subject(p_subject text)
RETURNS app.ulid
LANGUAGE sql SECURITY DEFINER SET search_path = app, pg_temp AS $$
  SELECT id FROM app.users
   WHERE oidc_subject = p_subject
     AND disabled_at IS NULL
     AND deleted_at IS NULL
$$;
REVOKE ALL ON FUNCTION app.user_id_for_subject(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.user_id_for_subject(text) TO mailroom_app;
-- Pin the definer deterministically (same reason as app.dequeue_due_jobs): if a
-- superuser applied the schema, an unpinned function would be owned by that
-- superuser and bypass RLS, hiding the production semantics where the definer is
-- the non-superuser mailroom_owner and IS subject to RLS.
ALTER FUNCTION app.user_id_for_subject(text) OWNER TO mailroom_owner;
-- mailroom_owner holds this implicitly in production (it owns app.users); the
-- explicit grant makes the definer path work in CI/dev where the table is owned
-- by the bootstrap superuser instead.
GRANT SELECT ON app.users TO mailroom_owner;

-- Boot-time operator provisioning (mailroom#24). mailroom_app has SELECT on
-- app.users and nothing else — scoped by users_self to its own row — so it
-- cannot INSERT a user, and at boot there is no identity to scope anything to
-- anyway. The server provisions its single configured operator through this one
-- narrow SECURITY DEFINER, run inside a SystemTx.
--
-- users_provision_insert is the INSERT half of the owner surface that
-- users_subject_lookup opened for SELECT: without it, FORCE ROW LEVEL SECURITY
-- would silently reject the definer's INSERT (the definer runs as mailroom_owner
-- and is itself subject to RLS — the jobs_dispatcher_owner subtlety again).
--
-- The candidate id is generated APP-side (ADR-007) and passed in; the function
-- returns the pre-existing id when the subject is already known, so calling it
-- on every boot is idempotent — it never duplicates a user and never mutates an
-- existing row (no email rewrite, no un-disabling). Get-or-create, nothing more.
--
-- Privilege note: this lets mailroom_app create a users row (role defaults to
-- 'user'; the signature offers no way to request 'admin') and learn one id. It is
-- not an escalation — mailroom_app can already assume any user id it knows via
-- SET mailroom.user_id — and it exposes no mail content.
CREATE POLICY users_provision_insert ON app.users
  FOR INSERT TO mailroom_owner
  WITH CHECK (true);

CREATE OR REPLACE FUNCTION app.provision_user(p_id app.ulid, p_subject text, p_email text)
RETURNS app.ulid
LANGUAGE plpgsql SECURITY DEFINER SET search_path = app, pg_temp AS $$
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
REVOKE ALL ON FUNCTION app.provision_user(app.ulid, text, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.provision_user(app.ulid, text, text) TO mailroom_app;
-- Pin the definer deterministically (same reason as app.user_id_for_subject).
ALTER FUNCTION app.provision_user(app.ulid, text, text) OWNER TO mailroom_owner;
-- Implicit in production (mailroom_owner owns app.users); explicit so the
-- definer path also works in CI/dev, where the bootstrap superuser owns it.
GRANT INSERT ON app.users TO mailroom_owner;

-- OIDC multi-user provisioning (mailroom#98). App-binding is the access gate:
-- ANY subject that completes the OIDC flow is provisioned on first login, and its
-- role is re-evaluated on EVERY login from the token's group membership.
-- app.upsert_login is that get-or-create-and-refresh (app.provision_user stays for
-- the boot-time dev-auth operator). It FAILS CLOSED on a disabled/soft-deleted
-- subject — returns NULL, mutates nothing — so such a user cannot log back in, the
-- same rule app.user_id_for_subject enforces. Its role argument is the one place
-- role is set from a verified token, which is deliberate and not an RLS escalation
-- (mailroom_app can already assume any user id via SET mailroom.user_id, and the
-- admin role governs the app plane, never RLS).
--
-- users_login_update is the UPDATE half of the owner surface that
-- users_provision_insert opened for INSERT and users_subject_lookup for SELECT:
-- without it, FORCE ROW LEVEL SECURITY silently rejects the definer's UPDATE (the
-- definer runs as mailroom_owner and is itself subject to RLS).
CREATE POLICY users_login_update ON app.users
  FOR UPDATE TO mailroom_owner
  USING (true) WITH CHECK (true);

CREATE OR REPLACE FUNCTION app.upsert_login(
  p_id app.ulid, p_subject text, p_email text, p_display_name text, p_role text
) RETURNS app.ulid
LANGUAGE plpgsql SECURITY DEFINER SET search_path = app, pg_temp AS $$
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
REVOKE ALL ON FUNCTION app.upsert_login(app.ulid, text, text, text, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.upsert_login(app.ulid, text, text, text, text) TO mailroom_app;
-- Pin the definer deterministically (same reason as app.user_id_for_subject).
ALTER FUNCTION app.upsert_login(app.ulid, text, text, text, text) OWNER TO mailroom_owner;
-- Implicit in production (mailroom_owner owns app.users); explicit so the definer
-- path also works in CI/dev, where the bootstrap superuser owns it.
GRANT UPDATE ON app.users TO mailroom_owner;

-- Per-user fan-out of the background loops (sync, indexer, Trash purge) needs the
-- set of enabled users. RLS blocks mailroom_app from listing app.users (users_self
-- scopes it to one row; a SystemTx has no identity), so it comes through this
-- narrow definer, exposing user IDS only — never mail. Same owner-pinned pattern.
CREATE OR REPLACE FUNCTION app.active_user_ids()
RETURNS SETOF app.ulid
LANGUAGE sql SECURITY DEFINER SET search_path = app, pg_temp AS $$
  SELECT id FROM app.users
   WHERE disabled_at IS NULL
     AND deleted_at IS NULL
   ORDER BY id
$$;
REVOKE ALL ON FUNCTION app.active_user_ids() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.active_user_ids() TO mailroom_app;
ALTER FUNCTION app.active_user_ids() OWNER TO mailroom_owner;

-- ----------------------------------------------------------------------------
-- 3. Per-user change sequencing (drives JMAP state strings)
-- ----------------------------------------------------------------------------
-- One counter row per (user, type). Writers take a row lock via
-- UPDATE ... RETURNING, which serializes sequence allocation per user+type —
-- correct and plenty fast at ≤ ~24 users. (A per-user hot row is the known
-- cost; revisit only if contention is ever measured.)
CREATE TABLE app.change_seq (
  user_id     app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  object_type app.object_type NOT NULL,
  last_seq    bigint NOT NULL DEFAULT 0,
  PRIMARY KEY (user_id, object_type)
);
ALTER TABLE app.change_seq ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.change_seq FORCE ROW LEVEL SECURITY;
CREATE POLICY change_seq_isolation ON app.change_seq
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());

-- The change log itself: the single ordered stream consumed by JMAP /changes,
-- the search indexer, and client sync cursors (spec §13/§14).
CREATE TABLE app.change_log (
  user_id     app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  object_type app.object_type NOT NULL,
  seq         bigint NOT NULL,
  object_id   app.ulid NOT NULL,
  op          app.change_op NOT NULL,
  created_at  timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, object_type, seq)
);
-- /changes scans: (user, type, seq > since) — covered by the PK.
-- Collapse step needs per-object lookups within the scanned range only.
ALTER TABLE app.change_log ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.change_log FORCE ROW LEVEL SECURITY;
CREATE POLICY change_log_isolation ON app.change_log
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());

-- Compaction floor: /changes requests with sinceState older than this floor
-- return cannotCalculateChanges (client refetches via /query). One row per
-- (user, type), advanced by the compaction job.
CREATE TABLE app.change_floor (
  user_id     app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  object_type app.object_type NOT NULL,
  floor_seq   bigint NOT NULL DEFAULT 0,
  PRIMARY KEY (user_id, object_type)
);
ALTER TABLE app.change_floor ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.change_floor FORCE ROW LEVEL SECURITY;
CREATE POLICY change_floor_isolation ON app.change_floor
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());

-- ----------------------------------------------------------------------------
-- 4. Accounts, identities, mailboxes
-- ----------------------------------------------------------------------------
CREATE TABLE app.accounts (
  id            app.ulid PRIMARY KEY,
  user_id       app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  name          text NOT NULL,
  provider_profile text NOT NULL DEFAULT 'generic',  -- 'migadu', 'gmail', ...
  imap_host     text NOT NULL,
  imap_port     int  NOT NULL DEFAULT 993,
  smtp_host     text NOT NULL,
  smtp_port     int  NOT NULL DEFAULT 465,
  auth_kind     text NOT NULL DEFAULT 'password',    -- 'password' | 'oauth2'
  -- credentials: AEAD blob under the user's credentials DEK (spec §9)
  credentials_ct  bytea NOT NULL,
  credentials_dek app.ulid NOT NULL,                 -- key id in crypto svc
  envelope_ver  smallint NOT NULL DEFAULT 1,
  -- outbound ledger configuration (spec §5): provider daily cap
  outbound_daily_quota int NOT NULL DEFAULT 100,
  sync_enabled  boolean NOT NULL DEFAULT true,
  created_at    timestamptz NOT NULL DEFAULT now(),
  -- presentation: user-chosen color for a unified inbox (NULL = none), spec §12
  color         text,
  -- INGEST-ONLY MODE (ADR-028, #521). false = this account syncs normally but
  -- Mailroom never constructs an IMAPWriter for it, so it CANNOT mutate the
  -- remote mailbox. Orthogonal to sync_enabled above (which gates the whole
  -- pass, ingest included) and to mailboxes.sync_scope (per-mailbox, pull-side
  -- only, and never read by the writeback path).
  --
  -- ⚠ KEEP THIS LAST. `ALTER TABLE ... ADD COLUMN` appends (migration 00056),
  -- and scripts/migrate-check.sh compares column ORDER between this file and
  -- the migration chain. Moving it up beside sync_enabled, where it reads
  -- better, is a DRIFT failure whose diff is one line moving.
  writeback_enabled boolean NOT NULL DEFAULT true,
  -- PER-ACCOUNT PUSH (#59, migration 00061): false = new mail in this account
  -- sends no Web Push to any device. KEEP LAST, for writeback_enabled's reason.
  notify_push   boolean NOT NULL DEFAULT true,
  UNIQUE (user_id, name)
);
ALTER TABLE app.accounts ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.accounts FORCE ROW LEVEL SECURITY;
CREATE POLICY accounts_isolation ON app.accounts
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());

-- Per-account sync-health telemetry (spec §12 account-health panel). One row per
-- account records the sync runner's last-pass outcome. Like app.outbound_ledger
-- this is OPERATIONAL state, NOT a JMAP-visible object: a write here appends no
-- change_log row and it is read on demand (Account/getHealth), never synced.
CREATE TABLE app.account_sync_status (
  account_id           app.ulid PRIMARY KEY REFERENCES app.accounts(id) ON DELETE CASCADE,
  user_id              app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  last_attempt_at      timestamptz,
  last_success_at      timestamptz,
  last_error           text,
  last_error_at        timestamptz,
  consecutive_failures int NOT NULL DEFAULT 0,
  updated_at           timestamptz NOT NULL DEFAULT now(),
  -- The download budget's meter (#52, ADR-047, migration 00079): a rolling
  -- 24 hours, as a ring of 25 one-hour buckets of message-body bytes
  -- (hour-since-epoch mod 25; 25 so the window is conservative by up to one
  -- hour, never short) and the start of the hour last written.
  -- Declared last to match the column order migration 00079 produces.
  download_hour        timestamptz,
  download_hours       bigint[],
  -- Receive-only (#52 increment 6, migration 00080): the provider refuses to
  -- take mail from the account as a policy ('smtp_auth_disabled' = Microsoft
  -- 5.7.139) and since when; NULL = it can send. Declared last to match the
  -- column order migration 00080 produces.
  send_blocked_code    text,
  send_blocked_at      timestamptz
);
ALTER TABLE app.account_sync_status ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.account_sync_status FORCE ROW LEVEL SECURITY;
CREATE POLICY account_sync_status_isolation ON app.account_sync_status
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());
CREATE INDEX account_sync_status_user ON app.account_sync_status (user_id, account_id);

CREATE TABLE app.identities (
  id            app.ulid PRIMARY KEY,
  user_id       app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  account_id    app.ulid NOT NULL REFERENCES app.accounts(id) ON DELETE CASCADE,
  kind          app.identity_kind NOT NULL DEFAULT 'fixed',
  -- fixed:           address = 'me@example.com', domain = NULL
  -- domain_wildcard: address = NULL, domain = 'example.com'
  --                  (validates any derived From under that domain — spec §5)
  address       citext,
  domain        citext,
  display_name  text NOT NULL DEFAULT '',
  signature_html text NOT NULL DEFAULT '',
  default_key_id app.ulid,                    -- FK added after app.user_keys
  enabled       boolean NOT NULL DEFAULT true,
  created_at    timestamptz NOT NULL DEFAULT now(),
  -- LAST, because migration 00031 adds it with ALTER TABLE ADD COLUMN and
  -- pg_dump emits columns in physical order — a tidier position here would
  -- read as drift to scripts/migrate-check.sh.
  wkd_hash      text,
  CHECK ( (kind = 'fixed'           AND address IS NOT NULL AND domain IS NULL)
       OR (kind = 'domain_wildcard' AND address IS NULL     AND domain IS NOT NULL) )
);
ALTER TABLE app.identities ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.identities FORCE ROW LEVEL SECURITY;
CREATE POLICY identities_isolation ON app.identities
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());
CREATE UNIQUE INDEX identities_fixed_uq
  ON app.identities (user_id, address) WHERE kind = 'fixed';
-- WKD publication (migration 00031, #48). See that migration for why the hash
-- is a stored column and why this index does not lead with user_id.
CREATE INDEX identities_wkd_hash ON app.identities (wkd_hash) WHERE wkd_hash IS NOT NULL;
-- TO mailroom_wkd, not mailroom_owner (migration 00035, #266): the definer now
-- runs as a role that owns no tables, which is what makes its column grants
-- mean anything. FORCE ROW LEVEL SECURITY applies to it the same way.
CREATE POLICY identities_wkd_lookup ON app.identities
  FOR SELECT TO mailroom_wkd
  USING (wkd_hash IS NOT NULL AND enabled);

COMMENT ON COLUMN app.identities.wkd_hash IS
  'z-base-32(SHA-1(lowercased local part)) of this identity''s address, per '
  'draft-koch-openpgp-webkey-service rev 21. Computed by internal/wkd and set '
  'when the identity''s key is PUBLISHED. NULL means not published, which is '
  'also what makes the row invisible to the anonymous WKD lookup policy.';
CREATE UNIQUE INDEX identities_wildcard_uq
  ON app.identities (user_id, domain) WHERE kind = 'domain_wildcard';

CREATE TABLE app.mailboxes (
  id            app.ulid PRIMARY KEY,
  user_id       app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  account_id    app.ulid NOT NULL REFERENCES app.accounts(id) ON DELETE CASCADE,
  imap_name     text NOT NULL,                -- raw IMAP mailbox name (UTF-7 decoded)
  display_name  text NOT NULL,
  special_use   text,                         -- RFC 6154: \Sent, \Trash, ... or manual mapping
  uidvalidity   bigint NOT NULL DEFAULT 0,
  last_seen_uid bigint NOT NULL DEFAULT 0,
  highest_modseq bigint NOT NULL DEFAULT 0,
  -- PERMANENTFLAGS probe (spec §6): can this mailbox store user keywords?
  -- Recorded ONLY from a read-write SELECT (the writeback session's), because
  -- a server answers the sync pass's EXAMINE with an empty PERMANENTFLAGS
  -- (#740). false until Mailroom has written to the mailbox at least once.
  accepts_keywords boolean NOT NULL DEFAULT false,
  subscribed    boolean NOT NULL DEFAULT true,
  -- 'full' = poll, verify and reap this mailbox; 'off' = do not look at it at all.
  -- An 'off' mailbox is skipped before SELECT (internal/mailsync/engine.go) and is
  -- therefore never a reap candidate either, so its already-synced mail is FROZEN
  -- rather than deleted: "we are not looking" must never become "the mailbox is
  -- empty". A third value 'headers' was advertised here for a long time and never
  -- implemented — it behaved as 'full', i.e. as its own opposite — and is now
  -- refused by the CHECK below; implementing it needs an ADR, because a
  -- headers-only message has no raw bytes and so cannot have a content hash
  -- (ADR-001). See migration 00026.
  sync_scope    text NOT NULL DEFAULT 'full'
                  CONSTRAINT mailboxes_sync_scope_known CHECK (sync_scope IN ('full', 'off')),
  created_at    timestamptz NOT NULL DEFAULT now(),
  -- Cadence cursor for the expunge reaper (mailroom#86, internal/mailsync): when
  -- this mailbox's live UID set was last diffed against its provenance rows.
  -- NULL = never reaped. Internal sync bookkeeping like the cursors above, NOT a
  -- JMAP-visible Mailbox property, so writing it appends no change_log row.
  last_reaped_at timestamptz,
  -- "FOLDERS FEED THE DESK" (migration 00046, #446): mail with live provenance
  -- here counts as unactioned exactly like INBOX, for AreaSummary's live-mail
  -- predicate (areas_resolve.go). A Mailbox property rather than an areas-side
  -- table, because it answers a question about THIS MAILBOX that has nothing
  -- to do with which area a message falls into once it counts.
  --
  -- Declared last to match the column order migration 00046 produces (ADD
  -- COLUMN appends); migrate-check diffs the two schemas and order counts.
  feeds_desk    boolean NOT NULL DEFAULT false,
  -- SKIP-UNCHANGED BASELINES (migration 00058, #620). LIST-STATUS reports each
  -- folder's UIDVALIDITY, UIDNEXT, MESSAGES and HIGHESTMODSEQ in the one LIST a
  -- pass already sends; these are what the engine compares them against.
  --   poll_*      — UIDNEXT/MESSAGES as the POLL's SELECT saw them (beside the
  --                 uidvalidity/highest_modseq cursors above): equal => the
  --                 SELECT would find nothing, so it is skipped.
  --   observed_*  — the four numbers as the last LIVE-UID OBSERVATION saw them,
  --                 written only in the transaction that stamps a real one.
  --                 ⚠ The observation's baseline, NOT the poll's: a poll whose
  --                 FETCH silently lost a message still advances the poll
  --                 cursors, and the observation is what finds that loss (#93).
  --   last_observed_full_at — the last REAL observation; a skipped one re-stamps
  --                 last_reaped_at but not this, so it bounds skipping to 24 h.
  -- NULL = unknown = do the work. Internal bookkeeping, no change_log row.
  --
  -- Declared last to match the column order migration 00058 produces (ADD
  -- COLUMN appends); migrate-check diffs the two schemas and order counts.
  poll_uidnext            bigint,
  poll_messages           bigint,
  observed_uidvalidity    bigint,
  observed_uidnext        bigint,
  observed_messages       bigint,
  observed_highest_modseq bigint,
  last_observed_full_at   timestamptz,
  -- The Gmail read path (#52, ADR-047, migration 00079). backfill_floor_uid:
  -- the newest-first import's floor — every UID from it up to last_seen_uid
  -- has been fetched, history below it has not; NULL = no floor (oldest-first,
  -- every non-Gmail mailbox). Also the lower bound of a #93 gap, so the gap
  -- backfill cannot read un-imported history as gaps. labels_swept_at: when
  -- the X-GM-LABELS sweep last read this mailbox; label_sweep_next_uid: where
  -- an interrupted sweep resumes (NULL = none). Bookkeeping, no change_log.
  -- Declared last to match the column order migration 00079 produces.
  backfill_floor_uid      bigint,
  labels_swept_at         timestamptz,
  label_sweep_next_uid    bigint,
  UNIQUE (account_id, imap_name)
);
ALTER TABLE app.mailboxes ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.mailboxes FORCE ROW LEVEL SECURITY;
CREATE POLICY mailboxes_isolation ON app.mailboxes
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());
CREATE INDEX mailboxes_user_account ON app.mailboxes (user_id, account_id);

-- ----------------------------------------------------------------------------
-- 5. Blobs, messages, provenance, threads, attachments
-- ----------------------------------------------------------------------------
-- Content-addressed encrypted blob metadata (spec §9/§14). The bytes live in
-- the BlobStore at a path derived from HMAC(server_key, user_id)/hash — the
-- path is derivable, so it is not stored.
CREATE TABLE app.blobs (
  user_id       app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  content_hash  app.sha256 NOT NULL,          -- hash of PLAINTEXT bytes
  size_plain    bigint NOT NULL,
  size_stored   bigint NOT NULL,              -- ciphertext incl. envelope
  dek_id        app.ulid NOT NULL,            -- per-user DEK reference
  envelope_ver  smallint NOT NULL DEFAULT 1,  -- AEAD envelope format version
  created_at    timestamptz NOT NULL DEFAULT now(),
  -- WHOSE BYTES THESE ARE (migration 00038, ADR-026, #55). 'mail' is the user's
  -- own — bodies, attachments, model artifacts — and is what every row was
  -- before the image proxy existed, so the default is a statement of fact.
  -- 'proxy_cache' is a third-party image fetched on the user's behalf.
  --
  -- ⚠ IT IS LOAD-BEARING FOR THE STORAGE QUOTA. StorageQuota sums size_stored
  -- over this table; without this column a stranger's marketing images would
  -- consume the quota that bounds the user's own mail, and a user pushed over it
  -- stops being able to receive or send. The exclusion lives in quota.go.
  --
  -- ⚠ AND FOR THE RECLAIM GUARD. A 'proxy_cache' row is referenced by
  -- app.proxy_image_cache, which reclaimUnreferencedBlobs must name explicitly —
  -- there is no foreign key, exactly as with attachments and ml_models.
  --
  -- LAST, after created_at, because migrate-check diffs pg_dump output and
  -- ALTER TABLE ADD COLUMN appends: a tidier position here would read as drift.
  kind          text NOT NULL DEFAULT 'mail',
  CONSTRAINT blobs_kind_known CHECK (kind IN ('mail', 'proxy_cache')),
  PRIMARY KEY (user_id, content_hash)
);
ALTER TABLE app.blobs ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.blobs FORCE ROW LEVEL SECURITY;
CREATE POLICY blobs_isolation ON app.blobs
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());
-- Partial index over just the cache rows: eviction and "how much of this store
-- is not the user's" both scan only this class.
CREATE INDEX blobs_proxy_cache ON app.blobs (user_id, created_at)
  WHERE kind = 'proxy_cache';

-- ONE CACHED PROXIED IMAGE (migration 00038, ADR-026 §4 step 4).
--
-- The cache is the ONLY thing that hides WHEN a message was opened, and only
-- from the second open onwards: without it every re-read re-pings the sender.
--
-- Keyed by the URL's HASH, not by (message, image), because the same image
-- genuinely recurs across messages — a publisher's logo appears in every issue —
-- and not holding it twice is the point of a content-addressed store.
--
-- ⚠ THE URL ITSELF IS NOT STORED. A proxied image URL usually carries a
-- per-recipient token in its path or query (in practice a large share of a
-- mailbox's images carry a `token-hash`), so the URL identifies the reader. The hash is enough
-- to find the entry again and cannot be turned back into the token.
CREATE TABLE app.proxy_image_cache (
  user_id      app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  url_hash     app.sha256 NOT NULL,          -- SHA-256 of the image URL
  content_hash app.sha256 NOT NULL,          -- blob ref (no FK, like attachments)
  content_type text NOT NULL,                -- already narrowed to the endpoint's allowlist
  size_bytes   bigint NOT NULL,
  fetched_at   timestamptz NOT NULL DEFAULT now(),
  -- Advanced on every hit so eviction is least-recently-USED rather than
  -- least-recently-fetched: an image nobody has looked at in months is the right
  -- one to drop even if it was fetched recently.
  last_used_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, url_hash)
);
ALTER TABLE app.proxy_image_cache ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.proxy_image_cache FORCE ROW LEVEL SECURITY;
CREATE POLICY proxy_image_cache_isolation ON app.proxy_image_cache
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());
CREATE INDEX proxy_image_cache_lru ON app.proxy_image_cache (user_id, last_used_at);
CREATE INDEX proxy_image_cache_by_content ON app.proxy_image_cache (user_id, content_hash);

-- Canonical messages. Identity = (user_id, content_hash) per ADR D1.
CREATE TABLE app.messages (
  id            app.ulid PRIMARY KEY,
  user_id       app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  content_hash  app.sha256 NOT NULL,          -- SHA-256 of raw RFC 5322 bytes
  thread_id     app.ulid,                     -- FK added after app.threads
  -- envelope projection (for list views; authoritative data is the blob)
  subject       text NOT NULL DEFAULT '',
  snippet       text NOT NULL DEFAULT '',
  from_name     text NOT NULL DEFAULT '',
  from_address  citext,
  sent_at       timestamptz,                  -- Date header (may be absent/lying)
  received_at   timestamptz NOT NULL,         -- earliest INTERNALDATE seen
  size_bytes    bigint NOT NULL,
  has_attachments boolean NOT NULL DEFAULT false,
  -- threading hints (NEVER identity — spec §4)
  message_id_hint text,                       -- Message-ID header, verbatim
  in_reply_to   text,
  references_ids text[] NOT NULL DEFAULT '{}',
  -- alias story: which identity this message was addressed to (spec §5)
  addressed_identity_id app.ulid REFERENCES app.identities(id) ON DELETE SET NULL,
  addressed_address citext,                   -- the literal derived alias
  -- canonical flags (merged across provenance; JMAP keywords model)
  is_seen       boolean NOT NULL DEFAULT false,
  is_flagged    boolean NOT NULL DEFAULT false,
  is_answered   boolean NOT NULL DEFAULT false,
  is_draft      boolean NOT NULL DEFAULT false,
  is_junk       boolean NOT NULL DEFAULT false, -- mirror of provider verdict
  keywords      text[] NOT NULL DEFAULT '{}', -- non-label IMAP keywords
  -- deletion lifecycle (ADR D9)
  trashed_at    timestamptz,
  purge_after   timestamptz,                  -- set when trashed; job hard-deletes
  created_at    timestamptz NOT NULL DEFAULT now(),
  -- Locally-authored messages that are not on the IMAP server YET (drafts and
  -- freshly-composed sends, spec §5) have no message_provenance row, because a
  -- provenance row is a (mailbox, uidvalidity, uid) coordinate and the server
  -- has not assigned one. local_mailbox_id is where such a message displays
  -- until the APPEND round-trips and real provenance arrives (whereupon the
  -- content-hash dedup, ADR-001, attaches it to THIS row). NULL for anything
  -- that came from the server.
  local_mailbox_id app.ulid REFERENCES app.mailboxes(id) ON DELETE SET NULL,
  -- CANONICAL PLACEMENT: the mailbox the USER moved this message to (archive,
  -- trash, or any folder), applied in the same transaction as the Email/set that
  -- asked for it. It OVERRIDES message_provenance for JMAP mailbox membership.
  --
  -- Provenance is a (mailbox, uidvalidity, uid) coordinate the IMAP server
  -- assigns, and it must stay pointing at where the bytes physically are until
  -- the writeback engine has actually moved them (that is how the engine finds
  -- them). So it cannot express "the user moved this" — and, with no
  -- VANISHED/expunge reconciliation, the source folder's row does not go away
  -- even after the move lands. Without a canonical column, an archived message
  -- therefore reappears in the inbox on the next poll and stays there.
  moved_mailbox_id app.ulid REFERENCES app.mailboxes(id) ON DELETE SET NULL,
  -- RETRACTABLE DELETION: set when the expunge reaper (mailroom#86) concludes
  -- this message is gone from every synced location and soft-deletes it
  -- (trashed_at, ADR-009). The reaper infers "no live provenance anywhere ⇒ gone
  -- everywhere", and that inference races with another client MOVING a message
  -- between two folders (we can poll the destination before the move and reap the
  -- source after it). vanished_at is what makes the inference withdrawable: when
  -- ingest later attaches a new live provenance row to a message that carries it,
  -- the message is demonstrably back and trashed_at/purge_after/vanished_at are
  -- all cleared (store.UserTx.RetractVanished). A message the USER trashed has
  -- vanished_at IS NULL and is never resurrected by that path.
  vanished_at   timestamptz,
  -- Pin (spec §6, #51). A TIMESTAMP, not a boolean: "when did I pin this" is
  -- orderable, which pinned-first list ordering needs, and NULL is a cheaper
  -- partial-index predicate than `= false`. JMAP-visible, so a change here
  -- appends an Email change_log row. Local-only — IMAP has no \Pinned.
  --
  -- Declared LAST to match migration 00012: ALTER TABLE ... ADD COLUMN appends,
  -- and migrate-check diffs the two dumps column-for-column, order included.
  pinned_at     timestamptz,
  -- Snooze (spec §6, #51), migration 00013. The wake TIME is the whole state:
  -- snoozed exactly while this is set and in the future, so waking is one UPDATE
  -- to NULL and "snoozed with no wake time" is unrepresentable. JMAP-visible;
  -- local-only (no IMAP move — see the migration for why). The wake rides the
  -- durable job model as kind 'snooze_wake'.
  snoozed_until timestamptz,
  -- When a snooze last returned this message (migration 00015, mailroom#152).
  -- Lets a list order by "effective arrival" — COALESCE(snooze_woke_at,
  -- received_at) — so mail comes back at the TOP rather than at the position its
  -- age dictates, WITHOUT rewriting received_at, which is the server's fact
  -- about when the mail arrived and is not ours to edit.
  snooze_woke_at timestamptz,
  -- Bulk/priority signals projected off the message BLOB (migration 00016,
  -- mailroom#157). They exist so the SERVE path and the TRAINING path see the
  -- same facts: before this, ml.factsFromRow left them permanently false, so the
  -- model could never learn that bulk mail is unimportant. They are also the
  -- discriminator automatic classification needs (#156) to separate a retailer's
  -- promotion from its receipt. Values, not booleans — rules match them as text.
  list_id          text,
  list_unsubscribe text,
  recipient_count  int,
  -- The conservative VERDICT of ml.AuthResultsFail over possibly many
  -- Authentication-Results hops, not a header: re-deriving it in SQL would mean
  -- re-implementing that parser.
  auth_fail        boolean NOT NULL DEFAULT false,
  -- RE-DELIVERY DEDUP (migration 00023, #206, ADR-018). content_hash above is
  -- IDENTITY and cannot see a re-delivery: each delivery is stamped with its own
  -- trace metadata, so the same message arriving twice hashes twice. dedup_hash is
  -- SHA-256 over an ALLOWLIST — Message-ID, Date, From, To, Cc, Subject and the
  -- body (ingest.DedupHash) — which was chosen because the measurement showed the
  -- bodies byte-identical in all 18 real duplicate groups while nine per-delivery
  -- header fields varied across two providers. Subtracting a denylist of those
  -- would rot on the next provider.
  --
  -- Nullable: backfilled online by `mailroom-admin backfill-dedup-hash` (it must
  -- decrypt each blob, so it is not a SQL UPDATE). NULL means "does not
  -- participate", which is how every message behaved before 00023. NOT unique —
  -- two rows sharing it is the condition this detects, and the collapse of the
  -- existing ones is a separate step (#206 part 3); until then ingest's lookup is
  -- what enforces it, and a lost race duplicates rather than failing a delivery.
  dedup_hash       bytea,
  -- FUNCTION SIGNALS, THE REST OF kind.Facts (migration 00027, mailroom#156).
  -- The other six header-only fields the deterministic kind classifier reads,
  -- alongside list_id/list_unsubscribe/recipient_count above: sender-
  -- independent by construction, so a replay can judge a message from a
  -- sender it has never seen. RAW header values, never a derived verdict —
  -- see the migration for why (a verdict ages with the classifier; a raw
  -- value can be re-scored under next month's weights without touching the
  -- database again). NULL collapses "header absent" and "not yet extracted"
  -- into one state, exactly like list_id/list_unsubscribe above and for the
  -- same reason: nothing downstream reads the difference.
  list_unsubscribe_post text,  -- List-Unsubscribe-Post (RFC 8058 one-click)
  auto_submitted        text,  -- Auto-Submitted (RFC 3834)
  precedence            text,  -- pre-standard bulk/list marker
  feedback_id           text,  -- presence alone means a high-volume ESP
  dkim_domains          text[], -- DKIM-Signature d= domains: the sending
                                 -- infrastructure, the highest-value signal
  return_path           text,  -- envelope sender (VERP/bounce-tracking tell)
  -- THE TAB CLASSIFIER'S VERDICT (migration 00029, #228, ADR-022) — NOT the
  -- message's tab. The tab stays derived:
  --     resolved_tab := COALESCE(mapping_tab, classified_tab)
  -- with mapping_tab read live through the label→tab join, so re-pointing a
  -- label still moves its mail on the next read with no backfill (ADR-021).
  -- Only the classifier's opinion is stored, because Email/query filters in SQL
  -- while the classifier is a Go decision list, and re-expressing those rules in
  -- SQL would be two implementations of one thing (#155).
  --
  -- classifier_version is per row so staleness is DETECTABLE: the set a
  -- re-classify must visit is exactly `classifier_version IS DISTINCT FROM
  -- $current`. NULL classified_tab on a message with no mapped label is
  -- invisible to a tab filter — the honest cost of a nullable column, which is
  -- why `mailroom-admin tabs` reports the NULL count.
  --
  -- Declared last to match the column order migration 00029 produces (ADD COLUMN
  -- appends); migrate-check diffs the two schemas and order counts.
  classified_tab        app.category_tab,
  classifier_version    int,
  -- When the flag last flipped false->true, or NULL when not flagged (#444,
  -- migration 00047): the in-tray's "waiting since". Set on whichever side
  -- flips is_flagged first — the user through JMAP, a rule at ingest, or an
  -- inbound flag seen on sync — and cleared back to NULL on unflag, so the two
  -- columns can never disagree about whether a message is flagged. Plain type
  -- (catalog-only ADD COLUMN, docs/migrations.md), unlike a domain-typed one.
  --
  -- Declared last to match migration 00047 (ADD COLUMN appends); migrate-check
  -- diffs the two schemas and order counts.
  flagged_at             timestamptz,
  -- THE LINE OVERRIDE (#464, migration 00049): which side of the desk's line
  -- the USER put this message on, and when. NULL is no override at all — the
  -- score decides, exactly as it did before this column existed — so nothing
  -- needed backfilling. 'surface' and 'drawer' are the user's own placement,
  -- written by Triage/correct (store.UserTx.SetLineSide) in the same
  -- transaction as the feedback event and the Email change_log row.
  --
  -- A USER'S PLACEMENT OUTRANKS THE MODEL AND IS NEVER CLEARED BY A
  -- RESCORING: no scoring path writes these columns, and Triage/set (the
  -- model reset/rollback valve) deliberately leaves them alone. Exactly ONE
  -- SQL expression consults them — store.lineSideSQL, shared by AreaSummary's
  -- counts, Email/query's `onSurface` and the drawer sweep — so the desk's
  -- counts, its lists and what the sweep archives cannot disagree about which
  -- side a message is on.
  --
  -- Declared last to match migration 00049 (ADD COLUMN appends); migrate-check
  -- diffs the two schemas and order counts.
  line_side              text CHECK (line_side IN ('surface','drawer')),
  line_side_at           timestamptz,
  -- "ARCHIVED IN MAILROOM" (#446, migration 00050): the message is OFF THE
  -- DESK while its bytes stay exactly where the provider has them. Day-one
  -- setup labels mail older than the window and then sets this — NOTHING MOVES
  -- ON THE SERVER, which is the promise the whole of #446 turns on.
  --
  -- Distinct from moved_mailbox_id (a REAL move, with a writeback intent) and
  -- from app.drawer_archives (also a real move, and keyed to an area and a
  -- dwell period). Read by exactly ONE SQL expression, store.deskLiveSQL, so
  -- AreaSummary counts, Email/query onDesk and the drawer sweep candidate scan
  -- cannot disagree about it — the sweep especially, which must never
  -- server-move mail day-one archived precisely because day-one promised
  -- nothing would move.
  --
  -- No index: only ever read as IS NULL inside a per-row predicate already
  -- bounded by user_id and a provenance EXISTS.
  --
  -- Declared last to match migration 00050 (ADD COLUMN appends); migrate-check
  -- diffs the two schemas and column order counts.
  desk_archived_at       timestamptz,
  UNIQUE (user_id, content_hash),
  FOREIGN KEY (user_id, content_hash)
    REFERENCES app.blobs (user_id, content_hash)
);
ALTER TABLE app.messages ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.messages FORCE ROW LEVEL SECURITY;
CREATE POLICY messages_isolation ON app.messages
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());
-- Hot paths (every index leads with user_id — RLS adds that predicate anyway):
CREATE INDEX messages_user_received  ON app.messages (user_id, received_at DESC);
CREATE INDEX messages_user_thread    ON app.messages (user_id, thread_id);
CREATE INDEX messages_user_msgid     ON app.messages (user_id, message_id_hint);
CREATE INDEX messages_user_trash     ON app.messages (user_id, purge_after)
  WHERE trashed_at IS NOT NULL;
CREATE INDEX messages_user_local_mbox ON app.messages (user_id, local_mailbox_id)
  WHERE local_mailbox_id IS NOT NULL;
CREATE INDEX messages_user_moved_mbox ON app.messages (user_id, moved_mailbox_id)
  WHERE moved_mailbox_id IS NOT NULL;
CREATE INDEX messages_user_pinned ON app.messages (user_id, pinned_at DESC)
  WHERE pinned_at IS NOT NULL;
CREATE INDEX messages_user_snoozed ON app.messages (user_id, snoozed_until)
  WHERE snoozed_until IS NOT NULL;
CREATE INDEX messages_user_woke ON app.messages (user_id, snooze_woke_at DESC)
  WHERE snooze_woke_at IS NOT NULL;
CREATE INDEX messages_user_bulk ON app.messages (user_id, from_address)
  WHERE list_unsubscribe IS NOT NULL OR list_id IS NOT NULL;
-- Re-delivery dedup (#206). Partial because only populated rows are looked up, and
-- while the backfill is still running that is most of the table.
CREATE INDEX messages_user_dedup_hash ON app.messages (user_id, dedup_hash)
  WHERE dedup_hash IS NOT NULL;
-- The tray reads "flagged, oldest first" per user (#444, migration 00047).
CREATE INDEX messages_flagged_since ON app.messages (user_id, flagged_at)
  WHERE is_flagged;
-- The desk asks "which side is this on" for the caller's mail on every render;
-- only overridden rows are interesting, so partial (#464, migration 00049).
CREATE INDEX messages_line_side ON app.messages (user_id, line_side)
  WHERE line_side IS NOT NULL;

-- WHICH MESSAGES KEEP A CACHED PROXIED IMAGE ALIVE (migration 00081, #922,
-- ADR-026 amendment of 2026-10-27). One row = "this message shows the image
-- cached under this url_hash" (app.proxy_image_cache above). A cached image
-- with any row here is exempt from both the cache's unread TTL and its size
-- cap; it lives until the last referencing message is DELETED from
-- app.messages, and the cascade turns that into "released" with no sweep to
-- remember. Trash does not release: Trash is restorable. No FK to
-- app.proxy_image_cache, deliberately (see the migration). Operational, not
-- JMAP-visible: a write appends no change_log row.
CREATE TABLE app.proxy_image_refs (
  user_id    app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  url_hash   app.sha256 NOT NULL,          -- app.proxy_image_cache.url_hash
  message_id app.ulid NOT NULL REFERENCES app.messages(id) ON DELETE CASCADE,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, url_hash, message_id)
);
ALTER TABLE app.proxy_image_refs ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.proxy_image_refs FORCE ROW LEVEL SECURITY;
CREATE POLICY proxy_image_refs_isolation ON app.proxy_image_refs
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());
CREATE INDEX proxy_image_refs_by_message ON app.proxy_image_refs (user_id, message_id);

CREATE TABLE app.threads (
  id            app.ulid PRIMARY KEY,
  user_id       app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  -- membership lives on messages.thread_id and is RECOMPUTABLE (spec §4);
  -- denormalized rollups for list views:
  subject       text NOT NULL DEFAULT '',
  message_count int NOT NULL DEFAULT 0,
  last_received_at timestamptz,
  created_at    timestamptz NOT NULL DEFAULT now(),
  -- Mute (spec §6, #51). Thread-scoped, because "stop bothering me about this
  -- conversation" is a property of the conversation, not of one message in it.
  -- JMAP-visible, so a change appends a THREAD change_log row — app.object_type
  -- already carries 'Thread', and Thread/get + Thread/changes already give it a
  -- sync channel. Local-only: IMAP has no thread-level mute.
  --
  -- Declared LAST to match migration 00012 (see pinned_at above).
  muted_at      timestamptz
);
ALTER TABLE app.threads ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.threads FORCE ROW LEVEL SECURITY;
CREATE POLICY threads_isolation ON app.threads
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());
CREATE INDEX threads_user_recent ON app.threads (user_id, last_received_at DESC);
CREATE INDEX threads_user_muted ON app.threads (user_id)
  WHERE muted_at IS NOT NULL;

ALTER TABLE app.messages
  ADD CONSTRAINT messages_thread_fk
  FOREIGN KEY (thread_id) REFERENCES app.threads(id) ON DELETE SET NULL;

-- Where each canonical message physically lives (spec §4).
CREATE TABLE app.message_provenance (
  id            app.ulid PRIMARY KEY,
  user_id       app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  message_id    app.ulid NOT NULL REFERENCES app.messages(id) ON DELETE CASCADE,
  account_id    app.ulid NOT NULL REFERENCES app.accounts(id) ON DELETE CASCADE,
  mailbox_id    app.ulid NOT NULL REFERENCES app.mailboxes(id) ON DELETE CASCADE,
  uid           bigint NOT NULL,
  uidvalidity   bigint NOT NULL,              -- generation this row belongs to
  state         app.provenance_state NOT NULL DEFAULT 'live',
  gm_msgid      text,                         -- Gmail X-GM-MSGID, metadata only
  remote_flags  text[] NOT NULL DEFAULT '{}', -- last flags seen on the server
  first_seen_at timestamptz NOT NULL DEFAULT now(),
  -- Gmail (#52, ADR-047, migration 00079): the X-GM-LABELS last reported for
  -- this copy, decoded; label changes are diffed against it. NULL = never
  -- observed. gm_thrid is X-GM-THRID, in gm_msgid's text form. Declared last
  -- to match the column order migration 00079 produces.
  gm_labels     text[],
  gm_thrid      text
);
ALTER TABLE app.message_provenance ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.message_provenance FORCE ROW LEVEL SECURITY;
CREATE POLICY provenance_isolation ON app.message_provenance
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());
-- A UID is unique within a mailbox generation; stale generations keep history.
CREATE UNIQUE INDEX provenance_live_uq
  ON app.message_provenance (mailbox_id, uidvalidity, uid);
CREATE INDEX provenance_user_message ON app.message_provenance (user_id, message_id);
-- X-GM-THRID threading hint (#52, migration 00079): Gmail copies only.
CREATE INDEX provenance_gm_thrid ON app.message_provenance (user_id, account_id, gm_thrid)
  WHERE gm_thrid IS NOT NULL;

CREATE TABLE app.attachments (
  id            app.ulid PRIMARY KEY,
  user_id       app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  message_id    app.ulid NOT NULL REFERENCES app.messages(id) ON DELETE CASCADE,
  part_path     text NOT NULL,                -- MIME part path, e.g. '2.1'
  filename      text,
  content_type  text NOT NULL DEFAULT 'application/octet-stream',
  size_bytes    bigint NOT NULL,
  content_hash  app.sha256,                   -- present once extracted to blob
  extracted_text_hash app.sha256,             -- opt-in search extraction (§7)
  -- ADR-027 §5's states, MINUS ONE. 'unknown' means a scan was attempted or
  -- deliberately skipped, and carries the SAME enforcement as 'flagged' (§5b) —
  -- otherwise an attacker never has to beat the scanner, only avoid it.
  --
  -- ⚠ 'released' IS NOT A VALUE THIS COLUMN TAKES, though §5 modelled it as one.
  -- It moved to app.attachment_releases in migration 00044; see that table for
  -- the three reasons. Nothing ever wrote it here, so nothing had to be moved.
  --
  -- Deliberately not a CHECK constraint: the vocabulary belongs to the
  -- application, and a constraint here would turn adding a state into a
  -- migration on a table the reclaim guard reads.
  scan_status   text NOT NULL DEFAULT 'unscanned', -- 'unscanned'|'clean'|'flagged'|'unknown'
  created_at    timestamptz NOT NULL DEFAULT now(),
  -- Verdict freshness (migration 00043, #395, ADR-027 §5c): a verdict is a claim
  -- with a timestamp, not a fact about a file.
  --
  -- ⚠ DECLARED AFTER created_at BECAUSE ALTER TABLE ADD COLUMN APPENDS. This
  -- file and db/migrations/*.sql must produce byte-identical schemas, and CI's
  -- migrate-check diffs them — including column ORDER. Writing these in the
  -- position that reads best put them before created_at here and after it
  -- there, which is a drift the job caught and no local run could: schema-apply
  -- and the integration suite both apply THIS file, so only the job that walks
  -- both paths can see the difference.
  --
  -- ⚠ ALL THREE NULLABLE, AND THE NULL IS A TRAP FOR THE RE-SCAN PREDICATE.
  -- §13 specifies `WHERE scanned_db_version < current`, which matches NOTHING
  -- for a never-scanned row (NULL < n is NULL, not true) — it would skip the
  -- entire backlog while logging success. Write
  -- `(scanned_db_version IS NULL OR scanned_db_version < $1)`.
  --
  -- ⚠ The DB's PUBLICATION timestamp is deliberately absent: it is a property of
  -- the daemon's current database, readable live from clamd's VERSION, and
  -- belongs in the health envelope rather than in one copy per row.
  scanned_at             timestamptz,
  scanned_db_version     integer,
  scanned_engine_version text,
  -- CONTENT IDENTITY for the part's decoded bytes (migration 00044, #396,
  -- ADR-027 §8): the key a quarantine and a release act on, because both are
  -- claims about BYTES and the same attachment sits on N different messages
  -- (measured: most rows, upper bound, in groups).
  --
  -- ⚠ NOT content_hash ABOVE, AND THE DISTINCTION IS LOAD-BEARING. That column
  -- is a BLOB REFERENCE — blobreclaim.go reads it to decide whether a blob is
  -- still in use — and attachproj leaves it NULL on purpose, because a part is a
  -- slice of the message blob and there is no per-attachment blob to name.
  -- Putting a part digest there would arm the reclaim guard against blobs that
  -- do not exist. This column is read by nothing but the quarantine.
  --
  -- ⚠ DECLARED AFTER THE OTHERS BECAUSE ALTER TABLE ADD COLUMN APPENDS (see above).
  part_hash              app.sha256,
  -- WHEN A PASS LAST VISITED THIS ROW, whatever came of it (migration 00045,
  -- #409/#407/#415). Distinct from scanned_at above, and the distinction is the
  -- whole point of the pair.
  --
  -- ⚠ scanned_at MEANS "WHEN THIS VERDICT WAS MADE" AND MUST KEEP MEANING THAT.
  -- ADR-027 §1a makes the age of a verdict what justifies the `> 6 months` tier
  -- never being re-scanned, and attachment-report prints it. A pass that read a
  -- blob and got nothing, or sent bytes and got an error, has not made a
  -- verdict — refreshing scanned_at for it would claim freshness in the one
  -- surface that exists to stop that claim being made. So it records the
  -- ATTEMPT here instead.
  --
  -- ⚠ THIS IS WHAT MAKES THE RE-SCAN PREDICATE TERMINATE. Without it, a part
  -- that decoded to zero bytes can never acquire a part_hash (attachscan.PartHash
  -- returns nil for it by design) and so never leaves the digest arm — and since
  -- MessagesDueForRescan is `ORDER BY m.id LIMIT 50`, fifty such rows at the head
  -- of the queue starve everything behind them forever while the job logs
  -- `messages=50` every thirty seconds. See store/rescan.go's rescanDueSQL.
  attempted_at           timestamptz,
  attempted_db_version   integer,
  UNIQUE (message_id, part_path)
);
ALTER TABLE app.attachments ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.attachments FORCE ROW LEVEL SECURITY;
CREATE POLICY attachments_isolation ON app.attachments
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());
CREATE INDEX attachments_user_message ON app.attachments (user_id, message_id);
-- The re-scan job's predicate (migration 00043). Anticipatory at 257 rows; the
-- query grows with the mailbox rather than with usage.
CREATE INDEX attachments_user_scanned_version ON app.attachments (user_id, scanned_db_version);
-- The quarantine's content key (migration 00044). Partial because only
-- populated rows are ever looked up, the same shape as messages_user_dedup_hash.
CREATE INDEX attachments_user_part_hash ON app.attachments (user_id, part_hash)
  WHERE part_hash IS NOT NULL;
-- The re-scan predicate's leading factor (migration 00045): "we have not already
-- tried this row at this signature version". Not partial — the IS NULL arm is
-- the one that matches every row no pass has ever visited.
CREATE INDEX attachments_user_attempted_version ON app.attachments (user_id, attempted_db_version);

-- A human's override of a quarantine, keyed on CONTENT (migration 00044, #396,
-- ADR-027 §8). ⚠ THIS IS WHERE §5'S FIFTH STATE MOVED: `released` was modelled
-- as a value of scan_status above, and three things were wrong with that. A
-- release is about BYTES, so as a column value it is an N-row write that a
-- later-arriving copy never receives; writing it over `flagged` DESTROYED the
-- verdict it overrode, which §5's own stability table says would never come
-- back; and the `IS DISTINCT FROM 'released'` guard protecting it was
-- remembered at each call site rather than structural. Here the verdict stays
-- and keeps being refreshed, every copy is covered by construction, and nothing
-- a scanner writes can reach this table. `released` is consequently NOT a value
-- scan_status ever takes; no data migration was needed because no code ever
-- wrote it.
CREATE TABLE app.attachment_releases (
  user_id     app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  -- The released CONTENT. Not a foreign key to anything: it names bytes, and the
  -- rows carrying those bytes come and go independently of the decision.
  part_hash   app.sha256 NOT NULL,
  released_at timestamptz NOT NULL DEFAULT now(),
  -- Where the user was standing when they decided. Audit only, hence SET NULL:
  -- deleting the message you released from does not un-release the bytes.
  released_from_message_id app.ulid REFERENCES app.messages(id) ON DELETE SET NULL,
  released_from_part       text,
  PRIMARY KEY (user_id, part_hash)
);
ALTER TABLE app.attachment_releases ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.attachment_releases FORCE ROW LEVEL SECURITY;
CREATE POLICY attachment_releases_isolation ON app.attachment_releases
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());

-- ----------------------------------------------------------------------------
-- 6. Labels & keyword mapping
-- ----------------------------------------------------------------------------
CREATE TABLE app.labels (
  id            app.ulid PRIMARY KEY,
  user_id       app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  name          text NOT NULL,
  color         text,                          -- '#rrggbb' or named token
  parent_id     app.ulid REFERENCES app.labels(id) ON DELETE SET NULL,
  mirror_to_imap boolean NOT NULL DEFAULT true, -- spec §6 default
  created_at    timestamptz NOT NULL DEFAULT now(),
  -- WHICH CATEGORY TAB THIS LABEL'S MAIL BELONGS TO (migration 00028, #219), or
  -- NULL for "not mapped" — which falls through to the tab classifier exactly as
  -- an unlabelled message does. A property of the label, like its colour: a join
  -- table would need its own cascade handling and could outlive the label it
  -- maps. JMAP-visible for free, since UpdateLabel already appends the Label
  -- change_log row for any field change.
  --
  -- Declared last to match the column order migration 00028 produces (ADD COLUMN
  -- appends); migrate-check diffs the two schemas and order counts.
  tab           app.category_tab,
  UNIQUE (user_id, name)
);
ALTER TABLE app.labels ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.labels FORCE ROW LEVEL SECURITY;
CREATE POLICY labels_isolation ON app.labels
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());

CREATE TABLE app.message_labels (
  user_id       app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  message_id    app.ulid NOT NULL REFERENCES app.messages(id) ON DELETE CASCADE,
  label_id      app.ulid NOT NULL REFERENCES app.labels(id) ON DELETE CASCADE,
  applied_by    text NOT NULL DEFAULT 'user',  -- 'user'|'rule'|'ml'|'agent'|'desk'|'setup'|'learned'
  -- 'learned' (#714, migration 00060, ADR-032): learned placement filed a new
  -- message into this area's own label at ingest, from the user's own filings
  -- on its sender. Never counted as evidence (only 'user' rows are). Comment
  -- only, like 'desk': there is no CHECK on this column.
  -- 'desk' (#442 increment 3): the desk applied its own area's label because
  -- the message's tab verdict placed it there ("areas are labels"). Comment
  -- only — pg_dump ignores comments, so this needed no migration; see
  -- internal/store/label_source.go's LabelByDesk for the full reasoning.
  created_at    timestamptz NOT NULL DEFAULT now(),
  -- WHICH rule applied it, when applied_by = 'rule' (#155). FK added after
  -- app.rules. ON DELETE SET NULL, never CASCADE: deleting a rule must not
  -- unlabel the mail it labelled — the attribution is disposable, the
  -- user-visible label is not.
  --
  -- Declared last to match the column order migration 00017 produces (ADD COLUMN
  -- appends); migrate-check diffs the two schemas and order counts.
  applied_by_rule_id app.ulid,
  PRIMARY KEY (user_id, message_id, label_id)
);
ALTER TABLE app.message_labels ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.message_labels FORCE ROW LEVEL SECURITY;
CREATE POLICY message_labels_isolation ON app.message_labels
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());
CREATE INDEX message_labels_by_label ON app.message_labels (user_id, label_id);
CREATE INDEX message_labels_by_rule ON app.message_labels (user_id, applied_by_rule_id)
  WHERE applied_by_rule_id IS NOT NULL;
-- Learned placement's evidence (#714, migration 00060): the user's own filings
-- only. Partial, because the desk's own rows are ~96% of this table and the
-- evidence query must not walk them at every ingest.
CREATE INDEX message_labels_user_filings ON app.message_labels (user_id, label_id)
  WHERE applied_by = 'user';

-- Persisted label↔IMAP-keyword mapping (spec §6): NEVER re-derived from the
-- label name — renames must not orphan keywords.
CREATE TABLE app.label_keyword_map (
  user_id       app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  label_id      app.ulid NOT NULL REFERENCES app.labels(id) ON DELETE CASCADE,
  keyword_atom  text NOT NULL,                 -- e.g. 'MailroomLabel-invoices-2'
  created_at    timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, label_id),
  UNIQUE (user_id, keyword_atom),
  CHECK (keyword_atom ~ '^[!#-''*+\--9;-Z^-~]+$')  -- IMAP atom charset, no specials
);
ALTER TABLE app.label_keyword_map ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.label_keyword_map FORCE ROW LEVEL SECURITY;
CREATE POLICY label_keyword_map_isolation ON app.label_keyword_map
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());

-- CATEGORY TABS (migration 00028, #219): which of the five Gmail-style tabs this
-- user has switched on, and how they are arranged. The other half of the feature
-- — where each label goes — is app.labels.tab above.
--
-- Rows are materialised LAZILY on first read (store.ListCategoryTabs), not by a
-- seed here: there is no Go-side user-provisioning step to hang one off (login
-- runs through app.upsert_login, a SECURITY DEFINER), and a migration-time
-- INSERT would create JMAP-visible rows with no change_log row.
--
-- A message's tab is DERIVED at read time — labels through the mapping, the rest
-- from the function signals (00027) — and is deliberately not a column on
-- app.messages. A stored tab would need re-deriving over the whole mailbox every
-- time the operator moved one label.
CREATE TABLE app.category_tabs (
  -- A ULID PK even though (user_id, tab) identifies the row: change_log.object_id
  -- is app.ulid NOT NULL, so the natural key alone could not append a change row.
  id         app.ulid PRIMARY KEY,
  user_id    app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  tab        app.category_tab NOT NULL,
  enabled    boolean NOT NULL DEFAULT true,
  position   int NOT NULL DEFAULT 0,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  -- Primary is not disableable: a disabled tab's mail folds into Primary, so a
  -- disabled Primary would be switched off and still holding every folded
  -- message. Unrepresentable rather than handled.
  CONSTRAINT category_tabs_primary_always_enabled CHECK (tab <> 'primary' OR enabled),
  UNIQUE (user_id, tab)
);
ALTER TABLE app.category_tabs ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.category_tabs FORCE ROW LEVEL SECURITY;
CREATE POLICY category_tabs_isolation ON app.category_tabs
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());
-- No further index: UNIQUE (user_id, tab) already leads with user_id, over at
-- most five rows per user.

-- ----------------------------------------------------------------------------
-- 7. Writeback intents (spec §4a) and durable jobs (spec §16)
-- ----------------------------------------------------------------------------
-- Intent DELTAS, never target states. Replay-safe via idempotency_key.
CREATE TABLE app.writeback_intents (
  id            app.ulid PRIMARY KEY,
  user_id       app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  message_id    app.ulid REFERENCES app.messages(id) ON DELETE CASCADE,
  account_id    app.ulid NOT NULL REFERENCES app.accounts(id) ON DELETE CASCADE,
  op            text NOT NULL,                 -- '+flag','-flag','+kw','-kw','move','append','expunge'
  payload       jsonb NOT NULL DEFAULT '{}',   -- op arguments (target mailbox, flag name, ...)
  idempotency_key text NOT NULL,
  state         app.intent_state NOT NULL DEFAULT 'pending',
  attempts      int NOT NULL DEFAULT 0,
  last_error    text,
  dead_reason   text,                          -- populated when state='dead'; SURFACED to user
  created_at    timestamptz NOT NULL DEFAULT now(),
  applied_at    timestamptz,
  -- When the row was moved to 'inflight'. Read by the reclaim arm of
  -- ClaimPendingIntents, which is what stops a crash between claim and
  -- completion stranding the intent forever (#520; app.jobs.locked_at is the
  -- same idea, migration 00041).
  claimed_at    timestamptz,
  -- What ASKED for this intent (#554 item 3, #555, migration 00057). Not who
  -- it will be applied to, and not why it later failed — `dead_reason` is the
  -- post-hoc failure explanation, this is the antecedent.
  --
  -- The vocabulary deliberately reuses app.message_labels.applied_by's strings
  -- where they mean the same thing, so 'rule' means the same actor in both
  -- tables, plus two subsystem values for the paths no person initiates:
  --   'user','rule','ml','agent','desk','setup'  -- see internal/store/label_source.go
  --   'purge'  -- the Trash purge sweep, phase 1 (internal/purge)
  --   'draft'  -- draft autosave / retirement (internal/store/drafts.go)
  --
  -- ⚠ NULLABLE, AND NULL IS NOT 'user'. NULL means "enqueued before 00057, and
  -- the actor is unrecoverable" (app.user_keys.origin uses NULL the same way).
  -- The idempotency key proves which PATH a historical row came from, never
  -- which ACTOR: a rule-applied label and a hand-applied one are byte-identical
  -- down enqueueEmailIntent. Backfilling those as 'user' is exactly the
  -- falsehood label_source.go was written to end -- "every row said 'user' by
  -- default, including the ones a rule created". Measured before deciding: on a
  -- live instance most rows were '+kw', all of which would have been mislabelled.
  -- It costs nothing to leave them NULL, because every pre-00057 row is already
  -- in a terminal state and source is only read for 'pending'/'suppressed'.
  --
  -- ⚠ KEEP THIS LAST. `ALTER TABLE ... ADD COLUMN` appends, and
  -- scripts/migrate-check.sh compares column ORDER between this file and the
  -- migration chain; moving it next to created_at/applied_at where it reads
  -- better is a DRIFT failure whose diff is one line moving. (claimed_at held
  -- this spot until 00057; the warning moved down with the new last column.)
  source        text,
  UNIQUE (user_id, idempotency_key),
  CONSTRAINT writeback_intents_source_known CHECK (
    source IS NULL OR source IN
      ('user', 'rule', 'ml', 'agent', 'desk', 'setup', 'purge', 'draft'))
);
ALTER TABLE app.writeback_intents ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.writeback_intents FORCE ROW LEVEL SECURITY;
CREATE POLICY writeback_intents_isolation ON app.writeback_intents
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());
CREATE INDEX writeback_pending ON app.writeback_intents (account_id, created_at)
  WHERE state IN ('pending', 'inflight');
CREATE INDEX writeback_dead ON app.writeback_intents (user_id, created_at)
  WHERE state = 'dead';

-- One durable job table for every timer in the product (spec §16):
-- snooze wake-ups, delayed submission (Undo Send / send-later), trash purge,
-- DEK lazy re-encryption, model snapshots, change-log compaction.
CREATE TABLE app.jobs (
  id            app.ulid PRIMARY KEY,
  user_id       app.ulid REFERENCES app.users(id) ON DELETE CASCADE, -- NULL = system job
  kind          text NOT NULL,                 -- 'snooze_wake','submit','purge_trash',...
  fire_at       timestamptz NOT NULL,
  payload       jsonb NOT NULL DEFAULT '{}',
  -- Unique only while the job is OUTSTANDING — see jobs_live_idempotency_key.
  idempotency_key text NOT NULL,
  state         app.job_state NOT NULL DEFAULT 'pending',
  attempts      int NOT NULL DEFAULT 0,
  max_attempts  int NOT NULL DEFAULT 5,
  locked_by     text,                          -- dispatcher instance id
  locked_at     timestamptz,
  last_error    text,
  created_at    timestamptz NOT NULL DEFAULT now(),
  completed_at  timestamptz
);
-- Jobs are dispatched by a system worker (runs as mailroom_app with a system
-- context), but user rows remain user-visible for e.g. "scheduled sends" UI.
ALTER TABLE app.jobs ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.jobs FORCE ROW LEVEL SECURITY;
CREATE POLICY jobs_user_visible ON app.jobs
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id() OR
         (user_id IS NULL AND app.current_user_id() IS NULL))
  WITH CHECK (user_id = app.current_user_id() OR
              (user_id IS NULL AND app.current_user_id() IS NULL));
-- Dispatcher scan path. The dispatcher fires timers across all users; it runs
-- in a SystemTx ('mailroom.user_id' UNSET) and reaches other users' jobs ONLY
-- through the SECURITY DEFINER app.dequeue_due_jobs below. Jobs are the ONE
-- deliberate, narrow cross-user surface — the function exposes job rows, never
-- mail content.
--
-- That definer runs as the function owner, mailroom_owner (pinned below). In
-- production mailroom_owner owns app.jobs and FORCE ROW LEVEL SECURITY applies
-- to the owner too, so without a policy for it the definer would silently see
-- zero rows and no timer would ever fire (mailroom#16). This policy IS that
-- narrow surface, made explicit: full visibility of job rows for mailroom_owner.
--
-- ⚠ NOT "a NOLOGIN role reachable only through the definer function", which is
-- what this said until #309 checked: mailroom_owner has LOGIN in every
-- deployment (migrations connect as it), so this ALL policy is reachable
-- directly with that credential. The containment is what a job row IS — a kind,
-- a schedule, a state and an ID-shaped payload — which is the same property the
-- EnqueueJob contract states and the reason app.jobs is the one deliberate
-- cross-user surface. See the role notes at the top of this file.
CREATE POLICY jobs_dispatcher_owner ON app.jobs
  FOR ALL TO mailroom_owner
  USING (true) WITH CHECK (true);
CREATE INDEX jobs_due ON app.jobs (fire_at) WHERE state = 'pending';
-- Idempotency keys are unique among OUTSTANDING jobs, not for all time
-- (mailroom#357). Jobs are never deleted, so an unqualified UNIQUE consumed the
-- key for the lifetime of the database — including for a job that DEAD-LETTERED
-- and would therefore never run. That silently defeated snooze's boot repair
-- path, which re-derives the same minute-truncated key from the same unwoken
-- message and so collided with the dead row forever. Every ON CONFLICT on this
-- key asks "is one already outstanding?", and these two states are the answer;
-- they are the same pair EnsureRecurringUserJob treats as outstanding.
-- ⚠ A caller inferring this arbiter must repeat the predicate in its ON CONFLICT
-- target (see internal/store/snooze.go); a bare column list raises 42P10.
-- See db/migrations/00042 for the full sequence.
CREATE UNIQUE INDEX jobs_live_idempotency_key ON app.jobs (idempotency_key)
  WHERE state IN ('pending', 'running');

CREATE OR REPLACE FUNCTION app.dequeue_due_jobs(p_worker text, p_limit int)
RETURNS SETOF app.jobs
LANGUAGE sql SECURITY DEFINER SET search_path = app, pg_temp AS $$
  UPDATE app.jobs j SET state = 'running', locked_by = p_worker, locked_at = now(),
                        attempts = attempts + 1
  WHERE j.id IN (
    SELECT id FROM app.jobs
    WHERE (state = 'pending' AND fire_at <= now())
       -- Stranded by a process that died holding the lock (#351 B3):
       -- locked_at was written on every dequeue and read by nothing, so a
       -- job left running by any restart stayed running forever, and
       -- EnsureRecurringUserJob counted the zombie as "already scheduled".
       -- See db/migrations/00041 for the lease trade-off.
       OR (state = 'running' AND locked_at IS NOT NULL
           AND locked_at < now() - interval '15 minutes')
    ORDER BY fire_at
    FOR UPDATE SKIP LOCKED
    LIMIT p_limit
  )
  RETURNING j.*;
$$;
REVOKE ALL ON FUNCTION app.dequeue_due_jobs(text, int) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.dequeue_due_jobs(text, int) TO mailroom_app;
-- Pin the definer deterministically. Applied by a superuser (as in CI/dev), the
-- function would otherwise be owned by that superuser and bypass RLS entirely,
-- hiding the production semantics where the definer is the non-superuser
-- mailroom_owner and IS subject to RLS. Forcing the owner makes CI exercise the
-- real path (mailroom#16).
ALTER FUNCTION app.dequeue_due_jobs(text, int) OWNER TO mailroom_owner;
-- mailroom_owner holds these implicitly in production (it owns every object);
-- the explicit grant is what makes the definer path work in CI/dev, where the
-- tables are owned by the bootstrap superuser instead.
GRANT SELECT, UPDATE ON app.jobs TO mailroom_owner;

-- Job-queue BACKLOG, for the mailroom_job_queue_depth gauge (#524). A tenth
-- SECURITY DEFINER, and a strictly smaller surface than dequeue_due_jobs beside
-- it: that one hands the same caller whole job rows across every user, this one
-- returns a single bigint that cannot name a user, a job or a payload.
--
-- It needs the definer for the same reason dequeue does. The dispatcher runs in
-- a SystemTx where app.current_user_id() is NULL, so the app.jobs policy matches
-- SYSTEM rows only and a plain count silently UNDERCOUNTS by every per-user job
-- (purge_trash, ml_train, snooze_wake, submit) — a plausible-looking wrong
-- number rather than an error.
--
-- DUE, not merely pending: app.jobs holds rows scheduled far in the future (a
-- snooze for next week), and counting those would make the gauge climb with
-- normal use. Jobs stranded in 'running' are excluded too — migration 00041's
-- lease returns them to the claimable set on their own, so counting them here
-- would double-count them as backlog while they are being reclaimed.
-- See db/migrations/00054 for the full argument.
CREATE OR REPLACE FUNCTION app.count_due_jobs()
RETURNS bigint
LANGUAGE sql SECURITY DEFINER STABLE SET search_path = app, pg_temp AS $$
  SELECT count(*) FROM app.jobs
   WHERE state = 'pending' AND fire_at <= now();
$$;
REVOKE ALL ON FUNCTION app.count_due_jobs() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.count_due_jobs() TO mailroom_app;
ALTER FUNCTION app.count_due_jobs() OWNER TO mailroom_owner;

-- Job-state transitions for the dispatcher (mailroom#13). Same narrow
-- cross-user surface as dequeue: SECURITY DEFINER owned by mailroom_owner,
-- guarded so only the worker that claimed a job (locked_by) can transition it
-- out of 'running' — this prevents a second dispatcher from completing or
-- failing a job it does not own.
--
-- complete_job: 'running' -> 'done'. Returns true when it transitioned a row,
-- NULL when the guard did not match (lost lock / already terminal).
CREATE OR REPLACE FUNCTION app.complete_job(p_id app.ulid, p_worker text)
RETURNS boolean
LANGUAGE sql SECURITY DEFINER SET search_path = app, pg_temp AS $$
  UPDATE app.jobs SET state = 'done', completed_at = now(),
                      locked_by = NULL, locked_at = NULL
  WHERE id = p_id AND locked_by = p_worker AND state = 'running'
  RETURNING true;
$$;
REVOKE ALL ON FUNCTION app.complete_job(app.ulid, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.complete_job(app.ulid, text) TO mailroom_app;
ALTER FUNCTION app.complete_job(app.ulid, text) OWNER TO mailroom_owner;

-- fail_job: a claimed job errored. If it still has attempts left it goes back
-- to 'pending' with fire_at = p_retry_at (caller-computed backoff) and its lock
-- released; once attempts have reached max_attempts it goes to 'dead' (the
-- surfaced dead-letter state). attempts was already incremented at dequeue, so
-- the comparison is on the current value. Returns the resulting state, or NULL
-- when the guard did not match.
CREATE OR REPLACE FUNCTION app.fail_job(p_id app.ulid, p_worker text,
                                        p_error text, p_retry_at timestamptz)
RETURNS app.job_state
LANGUAGE sql SECURITY DEFINER SET search_path = app, pg_temp AS $$
  UPDATE app.jobs SET
    state     = CASE WHEN attempts >= max_attempts THEN 'dead'::app.job_state
                     ELSE 'pending'::app.job_state END,
    fire_at   = CASE WHEN attempts >= max_attempts THEN fire_at
                     ELSE p_retry_at END,
    last_error = p_error,
    locked_by = NULL, locked_at = NULL
  WHERE id = p_id AND locked_by = p_worker AND state = 'running'
  RETURNING state;
$$;
REVOKE ALL ON FUNCTION app.fail_job(app.ulid, text, text, timestamptz) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.fail_job(app.ulid, text, text, timestamptz) TO mailroom_app;
ALTER FUNCTION app.fail_job(app.ulid, text, text, timestamptz) OWNER TO mailroom_owner;

-- ----------------------------------------------------------------------------
-- 8. Sending: submissions, outbound ledger, DSNs
-- ----------------------------------------------------------------------------
-- EmailSubmission (JMAP) + the delayed-submission engine (spec §5).
CREATE TABLE app.submissions (
  id            app.ulid PRIMARY KEY,
  user_id       app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  account_id    app.ulid NOT NULL REFERENCES app.accounts(id) ON DELETE CASCADE,
  identity_id   app.ulid NOT NULL REFERENCES app.identities(id),
  -- ON DELETE CASCADE (migration 00014, mailroom#148): without it a message with
  -- a send record can never be hard-deleted — the Trash purge and account
  -- deletion both fail on it. What is worth keeping outlives the message
  -- independently (dsn_events SET NULLs; outbound_ledger is per-account).
  message_id    app.ulid NOT NULL REFERENCES app.messages(id) ON DELETE CASCADE, -- the draft/built message
  from_address  citext NOT NULL,               -- validated against identity (spec §5)
  envelope_to   citext[] NOT NULL,
  send_at       timestamptz NOT NULL DEFAULT now(), -- future = undo window / send-later
  job_id        app.ulid REFERENCES app.jobs(id),
  state         text NOT NULL DEFAULT 'scheduled', -- 'scheduled'|'canceled'|'sending'|'sent'|'failed'
  smtp_response text,
  created_at    timestamptz NOT NULL DEFAULT now(),
  sent_at       timestamptz,
  -- RETRY SAFETY for the offline send queue (migration 00025, #62). A send is not
  -- idempotent the way an Email/set patch is: without this, replaying a submission
  -- after a lost RESPONSE (dropped connection, closed laptop, proxy timeout) creates
  -- a second submission and mails the recipient twice. The client generates one key
  -- per queued action; a repeat returns the SUBMISSION THAT ALREADY EXISTS.
  --
  -- Deliberately NOT a constraint on message_id: that would forbid a legitimate
  -- re-send after an Undo Send cancellation, and it answers "has this draft been
  -- sent?" when a retry needs to know "is this the same attempt I already made?".
  -- Same shape as app.jobs.idempotency_key.
  --
  -- Nullable: pre-00025 rows have none, and a caller that supplies none (the CLI) is
  -- legal — it is simply not retry-safe, which is how everything behaved before.
  --
  -- LAST in the table on purpose: migration 00025 ADDs it, and ADD COLUMN appends, so
  -- declaring it mid-table here would make this file disagree with what the migration
  -- actually produces (migrate-check compares the two dumps and says so).
  idempotency_key text,
  CONSTRAINT submissions_idempotency_key_bounded
    CHECK (idempotency_key IS NULL OR length(idempotency_key) BETWEEN 8 AND 200),
  -- SEND & ARCHIVE (migration 00059, #648): the messages to archive WHEN AND
  -- ONLY WHEN this submission is actually sent. The send worker reads it in the
  -- transaction that marks the row 'sent'; a failed, cancelled or exhausted
  -- submission never reaches that point, so it archives nothing. Not a foreign
  -- key (no element FKs; a named message may be deleted before the send), so
  -- the worker re-resolves each id under RLS and skips what it cannot see.
  -- NULL = archive nothing.
  --
  -- LAST in the table on purpose, like idempotency_key above: 00059 ADDs it and
  -- ADD COLUMN appends, so declaring it anywhere else here would make this file
  -- disagree with what the migration chain produces (migrate-check).
  archive_on_success app.ulid[],
  CONSTRAINT submissions_archive_on_success_bounded
    CHECK (archive_on_success IS NULL OR cardinality(archive_on_success) BETWEEN 1 AND 100),
  -- DISMISSING A FAILED SEND (migration 00059): when the user took a
  -- permanently failed submission off the Outbox (EmailSubmission/set update
  -- `dismissed: true`). Nothing is deleted — not this record, not the draft —
  -- the Outbox simply stops listing it. The CHECK makes it unrepresentable on
  -- any state but 'failed', so a dismissed row is never a send still pending.
  -- LAST, after archive_on_success, in the order 00059 ADDs them (ADD COLUMN
  -- appends; migrate-check compares column order).
  dismissed_at timestamptz,
  CONSTRAINT submissions_dismissed_only_failed
    CHECK (dismissed_at IS NULL OR state = 'failed'),
  -- THE API KEY THAT MADE THIS SEND (migration 00076, #49), NULL for a
  -- person. ArchiveAfterSend reads it so a key's Send & archive is the key's
  -- and keeps no learned placement on the user's behalf. Provenance, not a
  -- foreign key (a key is revoked, never deleted). LAST, in ADD COLUMN order.
  api_key_id app.ulid
);
ALTER TABLE app.submissions ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.submissions FORCE ROW LEVEL SECURITY;
CREATE POLICY submissions_isolation ON app.submissions
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());
CREATE INDEX submissions_user_state ON app.submissions (user_id, state, send_at);
-- Retry safety for the offline send queue (#62). Partial so the many NULL rows do not
-- collide, and per-user so two clients generating the same key cannot.
CREATE UNIQUE INDEX submissions_user_idempotency_key
  ON app.submissions (user_id, idempotency_key)
  WHERE idempotency_key IS NOT NULL;

-- Per-account, per-day outbound spend vs. provider caps (spec §5).
CREATE TABLE app.outbound_ledger (
  account_id    app.ulid NOT NULL REFERENCES app.accounts(id) ON DELETE CASCADE,
  user_id       app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  day           date NOT NULL,
  sent_count    int NOT NULL DEFAULT 0,
  PRIMARY KEY (account_id, day)
);
ALTER TABLE app.outbound_ledger ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.outbound_ledger FORCE ROW LEVEL SECURITY;
CREATE POLICY outbound_ledger_isolation ON app.outbound_ledger
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());

-- Parsed RFC 3464 DSNs / bounces, linked to the originating message (spec §4).
CREATE TABLE app.dsn_events (
  id            app.ulid PRIMARY KEY,
  user_id       app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  origin_message_id app.ulid REFERENCES app.messages(id) ON DELETE SET NULL,
  origin_submission_id app.ulid REFERENCES app.submissions(id) ON DELETE SET NULL,
  dsn_message_id app.ulid REFERENCES app.messages(id) ON DELETE SET NULL, -- the DSN itself
  recipient     citext NOT NULL,
  action        app.dsn_action NOT NULL,
  status_code   text,                          -- '5.1.1' etc.
  diagnostic    text,
  created_at    timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE app.dsn_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.dsn_events FORCE ROW LEVEL SECURITY;
CREATE POLICY dsn_events_isolation ON app.dsn_events
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());
CREATE INDEX dsn_by_origin ON app.dsn_events (user_id, origin_message_id);

-- ----------------------------------------------------------------------------
-- 9. Triage & ML
-- ----------------------------------------------------------------------------
CREATE TABLE app.triage_scores (
  user_id       app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  message_id    app.ulid NOT NULL REFERENCES app.messages(id) ON DELETE CASCADE,
  score         real NOT NULL,                 -- 0..1 priority
  model_version text NOT NULL,
  top_features  jsonb NOT NULL DEFAULT '[]',   -- explainability (spec §8)
  scored_at     timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, message_id)
);
ALTER TABLE app.triage_scores ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.triage_scores FORCE ROW LEVEL SECURITY;
CREATE POLICY triage_scores_isolation ON app.triage_scores
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());

CREATE TABLE app.feedback_events (
  id            app.ulid PRIMARY KEY,
  user_id       app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  message_id    app.ulid REFERENCES app.messages(id) ON DELETE CASCADE,
  action        text NOT NULL,                 -- 'open','reply','archive_unread','star',...
  context       jsonb NOT NULL DEFAULT '{}',   -- reply latency, surface, etc.
  created_at    timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE app.feedback_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.feedback_events FORCE ROW LEVEL SECURITY;
CREATE POLICY feedback_events_isolation ON app.feedback_events
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());
CREATE INDEX feedback_user_time ON app.feedback_events (user_id, created_at);
-- The clean-up ranking's neglect set (#53, migration 00062): the caller's
-- archive_unread events by message, without walking every other action.
CREATE INDEX feedback_archive_unread ON app.feedback_events (user_id, message_id)
  WHERE action = 'archive_unread';

-- Model registry: artifacts live in the blob store under blob discipline;
-- snapshots + lineage for rollback/reset (spec §8).
CREATE TABLE app.ml_models (
  id            app.ulid PRIMARY KEY,
  user_id       app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  model_kind    text NOT NULL,                 -- 'priority','labeler'
  version       int NOT NULL,
  parent_id     app.ulid REFERENCES app.ml_models(id),
  artifact_hash app.sha256 NOT NULL,           -- blob ref
  active        boolean NOT NULL DEFAULT false,
  created_at    timestamptz NOT NULL DEFAULT now(),
  UNIQUE (user_id, model_kind, version)
);
ALTER TABLE app.ml_models ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.ml_models FORCE ROW LEVEL SECURITY;
CREATE POLICY ml_models_isolation ON app.ml_models
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());
CREATE UNIQUE INDEX ml_models_one_active
  ON app.ml_models (user_id, model_kind) WHERE active;

CREATE TABLE app.rules (
  id            app.ulid PRIMARY KEY,
  user_id       app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  name          text NOT NULL,
  priority      int NOT NULL DEFAULT 100,
  conditions    jsonb NOT NULL,                -- T0 condition tree (spec §8)
  actions       jsonb NOT NULL,
  enabled       boolean NOT NULL DEFAULT true,
  system_suggested boolean NOT NULL DEFAULT false,
  created_at    timestamptz NOT NULL DEFAULT now(),
  UNIQUE (user_id, name)
);
ALTER TABLE app.rules ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.rules FORCE ROW LEVEL SECURITY;
CREATE POLICY rules_isolation ON app.rules
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());

-- Where the per-label classifier's ABSTENTIONS land (spec §8, #169): a queue of
-- "I do not know" for a human to answer. These are NOT labels — nothing here
-- affects mailbox membership, keywords or what any other client sees. Stored
-- rather than derived so the review queue cannot silently change under the
-- reader when a model retrains. Migration 00018 explains the rest.
CREATE TABLE app.label_suggestions (
  user_id     app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  message_id  app.ulid NOT NULL REFERENCES app.messages(id) ON DELETE CASCADE,
  label_id    app.ulid NOT NULL REFERENCES app.labels(id) ON DELETE CASCADE,
  -- ⚠ WHAT THIS MEANS DEPENDS ON `source` (migration 00036, #242). For a 'model'
  -- row it is the classifier's probability, in [0,1] and within the abstention
  -- band. For a 'rule' row it is rules.EvidenceConfidence(n) — a pure ORDERING
  -- RANK over how many messages the user filed under the label, with no
  -- probability semantics at all. It is NOT NULL, so a rule row has to put
  -- something here, and a constant would collapse the needs-a-category ORDER BY
  -- (jmap_email.go) to its date tie-break for every rule-sourced suggestion.
  confidence  double precision NOT NULL,
  model_version int NOT NULL DEFAULT 0,
  created_at  timestamptz NOT NULL DEFAULT now(),
  -- WHY (migration 00019, #176): the raw []ml.FeatureContribution behind the
  -- confidence, in the SAME shape as app.triage_scores.top_features so both
  -- "why?" panels render through one function. Nullable with no default: NULL is
  -- "no record of why" (a suggestion older than the column, or a model that
  -- could not explain itself) and '[]' is "the model explained and had nothing
  -- to say" — a default would turn the first into a claim of the second.
  --
  -- A rule-sourced row fills it with the same shape, using the `text` field
  -- (ml.FeatureContribution.Text) to carry a literal rendered condition —
  -- "alias is shop@example.com" — instead of a feature key, plus a trailing
  -- entry naming the evidence count. Weights are 0 there and that is deliberate:
  -- an AND rule's conjuncts are each individually necessary, so no per-conjunct
  -- weight exists to report and inventing one would be a fabricated number.
  --
  -- LAST, after created_at, because migrate-check diffs pg_dump output and
  -- ALTER TABLE ADD COLUMN appends: a tidier position here would read as drift.
  reasons     jsonb,
  -- WHICH SYSTEM ASKED (migration 00036, #242): 'model' = the per-label
  -- classifier via ingest's classifyMessage; 'rule' = an induced rule's
  -- `suggestLabel` action via applyRules. The client BRANCHES on this to decide
  -- whether confidence may be rendered as a percentage — for a rule row it may
  -- not, and the evidence count in `reasons` is shown instead. DEFAULT 'model' is
  -- a statement of fact, not an assumption: classifyMessage was the only writer
  -- this table ever had before #242.
  --
  -- After `reasons` for the same reason `reasons` is after `created_at`.
  source      text NOT NULL DEFAULT 'model',
  CONSTRAINT label_suggestions_source_known CHECK (source IN ('model', 'rule')),
  PRIMARY KEY (user_id, message_id, label_id)
);
ALTER TABLE app.label_suggestions ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.label_suggestions FORCE ROW LEVEL SECURITY;
CREATE POLICY label_suggestions_isolation ON app.label_suggestions
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());
CREATE INDEX label_suggestions_by_confidence
  ON app.label_suggestions (user_id, confidence DESC, message_id);

-- "This message does not need a category" (migration 00019, #176). The queue is
-- everything nothing has categorised — an open suggestion OR no label at all —
-- so a dismissal cannot be expressed by deleting suggestion rows: the message
-- would still be unlabelled and still queued. This is the tombstone that lets
-- the queue actually reach empty, and that stops a future retrain re-asking a
-- question the user already declined. It records NOTHING about a label being
-- wrong.
CREATE TABLE app.category_dismissals (
  user_id      app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  message_id   app.ulid NOT NULL REFERENCES app.messages(id) ON DELETE CASCADE,
  dismissed_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, message_id)
);
ALTER TABLE app.category_dismissals ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.category_dismissals FORCE ROW LEVEL SECURITY;
CREATE POLICY category_dismissals_isolation ON app.category_dismissals
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());

-- Content hashes a message USED to have, before its bytes were replaced in
-- place (migration 00033, #260). Today that happens in exactly one place:
-- compose-time PGP signing at submit (#47 increment 5).
--
-- ⚠ It exists because signing broke an assumption dedup relied on. The copy
-- already APPENDed to IMAP Drafts holds the pre-signature bytes, and once the
-- local row's hash moves, a sync that reads Drafts before the expunge lands
-- ingests that stale copy as a NEW message — a stray unsigned draft for a
-- message the user has already sent. Observed on a real send, not predicted:
-- sync beat the expunge by seven seconds.
--
-- The tombstone maps to the message id rather than being a bare denylist,
-- because the message was mutated IN PLACE and its id is still correct — so
-- ingest records the stale delivery as provenance against the real message,
-- exactly as the other dedup paths do (#206), instead of discarding it.
CREATE TABLE app.superseded_content (
  user_id      app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  content_hash bytea NOT NULL,
  message_id   app.ulid NOT NULL REFERENCES app.messages(id) ON DELETE CASCADE,
  created_at   timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, content_hash)
);
COMMENT ON TABLE app.superseded_content IS
  'Content hashes a message USED to have, after its bytes were replaced in '
  'place (currently only compose-time PGP signing). Ingest consults this so a '
  'stale IMAP copy of the pre-replacement bytes is recognised as the same '
  'message rather than ingested as a new one (#260).';
ALTER TABLE app.superseded_content ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.superseded_content FORCE ROW LEVEL SECURITY;
CREATE POLICY superseded_content_isolation ON app.superseded_content
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());

ALTER TABLE app.message_labels
  ADD CONSTRAINT message_labels_applied_by_rule_fk
  FOREIGN KEY (applied_by_rule_id) REFERENCES app.rules(id) ON DELETE SET NULL;

-- When a user takes back what a rule did (migration 00020, #170). Removing a
-- label is a DELETE, so the attribution goes with it and the rule keeps firing
-- forever with nothing recording that a human disagreed. ONE row per (rule,
-- message, label), not per event: a user who removes the same label twice has
-- been wronged about one message twice, and counting events would measure how
-- stubborn a rule is rather than how wrong. Evidence only — nothing here
-- disables or edits a rule; it enables the OFFER to narrow one.
CREATE TABLE app.rule_corrections (
  user_id      app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  rule_id      app.ulid NOT NULL REFERENCES app.rules(id) ON DELETE CASCADE,
  message_id   app.ulid NOT NULL REFERENCES app.messages(id) ON DELETE CASCADE,
  label_id     app.ulid NOT NULL REFERENCES app.labels(id) ON DELETE CASCADE,
  corrected_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, rule_id, message_id, label_id)
);
ALTER TABLE app.rule_corrections ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.rule_corrections FORCE ROW LEVEL SECURITY;
CREATE POLICY rule_corrections_isolation ON app.rule_corrections
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());
CREATE INDEX rule_corrections_by_rule
  ON app.rule_corrections (user_id, rule_id);

-- Remembering "show remote content", per sender (#202, migration 00021).
--
-- Remote images are blocked by default (spec §11) and the affordance that loads
-- them was per-message and did NOT persist, because "I trusted this one newsletter
-- once" must not become "this sender may track me forever". That is right about a
-- BLANKET allow; what it left missing was anywhere to record a NARROW one, so the
-- only choices were asking every time and never asking — and asking every time
-- about the same newsletter is how a safe default becomes something users route
-- around.
--
-- TWO MATCHERS, ONE CONCEPT: 'address' is exactly this From, 'domain' is anyone at
-- example.com. A person is an address; a publisher is a domain (one sender can reach
-- a mailbox from both mail.vendor.example and email.vendor-alt.example), so an
-- address-only allow would ask repeatedly about one decision.
--
-- NOT a flag on app.contacts: those rows are auto-created from received mail
-- (ADR-014), so a flag there mixes an explicit human decision into machine-made
-- rows, and a domain is not a contact. One table = one revocation path = one list
-- to audit, and auditability is load-bearing: an allow nobody can see or revoke is
-- the permanent consent the original comment feared, moved into a database.
--
-- NO GLOBAL ROW, by construction: the CHECK admits no kind='all' and there is no
-- wildcard value, so the schema cannot express "load everything" and no future UI
-- can accidentally offer it.
--
-- The SERVER still parks every remote URL unconditionally (internal/api/sanitize.go
-- never branches on a preference). This table is read by the CLIENT to decide what
-- to un-park — docs/hostile-content.md §2.3.
CREATE TABLE app.remote_allows (
  id         app.ulid PRIMARY KEY,
  user_id    app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  kind       text NOT NULL CHECK (kind IN ('address', 'domain')),
  value      citext NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (user_id, kind, value)
);
ALTER TABLE app.remote_allows ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.remote_allows FORCE ROW LEVEL SECURITY;
CREATE POLICY remote_allows_isolation ON app.remote_allows
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());
CREATE INDEX remote_allows_user_created
  ON app.remote_allows (user_id, created_at DESC);

-- Signatures and templates: named, reusable compose text (#56, migration 00022).
--
-- ONE TABLE FOR BOTH because they are the same thing wearing two hats — a named
-- piece of body text the user wrote once and reuses. A signature is appended and may
-- belong to an identity; a template seeds a whole message and never does. Everything
-- else (name, both bodies, which is default, editing, deleting) is identical, and
-- two tables would mean two JMAP objects and two settings surfaces for one question.
--
-- identities.signature_html STAYS and keeps working: it holds exactly one signature
-- per identity, is read by the JMAP Identity object's htmlSignature that clients
-- already use, and is treated as that identity's default when this table has nothing
-- for it. #56 only adds the ability to have SEVERAL and pick.
--
-- identity_id nullable, and the states differ: a signature with an identity is
-- offered when sending as that address (spec §5 — the signature for shop@ is not the
-- one for work@); with NULL it is offered for any. A template is CHECKed to never
-- have one. At most one default per (user, kind, identity), enforced by partial
-- unique indexes rather than application code, because two defaults is a state no UI
-- can render honestly.
CREATE TABLE app.compose_snippets (
  id          app.ulid PRIMARY KEY,
  user_id     app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  kind        text NOT NULL CHECK (kind IN ('signature', 'template')),
  identity_id app.ulid REFERENCES app.identities(id) ON DELETE CASCADE,
  CONSTRAINT compose_snippets_template_has_no_identity
    CHECK (kind <> 'template' OR identity_id IS NULL),
  name        text NOT NULL,
  body_html   text NOT NULL DEFAULT '',
  body_text   text NOT NULL DEFAULT '',
  CONSTRAINT compose_snippets_has_a_body
    CHECK (body_html <> '' OR body_text <> ''),
  is_default  boolean NOT NULL DEFAULT false,
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now(),
  UNIQUE (user_id, kind, name)
);
ALTER TABLE app.compose_snippets ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.compose_snippets FORCE ROW LEVEL SECURITY;
CREATE POLICY compose_snippets_isolation ON app.compose_snippets
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());
CREATE UNIQUE INDEX compose_snippets_one_default_per_identity
  ON app.compose_snippets (user_id, kind, identity_id)
  WHERE is_default AND identity_id IS NOT NULL;
CREATE UNIQUE INDEX compose_snippets_one_default_no_identity
  ON app.compose_snippets (user_id, kind)
  WHERE is_default AND identity_id IS NULL;
CREATE INDEX compose_snippets_user_kind ON app.compose_snippets (user_id, kind, name);

-- SAVED SEARCHES / SMART FOLDERS (migration 00024, #57). A named query, plus a
-- boolean for whether it is pinned to the sidebar — the matrix lists "saved
-- searches" and "smart folders" as two rows, but they are one feature and a flag.
--
-- The query is the OPERATOR STRING as typed, not a parsed filter tree, so a saved
-- search runs through exactly the same path as typing it: there is no second
-- interpretation that could disagree with the first. It is also what the user can
-- read back and edit. The accepted cost is that an invalid query (a label deleted
-- later) fails when it RUNS rather than when it was saved — the same failure typing
-- it by hand gives, so not a new class of surprise.
--
-- Local Mailroom state only: there is no IMAP concept to mirror, nothing is written
-- back, and it syncs between the user's own devices through the change log.
CREATE TABLE app.saved_searches (
  id         app.ulid PRIMARY KEY,
  user_id    app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  name       text NOT NULL,
  query      text NOT NULL,
  CONSTRAINT saved_searches_query_not_blank CHECK (btrim(query) <> ''),
  CONSTRAINT saved_searches_query_bounded   CHECK (length(query) <= 2000),
  CONSTRAINT saved_searches_name_not_blank  CHECK (btrim(name) <> ''),
  CONSTRAINT saved_searches_name_bounded    CHECK (length(name) <= 120),
  in_sidebar boolean NOT NULL DEFAULT true,
  -- Explicit ordering: the sidebar is a place a person arranges. Ties break by name
  -- so the order is total and stable rather than planner-dependent.
  position   int NOT NULL DEFAULT 0,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (user_id, name)
);
ALTER TABLE app.saved_searches ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.saved_searches FORCE ROW LEVEL SECURITY;
CREATE POLICY saved_searches_isolation ON app.saved_searches
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());
CREATE INDEX saved_searches_user_sidebar ON app.saved_searches (user_id, position, name);

-- ----------------------------------------------------------------------------
-- 10. Contacts (spec §10a)
-- ----------------------------------------------------------------------------
CREATE TABLE app.contacts (
  id            app.ulid PRIMARY KEY,
  user_id       app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  display_name  text NOT NULL DEFAULT '',
  vip           boolean NOT NULL DEFAULT false,
  blocked       boolean NOT NULL DEFAULT false,
  notes         text NOT NULL DEFAULT '',
  created_at    timestamptz NOT NULL DEFAULT now(),
  -- BLOCK SENDER (#59, migration 00061): where new mail from a blocked
  -- contact's addresses goes at ingest ('junk' is the default), and when it
  -- was blocked. blocked_at is set exactly while blocked is true. Declared
  -- LAST to match ADD COLUMN's append order (migrate-check).
  block_destination text NOT NULL DEFAULT 'junk' CHECK (block_destination IN ('junk', 'trash')),
  blocked_at    timestamptz,
  -- SAVED CONTACTS, NAME, MERGE (#816, migration 00072, ADR-041). Declared
  -- LAST to match ADD COLUMN's append order (migrate-check).
  -- saved_at: non-NULL = listed on the Contacts page (and when it was saved).
  saved_at      timestamptz,
  -- the user's own name for them; ingest never writes it. Displayed name is
  -- COALESCE(user_display_name, display_name).
  user_display_name text,
  -- a tombstone: merged into that contact, hidden from every read, kept so
  -- Contact/separate can restore its own state.
  merged_into   app.ulid REFERENCES app.contacts(id) ON DELETE CASCADE,
  CONSTRAINT contacts_blocked_stamped CHECK (blocked = (blocked_at IS NOT NULL)),
  CONSTRAINT contacts_notes_length CHECK (char_length(notes) <= 4000),
  CONSTRAINT contacts_user_display_name_length CHECK (char_length(user_display_name) <= 200)
);
ALTER TABLE app.contacts ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.contacts FORCE ROW LEVEL SECURITY;
CREATE POLICY contacts_isolation ON app.contacts
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());
-- VIP and blocked contacts are a handful among thousands of auto-derived rows,
-- and both sets are read hot: is:vip and the push counter (VIP), Settings and
-- the ingest-time block (blocked) (#59).
CREATE INDEX contacts_user_vip ON app.contacts (user_id) WHERE vip;
CREATE INDEX contacts_user_blocked ON app.contacts (user_id) WHERE blocked;

CREATE TABLE app.contact_addresses (
  id            app.ulid PRIMARY KEY,
  user_id       app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  contact_id    app.ulid NOT NULL REFERENCES app.contacts(id) ON DELETE CASCADE,
  address       citext NOT NULL,
  -- the alias story: which of MY addresses this correspondent used (spec §5)
  reached_me_at citext,
  first_seen_at timestamptz NOT NULL DEFAULT now(),
  last_seen_at  timestamptz NOT NULL DEFAULT now(),
  seen_count    int NOT NULL DEFAULT 1,
  -- How many times the USER has written to this address (migration 00016,
  -- mailroom#157). seen_count counts mail RECEIVED, so it measures how loudly a
  -- sender talks at you — which made a daily newsletter the "best known" contact
  -- in the mailbox and wrecked priority scoring. This is the signal that actually
  -- means correspondence. Bumped when a message whose From is one of the user's
  -- own identities is ingested, so an imported Sent folder counts too.
  outbound_count int NOT NULL DEFAULT 0,
  -- (migration 00072, #816) the home contact this address was merged in from
  -- (now a tombstone); NULL while it is in its home contact.
  merged_from_contact app.ulid REFERENCES app.contacts(id) ON DELETE SET NULL,
  UNIQUE (user_id, address)
);
ALTER TABLE app.contact_addresses ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.contact_addresses FORCE ROW LEVEL SECURITY;
CREATE POLICY contact_addresses_isolation ON app.contact_addresses
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());
CREATE INDEX contact_addresses_by_contact ON app.contact_addresses (user_id, contact_id);

-- Discovered public keys for correspondents (WKD/Autocrypt/manual).
CREATE TABLE app.contact_keys (
  id            app.ulid PRIMARY KEY,
  user_id       app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  contact_id    app.ulid NOT NULL REFERENCES app.contacts(id) ON DELETE CASCADE,
  kind          app.key_kind NOT NULL,
  fingerprint   text NOT NULL,
  public_key    bytea NOT NULL,
  source        text NOT NULL,                 -- 'wkd'|'autocrypt'|'manual'|'keyserver'
  fetched_at    timestamptz NOT NULL DEFAULT now(),
  -- (migration 00068, #47) the carrying message was authenticated for its From
  -- domain (DMARC pass / aligned local DKIM). Encryption ignores autocrypt keys
  -- where this is false; existing rows are false (nobody checked).
  authenticated boolean NOT NULL DEFAULT false,
  -- (migration 00069, #47) the user chose "keep the old key": never encrypted
  -- to, and a signature by it keeps showing as a changed key.
  refused       boolean NOT NULL DEFAULT false,
  -- (migration 00071, #815) soft delete: ContactKey/set destroy sets it, every
  -- reader that picks a key for encryption, verification or "current" display
  -- excludes the row, and a re-sighting revives it (ADR-040 §11).
  removed_at    timestamptz,
  -- (migration 00071, #818) when ContactKey/refresh last asked WKD about it.
  checked_at    timestamptz,
  -- (migration 00072, #816) the home contact this key was merged in from.
  merged_from_contact app.ulid REFERENCES app.contacts(id) ON DELETE SET NULL,
  UNIQUE (user_id, contact_id, fingerprint)
);
ALTER TABLE app.contact_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.contact_keys FORCE ROW LEVEL SECURITY;
CREATE POLICY contact_keys_isolation ON app.contact_keys
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());

-- Correspondent key history (migration 00071, #815, ADR-040 §11): one row per
-- real change to a held key, appended in the same savepoint as the change.
-- change_log cannot serve (it is compacted). No change_log row of its own.
CREATE TABLE app.contact_key_events (
  id             app.ulid PRIMARY KEY,
  user_id        app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  contact_key_id app.ulid NOT NULL REFERENCES app.contact_keys(id) ON DELETE CASCADE,
  kind           text NOT NULL CHECK (kind IN ('first_seen', 'renewed', 'updated', 'revoked_seen',
                                               'pinned', 'refused', 'removed')),
  source         text NOT NULL CHECK (source IN ('autocrypt', 'wkd', 'manual', 'keyserver', 'user')),
  at             timestamptz NOT NULL DEFAULT now(),
  expires_at     timestamptz
);
CREATE INDEX contact_key_events_by_key ON app.contact_key_events (user_id, contact_key_id, at);
ALTER TABLE app.contact_key_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.contact_key_events FORCE ROW LEVEL SECURITY;
CREATE POLICY contact_key_events_isolation ON app.contact_key_events
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());

-- What each Contact/merge borrowed (migration 00073, #834, ADR-041 §4): into's
-- state before the merge, the combined state the merge wrote, and the address
-- rows it moved. A separate that takes every one of those addresses back
-- restores each of into's fields that still equals what the merge wrote, and
-- deletes the row. The states are JSONB snapshots read back whole, never
-- queried by field. No change_log row of its own (not a JMAP object).
CREATE TABLE app.contact_merges (
  id            app.ulid PRIMARY KEY,
  user_id       app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  into_contact  app.ulid NOT NULL REFERENCES app.contacts(id) ON DELETE CASCADE,
  from_contact  app.ulid NOT NULL REFERENCES app.contacts(id) ON DELETE CASCADE,
  addresses     app.ulid[] NOT NULL,
  before_state  jsonb NOT NULL,
  after_state   jsonb NOT NULL,
  merged_at     timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT contact_merges_two_contacts CHECK (into_contact <> from_contact)
);
CREATE INDEX contact_merges_by_into ON app.contact_merges (user_id, into_contact, id);
ALTER TABLE app.contact_merges ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.contact_merges FORCE ROW LEVEL SECURITY;
CREATE POLICY contact_merges_isolation ON app.contact_merges
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());

-- Trust on first use for encryption (migration 00068, #47, ADR-038): the
-- fingerprint actually encrypted to for each address. A later send whose
-- resolved key differs is refused until the user confirms the change.
CREATE TABLE app.contact_key_pins (
  user_id     app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  address     citext NOT NULL,
  fingerprint text NOT NULL,
  source      text NOT NULL,
  pinned_at   timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, address)
);
ALTER TABLE app.contact_key_pins ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.contact_key_pins FORCE ROW LEVEL SECURITY;
CREATE POLICY contact_key_pins_isolation ON app.contact_key_pins
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());

-- ----------------------------------------------------------------------------
-- 11. User keys, API keys, push, audit, AI providers, sync jobs
-- ----------------------------------------------------------------------------
-- User signing/encryption keys. PRIVATE material is wrapped and usable only
-- via the crypto process (spec §9); this table stores wrapped bytes + refs.
CREATE TABLE app.user_keys (
  id            app.ulid PRIMARY KEY,
  user_id       app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  identity_id   app.ulid REFERENCES app.identities(id) ON DELETE SET NULL,
  kind          app.key_kind NOT NULL,
  fingerprint   text NOT NULL,
  public_key    bytea NOT NULL,
  private_key_ct bytea,                        -- wrapped; NULL for public-only entries
  dek_id        app.ulid,
  envelope_ver  smallint NOT NULL DEFAULT 1,
  decrypt_only  boolean NOT NULL DEFAULT false, -- retired keys kept forever (spec §9)
  wkd_published boolean NOT NULL DEFAULT false,
  created_at    timestamptz NOT NULL DEFAULT now(),
  revoked_at    timestamptz,
  -- Where the private half came from (migration 00039, #54). NULLABLE ON
  -- PURPOSE: NULL means NOT RECORDED, for rows written before 00039 existed.
  --
  -- ⚠ 'generated' means Mailroom holds the ONLY copy, so a hard delete destroys
  -- it and retroactively makes already-exported PGP mail unreadable. 'imported'
  -- means the user holds it elsewhere and losing this copy costs them nothing.
  -- NULL MUST NEVER BE READ AS 'imported' — see the migration for why the
  -- unknown case is the cautious one, not the safe one.
  --
  -- ⚠ IT SITS AFTER revoked_at BECAUSE ADD COLUMN APPENDS, AND migrate-check
  -- COMPARES COLUMN ORDER. Declaring it next to the other key-state columns,
  -- where it reads better, put it before created_at here and last in a
  -- migrations-built database — semantically identical, and a migrate-check
  -- DRIFT failure. Do not "tidy" it back up the struct.
  origin        text,
  -- (migration 00071, #815) when the key stopped being used (retire or revoke,
  -- whichever came first). NULL = NOT RECORDED, never "never retired": rows
  -- retired before 00071 have no date. LAST for the same reason as origin.
  retired_at    timestamptz,
  -- (migration 00071, #817) the key that replaced this one (UserKey/replace).
  replaced_by   app.ulid REFERENCES app.user_keys(id) ON DELETE SET NULL,
  CONSTRAINT user_keys_origin_known CHECK (origin IN ('generated', 'imported')),
  UNIQUE (user_id, fingerprint)
);
ALTER TABLE app.user_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.user_keys FORCE ROW LEVEL SECURITY;
CREATE POLICY user_keys_isolation ON app.user_keys
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());

-- What actually seals private_key_ct, recorded in the database itself
-- (migration 00030, ADR-024). dek_id predates ADR-024 and is NULL for pgp rows;
-- see the migration for why it is documented rather than dropped.
COMMENT ON TABLE app.user_keys IS
  'The user''s own signing/encryption keys. For kind=''pgp'', private_key_ct is '
  'sealed by the CRYPTO PROCESS under its own root secret (ADR-024), not under '
  'the application master key: pgp root -> HKDF(salt=user_id, '
  'info="mailroom-pgp-v1") -> wrapping key, which is derived per user and never '
  'stored. The application can read this ciphertext and cannot open it. '
  'Mutations append a ''Key'' change_log row.';

COMMENT ON COLUMN app.user_keys.origin IS
  'Where the private half came from: ''generated'' (minted in the crypto '
  'process; Mailroom holds the ONLY copy) or ''imported'' (the user holds it '
  'elsewhere too). NULL means NOT RECORDED — rows predating migration 00039, '
  'whose origin is unrecoverable. NULL must never be read as ''imported'': a '
  'hard delete of a generated key is irreversible and retroactively destroys '
  'the readability of already-exported mail (#54, ADR-024).';

COMMENT ON COLUMN app.user_keys.private_key_ct IS
  'Sealed private key (crypto.Seal envelope). For kind=''pgp'' the sealing key '
  'is derived from the crypto process''s independent root, and the AEAD '
  'associated data binds BOTH user_id and this row''s id — so moving these bytes '
  'to another user''s row, or another key''s row, yields ciphertext that does not '
  'open. NULL for public-only entries.';

COMMENT ON COLUMN app.user_keys.dek_id IS
  'UNUSED AND NULL FOR kind=''pgp'' (ADR-024). It dates from the pre-ADR-024 '
  'design in which private keys were sealed under the application''s master-key '
  'DEK hierarchy. They are not: the crypto process derives the wrapping key from '
  'its OWN root and stores no DEK. Do not infer from this column that a '
  'master-key holder can recover PGP private keys — they cannot, and that is the '
  'property the separate process exists for.';

COMMENT ON COLUMN app.user_keys.envelope_ver IS
  'internal/crypto envelope version of private_key_ct (crypto.Version1 = 1). '
  'Bumped by a change to the sealing scheme, not by key rotation.';

COMMENT ON COLUMN app.user_keys.identity_id IS
  'The identity this key signs for, or NULL if unbound. Binding also sets '
  'app.identities.default_key_id, which is a mutation of a JMAP-visible object '
  'and therefore appends an ''Identity'' change_log row of its own.';

-- The PGP root's boot-time check (migration 00055, #525). One row: a one-way
-- value derived from the crypto process's root, recorded at its first boot
-- against this database and compared at every boot after, so a restore paired
-- with the wrong root refuses to start instead of serving healthily until the
-- first signature fails. The migration carries the full argument — why a check
-- value rather than unsealing a stored key, and the trust-on-first-use bound
-- for a database that predates it.
--
-- ⚠ THE INVERSE OF "NO IDENTITY, NO ROWS". Its one row is visible ONLY where
-- app.current_user_id() IS NULL (a SystemTx — the crypto process's boot), and
-- is invisible and uninsertable under any user's identity. mailroom_app gets
-- SELECT and INSERT only (section 13): with no UPDATE and no DELETE, the
-- application — which shares that role — can neither overwrite the value nor
-- delete it. Resetting it is a superuser act.
CREATE TABLE app.pgp_root_check (
  singleton   boolean PRIMARY KEY DEFAULT true CHECK (singleton),
  info        text NOT NULL,
  check_value bytea NOT NULL CHECK (octet_length(check_value) = 32),
  recorded_at timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE app.pgp_root_check ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.pgp_root_check FORCE ROW LEVEL SECURITY;
CREATE POLICY pgp_root_check_system_read ON app.pgp_root_check
  FOR SELECT TO mailroom_app
  USING (app.current_user_id() IS NULL);
CREATE POLICY pgp_root_check_system_record ON app.pgp_root_check
  FOR INSERT TO mailroom_app
  WITH CHECK (app.current_user_id() IS NULL);

COMMENT ON TABLE app.pgp_root_check IS
  'One row: HKDF(pgp root, info="mailroom-pgp-root-check-v1"), recorded by the '
  'crypto process at its first boot against this database and compared at '
  'every boot after; a mismatch stops it before it listens (#525). Claims only '
  '"this is the root this database was built with", not "this root opens the '
  'stored keys" — the restore drill proves that. Not a secret. Insert-only for '
  'mailroom_app; resetting it is a superuser act (docs/backup-restore-runbook.md).';

-- WKD publication (migration 00031, #48; re-owned by 00035, #266). The
-- anonymous, cross-user lookup path. The definer is subject to RLS, so this
-- policy IS the exposure — written out here rather than hidden inside a
-- function body.
CREATE POLICY user_keys_wkd_lookup ON app.user_keys
  FOR SELECT TO mailroom_wkd
  USING (wkd_published);

-- ⚠ THE DEFINER RUNS AS mailroom_wkd, A ROLE THAT OWNS NO TABLES, AND THAT IS
-- THE WHOLE DESIGN (migration 00035, Forgejo #266).
--
-- Three earlier attempts tried to narrow mailroom_owner instead, and all three
-- failed for one reason none of them named: a SECURITY DEFINER that runs as the
-- TABLE OWNER can never be narrowed. PostgreSQL executes a foreign key's
-- referential-integrity action with the privileges of the table owner, so
-- taking SELECT away from that role takes it away from every ON DELETE CASCADE
-- and ON DELETE SET NULL at the same time.
--
--   #48    column GRANT excluding the sealed column  -> inert (owners hold
--                                                      column privileges implicitly)
--   #257   added a column-level REVOKE               -> inert (a column revoke
--                                                      cannot remove a table-level one)
--   00032  table REVOKE + narrow re-GRANT            -> effective, and it broke
--                                                      every cascade into these two
--                                                      tables. Live for 15 hours.
--   00034  restored the grant                        -> narrowing became decoration
--
-- mailroom_wkd owns nothing, so it has no implicit privileges to fall back on
-- and nothing else in the database runs as it. What is absent from the grants
-- below is genuinely absent, and removing it cannot break anything else.
--
-- app.identities omits display_name, signature_html, account_id and
-- default_key_id — nothing sealed lives here, so the stakes are privacy rather
-- than key compromise; it gets the same treatment anyway.
GRANT USAGE ON SCHEMA app TO mailroom_wkd;
GRANT SELECT (id, user_id, address, domain, kind, enabled, wkd_hash)
  ON app.identities TO mailroom_wkd;
-- app.user_keys omits private_key_ct, dek_id and envelope_ver: the sealed
-- material, and the envelope metadata describing how to open it.
GRANT SELECT (id, user_id, identity_id, kind, fingerprint, public_key,
              wkd_published, decrypt_only, revoked_at, created_at)
  ON app.user_keys TO mailroom_wkd;

-- ⚠ WHAT THIS BUYS, AND WHAT IT DOES NOT. Rewriting the function body to select
-- private_key_ct now fails at runtime with `permission denied` — the privilege
-- is not there to use, which is the property #48 claimed and never had.
--
-- It does NOT make mailroom_owner powerless: it owns these tables and can read
-- anything in them, exactly as before #257. That is inherent to ownership and is
-- no longer where the boundary sits. The boundary is that the anonymous,
-- cross-user path does not run as an owner at all. mailroom_owner keeps its
-- table-level SELECT (migration 00034) because the cascades above depend on it.
--
-- Safe for backups: scripts/backup.sh refuses to dump unless the connecting
-- role has rolsuper or rolbypassrls, so neither role is ever the dump role.

-- Since migration 00070 (#795, ADR-040) it returns every published PGP key at
-- the address that is live OR REVOKED — live first, newest first, at most 8 —
-- so a revoked key keeps being served after the current one and correspondents
-- who refresh the address learn of the revocation. A retired-but-unrevoked key
-- stays out. store.LookupWKDKey picks what is served (expiry is in the packets).
CREATE OR REPLACE FUNCTION app.wkd_public_key(p_domain text, p_hash text)
RETURNS TABLE (address text, public_key bytea, fingerprint text)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = app, pg_temp AS $$
  SELECT i.address::text, k.public_key, k.fingerprint
    FROM app.identities i
    JOIN app.user_keys k
      ON k.identity_id = i.id AND k.user_id = i.user_id
   WHERE i.wkd_hash = p_hash
     AND i.kind = 'fixed'
     AND i.enabled
     AND lower(split_part(i.address::text, '@', 2)) = lower(p_domain)
     AND k.wkd_published
     AND k.kind = 'pgp'
     AND (k.revoked_at IS NOT NULL OR NOT k.decrypt_only)
   ORDER BY (k.revoked_at IS NULL) DESC, k.created_at DESC
   LIMIT 8
$$;

-- ⚠ OWNING AN OBJECT IN A SCHEMA REQUIRES CREATE ON THAT SCHEMA, checked at
-- ALTER time only. Granting it permanently would let the WKD role create objects
-- in app — the kind of privilege this design exists to withhold. Grant,
-- transfer, take it back.
GRANT CREATE ON SCHEMA app TO mailroom_wkd;
ALTER FUNCTION app.wkd_public_key(text, text) OWNER TO mailroom_wkd;
REVOKE CREATE ON SCHEMA app FROM mailroom_wkd;

REVOKE ALL ON FUNCTION app.wkd_public_key(text, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.wkd_public_key(text, text) TO mailroom_app;

COMMENT ON FUNCTION app.wkd_public_key(text, text) IS
  'Anonymous, cross-user WKD lookup. Owned by mailroom_wkd — a role that owns '
  'no tables and holds only the narrow column grants this needs — so it CANNOT '
  'read private_key_ct even if this body were rewritten to try (#266). Owning '
  'it with mailroom_owner, as #48/#257 did, made that guarantee unobtainable: '
  'a table owner must keep full privileges or foreign-key cascades fail.';

ALTER TABLE app.identities
  ADD CONSTRAINT identities_default_key_fk
  FOREIGN KEY (default_key_id) REFERENCES app.user_keys(id) ON DELETE SET NULL;

-- Scoped API keys / PATs (spec §13a). Hardened defaults live in app logic;
-- the schema records limits so they are enforceable + auditable.
CREATE TABLE app.api_keys (
  id            app.ulid PRIMARY KEY,
  user_id       app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  name          text NOT NULL,
  token_hash    bytea NOT NULL UNIQUE,         -- hash only; token shown once
  scopes        text[] NOT NULL,               -- 'mail:read','mail:send',...
  account_ids   app.ulid[],                    -- NULL = all the user's accounts
  label_ids     app.ulid[],                    -- optional label restriction
  confirm_mode  boolean NOT NULL DEFAULT false,
  rate_per_min  int NOT NULL DEFAULT 60,       -- mandatory (spec §13a)
  daily_budget  int NOT NULL DEFAULT 500,      -- mandatory (spec §13a)
  created_at    timestamptz NOT NULL DEFAULT now(),
  last_used_at  timestamptz,
  revoked_at    timestamptz,
  -- Migration 00074 (#49, ADR-042), appended in ADD COLUMN order. Which
  -- DESTRUCTIVE scopes wait for a human; supersedes confirm_mode above, which is
  -- never read and stays until a contract step (ADR-008).
  confirm_scopes text[] NOT NULL DEFAULT '{}',
  -- The first characters of the token body, so a list can say which key is
  -- which (35 of 256 bits). The token itself is never stored.
  token_hint    text NOT NULL DEFAULT '',
  -- Set by the mass-delete rule (when suspend_on_anomaly) or by hand; unlike
  -- revoked_at it can be cleared.
  suspended_at  timestamptz,
  suspended_reason text,
  suspend_on_anomaly boolean NOT NULL DEFAULT true,
  UNIQUE (user_id, name),
  CONSTRAINT api_keys_scopes_closed CHECK (
    cardinality(scopes) > 0 AND scopes <@ ARRAY['mail:read','mail:send','mail:draft','mail:move',
      'mail:delete','labels:read','labels:write','search:read','contacts:read','contacts:write']::text[]),
  CONSTRAINT api_keys_confirm_destructive CHECK (
    confirm_scopes <@ ARRAY['mail:send','mail:delete']::text[]),
  CONSTRAINT api_keys_rate_bounds CHECK (rate_per_min BETWEEN 1 AND 600),
  CONSTRAINT api_keys_budget_bounds CHECK (daily_budget BETWEEN 1 AND 20000),
  CONSTRAINT api_keys_hash_is_sha256 CHECK (octet_length(token_hash) = 32)
);
ALTER TABLE app.api_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.api_keys FORCE ROW LEVEL SECURITY;
CREATE POLICY api_keys_isolation ON app.api_keys
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());
COMMENT ON COLUMN app.api_keys.confirm_mode IS
  'SUPERSEDED by confirm_scopes (migration 00074, ADR-042) and never read. '
  'Kept until a contract step drops it (ADR-008).';

-- What a key did (#49, migration 00074, ADR-042): one row per method call a key
-- made (admitted or not), per anomaly, and per approval decision. NEVER message
-- content: ids and counts only. cost/deletes/sends are columns because the
-- limits and the anomaly rules query them. Written once; error_type (the outcome
-- of a call admitted before it ran) is the only column the app may change.
CREATE TABLE app.api_key_events (
  id            app.ulid PRIMARY KEY,
  user_id       app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  key_id        app.ulid NOT NULL REFERENCES app.api_keys(id) ON DELETE CASCADE,
  at            timestamptz NOT NULL DEFAULT now(),
  method        text NOT NULL,
  outcome       text NOT NULL CHECK (outcome IN ('ok', 'denied', 'rate_limited', 'over_budget',
                  'pending', 'approved', 'denied_by_user', 'expired', 'anomaly')),
  cost          int NOT NULL DEFAULT 0 CHECK (cost >= 0),
  deletes       int NOT NULL DEFAULT 0 CHECK (deletes >= 0),
  sends         int NOT NULL DEFAULT 0 CHECK (sends >= 0),
  summary       jsonb NOT NULL DEFAULT '{}',
  client_ip     inet,
  pending_id    app.ulid,
  error_type    text,
  -- Which anomaly rule an 'anomaly' row records (ADR-042 §6); NULL otherwise.
  anomaly       text CHECK (anomaly IN ('mass_delete', 'mass_send', 'off_hours', 'critical_refusals')),
  -- Why a refused call was refused, from the server's fixed list (#49 review;
  -- 'pending_cap' added by migration 00075, #852); NULL for every other row. A
  -- column because the per-minute caps count by it.
  reason        text CHECK (reason IN ('method', 'session_only', 'restriction', 'arguments', 'scope', 'rate', 'budget', 'approval', 'mass_delete', 'mass_send', 'pending_cap')),
  CONSTRAINT api_key_events_anomaly_named CHECK ((outcome = 'anomaly') = (anomaly IS NOT NULL))
);
CREATE INDEX api_key_events_by_key ON app.api_key_events (user_id, key_id, at);
ALTER TABLE app.api_key_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.api_key_events FORCE ROW LEVEL SECURITY;
CREATE POLICY api_key_events_isolation ON app.api_key_events
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());
GRANT SELECT, INSERT ON app.api_key_events TO mailroom_app;
GRANT UPDATE (error_type) ON app.api_key_events TO mailroom_app;

-- A destructive call held for a human (#49, migration 00074, ADR-042 §7).
-- payload is the canonical request (ids and a draft content hash, never
-- bodies), read back whole; summary is built server-side for the person
-- deciding. Expires to 'expired' after 24 hours.
CREATE TABLE app.api_key_pending (
  id            app.ulid PRIMARY KEY,
  user_id       app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  key_id        app.ulid NOT NULL REFERENCES app.api_keys(id) ON DELETE CASCADE,
  kind          text NOT NULL CHECK (kind IN ('send', 'delete')),
  method        text NOT NULL,
  payload       jsonb NOT NULL,
  summary       text NOT NULL,
  created_at    timestamptz NOT NULL DEFAULT now(),
  expires_at    timestamptz NOT NULL,
  state         text NOT NULL DEFAULT 'pending'
                  CHECK (state IN ('pending', 'approved', 'denied', 'expired', 'failed')),
  decided_at    timestamptz,
  detail        text,
  -- When an APPROVED request was admitted to run, written in the same
  -- transaction as its 'approved' audit row. An approved row without it after
  -- ten minutes never ran (the process stopped between the decision and the
  -- run) and the sweep marks it failed (ADR-042 §7).
  ran_at        timestamptz,
  CONSTRAINT api_key_pending_decided CHECK ((state = 'pending') = (decided_at IS NULL))
);
CREATE INDEX api_key_pending_by_state ON app.api_key_pending (user_id, state, expires_at);
CREATE INDEX api_key_pending_by_key ON app.api_key_pending (user_id, key_id, created_at);
ALTER TABLE app.api_key_pending ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.api_key_pending FORCE ROW LEVEL SECURITY;
CREATE POLICY api_key_pending_isolation ON app.api_key_pending
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());
GRANT SELECT, INSERT, UPDATE ON app.api_key_pending TO mailroom_app;

-- The drafts each API key wrote (#49 review, ADR-042 §3): a key may submit
-- ONLY a draft it created itself, so a send key cannot mail out what the user
-- (or another key) has been writing. Written in the transaction that creates
-- the draft; a row goes with its message or its key.
CREATE TABLE app.api_key_drafts (
  user_id       app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  message_id    app.ulid NOT NULL REFERENCES app.messages(id) ON DELETE CASCADE,
  key_id        app.ulid NOT NULL REFERENCES app.api_keys(id) ON DELETE CASCADE,
  created_at    timestamptz NOT NULL DEFAULT now(),
  -- A KEYED fingerprint of the create the key sent (#872, migration 00077):
  -- HMAC-SHA256 under a key derived from the user's mail-blob DEK, over the
  -- normalised from, to/cc/bcc, subject, threading, bodies and attachments.
  -- A key's identical draft within ten minutes is collapsed into the first
  -- (store.RecentAPIKeyDraft). Keyed because recipients and bodies are
  -- otherwise only in the encrypted blob store: a plain hash here would let
  -- anyone holding a database copy confirm a guess at a draft's content.
  -- NULL for rows written before 00077; those are never collapsed into.
  content_fingerprint bytea,
  PRIMARY KEY (user_id, message_id)
);
CREATE INDEX api_key_drafts_by_fingerprint ON app.api_key_drafts (user_id, key_id, content_fingerprint)
  WHERE content_fingerprint IS NOT NULL;
ALTER TABLE app.api_key_drafts ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.api_key_drafts FORCE ROW LEVEL SECURITY;
CREATE POLICY api_key_drafts_isolation ON app.api_key_drafts
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());
GRANT SELECT, INSERT ON app.api_key_drafts TO mailroom_app;

-- Refusals past a key's per-minute caps (#49 review): a key's refusals are
-- written in full to api_key_events up to rate_per_min routine and
-- 5 x rate_per_min security-meaningful ones a minute (store.CriticalRefusalReasons);
-- past those they are counted here, one row per (minute, outcome, method,
-- reason), so the log stays bounded and complete in aggregate. count is a
-- column because the rate limit sums it.
CREATE TABLE app.api_key_suppressed_refusals (
  user_id       app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  key_id        app.ulid NOT NULL REFERENCES app.api_keys(id) ON DELETE CASCADE,
  minute        timestamptz NOT NULL,
  outcome       text NOT NULL CHECK (outcome IN ('denied', 'rate_limited', 'over_budget')),
  -- The method name only when it is one this server has; otherwise 'unknown'.
  -- Never client text: a key could otherwise mint a row per invented name.
  method        text NOT NULL,
  reason        text NOT NULL CHECK (reason IN ('method', 'session_only', 'restriction', 'arguments', 'scope', 'rate', 'budget', 'approval', 'mass_delete', 'mass_send', 'pending_cap')),
  count         int NOT NULL CHECK (count > 0),
  PRIMARY KEY (user_id, key_id, minute, outcome, method, reason)
);
ALTER TABLE app.api_key_suppressed_refusals ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.api_key_suppressed_refusals FORCE ROW LEVEL SECURITY;
CREATE POLICY api_key_suppressed_refusals_isolation ON app.api_key_suppressed_refusals
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());
GRANT SELECT, INSERT ON app.api_key_suppressed_refusals TO mailroom_app;
GRANT UPDATE (count) ON app.api_key_suppressed_refusals TO mailroom_app;

-- Bearer authentication (#49, ADR-042 §2). The ONLY cross-user read of
-- app.api_keys: by the hash of a presented token, returning the owning user and
-- the key id, for a key neither revoked nor suspended whose user can still sign
-- in. Everything else about the key is read under the user's own UserTx.
CREATE POLICY api_keys_token_lookup ON app.api_keys
  FOR SELECT TO mailroom_owner
  USING (revoked_at IS NULL AND suspended_at IS NULL);

CREATE OR REPLACE FUNCTION app.api_key_for_token(p_hash bytea)
RETURNS TABLE (user_id app.ulid, key_id app.ulid)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = app, pg_temp AS $$
  SELECT k.user_id, k.id
    FROM app.api_keys k
    JOIN app.users u ON u.id = k.user_id
   WHERE k.token_hash = p_hash
     AND k.revoked_at IS NULL
     AND k.suspended_at IS NULL
     AND u.disabled_at IS NULL
     AND u.deleted_at IS NULL
$$;
REVOKE ALL ON FUNCTION app.api_key_for_token(bytea) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.api_key_for_token(bytea) TO mailroom_app;
ALTER FUNCTION app.api_key_for_token(bytea) OWNER TO mailroom_owner;
GRANT SELECT ON app.api_keys TO mailroom_owner;

-- The pending-approval expiry sweep (ADR-042 §7): which users have a pending
-- request past its deadline. User ids only; the expiry runs in each UserTx.
CREATE POLICY api_key_pending_due_lookup ON app.api_key_pending
  FOR SELECT TO mailroom_owner
  USING ((state = 'pending' AND expires_at <= now())
      OR (state = 'approved' AND ran_at IS NULL AND decided_at <= now() - interval '10 minutes'));

CREATE OR REPLACE FUNCTION app.api_key_pending_due_users()
RETURNS SETOF app.ulid
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = app, pg_temp AS $$
  SELECT DISTINCT p.user_id
    FROM app.api_key_pending p
   WHERE (p.state = 'pending' AND p.expires_at <= now())
      OR (p.state = 'approved' AND p.ran_at IS NULL AND p.decided_at <= now() - interval '10 minutes')
$$;
REVOKE ALL ON FUNCTION app.api_key_pending_due_users() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.api_key_pending_due_users() TO mailroom_app;
ALTER FUNCTION app.api_key_pending_due_users() OWNER TO mailroom_owner;
GRANT SELECT ON app.api_key_pending TO mailroom_owner;

CREATE TABLE app.push_subscriptions (
  id            app.ulid PRIMARY KEY,
  user_id       app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  endpoint      text NOT NULL,
  keys_p256dh   text NOT NULL,
  keys_auth     text NOT NULL,
  device_name   text NOT NULL DEFAULT '',
  -- NEVER READ, and superseded by notify_mode (#59, #626 §E). It defaulted to
  -- true without anyone choosing it, so it is not migrated into the mode; it
  -- stays only because dropping a column is a later contract step (ADR-008).
  important_only boolean NOT NULL DEFAULT true,
  created_at    timestamptz NOT NULL DEFAULT now(),
  expired_at    timestamptz,
  -- THE PER-DEVICE PUSH MODE (#59, migration 00061): 'all' (every message,
  -- the default and the behaviour before it existed), 'above_line', 'vip',
  -- 'above_line_or_vip'. internal/notify decides; the push counter
  -- (store.PushCounts) measures. Declared LAST (ADD COLUMN append order).
  notify_mode   text NOT NULL DEFAULT 'all'
    CHECK (notify_mode IN ('all', 'above_line', 'vip', 'above_line_or_vip')),
  UNIQUE (user_id, endpoint)
);
ALTER TABLE app.push_subscriptions ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.push_subscriptions FORCE ROW LEVEL SECURITY;
CREATE POLICY push_subscriptions_isolation ON app.push_subscriptions
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());

-- Append-only audit log (spec §11). Users see their own rows; admin sees
-- admin-plane rows (user_id IS NULL) through the admin role.
CREATE TABLE app.audit_log (
  id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  user_id       app.ulid REFERENCES app.users(id) ON DELETE SET NULL,
  actor         text NOT NULL,                 -- 'user'|'api_key:<id>'|'system'|'admin:<id>'
  action        text NOT NULL,                 -- 'message.delete','key.sign','conflict.resolved',...
  object_type   text,
  object_id     text,
  detail        jsonb NOT NULL DEFAULT '{}',
  created_at    timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE app.audit_log ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.audit_log FORCE ROW LEVEL SECURITY;
CREATE POLICY audit_user_read ON app.audit_log
  FOR SELECT TO mailroom_app
  USING (user_id = app.current_user_id());
CREATE POLICY audit_user_insert ON app.audit_log
  FOR INSERT TO mailroom_app
  WITH CHECK (user_id = app.current_user_id() OR user_id IS NULL);
CREATE POLICY audit_admin_read ON app.audit_log
  FOR SELECT TO mailroom_admin
  USING (user_id IS NULL);
-- The tombstone writer. app.hard_delete_user runs as mailroom_owner, and
-- FORCE ROW LEVEL SECURITY binds the owner too — without this the INSERT is
-- refused with "new row violates row-level security policy" (migration 00040,
-- #54). Unlike the users_owner_delete case this failure is LOUD, but it would
-- have fired after the files were already deleted.
CREATE POLICY audit_owner_insert ON app.audit_log
  FOR INSERT TO mailroom_owner
  WITH CHECK (true);
-- Append-only is enforced by grants below (no UPDATE/DELETE granted).

CREATE TABLE app.ai_providers (
  id            app.ulid PRIMARY KEY,
  user_id       app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  name          text NOT NULL,
  base_url      text NOT NULL,                 -- OpenAI-compatible endpoint
  api_key_ct    bytea,                         -- wrapped credential
  dek_id        app.ulid,
  model         text NOT NULL DEFAULT '',
  external      boolean NOT NULL DEFAULT false, -- triggers data-egress warning (spec §8)
  daily_token_budget bigint NOT NULL DEFAULT 0,
  enabled       boolean NOT NULL DEFAULT false,
  created_at    timestamptz NOT NULL DEFAULT now(),
  UNIQUE (user_id, name)
);
ALTER TABLE app.ai_providers ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.ai_providers FORCE ROW LEVEL SECURITY;
CREATE POLICY ai_providers_isolation ON app.ai_providers
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());

-- Long-running ingest runs: bulk import + backfill progress (spec §4).
CREATE TABLE app.sync_jobs (
  id            app.ulid PRIMARY KEY,
  user_id       app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  account_id    app.ulid REFERENCES app.accounts(id) ON DELETE CASCADE,
  kind          text NOT NULL,                 -- 'bulk_import','backfill','reindex'
  source        text,                          -- 'mbox'|'maildir'|'takeout'|NULL
  state         app.job_state NOT NULL DEFAULT 'pending',
  progress_done bigint NOT NULL DEFAULT 0,
  progress_total bigint,
  cursor        jsonb NOT NULL DEFAULT '{}',   -- resumability (spec §4)
  last_error    text,
  created_at    timestamptz NOT NULL DEFAULT now(),
  completed_at  timestamptz
);
ALTER TABLE app.sync_jobs ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.sync_jobs FORCE ROW LEVEL SECURITY;
CREATE POLICY sync_jobs_isolation ON app.sync_jobs
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());

-- ----------------------------------------------------------------------------
-- 12. Areas, and desk/client preferences (migration 00046, #442 increment 1)
-- ----------------------------------------------------------------------------
-- The desk rebuild's persistent objects: every message resolves to exactly one
-- AREA (fed by tabs and/or labels, "areas are labels" — placing a message in
-- an area applies its own label), plus the two settings surfaces the desk
-- needs to agree across a person's own devices. See migration 00046 for why
-- these are server-side state rather than client-local preferences, why feeds
-- are rows rather than JSONB, and why the area owns its label outright.
CREATE TABLE app.areas (
  id                app.ulid PRIMARY KEY,
  user_id           app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  name              text NOT NULL,
  position          int NOT NULL DEFAULT 0,
  -- The area's own label: placing a message in the area applies it. ON DELETE
  -- RESTRICT, not CASCADE — an area with no label could not receive a message
  -- at all, so delete the area instead of the label out from under it.
  label_id          app.ulid NOT NULL REFERENCES app.labels(id) ON DELETE RESTRICT,
  -- The dwell period in days before the drawer half of this area's mail is
  -- swept, once auto_archive (below) is true. NULL = inherit
  -- desk_preferences.drawer_default_days (migration 00048) rather than
  -- "never" — auto_archive is the on/off switch; this is only the period.
  auto_archive_days int CHECK (auto_archive_days IS NULL OR auto_archive_days > 0),
  hidden_from_rail  boolean NOT NULL DEFAULT false,
  created_at        timestamptz NOT NULL DEFAULT now(),
  updated_at        timestamptz NOT NULL DEFAULT now(),
  UNIQUE (user_id, label_id),
  -- Whether this drawer auto-archives at all (migration 00048, #443), default
  -- ON. Declared LAST to match ALTER TABLE ... ADD COLUMN's append order —
  -- migrate-check diffs the two schemas column-for-column, order included.
  auto_archive      boolean NOT NULL DEFAULT true
);
CREATE UNIQUE INDEX areas_user_lower_name ON app.areas (user_id, lower(name));
ALTER TABLE app.areas ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.areas FORCE ROW LEVEL SECURITY;
CREATE POLICY areas_isolation ON app.areas
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());

-- One row per feed (a tab or a label, never both/neither); "at most one area"
-- is enforced by the two partial unique indexes below, not by application
-- code. A feed is part of the Area object for JMAP purposes: the store appends
-- the Area change_log row for the OWNING area when a feed changes, never a row
-- of its own — app.object_type carries no 'AreaFeed' value.
CREATE TABLE app.area_feeds (
  id         app.ulid PRIMARY KEY,
  user_id    app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  area_id    app.ulid NOT NULL REFERENCES app.areas(id) ON DELETE CASCADE,
  tab        app.category_tab,
  label_id   app.ulid REFERENCES app.labels(id) ON DELETE CASCADE,
  created_at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT area_feeds_exactly_one CHECK ((tab IS NULL) <> (label_id IS NULL))
);
CREATE UNIQUE INDEX area_feeds_tab_once   ON app.area_feeds (user_id, tab)      WHERE tab IS NOT NULL;
CREATE UNIQUE INDEX area_feeds_label_once ON app.area_feeds (user_id, label_id) WHERE label_id IS NOT NULL;
CREATE INDEX area_feeds_area ON app.area_feeds (user_id, area_id);
ALTER TABLE app.area_feeds ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.area_feeds FORCE ROW LEVEL SECURITY;
CREATE POLICY area_feeds_isolation ON app.area_feeds
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());

-- A singleton per user (0.5 has exactly one desk): the change_log object_id
-- for it is the user_id itself rather than a manufactured row id.
CREATE TABLE app.desk_preferences (
  user_id             app.ulid PRIMARY KEY REFERENCES app.users(id) ON DELETE CASCADE,
  line_threshold      real NOT NULL DEFAULT 0.5 CHECK (line_threshold >= 0 AND line_threshold <= 1),
  drawer_default_days int  NOT NULL DEFAULT 30 CHECK (drawer_default_days > 0),
  tray_nag_days       int  NOT NULL DEFAULT 7  CHECK (tray_nag_days > 0),
  updated_at          timestamptz NOT NULL DEFAULT now(),
  -- The drawer sweep's login-level MASTER SWITCH (migration 00048, #443),
  -- default OFF: the sweep is seeded and self-rescheduling from boot and
  -- areas default to auto_archive = true, so an unarmed switch is what keeps
  -- mail from moving before the operator has read `mailroom-admin
  -- drawer-report` (CLAUDE.md "build the report before the fix"). Turning it
  -- on is a deliberate act, still available today via `drawer-report
  -- --enable`; day-one setup's final step (#446,
  -- web/src/app/ui/setup/SetupDialsStep.tsx) now ALSO sets it, true or false,
  -- from the period the user chose there.
  -- Declared LAST to match ALTER TABLE ... ADD COLUMN's append order —
  -- migrate-check diffs the two schemas column-for-column, order included.
  drawer_sweep_enabled boolean NOT NULL DEFAULT false,
  -- DAY-ONE SETUP (#446, migration 00050): NULL means "this login has not been
  -- through day-one; the desk opens to setup". Migration 00050 backfills every
  -- pre-existing row to its updated_at, INLINE (one row per login — this table
  -- is a singleton), so no established desk is ever interrupted by a wizard.
  -- Declared LAST to match ADD COLUMN append order; migrate-check diffs the
  -- two schemas column-for-column, order included.
  setup_completed_at timestamptz,
  -- LEARNED PLACEMENT's off switch (#714, migration 00060, ADR-032), enforced
  -- by the store at ingest, not only by the client. Default ON: the operator
  -- chose for it to act immediately. Declared LAST (ADD COLUMN append order).
  learned_placement_enabled boolean NOT NULL DEFAULT true,
  -- QUIET HOURS (#59, migration 00061), off by default. start/end are minutes
  -- after LOCAL midnight in quiet_hours_timezone (an IANA name the API
  -- validates); days is a bitmask of the weekday a quiet period STARTS on,
  -- bit 0 = Sunday. During quiet hours only VIP pushes go through, and only
  -- while allow_vip is on (default on). internal/notify owns the arithmetic.
  quiet_hours_enabled   boolean  NOT NULL DEFAULT false,
  quiet_hours_start     smallint NOT NULL DEFAULT 1320 CHECK (quiet_hours_start >= 0 AND quiet_hours_start < 1440),
  quiet_hours_end       smallint NOT NULL DEFAULT 420 CHECK (quiet_hours_end >= 0 AND quiet_hours_end < 1440),
  quiet_hours_days      smallint NOT NULL DEFAULT 127 CHECK (quiet_hours_days >= 0 AND quiet_hours_days <= 127),
  quiet_hours_timezone  text     NOT NULL DEFAULT 'UTC',
  quiet_hours_allow_vip boolean  NOT NULL DEFAULT true,
  -- FOLLOW-UP NUDGES (#58, migration 00064). "No one answered your mail" is on
  -- by default; "you haven't replied to this" is opt-in (its measured
  -- precision is low), after nudge_inbound_days. internal/nudge owns the rules.
  -- Declared LAST (ADD COLUMN append order; migrate-check).
  nudge_outbound_enabled boolean  NOT NULL DEFAULT true,
  nudge_inbound_enabled  boolean  NOT NULL DEFAULT false,
  nudge_inbound_days     smallint NOT NULL DEFAULT 3 CHECK (nudge_inbound_days >= 1 AND nudge_inbound_days <= 14)
);
ALTER TABLE app.desk_preferences ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.desk_preferences FORCE ROW LEVEL SECURITY;
CREATE POLICY desk_preferences_isolation ON app.desk_preferences
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());

-- NOT a singleton like desk_preferences: a phone and a browser legitimately
-- want different density and pane defaults, so the row is keyed on which KIND
-- of client asked, not on the user alone.
CREATE TABLE app.client_preferences (
  id             app.ulid PRIMARY KEY,
  user_id        app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  family         app.client_family NOT NULL,
  theme          text NOT NULL DEFAULT 'system' CHECK (theme IN ('system', 'light', 'dark')),
  density        text NOT NULL DEFAULT 'normal' CHECK (density IN ('compact', 'normal', 'relaxed')),
  message_opens  text NOT NULL DEFAULT 'beside' CHECK (message_opens IN ('beside', 'focused')),
  pane_split_pct int  NOT NULL DEFAULT 58 CHECK (pane_split_pct BETWEEN 20 AND 80),
  updated_at     timestamptz NOT NULL DEFAULT now(),
  -- READING COMFORT (#514, migration 00052). Two display preferences that
  -- answer one complaint — "no matter what we do, we're cutting off the
  -- subject with the first line" — and belong to a CLIENT FAMILY for the same
  -- reason density does: three lines per row is right on a phone and wrong on
  -- a wide monitor.
  --
  -- Both DEFAULT TO TODAY'S BEHAVIOUR ('m' is the current 14px root size, the
  -- middle of five steps; 'oneLine' is the single-line row), so applying the
  -- migration changes nothing anybody is looking at. That is also why 00052
  -- carries no DML at all: NOT NULL + DEFAULT fills every existing row from
  -- the ALTER itself, and a backfill UPDATE here would have had to be wrapped
  -- in NO FORCE/FORCE (docs/migrations.md, #505) to match a single row.
  --
  -- Declared LAST to match ADD COLUMN append order; migrate-check diffs the
  -- two schemas column-for-column, order included.
  text_size      text NOT NULL DEFAULT 'm' CHECK (text_size IN ('xs', 's', 'm', 'l', 'xl')),
  row_layout     text NOT NULL DEFAULT 'oneLine' CHECK (row_layout IN ('oneLine', 'multiLine')),
  UNIQUE (user_id, family)
);
ALTER TABLE app.client_preferences ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.client_preferences FORCE ROW LEVEL SECURITY;
CREATE POLICY client_preferences_isolation ON app.client_preferences
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());

-- The drawer auto-archive job's audit table and idempotency guard (migration
-- 00048, mailroom#443): a row here means "a drawer sweep archived this
-- message", once, forever — the guard that keeps a message the user moves
-- back to the inbox from being swept a second time once it ages past the
-- period again. NOT a JMAP-visible object (app.object_type carries no
-- 'DrawerArchive' value); it surfaces only as Email.drawerArchivedAt and
-- Area/getSummary's archivedByDrawer count. SET NULL on area_id, not CASCADE:
-- deleting an area must not erase the record that its drawer archived this
-- message, so area_name is carried alongside to keep the history legible
-- after the area itself is gone.
CREATE TABLE app.drawer_archives (
  user_id      app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  message_id   app.ulid NOT NULL REFERENCES app.messages(id) ON DELETE CASCADE,
  area_id      app.ulid REFERENCES app.areas(id) ON DELETE SET NULL,
  area_name    text NOT NULL,
  period_days  int  NOT NULL CHECK (period_days > 0),
  archived_at  timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, message_id)
);
CREATE INDEX drawer_archives_by_area ON app.drawer_archives (user_id, area_id, archived_at);
ALTER TABLE app.drawer_archives ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.drawer_archives FORCE ROW LEVEL SECURITY;
CREATE POLICY drawer_archives_isolation ON app.drawer_archives
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());

-- DAY-ONE SETUP RUNS (#446, migration 00050): one row per bulk import of a
-- folder-sorted mailbox onto the desk — label mail older than the window,
-- archive it IN MAILROOM (nothing moves on the server), optionally bring a
-- folder to the desk. A JMAP-VISIBLE OBJECT (app.object_type carries
-- 'SetupRun'): the client polls status and counters while the chunked job
-- drains, so both have to reach other devices through the change log.
--
-- PLANNED beside ACTUAL, as separate columns: "we said N and wrote N"
-- is the only way the preview promise and the run outcome can be compared, and
-- one set of columns cannot say both. left_for_later/unclassified are
-- planned-side only — they describe mail the run deliberately did not touch.
--
-- `plan` is JSONB and that is allowed: nothing queries it by field (CLAUDE.md
-- forbids JSONB for QUERYABLE state). It is the operator per-folder choices,
-- read back whole; every queryable fact is a column.
CREATE TABLE app.setup_runs (
  id             app.ulid PRIMARY KEY,
  user_id        app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  kind           text NOT NULL CHECK (kind IN ('import')),
  -- 'undoing' is a state, not a flag: the SAME job kind drains both directions
  -- and reads this column to decide which way it is going (internal/setup).
  status         text NOT NULL CHECK (status IN ('queued','running','done','failed','undoing','undone')),
  window_days    int NOT NULL CHECK (window_days > 0),
  plan           jsonb NOT NULL,
  planned_labels   int NOT NULL DEFAULT 0,
  labels_written   int NOT NULL DEFAULT 0,
  planned_archived int NOT NULL DEFAULT 0,
  archived         int NOT NULL DEFAULT 0,
  planned_moves    int NOT NULL DEFAULT 0,
  moved            int NOT NULL DEFAULT 0,
  left_for_later   int NOT NULL DEFAULT 0,
  -- Mail the census could not classify (no From address). COUNTED, NEVER
  -- HIDDEN (CLAUDE.md: every report states its denominator).
  unclassified     int NOT NULL DEFAULT 0,
  error          text,
  created_at     timestamptz NOT NULL DEFAULT now(),
  started_at     timestamptz,
  finished_at    timestamptz,
  undone_at      timestamptz
);
CREATE INDEX setup_runs_user_created ON app.setup_runs (user_id, created_at);
ALTER TABLE app.setup_runs ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.setup_runs FORCE ROW LEVEL SECURITY;
CREATE POLICY setup_runs_isolation ON app.setup_runs
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());

-- THE UNDO JOURNAL (#446, migration 00050). Step 2 of day-one is "undoable as
-- a whole", and that is a promise no counter can keep: to reverse the run you
-- have to know PER MESSAGE what it actually did — whether the label was
-- already there (removing it would take away the user own filing), whether the
-- message was already archived in Mailroom, and which folder it came out of.
-- Deriving the inverse from the plan cannot tell "this run wrote it" from "it
-- was already like that", so undo would destroy state the run never created.
--
-- ⚠ A ROW IS WRITTEN EVEN WHEN NOTHING CHANGED. The chunk cursor IS "has this
-- message a journal row for this run yet", so a message the run examined and
-- found already labelled and already archived must still get a row, or every
-- subsequent chunk would select it again and the job would never finish.
--
-- NOT a JMAP-visible object of its own: no 'SetupJournal' object type and no
-- change_log row. What a client syncs is the SetupRun and the Emails touched.
CREATE TABLE app.setup_journal (
  id                    app.ulid PRIMARY KEY,
  user_id               app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  run_id                app.ulid NOT NULL REFERENCES app.setup_runs(id) ON DELETE CASCADE,
  message_id            app.ulid NOT NULL REFERENCES app.messages(id) ON DELETE CASCADE,
  -- The label THIS RUN inserted, NULL when the message already carried it (or
  -- wanted none). Undo removes only a label named here, and only while it is
  -- still attributed to the setup.
  label_id              app.ulid REFERENCES app.labels(id) ON DELETE CASCADE,
  desk_archived         boolean NOT NULL DEFAULT false,
  -- SET NULL, not CASCADE: losing the folder must not delete the record that
  -- the run moved this message (drawer_archives.area_id reasoning) — undo then
  -- simply cannot move it back, which is the honest outcome.
  moved_from_mailbox_id app.ulid REFERENCES app.mailboxes(id) ON DELETE SET NULL,
  created_at            timestamptz NOT NULL DEFAULT now()
);
-- ⚠ "ONE ROW PER MESSAGE PER RUN" IS ENFORCED HERE, NOT TRUSTED TO THE CHUNK
-- QUERY. The chunk query skips journaled messages, so in a single drainer the
-- constraint can never fire — but a run can legitimately have TWO drainers at
-- once: `mailroom-admin day-one --apply` drives chunks inline while the job it
-- also enqueued is sitting in the dispatcher. Two concurrent chunks can both
-- select a message whose journal row neither has committed yet, and the loser
-- would otherwise write a second row and double-count its own counters. The
-- unique index makes the loser's whole chunk abort and roll back — including the
-- counter increments — and its retry then finds the row and skips the message.
-- Fail loud and retry, rather than a run whose journal quietly says it touched
-- more mail than it did. Leading with (user_id, run_id) it also serves the chunk
-- cursor's own lookup, so it replaces a plain index rather than adding to one.
CREATE UNIQUE INDEX setup_journal_user_run_message
  ON app.setup_journal (user_id, run_id, message_id);
ALTER TABLE app.setup_journal ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.setup_journal FORCE ROW LEVEL SECURITY;
CREATE POLICY setup_journal_isolation ON app.setup_journal
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());

-- LEARNED PLACEMENT'S LOG (#714, migration 00060, ADR-032): one row per
-- message learned placement filed at ingest, written in that ingest's
-- transaction. The correction gate counts kept vs corrected per area from it;
-- a correction blocks the key (key_kind, key_value) that decided. Not a JMAP
-- object, so it owns no change_log rows (the label itself rides the ingest's
-- Email `created` row). See the migration for each column's meaning.
CREATE TABLE app.learned_placements (
  user_id      app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  message_id   app.ulid NOT NULL REFERENCES app.messages(id) ON DELETE CASCADE,
  key_kind     text NOT NULL CHECK (key_kind IN ('address', 'domain')),
  key_value    text NOT NULL,
  area_id      app.ulid NOT NULL REFERENCES app.areas(id) ON DELETE CASCADE,
  decided_at   timestamptz NOT NULL DEFAULT now(),
  -- 'withdrawn' (migration 00076, #49): an API key took the learned label
  -- off before anyone judged the placement; neither kept nor corrected.
  outcome      text NOT NULL DEFAULT 'open' CHECK (outcome IN ('open', 'kept', 'corrected', 'withdrawn')),
  kept_at      timestamptz,
  kept_by      text CHECK (kept_by IN ('age', 'archive')),
  corrected_at timestamptz,
  -- Migration 00076: LAST, in ADD COLUMN order (migrate-check compares order).
  withdrawn_at timestamptz,
  PRIMARY KEY (user_id, message_id),
  CONSTRAINT learned_placements_kept_stamped
    CHECK ((outcome = 'kept') = (kept_at IS NOT NULL AND kept_by IS NOT NULL)),
  CONSTRAINT learned_placements_corrected_stamped
    CHECK ((outcome = 'corrected') = (corrected_at IS NOT NULL)),
  CONSTRAINT learned_placements_withdrawn_stamped
    CHECK ((outcome = 'withdrawn') = (withdrawn_at IS NOT NULL))
);
CREATE INDEX learned_placements_by_area ON app.learned_placements (user_id, area_id, outcome);
CREATE INDEX learned_placements_corrections ON app.learned_placements (user_id, key_kind, key_value)
  WHERE outcome = 'corrected';
CREATE INDEX learned_placements_open ON app.learned_placements (user_id, decided_at)
  WHERE outcome = 'open';
ALTER TABLE app.learned_placements ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.learned_placements FORCE ROW LEVEL SECURITY;
CREATE POLICY learned_placements_isolation ON app.learned_placements
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());

-- The operator's re-enable for an area whose correction gate stopped it
-- (#714): the gate counts only placements decided after reset_at.
CREATE TABLE app.learned_placement_gate_resets (
  user_id  app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  area_id  app.ulid NOT NULL REFERENCES app.areas(id) ON DELETE CASCADE,
  reset_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, area_id)
);
ALTER TABLE app.learned_placement_gate_resets ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.learned_placement_gate_resets FORCE ROW LEVEL SECURITY;
CREATE POLICY learned_placement_gate_resets_isolation ON app.learned_placement_gate_resets
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());

-- CALENDAR INVITATIONS (#725, migration 00063, ADR-033). An INDEX of the
-- invitation and cancellation messages by event UID, written by ingest in the
-- message's transaction, for the one question one message's bytes cannot
-- answer: is there a newer version, or a cancellation? Matched only on
-- (uid, recurrence_id, organizer) AND only a `verified` message (authenticated
-- as the organiser) changes another message's card, so a stranger's CANCEL for
-- a UID they learned does not cancel it. Not a JMAP object of its own: the card is
-- Email.calendarInvite, and the message's Email change rows cover it. See the
-- migration for each column.
CREATE TABLE app.calendar_invites (
  user_id       app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  message_id    app.ulid NOT NULL REFERENCES app.messages(id) ON DELETE CASCADE,
  uid           text NOT NULL CHECK (length(uid) BETWEEN 1 AND 512),
  recurrence_id text NOT NULL DEFAULT '' CHECK (length(recurrence_id) <= 64),
  sequence      integer NOT NULL CHECK (sequence >= 0),
  method        text NOT NULL CHECK (method IN ('REQUEST', 'CANCEL')),
  organizer     citext NOT NULL CHECK (length(organizer) BETWEEN 3 AND 320),
  verified      boolean NOT NULL DEFAULT false,
  recorded_at   timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, message_id)
);
CREATE INDEX calendar_invites_by_uid ON app.calendar_invites (user_id, uid);
ALTER TABLE app.calendar_invites ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.calendar_invites FORCE ROW LEVEL SECURITY;
CREATE POLICY calendar_invites_isolation ON app.calendar_invites
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());

-- The user's own answers to invitations (#725, migration 00063): one row per
-- Accept / Maybe / Decline sent, bound to the EmailSubmission carrying the
-- iTIP REPLY. The current answer is the newest row whose submission was not
-- cancelled, so Undo Send takes it back. Never updated.
CREATE TABLE app.calendar_responses (
  id            app.ulid PRIMARY KEY,
  user_id       app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  message_id    app.ulid REFERENCES app.messages(id) ON DELETE SET NULL,
  uid           text NOT NULL CHECK (length(uid) BETWEEN 1 AND 512),
  recurrence_id text NOT NULL DEFAULT '' CHECK (length(recurrence_id) <= 64),
  organizer     citext NOT NULL CHECK (length(organizer) BETWEEN 3 AND 320),
  sequence      integer NOT NULL CHECK (sequence >= 0),
  partstat      text NOT NULL CHECK (partstat IN ('ACCEPTED', 'TENTATIVE', 'DECLINED')),
  submission_id app.ulid REFERENCES app.submissions(id) ON DELETE SET NULL,
  responded_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX calendar_responses_by_uid ON app.calendar_responses (user_id, uid, responded_at);
CREATE INDEX calendar_responses_by_submission ON app.calendar_responses (user_id, submission_id)
  WHERE submission_id IS NOT NULL;
ALTER TABLE app.calendar_responses ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.calendar_responses FORCE ROW LEVEL SECURITY;
CREATE POLICY calendar_responses_isolation ON app.calendar_responses
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());

-- FOLLOW-UP NUDGE DISMISSALS (#58, migration 00064): the user's "Done" on a
-- nudge. Nudges are computed on read (FollowUp/get), never stored; this is the
-- one fact the computation cannot derive. A dismissed message never resurfaces
-- as a nudge of either kind.
CREATE TABLE app.nudge_dismissals (
  user_id      app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  message_id   app.ulid NOT NULL REFERENCES app.messages(id) ON DELETE CASCADE,
  kind         text NOT NULL CHECK (kind IN ('outbound', 'inbound')),
  dismissed_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, message_id)
);
ALTER TABLE app.nudge_dismissals ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.nudge_dismissals FORCE ROW LEVEL SECURITY;
CREATE POLICY nudge_dismissals_isolation ON app.nudge_dismissals
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());

-- OUT OF OFFICE (#50, migration 00065): the vacation responder Mailroom writes
-- to the provider as a ManageSieve script (internal/outofoffice). One row per
-- mail account; JMAP-visible as 'OutOfOffice' with the account id as its id.
-- enabled is true only once the server accepted and activated the script;
-- installed_sha256 is the exact script stored, so drift can be seen;
-- server_state/server_detail/checked_at record the last look at the server;
-- contacts_included/contacts_total state the bound on a contacts-only list.
-- See the migration for the full commentary.
CREATE TABLE app.out_of_office (
  user_id           app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  account_id        app.ulid NOT NULL UNIQUE REFERENCES app.accounts(id) ON DELETE CASCADE,
  enabled           boolean NOT NULL DEFAULT false,
  from_at           timestamptz,
  to_at             timestamptz,
  subject           text NOT NULL DEFAULT '',
  body              text NOT NULL DEFAULT '',
  reply_days        smallint NOT NULL DEFAULT 7 CHECK (reply_days >= 1 AND reply_days <= 30),
  from_address      text NOT NULL DEFAULT '',
  contacts_only     boolean NOT NULL DEFAULT false,
  installed_sha256  bytea CHECK (installed_sha256 IS NULL OR length(installed_sha256) = 32),
  server_state      text NOT NULL DEFAULT 'off'
                    CHECK (server_state IN ('off', 'active', 'displaced', 'missing', 'modified', 'error')),
  server_detail     text NOT NULL DEFAULT '',
  checked_at        timestamptz,
  contacts_included int,
  contacts_total    int,
  updated_at        timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, account_id),
  CONSTRAINT out_of_office_window CHECK (from_at IS NULL OR to_at IS NULL OR from_at < to_at)
);
ALTER TABLE app.out_of_office ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.out_of_office FORCE ROW LEVEL SECURITY;
CREATE POLICY out_of_office_isolation ON app.out_of_office
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());

-- LOCAL DKIM AT INGEST (#776, ADR-036, migration 00066): what Mailroom's own
-- DKIM check found on a message that arrived with no trusted
-- Authentication-Results. Written once, in the ingest transaction (so it rides
-- the message's Email 'created' change_log row); never updated. Only an aligned
-- pass is displayed. A transient DNS failure writes no row ("not checked").
CREATE TABLE app.message_local_auth (
  message_id app.ulid NOT NULL REFERENCES app.messages(id) ON DELETE CASCADE,
  user_id    app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  method     text NOT NULL CHECK (method = 'dkim'),
  result     text NOT NULL CHECK (result IN ('pass', 'fail_body', 'fail_signature',
               'key_not_found', 'key_revoked', 'permerror', 'unsupported')),
  domain     text NOT NULL DEFAULT '',
  aligned    boolean NOT NULL,
  display_blocked text NOT NULL DEFAULT '',
  checked_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (message_id, method)
);
ALTER TABLE app.message_local_auth ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.message_local_auth FORCE ROW LEVEL SECURITY;
CREATE POLICY message_local_auth_isolation ON app.message_local_auth
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());

-- THE PERSISTENT DECRYPT TRAIL (#47 increment 6b, ADR-037, migration 00067).
-- One row per crypto-process `decrypt` call, written BY THE CRYPTO PROCESS
-- before it returns plaintext (an application-written row is one a compromised
-- application would skip). Append-only for mailroom_app (section 13). No
-- content of any kind. message_id has no foreign key: the trail outlives the
-- message.
CREATE TABLE app.pgp_decrypt_audit (
  id              app.ulid PRIMARY KEY,
  user_id         app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  message_id      app.ulid NOT NULL,
  purpose         text NOT NULL CHECK (purpose IN ('body', 'attachment')),
  outcome         text NOT NULL CHECK (outcome IN ('decrypted', 'failed')),
  key_fingerprint text,
  decrypted_at    timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT pgp_decrypt_audit_key_iff_decrypted
    CHECK ((outcome = 'decrypted') = (key_fingerprint IS NOT NULL))
);
CREATE INDEX pgp_decrypt_audit_user_time ON app.pgp_decrypt_audit (user_id, decrypted_at);
ALTER TABLE app.pgp_decrypt_audit ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.pgp_decrypt_audit FORCE ROW LEVEL SECURITY;
CREATE POLICY pgp_decrypt_audit_isolation ON app.pgp_decrypt_audit
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());

-- MAIL-PROVIDER OAUTH (#52, ADR-046, migration 00078). The QUERYABLE half of an
-- OAuth account's grant, 1:1 with the account: provider, subject, granted
-- scopes, state. The refresh token itself is in accounts.credentials_ct (sealed,
-- ADR-013); access tokens are never stored. `generation` is the compare-and-swap
-- counter for refresh-token rotation (the store bumps it WHERE generation = the
-- value it read). Rotation appends NO change_log row; a state transition appends
-- an Account row (ADR-046 §4).
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

-- A pending OAuth connect (#52, ADR-046, migration 00078): single-use (consumed
-- with DELETE ... RETURNING), bound to the user who started it, short-lived.
-- The OAuth `state` is kept only as its SHA-256 and the PKCE verifier only
-- sealed. Never updated.
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

-- Server-side browser sessions (#937, ADR-048, migration 00082). One row per
-- signed-in browser; the cookie's secret is kept only as its SHA-256, the
-- refresh and ID tokens only sealed (key derived per user from the master key,
-- so there is no per-row DEK). No change_log rows: not a JMAP-visible object.
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
REVOKE ALL ON FUNCTION app.session_for_token(bytea) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.session_for_token(bytea) TO mailroom_app;
ALTER FUNCTION app.session_for_token(bytea) OWNER TO mailroom_owner;
GRANT SELECT ON app.sessions TO mailroom_owner;

-- Signed-in devices and the session sweep (#937, ADR-048 sections 6-7, migration 00083).
ALTER TABLE app.push_subscriptions
  ADD COLUMN session_id app.ulid REFERENCES app.sessions(id) ON DELETE SET NULL;
CREATE INDEX push_subscriptions_user_session ON app.push_subscriptions (user_id, session_id);

-- What the sweep may delete: a row revoked more than 7 days ago, or a live row
-- unseen for more than 45 days (past the provider's 30-day limit, with margin).
-- The SELECT policy is the one that makes the DELETE work at all.
CREATE POLICY sessions_sweep_select ON app.sessions
  FOR SELECT TO mailroom_owner
  USING ((revoked_at IS NOT NULL AND revoked_at < now() - interval '7 days')
      OR (revoked_at IS NULL AND last_seen_at < now() - interval '45 days'));
CREATE POLICY sessions_sweep_delete ON app.sessions
  FOR DELETE TO mailroom_owner
  USING ((revoked_at IS NOT NULL AND revoked_at < now() - interval '7 days')
      OR (revoked_at IS NULL AND last_seen_at < now() - interval '45 days'));
GRANT DELETE ON app.sessions TO mailroom_owner;

-- The sweep also deletes the push subscriptions of the sessions it deletes (a
-- subscription must never outlive its session as an UNBOUND one that keeps
-- receiving pushes, which carry text derived from mail). That needs owner
-- policies on app.push_subscriptions, confined to rows whose session the sweep
-- may delete; the SELECT policy is again what lets the DELETE see any row.
CREATE POLICY push_subscriptions_sweep_select ON app.push_subscriptions
  FOR SELECT TO mailroom_owner
  USING (session_id IS NOT NULL AND EXISTS (
    SELECT 1 FROM app.sessions s
     WHERE s.id = push_subscriptions.session_id
       AND ((s.revoked_at IS NOT NULL AND s.revoked_at < now() - interval '7 days')
         OR (s.revoked_at IS NULL AND s.last_seen_at < now() - interval '45 days'))));
CREATE POLICY push_subscriptions_sweep_delete ON app.push_subscriptions
  FOR DELETE TO mailroom_owner
  USING (session_id IS NOT NULL AND EXISTS (
    SELECT 1 FROM app.sessions s
     WHERE s.id = push_subscriptions.session_id
       AND ((s.revoked_at IS NOT NULL AND s.revoked_at < now() - interval '7 days')
         OR (s.revoked_at IS NULL AND s.last_seen_at < now() - interval '45 days'))));

-- Returns one row per reason, ZERO INCLUDED: a run that deleted nothing must be
-- distinguishable from a report that cannot see a reason at all (CLAUDE.md,
-- per-signal counts). 'unseen' is a live row nobody used for 45 days.
CREATE OR REPLACE FUNCTION app.sweep_sessions(p_now timestamptz)
RETURNS TABLE (reason text, deleted bigint)
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = app, pg_temp AS $$
DECLARE
  v_push_unseen  bigint;
  v_push_revoked bigint;
BEGIN
  -- Push subscriptions first, while their sessions still exist: counted apart,
  -- so "sessions swept" and "subscriptions swept" are never one number.
  -- push_unseen: the session was live and unseen for 45 days. push_revoked: a
  -- leftover of a session revoked before sign-out deleted its subscriptions.
  WITH gone AS (
    DELETE FROM app.push_subscriptions p USING app.sessions s
     WHERE p.session_id = s.id
       AND ((s.revoked_at IS NOT NULL AND s.revoked_at < p_now - interval '7 days')
         OR (s.revoked_at IS NULL AND s.last_seen_at < p_now - interval '45 days'))
    RETURNING (s.revoked_at IS NULL) AS unseen
  )
  SELECT count(*) FILTER (WHERE unseen), count(*) FILTER (WHERE NOT unseen)
    INTO v_push_unseen, v_push_revoked FROM gone;

  RETURN QUERY
  WITH gone AS (
    DELETE FROM app.sessions s
     WHERE (s.revoked_at IS NOT NULL AND s.revoked_at < p_now - interval '7 days')
        OR (s.revoked_at IS NULL AND s.last_seen_at < p_now - interval '45 days')
    RETURNING CASE WHEN s.revoked_at IS NULL THEN 'unseen' ELSE s.revoked_reason END AS why
  )
  SELECT k.why, count(g.why)::bigint
    FROM (VALUES ('signed_out'), ('signed_out_elsewhere'), ('replaced'),
                 ('refused'), ('expired'), ('unseen')) AS k(why)
    LEFT JOIN gone g ON g.why = k.why
   GROUP BY k.why
  UNION ALL SELECT 'push_unseen', v_push_unseen
  UNION ALL SELECT 'push_revoked', v_push_revoked
  ORDER BY 1;
END
$$;
REVOKE ALL ON FUNCTION app.sweep_sessions(timestamptz) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.sweep_sessions(timestamptz) TO mailroom_app;
ALTER FUNCTION app.sweep_sessions(timestamptz) OWNER TO mailroom_owner;

-- ----------------------------------------------------------------------------
-- 13. Grants
-- ----------------------------------------------------------------------------
GRANT USAGE ON SCHEMA app TO mailroom_app, mailroom_admin;

-- App plane: CRUD on user-scoped tables, RLS-constrained. No TRUNCATE.
GRANT SELECT, INSERT, UPDATE, DELETE ON
  app.change_seq, app.change_log, app.change_floor,
  app.accounts, app.identities, app.mailboxes,
  app.blobs, app.messages, app.threads, app.message_provenance, app.attachments,
  app.labels, app.message_labels, app.label_keyword_map, app.category_tabs,
  app.writeback_intents, app.jobs,
  app.submissions, app.outbound_ledger, app.dsn_events, app.account_sync_status,
  app.triage_scores, app.feedback_events, app.ml_models, app.rules,
  app.label_suggestions,
  app.contacts, app.contact_addresses, app.contact_keys,
  -- (app.category_dismissals is granted separately below: it takes no UPDATE.)
  app.rule_corrections, app.remote_allows, app.compose_snippets, app.saved_searches,
  app.user_keys, app.api_keys, app.push_subscriptions,
  app.ai_providers, app.sync_jobs,
  app.areas, app.area_feeds, app.desk_preferences, app.client_preferences,
  app.drawer_archives, app.setup_runs, app.setup_journal,
  app.learned_placements, app.learned_placement_gate_resets,
  app.out_of_office
TO mailroom_app;

-- Category dismissals (#176): a tombstone is written once and removed, never
-- edited — there is nothing about "this does not need a category" to update.
GRANT SELECT, INSERT, DELETE ON app.category_dismissals TO mailroom_app;

-- Superseded content hashes (#260): written once, removed, never edited — so
-- no UPDATE, same as the dismissals above.
GRANT SELECT, INSERT, DELETE ON app.superseded_content TO mailroom_app;

-- Follow-up nudge dismissals (#58): a "Done" is written once and goes only
-- with its message — no UPDATE, like the category dismissals above.
GRANT SELECT, INSERT, DELETE ON app.nudge_dismissals TO mailroom_app;

-- Local DKIM verdicts (#776): written once with the message, gone with it; no UPDATE.
GRANT SELECT, INSERT, DELETE ON app.message_local_auth TO mailroom_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON app.proxy_image_cache TO mailroom_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON app.proxy_image_refs TO mailroom_app;

-- Quarantine releases (#396): made once and revocable, never edited — so no
-- UPDATE, the same shape as the two tombstone tables above.
GRANT SELECT, INSERT, DELETE ON app.attachment_releases TO mailroom_app;

-- Calendar invitations (#725): the index is written by ingest and may be
-- replaced, never edited; the answers are appended, never edited or removed.
GRANT SELECT, INSERT, DELETE ON app.calendar_invites TO mailroom_app;
GRANT SELECT, INSERT ON app.calendar_responses TO mailroom_app;

-- Users table: app reads self (RLS); INSERT/UPDATE of users is admin-plane.
GRANT SELECT ON app.users TO mailroom_app;
GRANT SELECT, INSERT, UPDATE ON app.users TO mailroom_admin;
-- (no DELETE grant even to admin: user removal is a soft-delete + purge job)

-- Audit: append + read only. Nobody gets UPDATE/DELETE.
GRANT SELECT, INSERT ON app.audit_log TO mailroom_app;
GRANT SELECT ON app.audit_log TO mailroom_admin;

-- Decrypt trail (#47, migration 00067): append-only, like the audit grant
-- above. A compromised application shares this role and must not be able to
-- erase a decryption.
GRANT SELECT, INSERT ON app.pgp_decrypt_audit TO mailroom_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON app.contact_key_pins TO mailroom_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON app.contact_key_events TO mailroom_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON app.contact_merges TO mailroom_app;

-- Mail-provider OAuth (#52, migration 00078). Grants change state and rotate;
-- flows are written once and consumed by DELETE, never edited — no UPDATE.
GRANT SELECT, INSERT, UPDATE, DELETE ON app.oauth_grants TO mailroom_app;
GRANT SELECT, INSERT, DELETE ON app.oauth_flows TO mailroom_app;

-- PGP root check (#525): recorded once and never edited or removed by the
-- application, so SELECT and INSERT only — the shape of the audit grant above.
-- Its policies further confine both to the identity-less context.
GRANT SELECT, INSERT ON app.pgp_root_check TO mailroom_app;

-- mailroom_admin deliberately receives NO grants on mail-content tables:
-- messages, blobs, attachments, labels, contacts, user_keys, etc. Absence of
-- privilege + RLS + audit = the §10 "admin can't read mail" claim, honestly.

COMMIT;

-- ─────────────────────────────────────────────────────────────────────────────
-- The hard delete itself.
--
-- ⚠ IT MUST RUN AS THE TABLE OWNER, which is why it is a SECURITY DEFINER and
-- not a statement the application issues. mailroom_app has no DELETE grant on
-- app.users and no policy there — deliberately, per ADR-009 — so the delete has
-- exactly one narrow, auditable way in, exactly as provisioning does.
--
-- ⚠ THE TOMBSTONE IS WRITTEN BEFORE THE DELETE, AND ITS user_id COLUMN IS NOT
-- WHERE THE USER ID SURVIVES. app.audit_log.user_id REFERENCES app.users ON
-- DELETE SET NULL, so the cascade would blank it microseconds later and the
-- record of WHOSE data was destroyed would be gone — the one fact the row exists
-- to preserve. The id is therefore written into object_id (plain text, no
-- foreign key) as well, which is what keeps the derived blob prefix computable
-- for a later re-sweep.
--
-- The cascade does the rest: all 43 user-scoped tables carry an FK to
-- app.users, forty-one of them ON DELETE CASCADE and audit_log ON DELETE SET
-- NULL, so one DELETE reaches everything. Referential actions run with the
-- owner's privileges and are not filtered by RLS.
--
-- Returns the number of rows deleted from app.users: 1 normally, 0 if the id
-- does not exist. ⚠ A CALLER MUST CHECK IT. Zero is exactly what the missing
-- policy used to produce, and "it ran without erroring" has never been evidence
-- of anything here.
CREATE OR REPLACE FUNCTION app.hard_delete_user(p_id app.ulid, p_actor text)
RETURNS integer
LANGUAGE plpgsql SECURITY DEFINER SET search_path = app, pg_temp AS $$
DECLARE
  v_deleted integer;
BEGIN
  DELETE FROM app.users WHERE id = p_id;
  GET DIAGNOSTICS v_deleted = ROW_COUNT;

  -- ⚠ THE TOMBSTONE IS WRITTEN AFTER THE DELETE, AND ITS user_id IS NULL BY
  -- CONSTRUCTION. Both were learned from a test rather than from review.
  --
  -- Writing it first, with user_id = p_id, fails two ways at once. For an id
  -- that does not exist the FK rejects the INSERT, so the function errors
  -- before it can report the far more useful "removed no rows". And for an id
  -- that DOES exist, app.audit_log.user_id REFERENCES app.users ON DELETE SET
  -- NULL, so the cascade blanks the column microseconds later anyway — the
  -- value was never going to survive.
  --
  -- So the id lives in object_id (plain text, no foreign key) and in detail.
  -- That is what keeps the derived blob prefix computable for a later re-sweep,
  -- which is the whole reason the row exists.
  IF v_deleted > 0 THEN
    INSERT INTO app.audit_log (user_id, actor, action, object_type, object_id, detail)
         VALUES (NULL, p_actor, 'user.hard_delete', 'User', p_id::text,
                 jsonb_build_object('user_id', p_id::text));
  END IF;

  RETURN v_deleted;
END;
$$;
REVOKE ALL ON FUNCTION app.hard_delete_user(app.ulid, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.hard_delete_user(app.ulid, text) TO mailroom_app;
ALTER FUNCTION app.hard_delete_user(app.ulid, text) OWNER TO mailroom_owner;
