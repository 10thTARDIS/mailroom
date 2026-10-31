-- SPDX-License-Identifier: AGPL-3.0-only
--
-- THE LINE OVERRIDE: A CORRECTION MOVES THE MESSAGE ACROSS THE LINE NOW
-- (mailroom#464, #375 §5, Direction 7 "Desk and Drawers", #372).
--
-- Shift+↑/↓ moves a row across the desk's line, and that crossing IS the
-- correction the model learns from. Until now `Triage/correct` recorded only
-- an app.feedback_events row — training data, consumed the next time the
-- model is retrained. Nothing the desk READS changed: AreaSummary's counts,
-- Email/query's `onSurface`, and the drawer sweep's candidate predicate all
-- split on the raw app.triage_scores.score against desk_preferences.
-- line_threshold. So the row the user just pulled above the line stayed in
-- the drawer, and — worse — the sweep could archive it out from under them
-- once it was old enough.
--
-- line_side is that placement, made durable:
--
--   NULL      no override; the score decides (every row before this migration,
--             which is why there is no backfill — NULL is already every
--             existing row's true state, not an unknown).
--   'surface' the user put this above the line.
--   'drawer'  the user put this below it.
--
-- A USER'S PLACEMENT OUTRANKS THE MODEL AND IS NEVER CLEARED BY A RESCORING.
-- That is the whole point: a re-score that could silently undo the placement
-- would put the message back where the user just said it does not belong, and
-- the sweep would then be free to archive it. Nothing in the scoring path
-- writes these columns; only Triage/correct does (store.UserTx.SetLineSide),
-- and Triage/set — the model reset/rollback valve — deliberately leaves them
-- alone: rolling the MODEL back is not the user retracting their own edits.
--
-- # Why a column rather than deriving it from app.feedback_events
--
-- The last mark_important/mark_not_important event per message would, in
-- principle, answer the same question. It is the wrong shape three times
-- over: (1) feedback_events is an append-only TRAINING log — a signal, not a
-- placement, and other paths (archive_unread, reply, …) write it too; (2)
-- three separate SQL predicates (the summary counts, the query filter, the
-- sweep) would each need a per-message "latest row of a kind" lateral, on the
-- hot desk path; (3) a training log is legitimately prunable, and pruning it
-- must not move the user's mail. One indexed column, read by ONE shared SQL
-- expression (store.lineSideSQL), is the honest representation.
--
-- # text + CHECK, not an enum
--
-- A closed two-value vocabulary, enforced in the database, following
-- app.mailboxes.sync_scope (migration 00026) rather than app.category_tab:
-- a CHECK is ordinary DDL to extend, while a new enum type would be a third
-- catalog type to keep in step with a Go constant set that already validates
-- the value before it reaches SQL (store.LineSide).
--
-- # Cost on app.messages, MEASURED rather than inferred
--
-- docs/migrations.md warns that ADD COLUMN of a DOMAIN type rewrites the
-- whole table, and names app.messages as the one to keep clear of. Neither
-- column here is domain-typed, but the CHECK is not free either: PostgreSQL
-- verifies it with a Phase-3 scan (no rewrite — relfilenode is unchanged).
-- Measured on postgres:18, a 1,000,000-row table:
--
--   | ALTER TABLE t ADD COLUMN …                          | time    |
--   |-----------------------------------------------------|---------|
--   | plain `text`                                        | 0.8 ms  |
--   | `text CHECK (c IN ('surface','drawer'))`            | 36.5 ms |
--   | `timestamptz`                                       | 0.9 ms  |
--
-- So the verification scan is real but two orders of magnitude below the
-- domain-type rewrite docs/migrations.md measured (689 ms at 1M rows, 2.0 s
-- at 300k), and `mailroom-migrate` is a one-shot the app waits on. A
-- deployment with a messages table orders of magnitude larger than that
-- should add the constraint NOT VALID and VALIDATE it separately instead of
-- copying this line verbatim; the resulting catalog state is identical.

-- +goose Up

-- ⚠ ADD COLUMN APPENDS, and db/schema.sql must mirror the resulting column
-- ORDER exactly — migrate-check diffs a pg_dump of this history against one
-- of the reference schema, column for column (the same note migration 00047
-- leaves for flagged_at).
ALTER TABLE app.messages
  ADD COLUMN line_side text CHECK (line_side IN ('surface','drawer')),
  ADD COLUMN line_side_at timestamptz;

-- The desk reads "which side is this on" for the caller's mail on every
-- render; only overridden rows are interesting, so the index is partial, and
-- it leads with user_id because RLS appends that predicate to every plan
-- (CLAUDE.md).
CREATE INDEX messages_line_side ON app.messages (user_id, line_side)
  WHERE line_side IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS app.messages_line_side;
ALTER TABLE app.messages DROP COLUMN line_side_at;
ALTER TABLE app.messages DROP COLUMN line_side;
