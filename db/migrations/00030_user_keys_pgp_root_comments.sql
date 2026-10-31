-- SPDX-License-Identifier: AGPL-3.0-only

-- WHAT SEALS app.user_keys.private_key_ct, WRITTEN DOWN IN THE DATABASE
-- (Forgejo #47 increment 3, ADR-024). Comments only — no data, no DDL that
-- rewrites a row, no index. Applying this to the deployed instance is a no-op
-- in every sense a user could notice.
--
-- WHY A MIGRATION THAT CHANGES NO DATA IS WORTH ITS NUMBER.
--
-- app.user_keys predates ADR-024: it shipped in the baseline (00001) carrying
-- `dek_id` and the word "wrapped", both of which describe a design that no
-- longer exists. In that design a private key was sealed under the application's
-- MASTER-KEY hierarchy, and dek_id pointed into it.
--
-- ADR-024 replaced that. PGP private keys are sealed by the crypto process under
-- its OWN root secret:
--
--     pgp root ──HKDF(salt=userID, info="mailroom-pgp-v1")──▶ wrapping key
--
-- The wrapping key is derived per user and NEVER STORED, so for a 'pgp' row
-- there is no DEK, and dek_id has nothing to point at. It stays NULL.
--
-- The danger being closed is not a bug — the column is harmless — it is a
-- READING. A future maintainer who finds a nullable `dek_id` on the table that
-- holds identity keys will reasonably conclude the master key seals them, and
-- from there that a master-key holder can recover them. That conclusion is
-- wrong, and it is wrong in the direction that gets a boundary deleted during a
-- refactor because it looked redundant.
--
-- dek_id is NOT DROPPED. Migrations are expand-and-contract (ADR-008); dropping
-- a column is a contract step and belongs to a later change, if ever. A NULL
-- column costs nothing. A misleading one costs an argument at the wrong moment.
--
-- envelope_ver KEEPS ITS MEANING and stays 1: it is the internal/crypto envelope
-- version (crypto.Version1), which is real, is what crypto.Seal writes, and is
-- what a change to the sealing scheme would bump.

-- +goose Up
COMMENT ON TABLE app.user_keys IS
  'The user''s own signing/encryption keys. For kind=''pgp'', private_key_ct is '
  'sealed by the CRYPTO PROCESS under its own root secret (ADR-024), not under '
  'the application master key: pgp root -> HKDF(salt=user_id, '
  'info="mailroom-pgp-v1") -> wrapping key, which is derived per user and never '
  'stored. The application can read this ciphertext and cannot open it. '
  'Mutations append a ''Key'' change_log row.';

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

-- +goose Down
COMMENT ON TABLE app.user_keys IS NULL;
COMMENT ON COLUMN app.user_keys.private_key_ct IS NULL;
COMMENT ON COLUMN app.user_keys.dek_id IS NULL;
COMMENT ON COLUMN app.user_keys.envelope_ver IS NULL;
COMMENT ON COLUMN app.user_keys.identity_id IS NULL;
