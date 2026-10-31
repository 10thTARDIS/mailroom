-- SPDX-License-Identifier: AGPL-3.0-only

-- Pin (per message) and mute (per thread) — the local-first organization
-- primitives from spec §6, "Snooze / mute-thread / pin ... implemented locally
-- with writeback where representable" (mailroom#51).
--
-- Unlike app.account_sync_status or app.triage_scores, these ARE JMAP-visible
-- state: they change what every client shows and must sync across devices. So a
-- write to either column appends a change_log row in the same transaction —
-- Email for a pin, Thread for a mute — per the load-bearing invariant in
-- CLAUDE.md. app.object_type already carries both 'Email' and 'Thread', and
-- Thread/get + Thread/changes already exist, so a muted thread has a sync
-- channel from day one. No new object type is needed.
--
-- TIMESTAMPS, NOT BOOLEANS, and deliberately so:
--   * "when did I pin this" is orderable, which is what a pinned-first list
--     ordering needs; a boolean throws that away and cannot be recovered.
--   * NULL is a far cheaper partial-index predicate than `= false`, and the
--     overwhelmingly common case is "not pinned" / "not muted" — the partial
--     indexes below therefore stay tiny no matter how large the mailbox grows.
--
-- Neither is representable as an IMAP flag in any portable way (there is no
-- \Pinned, and mute is a thread-level notion IMAP has no concept of), so both
-- stay local-only: no writeback intent, nothing to reconcile on sync. That is
-- the "where representable" escape hatch in §6 doing its job.
--
-- Both indexes lead with user_id because RLS appends that predicate to every
-- plan (CLAUDE.md); a partial index that did not would not be used.
--
-- Additive and forward-only (ADR-008): two nullable columns, no backfill, no
-- rewrite — an ALTER TABLE ... ADD COLUMN with no default is O(1) metadata-only
-- on PostgreSQL 11+, so this is safe on a large messages table. The Down drops
-- them cleanly.

-- +goose Up
ALTER TABLE app.messages ADD COLUMN pinned_at timestamptz;
ALTER TABLE app.threads  ADD COLUMN muted_at  timestamptz;

CREATE INDEX messages_user_pinned ON app.messages (user_id, pinned_at DESC)
  WHERE pinned_at IS NOT NULL;
CREATE INDEX threads_user_muted ON app.threads (user_id)
  WHERE muted_at IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS app.threads_user_muted;
DROP INDEX IF EXISTS app.messages_user_pinned;
ALTER TABLE app.threads  DROP COLUMN muted_at;
ALTER TABLE app.messages DROP COLUMN pinned_at;
