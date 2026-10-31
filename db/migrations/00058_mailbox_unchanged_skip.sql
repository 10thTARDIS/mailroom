-- SPDX-License-Identifier: AGPL-3.0-only
--
-- SKIP THE FOLDERS THAT DID NOT CHANGE (mailroom#620).
--
-- A real account can have hundreds of folders. Every pass SELECTs every one of them,
-- and every hour most of them are also EXAMINEd and UID SEARCHed for the
-- expunge reaper and the gap backfill (#86, #93) — overwhelmingly folders in
-- which nothing has happened since the last time we looked. A server with
-- LIST-STATUS (RFC 5819, folded into IMAP4rev2) reports UIDVALIDITY, UIDNEXT,
-- MESSAGES and HIGHESTMODSEQ for every folder in the ONE LIST each pass
-- already sends; comparing those four numbers against what we recorded tells
-- the engine which folders cannot have changed. This migration stores what it
-- compares against. TWO baselines, deliberately, because the two skips need
-- different ones:
--
--   poll_uidnext, poll_messages — UIDNEXT and MESSAGES as the POLL's own SELECT
--     reported them, written by UpdateMailboxCursors beside the existing
--     uidvalidity/highest_modseq cursors. Together they are the status the
--     poll last reconciled against; equal on the next LIST means the SELECT
--     would find nothing new and no flag change.
--
--   observed_uidvalidity, observed_uidnext, observed_messages,
--   observed_highest_modseq — the same four numbers as the LIVE-UID
--     OBSERVATION saw them, written only in the transaction that stamps a real
--     observation (last_reaped_at). ⚠ NOT THE POLL'S. A poll whose FETCH
--     silently lost a message (#93) still advances the poll cursors; the
--     observation is the only thing that finds that loss, so the question
--     "may the observation be skipped" must be asked against what the LAST
--     OBSERVATION saw, never against what the last poll saw. Using the poll's
--     baseline would skip exactly the check that detects the loss.
--
--   last_observed_full_at — when a REAL observation last ran. Separate from
--     last_reaped_at because a skipped observation re-stamps last_reaped_at
--     (the cadence) and must NOT reset this: it is the 24-hour backstop that
--     forces a real UID SEARCH however long a folder reads as unchanged.
--
-- ALL NULLABLE, NO BACKFILL. NULL means "unknown", and unknown means "do the
-- work": the first pass after this migration SELECTs and observes exactly as
-- before, and the baselines fill in as it goes. There is nothing a backfill
-- could put here that would be true — no earlier pass recorded these numbers.
--
-- Internal sync bookkeeping, like the cursors beside them: not JMAP-visible,
-- so writing them appends no change_log row. app.mailboxes is user-scoped; the
-- existing mailboxes_isolation policy and the table-level GRANT to
-- mailroom_app cover new columns, and no SECURITY DEFINER function reads or
-- writes them.

-- +goose Up

ALTER TABLE app.mailboxes
  ADD COLUMN poll_uidnext            bigint,
  ADD COLUMN poll_messages           bigint,
  ADD COLUMN observed_uidvalidity    bigint,
  ADD COLUMN observed_uidnext        bigint,
  ADD COLUMN observed_messages       bigint,
  ADD COLUMN observed_highest_modseq bigint,
  ADD COLUMN last_observed_full_at   timestamptz;

-- +goose Down

ALTER TABLE app.mailboxes
  DROP COLUMN IF EXISTS last_observed_full_at,
  DROP COLUMN IF EXISTS observed_highest_modseq,
  DROP COLUMN IF EXISTS observed_messages,
  DROP COLUMN IF EXISTS observed_uidnext,
  DROP COLUMN IF EXISTS observed_uidvalidity,
  DROP COLUMN IF EXISTS poll_messages,
  DROP COLUMN IF EXISTS poll_uidnext;
