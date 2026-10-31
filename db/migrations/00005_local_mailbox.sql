-- SPDX-License-Identifier: AGPL-3.0-only

-- Locally-authored messages that are not on the IMAP server yet (mailroom#22,
-- spec §5: compose/drafts/send).
--
-- A message's mailbox membership is derived from app.message_provenance, whose
-- key is a (mailbox, uidvalidity, uid) coordinate the SERVER assigns. A draft
-- Mailroom just composed has no such coordinate until its APPEND round-trips,
-- so it would be a message in no mailbox at all — invisible in Drafts until the
-- next sync pass. app.messages.local_mailbox_id is the local-only placement for
-- exactly that window; when the appended copy comes back down, content-hash
-- dedup (ADR-001) attaches real provenance to the same row and the two agree.
--
-- Additive and nullable: no backfill, no rewrite (ADR-008 expand-and-contract).
-- The column is appended at the end of app.messages so the reference DDL in
-- db/schema.sql and this history produce byte-identical pg_dump output
-- (scripts/migrate-check.sh).

-- +goose Up
ALTER TABLE app.messages
  ADD COLUMN local_mailbox_id app.ulid
    REFERENCES app.mailboxes(id) ON DELETE SET NULL;

CREATE INDEX messages_user_local_mbox ON app.messages (user_id, local_mailbox_id)
  WHERE local_mailbox_id IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS app.messages_user_local_mbox;
ALTER TABLE app.messages DROP COLUMN IF EXISTS local_mailbox_id;
