-- SPDX-License-Identifier: AGPL-3.0-only

-- CONSTRAINING sync_scope TO THE VALUES THE ENGINE ACTUALLY IMPLEMENTS
-- (#62 — selective sync / sync scope).
--
-- `app.mailboxes.sync_scope` has been in the baseline schema since the start, with a
-- comment advertising three values:
--
--     sync_scope text NOT NULL DEFAULT 'full',   -- 'full' | 'headers' | 'off'
--
-- Two of those three are real. `internal/mailsync/engine.go` tests exactly one value —
-- `syncScopeOff` — and skips the mailbox before SELECT when it matches. Everything else,
-- including the literal string 'headers', falls through to a FULL sync.
--
-- SO 'headers' TODAY MEANS ITS OPPOSITE. A folder set to 'headers' does not sync headers;
-- it syncs completely. There is no CHECK constraint, so nothing prevents that state, and
-- until this migration nothing could even set the column except direct SQL — which is
-- precisely how an operator would have reached for the value the schema advertises.
--
-- WHY 'headers' IS NOT BEING IMPLEMENTED INSTEAD, since that is the other way to close
-- this. Message identity is `SHA-256(raw RFC 5322 bytes)` (ADR-001): the content hash is
-- the blob address and the FK hub of the schema. A headers-only message HAS no raw bytes,
-- so it cannot be an `app.messages` row at all — it would need a parallel "stub" concept
-- with its own identity, its own dedup story, its own promotion path to a full message, and
-- a decision about what every consumer of `content_hash` does when it is absent. That is an
-- architecture change requiring an ADR, not an enum value. If it is ever wanted, it arrives
-- deliberately.
--
-- The rewrite below is behaviour-PRESERVING, which is the only reason it is safe to do
-- silently: a row reading 'headers' is being synced fully right now, and 'full' is the value
-- that says so. Nothing about what the engine does to that mailbox changes.

-- +goose Up

-- Behaviour-preserving normalisation (see above): whatever a row says, if it is not 'off'
-- the engine has been giving it a full sync, so 'full' is what it means.
UPDATE app.mailboxes SET sync_scope = 'full' WHERE sync_scope NOT IN ('full', 'off');

ALTER TABLE app.mailboxes
  ADD CONSTRAINT mailboxes_sync_scope_known
  CHECK (sync_scope IN ('full', 'off'));

-- (The column's documentation lives as an ordinary `--` comment in db/schema.sql, which
-- is where every other column in this schema is documented. A COMMENT ON would be the
-- only one in the database and would show up as a permanent migrate-check diff.)

-- +goose Down
ALTER TABLE app.mailboxes DROP CONSTRAINT IF EXISTS mailboxes_sync_scope_known;
