-- SPDX-License-Identifier: AGPL-3.0-only
--
-- A partial index over the ingest-only ledger (ADR-028 amendment A6).
--
-- `SuppressedIntentCount` runs on every Account/getHealth poll, and the
-- writeback-ledger report reads the same rows by op and by source. The table's
-- two partial indexes are `writeback_pending` (state IN ('pending','inflight'))
-- and `writeback_dead` (state = 'dead'), so a suppressed row was reachable only
-- through a scan of the user's intents: on a long shadow run against a large
-- mailbox the ledger is the biggest part of the table.
--
-- The index is PARTIAL on exactly the predicate the queries use, and it leads
-- with user_id like every hot index in this schema (RLS appends that predicate
-- to every plan). The predicate here must stay textually `state = 'suppressed'`
-- or the planner will not use it.
--
-- EXPAND-ONLY (ADR-008): one plain CREATE INDEX, no column or data change. The
-- table is small for any instance that has not run a shadow period, and the
-- partial predicate makes the index empty for it.

-- +goose Up
CREATE INDEX writeback_suppressed ON app.writeback_intents (user_id, account_id)
  WHERE state = 'suppressed';

-- +goose Down
DROP INDEX IF EXISTS app.writeback_suppressed;
