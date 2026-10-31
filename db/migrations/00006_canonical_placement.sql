-- SPDX-License-Identifier: AGPL-3.0-only

-- Canonical mailbox placement for a message the USER moved (mailroom#84).
--
-- Mailbox membership is derived from app.message_provenance, whose key is a
-- (mailbox, uidvalidity, uid) coordinate the IMAP SERVER assigns. That makes it
-- unable to express "the user moved this message" for two reasons:
--
--   1. the writeback engine needs provenance to keep pointing at where the bytes
--      physically are, or it cannot find the message to move it (it reads a
--      missing live row as a conflict and drops the delta);
--   2. sync has no VANISHED/expunge reconciliation (CONDSTORE only — see
--      internal/mailsync/doc.go), so the SOURCE folder's provenance row survives
--      the move anyway.
--
-- The result was that archiving a message left it in the inbox: it bounced back
-- on the next poll and stayed. app.messages.moved_mailbox_id is the canonical
-- answer — set in the same transaction as the Email/set, it overrides provenance
-- for JMAP membership (Email/get mailboxIds, Email/query inMailbox, Mailbox
-- totals) while provenance keeps doing its job for writeback.
--
-- Additive and nullable: no backfill, no rewrite (ADR-008 expand-and-contract).
-- Appended at the end of app.messages so this history and the reference DDL in
-- db/schema.sql produce byte-identical pg_dump output (scripts/migrate-check.sh).

-- +goose Up
ALTER TABLE app.messages
  ADD COLUMN moved_mailbox_id app.ulid
    REFERENCES app.mailboxes(id) ON DELETE SET NULL;

CREATE INDEX messages_user_moved_mbox ON app.messages (user_id, moved_mailbox_id)
  WHERE moved_mailbox_id IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS app.messages_user_moved_mbox;
ALTER TABLE app.messages DROP COLUMN IF EXISTS moved_mailbox_id;
