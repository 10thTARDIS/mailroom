-- SPDX-License-Identifier: AGPL-3.0-only

-- WEB KEY DIRECTORY PUBLICATION (Forgejo #48, spec §9, ADR-024).
--
-- WKD lookup is the first surface in Mailroom that is BOTH anonymous and
-- cross-user, and those two facts together decide this whole migration.
--
-- A correspondent's mail client asks "what is the key for alice@example.com?"
-- with no session, no cookie and no identity. Under FORCE ROW LEVEL SECURITY
-- and app.current_user_id() = NULL that is, correctly, zero rows. So the lookup
-- goes through a narrow SECURITY DEFINER — the mechanism this schema already
-- uses for login (app.user_id_for_subject) and boot provisioning
-- (app.provision_user), and the only sanctioned way to cross users.
--
-- ⚠ THE DEFINER IS ITSELF SUBJECT TO RLS. mailroom_owner does not escape the
-- policies (that is what FORCE means), so the function only works because of
-- the two deliberately narrow policies below. They are the exposure, written
-- out where it can be read and argued with, rather than hidden inside a
-- function body.
--
-- WHAT THE POLICIES ALLOW, PRECISELY:
--   * identities: only rows the operator has PUBLISHED (wkd_hash IS NOT NULL)
--     and not disabled. An unpublished address is invisible.
--   * user_keys:  only rows flagged wkd_published.
--
-- ⚠ AND THE PRIVILEGE ON private_key_ct IS EXPLICITLY REVOKED, which is the
-- point. mailroom_owner is granted SELECT on the PUBLIC columns of
-- app.user_keys and has the sealed column taken away — because it OWNS the
-- table in this deployment, and an owner's implicit column privileges make a
-- column-level GRANT alone a no-op. See the REVOKE below for the full argument.
-- The result is that the WKD path cannot read sealed private key material even
-- if a future author rewrites the function body to try: defence that survives
-- the code being wrong, which is the only kind worth having on an anonymous
-- endpoint.
--
-- WHY THE HASH IS A COLUMN AND NOT COMPUTED IN SQL. WKD names a key by
-- z-base-32(SHA-1(lowercased local part)) — a permuted alphabet that is NOT
-- RFC 4648 base32 and is unpleasant to express in SQL. It is computed in Go
-- (internal/wkd), where it is tested against the published draft test vector,
-- and stored here so the lookup is a plain indexed equality. One
-- implementation, tested against the specification, not two that can drift.
--
-- wkd_hash is set at PUBLISH time rather than at identity creation, so there is
-- no backfill: an identity that has never been published simply has NULL, which
-- is also exactly the value that makes it invisible to the policy above.
-- Addresses are effectively immutable (UpdateIdentity patches only display
-- name, signature and enabled), so a stored hash cannot go stale.

-- +goose Up

ALTER TABLE app.identities ADD COLUMN wkd_hash text;

-- ⚠ This index deliberately does NOT lead with user_id, which is the house rule
-- everywhere else (RLS appends that predicate to every plan). WKD is a PUBLIC
-- DIRECTORY: the lookup is by (hash, domain) across all users and has no user
-- to lead with. Stated here so the exception reads as a decision rather than an
-- oversight.
CREATE INDEX identities_wkd_hash ON app.identities (wkd_hash) WHERE wkd_hash IS NOT NULL;

COMMENT ON COLUMN app.identities.wkd_hash IS
  'z-base-32(SHA-1(lowercased local part)) of this identity''s address, per '
  'draft-koch-openpgp-webkey-service rev 21. Computed by internal/wkd and set '
  'when the identity''s key is PUBLISHED. NULL means not published, which is '
  'also what makes the row invisible to the anonymous WKD lookup policy.';

-- The narrow read paths the definer needs. Both are SELECT-only, both are
-- restricted to deliberately published rows, and neither is reachable by
-- mailroom_app (which has its own isolation policies and its own user).
CREATE POLICY identities_wkd_lookup ON app.identities
  FOR SELECT TO mailroom_owner
  USING (wkd_hash IS NOT NULL AND enabled);

CREATE POLICY user_keys_wkd_lookup ON app.user_keys
  FOR SELECT TO mailroom_owner
  USING (wkd_published);

-- Column-level GRANT: private_key_ct is absent, on purpose. See the header.
GRANT SELECT (id, user_id, address, domain, kind, enabled, wkd_hash)
  ON app.identities TO mailroom_owner;
GRANT SELECT (id, user_id, identity_id, kind, fingerprint, public_key,
              wkd_published, decrypt_only, revoked_at, created_at)
  ON app.user_keys TO mailroom_owner;

-- ⚠ AND THE REVOKE, WITHOUT WHICH THE GRANT ABOVE IS DECORATION.
--
-- In this deployment `mailroom-migrate up` runs as mailroom_owner, so
-- mailroom_owner OWNS app.user_keys — and a table owner holds every column
-- privilege implicitly. A column-level GRANT to the owner therefore grants
-- nothing it did not already have, and SELECT private_key_ct would succeed.
--
-- The narrow-grant claim would then have been true only in CI, where
-- scripts/integration-test.sh applies db/schema.sql as the postgres superuser
-- and the owner is someone else. That is an assertion passing against a
-- configuration that does not ship (#131), and it is the shape of mistake this
-- project has paid for before.
--
-- So the sealed column is revoked explicitly. The owner can of course re-grant
-- it — ownership is ownership — but doing so is then a deliberate, visible act
-- rather than a silent default.
--
-- ⚠ AND THE TEST DOES NOT PROVE THIS. scripts/integration-test.sh applies
-- db/schema.sql as the postgres superuser, so in CI mailroom_owner does not own
-- the table and the GRANT's omission alone already denies the column: deleting
-- this REVOKE leaves the suite green (measured, not assumed). The isolation
-- test is therefore a regression guard on the GRANT LIST, and the revoke's
-- effect is only observable under production ownership. Recorded here rather
-- than implied, because an unverified control that reads as verified is how the
-- GRANT above came to be decoration in the first place.
--
-- Safe for backups: scripts/backup.sh refuses to dump unless the connecting
-- role has rolsuper or rolbypassrls, so mailroom_owner is never the dump role.
REVOKE SELECT (private_key_ct) ON app.user_keys FROM mailroom_owner;

-- The lookup. Everything it returns is PUBLIC BY DEFINITION — the published
-- address, the public key, the fingerprint. There is no variant of this that
-- returns anything else, and the column-level grant above means there could not
-- be one: private_key_ct is not readable by the role this runs as.
--
-- The address is returned because the shareable key page (internal/keypub)
-- shows which address a key belongs to, and that page is reached by the same
-- anonymous path. Publishing an address IS publishing it.
--
-- decrypt_only and revoked keys are excluded: a retired key is kept forever so
-- old mail stays readable (spec §9), but publishing it would tell the world to
-- encrypt new mail to a key the user has stopped using.
CREATE OR REPLACE FUNCTION app.wkd_public_key(p_domain text, p_hash text)
RETURNS TABLE (address text, public_key bytea, fingerprint text)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = app AS $$
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
     AND NOT k.decrypt_only
     AND k.revoked_at IS NULL
   ORDER BY k.created_at DESC
   LIMIT 1
$$;

REVOKE ALL ON FUNCTION app.wkd_public_key(text, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.wkd_public_key(text, text) TO mailroom_app;

-- +goose Down
DROP FUNCTION IF EXISTS app.wkd_public_key(text, text);
DROP POLICY IF EXISTS user_keys_wkd_lookup ON app.user_keys;
DROP POLICY IF EXISTS identities_wkd_lookup ON app.identities;
DROP INDEX IF EXISTS app.identities_wkd_hash;
ALTER TABLE app.identities DROP COLUMN IF EXISTS wkd_hash;
