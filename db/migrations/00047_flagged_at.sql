-- SPDX-License-Identifier: AGPL-3.0-only
--
-- THE DESK'S IN-TRAY NEEDS A "SINCE", NOT JUST A "WHETHER" (mailroom#444).
--
-- The in-tray (#372, direction 7) is the user's explicit still-to-address
-- state: ONE flag, riding IMAP \Flagged so every other client agrees (decided
-- 2026-09-07 on #444 — no Mailroom-only keyword). Its strip and its full view
-- show how long each item has WAITED ("waiting 9 d"; "3 waiting over a week")
-- — with one flag and no reasons, that is the useful second column, and the
-- nag is what makes the tray cheaper than leaving mail unread.
-- app.messages.is_flagged is a boolean; nothing records WHEN it flipped.
--
-- Deriving the time from change_log was rejected: change_log rows are
-- per-OBJECT, with no field-level reason recorded, so answering "when was
-- THIS flag set" would mean re-deriving it from Email `updated` rows that
-- also cover every other property Email/set can touch — the tray would need
-- it on every row it renders, which is exactly the kind of cost a column
-- exists to avoid (the same reasoning pinned_at and snoozed_until already
-- follow on this table).
--
-- So: a timestamp, set on whichever side flips the flag first — the user
-- through JMAP (ApplyEmailUpdate), a rule at ingest (applyRuleAction's
-- ActionFlag case), or an inbound flag already sitting on the server when
-- Mailroom first sees the message (InsertMessage) or sees it change on a
-- later sync (UpdateMessageFlagsIfChanged). The rule enforced in SQL at every
-- one of those sites, never in Go, is: flagged_at becomes now() on a
-- false→true transition, NULL on true→false, and is otherwise left alone —
-- so the two columns cannot disagree about whether a message is flagged.
--
-- PLAIN timestamptz, CATALOG-ONLY: not a domain type, so this is the 2 ms
-- case docs/migrations.md warns about rather than the 689 ms/2.0 s one — load
-- bearing on app.messages, the largest table by orders of magnitude.
--
-- THE BACKFILL RUNS INLINE, NOT AS AN ONLINE JOB, because the population it
-- touches is tiny: flags have barely been used on the mailbox checked, so
-- this is nothing like the 10^6-10^7-row tables expand-and-contract exists
-- for (docs/migrations.md). A deployment that already has a very large
-- flagged population should NOT copy this migration verbatim — it should
-- follow ADR-008's expand/backfill-job/contract shape instead: add the column
-- here, backfill it with an app.jobs-driven batch walk, and only then treat
-- flagged_at as populated.
--
-- received_at IS A LOWER BOUND ON THE TRUE FLAG TIME, not the true time
-- itself — a message flagged long after it arrived reads as "waiting since it
-- arrived" rather than "waiting since it was starred", which is wrong in the
-- direction of UNDERSTATING the wait, never overstating it. It is also the
-- only fact ingest or a backfill can possibly have: nothing before this
-- migration recorded when a flag was actually set, on this server or any
-- other IMAP client that set \Flagged before Mailroom ever synced the
-- mailbox. now() would be actively wrong here — an old starred message would
-- read "waiting 0 d" the instant it was first ingested, which is a worse lie
-- than an understated wait.

-- +goose Up

ALTER TABLE app.messages ADD COLUMN flagged_at timestamptz;

-- Backfill: every row already flagged gets the best "since" we have.
-- received_at is a LOWER BOUND on when the flag was set (it cannot predate
-- the message) — see the header for why this is a plain UPDATE rather than a
-- batched backfill job.
UPDATE app.messages SET flagged_at = received_at WHERE is_flagged AND flagged_at IS NULL;

-- The tray reads "flagged, oldest first" per user: a partial index, user_id
-- leading so RLS's appended predicate is served by the same scan (CLAUDE.md).
CREATE INDEX messages_flagged_since ON app.messages (user_id, flagged_at) WHERE is_flagged;

-- +goose Down
DROP INDEX IF EXISTS app.messages_flagged_since;
ALTER TABLE app.messages DROP COLUMN flagged_at;
