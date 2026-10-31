-- SPDX-License-Identifier: AGPL-3.0-only

-- RESTORE mailroom_owner's TABLE-LEVEL SELECT (Forgejo #266, reverting the
-- privilege half of 00032; refs #257, #47/#48).
--
-- ⚠ 00032 BROKE CASCADE DELETES, AND IT WAS LIVE IN PRODUCTION.
--
-- PostgreSQL executes a foreign key's referential-integrity action with the
-- privileges of the TABLE OWNER, not of the caller. 00032 revoked
-- mailroom_owner's table-level SELECT on app.user_keys and app.identities so
-- that a narrow column GRANT would actually bite — and in doing so removed the
-- privilege those RI actions depend on. Every cascade INTO either table fails:
--
--   identities.account_id  -> accounts  ON DELETE CASCADE   (delete an account)
--   user_keys.user_id      -> users     ON DELETE CASCADE   (delete a user)
--   user_keys.identity_id  -> identities ON DELETE SET NULL (delete an identity)
--   identities.default_key_id -> user_keys ON DELETE SET NULL (delete a key)
--
-- Observed, not theorised: TestDeleteAccountCascadesAndRemovesOwnMail fails
-- with `permission denied for table identities` the moment the test harness
-- applies the schema under production ownership. Restoring this one grant makes
-- it pass again.
--
-- ⚠ WHY NOBODY CAUGHT IT: scripts/integration-test.sh applied db/schema.sql as
-- the postgres superuser, so mailroom_owner did not own these tables in CI and
-- never held the privilege 00032 removed. The revoke was a no-op there and the
-- suite was green. This migration ships alongside the harness change that ends
-- that blindness — the same change that found this.
--
-- WHAT THIS COSTS. The column narrowing on app.user_keys goes back to being
-- decoration: mailroom_owner can read private_key_ct again, exactly as it could
-- before #257. That is a real loss of a defence-in-depth layer and it is being
-- accepted deliberately, because a broken account-deletion path is a worse
-- problem than a missing second line of defence on a path whose first line
-- (the function body) is correct and tested.
--
-- ⚠ THE CONTROL IS NOT ABANDONED, IT IS MISDESIGNED. Making a SECURITY DEFINER
-- run as the table OWNER is what forces this trade: an owner must hold full
-- privileges for RI to work, so it can never be the role you narrow. The fix is
-- to own the definer with a purpose-built role that owns no tables and holds
-- only the narrow column grants. That is #266, and it is deliberately not
-- attempted in the same change as restoring correctness.

-- +goose Up

-- Table-level SELECT back. The column grants from 00032 remain and are now
-- redundant rather than harmful; they are left in place so the intent stays
-- legible and #266 has less to reconstruct.
GRANT SELECT ON app.user_keys TO mailroom_owner;
GRANT SELECT ON app.identities TO mailroom_owner;

-- +goose Down
REVOKE SELECT ON app.user_keys FROM mailroom_owner;
GRANT SELECT (id, user_id, identity_id, kind, fingerprint, public_key,
              wkd_published, decrypt_only, revoked_at, created_at)
  ON app.user_keys TO mailroom_owner;
REVOKE SELECT ON app.identities FROM mailroom_owner;
GRANT SELECT (id, user_id, address, domain, kind, enabled, wkd_hash)
  ON app.identities TO mailroom_owner;
