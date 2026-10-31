-- SPDX-License-Identifier: AGPL-3.0-only
--
-- A KEY'S REPEATED DRAFT IS COLLAPSED INTO THE FIRST (#872, ADR-045 §2).
--
-- EXPAND-ONLY (ADR-008). app.api_key_drafts gains content_fingerprint, a
-- keyed HMAC of the Email/set create a key sent; a key's identical create
-- within ten minutes returns the draft it already wrote instead of a second
-- one. A plain nullable bytea column is a catalog-only change (not a DOMAIN:
-- docs/migrations.md). Rows written before this migration stay NULL and are
-- never matched, which is the safe reading of "no fingerprint": the next
-- identical call writes a draft, as it did before. The table holds one row
-- per draft a key wrote, so the partial index builds at once.

-- +goose Up
ALTER TABLE app.api_key_drafts ADD COLUMN content_fingerprint bytea;
CREATE INDEX api_key_drafts_by_fingerprint ON app.api_key_drafts (user_id, key_id, content_fingerprint)
  WHERE content_fingerprint IS NOT NULL;

-- +goose Down
DROP INDEX app.api_key_drafts_by_fingerprint;
ALTER TABLE app.api_key_drafts DROP COLUMN content_fingerprint;
