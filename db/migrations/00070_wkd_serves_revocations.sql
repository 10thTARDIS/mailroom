-- SPDX-License-Identifier: AGPL-3.0-only
--
-- WKD SERVES A REVOKED KEY AFTER THE CURRENT ONE (#795, ADR-040).
--
-- Until now app.wkd_public_key returned at most ONE row — the newest published,
-- non-decrypt-only, non-revoked key — so revoking a key made it vanish from the
-- directory. A correspondent who had already fetched it kept encrypting to it,
-- because nothing they could fetch said otherwise. draft-koch-openpgp-webkey-
-- service anticipates exactly this: a server may return revoked keys in
-- addition to a new key, as concatenated key blocks.
--
-- So the function now returns every published PGP key at the address that is
-- either live (not decrypt-only) or REVOKED, live keys first and newest first,
-- bounded at 8 rows. A retired-but-unrevoked key (decrypt_only, revoked_at NULL)
-- stays out, as before: retiring is "stop advertising", not "tell the world".
-- Which rows are actually served — the newest live key that can still be
-- encrypted to, followed by the revocations — is decided in Go
-- (store.LookupWKDKey), because expiry lives in the key's packets, not in a
-- column.
--
-- ⚠ THE SIGNATURE IS UNCHANGED, AND THAT IS DELIBERATE. Same arguments, same
-- RETURNS TABLE, so this is CREATE OR REPLACE: the function keeps its owner
-- (mailroom_wkd, migration 00035), its ACL and its comment. Changing the
-- return type would need DROP + CREATE + the whole ownership dance of 00035,
-- re-opening the control that took four attempts to get right. Nothing about
-- what the definer may READ changes: revoked_at and decrypt_only are already in
-- mailroom_wkd's column grant, and private_key_ct is still not.
--
-- EXPAND-ONLY (ADR-008): a function body, no table change.

-- +goose Up
-- +goose StatementBegin
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
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
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
     AND NOT k.decrypt_only
     AND k.revoked_at IS NULL
   ORDER BY k.created_at DESC
   LIMIT 1
$$;
-- +goose StatementEnd
