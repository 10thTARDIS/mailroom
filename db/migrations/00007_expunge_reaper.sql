-- SPDX-License-Identifier: AGPL-3.0-only

-- Expunge/VANISHED reconciliation — the "reaper" (mailroom#86).
--
-- Sync is CONDSTORE-only (QRESYNC is unusable through go-imap v2 beta.8 — see
-- internal/sync/NOTES.md), so nothing ever told Mailroom that a message had been
-- expunged or moved away on the server. app.message_provenance only ever GREW:
-- a message deleted on the user's phone never left its Mailroom folder, ever.
--
-- The fix is a periodic reaper pass (internal/mailsync): per mailbox, ask the
-- server for the live UID set (UID SEARCH ALL), diff it against the provenance
-- rows we hold for that (mailbox, uidvalidity), and drop the rows the server no
-- longer has. Two columns support it:
--
--   app.mailboxes.last_reaped_at — the reaper's per-mailbox cadence cursor. The
--     reaper runs far less often than the incremental poll (hourly-ish vs.
--     minutes), and the cadence must survive a restart and spread itself across
--     an account's mailboxes, so it is persisted per mailbox rather than held in
--     process memory. Like the other sync cursors on this table it is internal
--     bookkeeping, NOT a JMAP-visible Mailbox property, so writing it appends no
--     change_log row (see internal/store/mailboxes.go's file header).
--
--   app.messages.vanished_at — set when the reaper concludes a message is gone
--     from EVERY synced location and soft-deletes it (trashed_at, ADR-009; never
--     a hard delete). It is what makes that conclusion RETRACTABLE, and it is
--     load-bearing for safety.
--
--     The reaper's inference is "no live provenance anywhere ⇒ gone everywhere".
--     That inference has a real race: another client can move a message out of
--     INBOX and into a folder AFTER we polled that folder but BEFORE we reap
--     INBOX, and for that instant the message is legitimately in no folder we
--     know about. Without a retraction the message would stay invisible forever
--     while sitting perfectly happily on the server — mail loss by inference.
--
--     So: when ingest later attaches a NEW live provenance row to a message with
--     vanished_at set, the message is demonstrably back on the server, the
--     inference was wrong, and it is withdrawn (trashed_at/purge_after/
--     vanished_at all cleared — store.UserTx.RetractVanished). A message the USER
--     trashed has vanished_at IS NULL and is therefore NEVER resurrected by this
--     path, which is the whole reason the reaper's trash is distinguishable from
--     the user's.
--
-- Both columns are additive and nullable: no backfill, no rewrite (ADR-008
-- expand-and-contract). Each is appended at the END of its table's column list so
-- this history and the reference DDL in db/schema.sql produce byte-identical
-- pg_dump output (scripts/migrate-check.sh).

-- +goose Up
ALTER TABLE app.mailboxes
  ADD COLUMN last_reaped_at timestamptz;

ALTER TABLE app.messages
  ADD COLUMN vanished_at timestamptz;

-- +goose Down
ALTER TABLE app.messages DROP COLUMN IF EXISTS vanished_at;
ALTER TABLE app.mailboxes DROP COLUMN IF EXISTS last_reaped_at;
