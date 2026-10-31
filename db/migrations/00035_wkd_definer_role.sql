-- SPDX-License-Identifier: AGPL-3.0-only

-- THE WKD DEFINER STOPS RUNNING AS THE TABLE OWNER (Forgejo #266; refs #47,
-- #48, #257, migrations 00031/00032/00034).
--
-- ⚠ THIS IS THE FOURTH ATTEMPT AT ONE CONTROL, AND THE FIRST THAT CAN WORK.
-- The three before it all failed for the same reason, which none of them named:
--
--   #48    column GRANT excluding the sealed column   -> inert: an owner holds
--                                                       column privileges implicitly
--   #257   added a column-level REVOKE                -> inert: a column revoke
--                                                       cannot remove a table-level
--                                                       privilege
--   00032  table REVOKE + narrow re-GRANT             -> WORKED, and broke every FK
--                                                       cascade into app.user_keys and
--                                                       app.identities. Live for 15h.
--   00034  restored the grant                         -> the narrowing is decoration again
--
-- ⚠ THE ERROR IS THE DEFINER'S OWNER, NOT THE GRANTS. A SECURITY DEFINER that
-- runs as the TABLE OWNER can never be narrowed, because an owner must hold full
-- privileges for the database to function: PostgreSQL executes a foreign key's
-- referential-integrity action with the privileges of the table owner, so taking
-- SELECT away from that role takes it away from every ON DELETE CASCADE and
-- ON DELETE SET NULL as well. Every attempt to narrow mailroom_owner either does
-- nothing or breaks something, because that role is load-bearing for reasons
-- that have nothing to do with WKD.
--
-- mailroom_owner was chosen as the definer's owner because it was already there.
-- That is the whole mistake.
--
-- THE FIX: own the function with a role that owns NOTHING. mailroom_wkd holds
-- exactly the two column grants the lookup needs and not one privilege more. It
-- is not a table owner anywhere, so it has no implicit privileges to fall back
-- on and nothing else in the database runs as it — which means the narrowing is
-- finally real AND narrowing it cannot break anything else. Rewriting the
-- function body to read sealed key material now fails at runtime with
-- `permission denied`, which is the property #48 claimed and never had.
--
-- WHAT THIS DELIBERATELY DOES NOT DO: it does not try to take anything away from
-- mailroom_owner. mailroom_owner still owns the tables and can still read
-- private_key_ct, exactly as it could before #257 — that is inherent to
-- ownership and is no longer where the boundary is. The boundary is that the
-- ANONYMOUS, CROSS-USER path does not run as an owner at all.
--
-- ⚠ VERIFIED ON A REAL POSTGRESQL UNDER PRODUCTION OWNERSHIP before being
-- written, because the three previous attempts were each verified against a CI
-- harness that applied the schema as `postgres` — where mailroom_owner owned
-- nothing and every arrangement of GRANT and REVOKE looked identical. The
-- harness now applies the schema as mailroom_owner, so the integration suite
-- can see this too. Measured: function owner is mailroom_wkd; mailroom_wkd owns
-- zero relations; has_column_privilege(mailroom_wkd, private_key_ct) false and
-- public_key true; the lookup still resolves a published key called as
-- mailroom_app and returns nothing for an unpublished one; and all four cascades
-- below still fire.

-- +goose Up

-- ⚠ THE MEMBERSHIP HALF OF THIS IS NOT DECORATION, AND IT IS NOT UNCONDITIONAL.
--
-- ALTER FUNCTION ... OWNER TO requires the current owner to be a MEMBER of the
-- target role. Two situations reach this migration and they need different
-- things:
--
--   * NORMAL UPGRADE — mailroom_owner has CREATEROLE and creates mailroom_wkd
--     right here, so PostgreSQL 16 gives it membership WITH ADMIN OPTION
--     automatically and there is nothing to do.
--   * A RESTORED DATABASE — docs/backup-restore-runbook.md tells a human to
--     pre-create the roles as a superuser BEFORE pg_restore, because the dump
--     names them as owners and pg_restore cannot invent them. mailroom_owner
--     then did NOT create mailroom_wkd, holds no ADMIN OPTION on it, and
--     `GRANT mailroom_wkd TO mailroom_owner` fails with `permission denied to
--     grant role`.
--
-- Measured, not imagined: the restore drill failed exactly this way on the first
-- attempt at this migration.
--
-- ⚠ AND THE OBVIOUS FIX IS WRONG, WHICH THE DRILL ALSO CAUGHT. Guarding the
-- GRANT with `IF NOT pg_has_role('mailroom_owner','mailroom_wkd','MEMBER')`
-- skips it in the NORMAL case and then fails at the ALTER with `must be able to
-- SET ROLE`: in PostgreSQL 16 a CREATEROLE role that creates a role receives
-- ADMIN OPTION on it, which satisfies pg_has_role('...','MEMBER'), but NOT the
-- SET membership that ALTER ... OWNER TO actually requires. The explicit GRANT
-- is doing real work and must be attempted every time.
--
-- So: attempt it, and swallow ONLY insufficient_privilege — the pre-created-role
-- case, where the operator was told to grant the membership themselves. If they
-- did, the ALTER below succeeds; if they did not, it fails there with a message
-- that names the problem. Nothing is assumed either way.

-- +goose StatementBegin
DO $$
BEGIN
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'mailroom_wkd') THEN
    CREATE ROLE mailroom_wkd NOLOGIN;
  END IF;
  BEGIN
    EXECUTE 'GRANT mailroom_wkd TO mailroom_owner';
  EXCEPTION WHEN insufficient_privilege THEN
    NULL;
  END;
END $$;
-- +goose StatementEnd

-- The definer's body reads app.identities and app.user_keys by qualified name,
-- so the role executing it needs USAGE on the schema. USAGE only: see the
-- CREATE dance below for why that one is temporary.
GRANT USAGE ON SCHEMA app TO mailroom_wkd;

-- ⚠ THE NARROW GRANTS, AND THIS TIME THEY BITE. mailroom_wkd owns nothing, so
-- it holds nothing implicitly and there is no table-level privilege sitting
-- behind these to make them decoration. What is absent is absent.
--
-- app.identities: omits display_name, signature_html, account_id and
-- default_key_id. Nothing sealed lives here, so the stakes are privacy rather
-- than key compromise — it gets the same treatment anyway.
GRANT SELECT (id, user_id, address, domain, kind, enabled, wkd_hash)
  ON app.identities TO mailroom_wkd;
-- app.user_keys: omits private_key_ct, dek_id and envelope_ver — the sealed
-- material, and the envelope metadata describing how to open it.
GRANT SELECT (id, user_id, identity_id, kind, fingerprint, public_key,
              wkd_published, decrypt_only, revoked_at, created_at)
  ON app.user_keys TO mailroom_wkd;

-- The policies move with the role. They remain the real exposure of this
-- surface — written out here rather than hidden in a function body — and
-- FORCE ROW LEVEL SECURITY applies to mailroom_wkd exactly as it did to
-- mailroom_owner, because it is not the owner of these tables either.
DROP POLICY IF EXISTS identities_wkd_lookup ON app.identities;
CREATE POLICY identities_wkd_lookup ON app.identities
  FOR SELECT TO mailroom_wkd
  USING (wkd_hash IS NOT NULL AND enabled);

DROP POLICY IF EXISTS user_keys_wkd_lookup ON app.user_keys;
CREATE POLICY user_keys_wkd_lookup ON app.user_keys
  FOR SELECT TO mailroom_wkd
  USING (wkd_published);

-- ⚠ OWNING AN OBJECT IN A SCHEMA REQUIRES CREATE ON THAT SCHEMA, and the check
-- happens at ALTER time only. Granting it permanently would let the WKD role
-- create objects in app, which is precisely the kind of privilege this whole
-- migration exists to avoid handing out. So: grant, transfer, take it back.
-- Measured afterwards — has_schema_privilege(mailroom_wkd,'app','CREATE') is
-- false and USAGE is true, with the function still owned by mailroom_wkd.
GRANT CREATE ON SCHEMA app TO mailroom_wkd;
ALTER FUNCTION app.wkd_public_key(text, text) OWNER TO mailroom_wkd;
REVOKE CREATE ON SCHEMA app FROM mailroom_wkd;

-- Re-asserted after the ownership change so the executable surface is stated in
-- one place rather than inherited from whatever the previous owner's ACL was.
REVOKE ALL ON FUNCTION app.wkd_public_key(text, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.wkd_public_key(text, text) TO mailroom_app;

-- ⚠ AND THE COLUMN GRANTS TO mailroom_owner GO AWAY, because they are a claim
-- that is not true. 00034 left them in place so the intent stayed legible while
-- #266 was outstanding; keeping them now would leave the schema asserting a
-- narrowing on the owner that its table-level SELECT overrides — which is the
-- exact misreading that cost three attempts. The table-level GRANT from 00034
-- stays and is what the cascades depend on.
REVOKE SELECT (id, user_id, address, domain, kind, enabled, wkd_hash)
  ON app.identities FROM mailroom_owner;
REVOKE SELECT (id, user_id, identity_id, kind, fingerprint, public_key,
               wkd_published, decrypt_only, revoked_at, created_at)
  ON app.user_keys FROM mailroom_owner;

COMMENT ON FUNCTION app.wkd_public_key(text, text) IS
  'Anonymous, cross-user WKD lookup. Owned by mailroom_wkd — a role that owns '
  'no tables and holds only the narrow column grants this needs — so it CANNOT '
  'read private_key_ct even if this body were rewritten to try (#266). Owning '
  'it with mailroom_owner, as #48/#257 did, made that guarantee unobtainable: '
  'a table owner must keep full privileges or foreign-key cascades fail.';

-- +goose Down

-- Hand the function back to mailroom_owner and restore the (inert) column
-- grants, so a rollback lands exactly on 00034's state.
GRANT CREATE ON SCHEMA app TO mailroom_wkd;
ALTER FUNCTION app.wkd_public_key(text, text) OWNER TO mailroom_owner;
REVOKE CREATE ON SCHEMA app FROM mailroom_wkd;
REVOKE ALL ON FUNCTION app.wkd_public_key(text, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.wkd_public_key(text, text) TO mailroom_app;

DROP POLICY IF EXISTS identities_wkd_lookup ON app.identities;
CREATE POLICY identities_wkd_lookup ON app.identities
  FOR SELECT TO mailroom_owner
  USING (wkd_hash IS NOT NULL AND enabled);
DROP POLICY IF EXISTS user_keys_wkd_lookup ON app.user_keys;
CREATE POLICY user_keys_wkd_lookup ON app.user_keys
  FOR SELECT TO mailroom_owner
  USING (wkd_published);

GRANT SELECT (id, user_id, address, domain, kind, enabled, wkd_hash)
  ON app.identities TO mailroom_owner;
GRANT SELECT (id, user_id, identity_id, kind, fingerprint, public_key,
              wkd_published, decrypt_only, revoked_at, created_at)
  ON app.user_keys TO mailroom_owner;

REVOKE SELECT (id, user_id, address, domain, kind, enabled, wkd_hash)
  ON app.identities FROM mailroom_wkd;
REVOKE SELECT (id, user_id, identity_id, kind, fingerprint, public_key,
               wkd_published, decrypt_only, revoked_at, created_at)
  ON app.user_keys FROM mailroom_wkd;
REVOKE USAGE ON SCHEMA app FROM mailroom_wkd;
