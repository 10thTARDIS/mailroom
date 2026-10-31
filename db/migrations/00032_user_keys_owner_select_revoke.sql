-- SPDX-License-Identifier: AGPL-3.0-only

-- MAKE THE WKD PRIVILEGE NARROWING ACTUALLY BITE (Forgejo #257, refs #47/#48,
-- ADR-024).
--
-- Migration 00031 added `REVOKE SELECT (private_key_ct) ON app.user_keys FROM
-- mailroom_owner` and described it as "the load-bearing half" — the thing that
-- makes the anonymous WKD path unable to read sealed key material even if a
-- future author rewrites app.wkd_public_key to try.
--
-- ⚠ IT DID NOTHING. Measured on the deployed database immediately after
-- v1.31.0, i.e. in the ownership model that actually ships:
--
--   table owner      = mailroom_owner
--   private_key_ct   = true      <- has_column_privilege(mailroom_owner, …)
--   public_key(ctrl) = true      <- positive control
--   relacl = {mailroom_owner=arwdDxt/mailroom_owner,mailroom_app=arwd/…}
--   private_key_ct -> <none>     <- no column ACL at all
--
-- In PostgreSQL a column is readable if the role holds SELECT on EITHER the
-- column OR the whole table. mailroom_owner holds it on the whole table (the
-- `r` in `arwdDxt`, its owner privileges materialised the first time anything
-- was granted on this table). A COLUMN-level revoke cannot remove a TABLE-level
-- privilege, so 00031's revoke merely left private_key_ct with no column entry
-- — which is precisely the state it was already in.
--
-- So the control written to fix an inert control was inert for a sibling
-- reason: 00031's GRANT was decoration because the owner holds column
-- privileges implicitly; its REVOKE was decoration because the owner holds the
-- TABLE privilege explicitly.
--
-- ⚠ THE ORDER BELOW IS LOAD-BEARING AND IS NOT A STYLE CHOICE. `REVOKE SELECT
-- ON <table>` also drops every column-level SELECT grant on that table. Doing
-- it in 00031's order (grant the narrow list, then revoke) leaves mailroom_owner
-- unable to read ANY column, which breaks app.wkd_public_key outright and takes
-- every WKD lookup with it. That failure was observed in a throwaway Postgres
-- while establishing this fix: the positive control went red alongside the
-- negative one. Revoke first, then re-grant.

-- +goose Up

-- 1. Remove the table-level SELECT that made the column narrowing meaningless.
--    Only SELECT: mailroom_owner keeps INSERT/UPDATE/DELETE/etc. so migrations
--    can still maintain this table, and ownership rights (ALTER, DROP, index
--    creation) are not part of relacl and are untouched.
REVOKE SELECT ON app.user_keys FROM mailroom_owner;

-- 2. Re-grant the narrow list. This is the same list 00031 granted; it is
--    repeated because step 1 dropped it.
--
--    private_key_ct, dek_id and envelope_ver are absent on purpose: the sealed
--    material and the envelope metadata that describes how to open it.
GRANT SELECT (id, user_id, identity_id, kind, fingerprint, public_key,
              wkd_published, decrypt_only, revoked_at, created_at)
  ON app.user_keys TO mailroom_owner;

-- 3. app.identities carries the SAME inert pattern, and is fixed here rather
--    than left behind.
--
--    00031 gave mailroom_owner a column-level SELECT on identities too, with no
--    revoke at all — so it is narrow in appearance only, exactly as the
--    user_keys grant was. Nothing SEALED lives in this table, so the stakes are
--    lower: what the omitted columns hold is display_name, signature_html,
--    account_id and default_key_id, for identities the operator has deliberately
--    published. A rewritten definer body leaking a signature is a privacy bug,
--    not a key compromise.
--
--    It is fixed anyway, because the whole lesson of #257 is that a narrowing
--    which reads as enforced and is not is worse than no narrowing: the next
--    reader compares the two grants and reasonably concludes both bite.
--
--    Same bounded blast radius as above — app.wkd_public_key is the only thing
--    that reads app.identities as mailroom_owner, and every column it touches
--    (id, user_id, address, kind, enabled, wkd_hash) is in the list below.
REVOKE SELECT ON app.identities FROM mailroom_owner;
GRANT SELECT (id, user_id, address, domain, kind, enabled, wkd_hash)
  ON app.identities TO mailroom_owner;

-- ⚠ WHAT THIS DOES AND DOES NOT BUY, stated because overstating it is the
-- mistake this migration exists to correct.
--
-- It DOES mean the WKD definer cannot read sealed key material: the privilege
-- is not there to use, so rewriting the function body cannot widen it.
--
-- It does NOT make the owner powerless — ownership is ownership, and
-- mailroom_owner can re-grant itself at any time (confirmed by experiment).
-- The goal is only that widening this surface becomes a deliberate, visible
-- act in a migration rather than a silent default.
--
-- ⚠ AND A FUTURE MIGRATION THAT READS private_key_ct WILL NOW FAIL with
-- "permission denied", because migrations run as mailroom_owner. That is the
-- intended behaviour — a re-seal or rotation migration touching sealed key
-- material should have to say so — but it is a live tripwire, not a
-- theoretical one, so it is written here where whoever hits it will look.
--
-- ⚠ CI STILL CANNOT SEE THE EFFECT ON THIS TABLE. scripts/integration-test.sh
-- applies db/schema.sql as the postgres superuser, so mailroom_owner does not
-- own app.user_keys there and never had the table-level privilege this removes.
-- The assertion in internal/store on the real table therefore remains a
-- regression guard on the GRANT LIST only. What IS falsifiable in CI is
-- TestOwnerColumnGrantNeedsTableLevelRevoke, which builds this exact ownership
-- model in a scratch schema and fails if the pattern reverts to column-only.
-- The effect on app.user_keys itself is verified by measuring the deployed
-- database after this ships — the same way the defect was found.
--
-- Safe for backups: scripts/backup.sh refuses to dump unless the connecting
-- role has rolsuper or rolbypassrls, so mailroom_owner is never the dump role.

-- +goose Down

-- Restore 00031's end state (the inert one): give the table-level SELECT back
-- and re-apply the column-level revoke that could never bite.
GRANT SELECT ON app.user_keys TO mailroom_owner;
REVOKE SELECT (private_key_ct) ON app.user_keys FROM mailroom_owner;
GRANT SELECT ON app.identities TO mailroom_owner;
