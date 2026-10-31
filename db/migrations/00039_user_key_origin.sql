-- SPDX-License-Identifier: AGPL-3.0-only

-- WHERE THE PRIVATE KEY CAME FROM (#54).
--
-- app.user_keys records what a key IS and never recorded where it came from,
-- and that one missing fact decides whether a hard delete is a service or a
-- destruction:
--
--   IMPORTED  — the user handed Mailroom a key they already hold. Mailroom's
--               copy is a convenience. Deleting the user destroys a duplicate,
--               and an export without key material loses the user nothing,
--               because they can still decrypt their exported mail themselves.
--
--   GENERATED — the key was minted inside the crypto process and its private
--               half has never existed anywhere else. Mailroom's copy is the
--               ONLY copy. Deleting the user destroys it, and every
--               PGP-encrypted message that user ever exported becomes
--               permanently unreadable — retroactively, including exports
--               taken long before the deletion.
--
-- The rows are identical in every other respect. Nothing in the schema, and
-- nothing in app.audit_log (which held no rows on the instance this was written for),
-- distinguishes them.
--
-- ⚠ THIS COLUMN IS NULLABLE, AND THE NULL IS THE POINT — read the contrast with
-- 00036 before "tidying" it into a NOT NULL DEFAULT.
--
-- 00036 added label_suggestions.source as NOT NULL DEFAULT 'model' and said, at
-- length, that the default was A FACT: classifyMessage was provably the only
-- writer that table had ever had, so backfilling recorded what was already true.
--
-- Here the opposite holds. `pgp-generate` and `pgp-import` BOTH exist, both
-- reach app.user_keys through the same pgpkeyd.StoreVault.Insert, and both
-- write byte-identical rows. For a key that already exists there is no
-- evidence anywhere of which path created it. Backfilling to 'imported' would
-- invent a fact that makes a delete look safe; backfilling to 'generated' would
-- invent one that blocks deletes that are fine. So pre-existing rows stay NULL,
-- which reads as exactly what it is: NOT RECORDED.
--
-- ⚠ AND NULL MUST NOT BE TREATED AS 'imported' BY ANY READER. "We do not know
-- whether the user has another copy" is the cautious case, not the safe one.
-- `mailroom-admin delete-user` reports unknown-origin keys separately and by
-- name for that reason, and #54's --apply must refuse rather than guess.
--
-- The value of adding it NOW, before either export or --apply exists, is that
-- it only ever works FORWARD. Every key created between today and whenever this
-- column might otherwise have landed would be permanently ambiguous, and there
-- is no later migration that can recover the answer. The table currently holds
-- a single row on the instance this was written for; the cost of doing this now is a migration
-- against a single-row table, and the cost of doing it later is a class of keys
-- nobody can ever classify.
--
-- Forward-only and expand-and-contract (ADR-008): the column is additive and
-- nullable, so nothing has to be backfilled, no reader that ignores it changes
-- behaviour, and there is no deployment ordering requirement.
--
-- NO INDEX. Nothing filters on origin, and nothing plausibly will: every reader
-- of this column has already selected a specific user's keys through the
-- user_keys_isolation policy, which probes (user_id, ...). A per-user key set is
-- single digits. An index here would cost a write on every key insert to serve a
-- plan no query would choose.

-- +goose Up
ALTER TABLE app.user_keys
  ADD COLUMN origin text;

-- Named explicitly, like label_suggestions_source_known (00036) and
-- mailboxes_sync_scope_known (00026), so the constraint has the same name
-- whether a database was built from the migrations or straight from
-- db/schema.sql — migrate-check diffs pg_dump output of both, and a
-- system-generated name is one more thing that has to agree by luck.
--
-- The CHECK permits NULL by construction (a CHECK is satisfied when its
-- expression is NULL), which is what keeps "not recorded" representable.
ALTER TABLE app.user_keys
  ADD CONSTRAINT user_keys_origin_known
  CHECK (origin IN ('generated', 'imported'));

COMMENT ON COLUMN app.user_keys.origin IS
  'Where the private half came from: ''generated'' (minted in the crypto '
  'process; Mailroom holds the ONLY copy) or ''imported'' (the user holds it '
  'elsewhere too). NULL means NOT RECORDED — rows predating migration 00039, '
  'whose origin is unrecoverable. NULL must never be read as ''imported'': a '
  'hard delete of a generated key is irreversible and retroactively destroys '
  'the readability of already-exported mail (#54, ADR-024).';

-- +goose Down
ALTER TABLE app.user_keys
  DROP CONSTRAINT IF EXISTS user_keys_origin_known;
ALTER TABLE app.user_keys DROP COLUMN IF EXISTS origin;
