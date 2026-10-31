-- SPDX-License-Identifier: AGPL-3.0-only
--
-- Quarantine enforcement: a content identity for an attachment part, and a
-- release that is keyed on that content rather than on a row (mailroom#396,
-- ADR-027 §8, increment 5).
--
-- ⚠ WHY A NEW COLUMN RATHER THAN THE ONE §8 NAMES. ADR-027 §8 says a quarantine
-- "must hit all of them — the same attachment exists as N rows sharing a
-- content_hash". Measured against a live database before writing this file: none of
-- the attachment rows had content_hash set,
--
-- and that is deliberate, not a gap. `app.attachments.content_hash` is a BLOB
-- REFERENCE — `blobreclaim.go` reads it to decide whether a blob is still in
-- use — and `internal/attachproj.Rows` leaves it NULL on purpose because a
-- part's bytes are a slice of the message blob and there is no per-attachment
-- blob to name. Writing a part digest into it would arm the reclaim guard
-- against blobs that do not exist and make "is this blob referenced" mean two
-- different things.
--
-- So the join key §8 requires had to be created. `part_hash` is the SHA-256 of
-- the part's DECODED bytes: a content identity, never a blob reference, and
-- nothing reads it as one.
--
-- ⚠ AND THE DUPLICATION IS REAL, THOUGH NOT FOR §8'S STATED REASON. §8 cites
-- re-delivery (#206). Measured: ZERO messages shared a dedup_hash on that
-- corpus. What does duplicate is the other case `blobreclaim.go` already names
-- — the same attachment on several DIFFERENT messages — and by an upper bound
-- of (filename, size, content_type) equality it covers most rows in
-- groups: one newsletter image appears on a dozen messages. That bound is an upper
-- one and provably loose (eight `signature.asc` at 307 bytes are eight
-- different signatures), which is exactly why the true figure needs this
-- column and `attachment-report` prints it.

-- +goose Up

-- The content identity. NULL until a scan writes one: a digest is computed in
-- internal/attachscan where the decoded bytes already are, so ingest and
-- re-scan cannot derive it differently, and neither path pays an extra read.
--
-- ⚠ DECLARED LAST BECAUSE ALTER TABLE ADD COLUMN APPENDS. db/schema.sql and
-- this chain must produce byte-identical schemas and CI's migrate-check diffs
-- them INCLUDING COLUMN ORDER — the failure v1.60.0 shipped, which no local
-- run could catch because schema-apply and the integration suite both apply
-- schema.sql, the file that can be wrong.
ALTER TABLE app.attachments
  ADD COLUMN part_hash app.sha256;

-- Partial, because only populated rows are ever looked up — the same shape as
-- messages_user_dedup_hash. Leads with user_id: RLS appends that predicate to
-- every plan (CLAUDE.md).
CREATE INDEX attachments_user_part_hash
  ON app.attachments (user_id, part_hash)
  WHERE part_hash IS NOT NULL;

-- A human's override of a quarantine, keyed on CONTENT.
--
-- ⚠ THIS IS WHERE ADR-027 §5'S FIFTH STATE MOVED, AND THE MOVE IS THE POINT.
-- §5 modelled `released` as a value of `scan_status`, with re-scan forbidden
-- from overwriting it (`AND scan_status IS DISTINCT FROM 'released'`). Three
-- things are wrong with a release living in that column, and all three are
-- fixed by it living here:
--
--   1. A RELEASE IS ABOUT BYTES, NOT ABOUT A ROW. §8 requires a release to
--      reach every copy. As a column value that is an N-row write that has to
--      be remembered at every call site, and a copy that ARRIVES LATER gets
--      none of it. As a row keyed on part_hash it is one write and every copy,
--      past and future, is covered by construction.
--   2. IT DESTROYED THE VERDICT IT OVERRODE. Writing 'released' over 'flagged'
--      loses what the scanner said, and §5's own stability table says a
--      malicious verdict essentially never reverts — so the fact would never
--      come back. Here the verdict stays `flagged` and keeps being refreshed;
--      the release sits alongside it. "We flagged this, you released it" is a
--      thing the client can now say.
--   3. THE GUARD WAS REMEMBERED, NOT STRUCTURAL. Any future writer of
--      scan_status that forgot `IS DISTINCT FROM 'released'` would silently
--      revoke a user's decision. Nothing a scanner writes can reach this table.
--
-- No data migration: `released` was never written by any code — SetScanVerdicts
-- refused it by construction and nothing else writes scan_status — so no
-- deployment can hold such a row. Checked, not assumed: a live database held
-- only `clean` and `unknown`.
CREATE TABLE app.attachment_releases (
  user_id     app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  -- The released CONTENT. Not a foreign key to anything: it names bytes, and
  -- the rows carrying those bytes come and go independently of the decision.
  part_hash   app.sha256 NOT NULL,
  released_at timestamptz NOT NULL DEFAULT now(),
  -- Where the user was standing when they decided. Audit only — the release
  -- applies to the content regardless, so this is ON DELETE SET NULL: deleting
  -- the message you released from does not un-release the bytes.
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
-- SELECT/INSERT/DELETE but NOT UPDATE, the same shape as app.category_dismissals
-- and app.superseded_content: a release is made once and can be revoked, and
-- there is nothing about "the user accepted these bytes" to edit.
GRANT SELECT, INSERT, DELETE ON app.attachment_releases TO mailroom_app;

-- +goose Down
DROP TABLE app.attachment_releases;
DROP INDEX app.attachments_user_part_hash;
ALTER TABLE app.attachments DROP COLUMN part_hash;
