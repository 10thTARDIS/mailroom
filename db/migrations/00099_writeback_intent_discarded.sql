-- SPDX-License-Identifier: AGPL-3.0-only
--
-- A sixth writeback intent state, 'discarded': the end of the ingest-only
-- ledger's lifecycle (ADR-028 amendment A3).
--
-- The ledger a shadow run accumulates (state 'suppressed') can be replayed with
-- `mailroom-admin writeback-ledger --apply`, but until now there was no way to
-- decide NOT to replay it: "I will never apply these" was a standing state
-- rather than a terminal one, so the ledger could only grow, was counted in
-- every Account/getHealth poll for ever, and a stray --apply months later could
-- still fire it. `--discard --apply` moves the rows here.
--
-- 'discarded' is TERMINAL and NEVER CLAIMED, like 'suppressed'. It is a new
-- value rather than a reuse of 'dead' because 'dead' means "the engine tried and
-- failed", is surfaced to the user as a dead letter, and is read by
-- Account/getHealth; a deliberate decision by the operator is none of those.
--
-- ⚠ ADD VALUE runs safely inside goose's per-migration transaction on
-- PostgreSQL 12+ as long as the new value is not USED in the same transaction;
-- it is not (the transition is runtime code, `--discard --apply`). Same pattern
-- and same caveat as migrations 00009 and 00056.
--
-- EXPAND-ONLY (ADR-008): one enum value, no column and no data change.

-- +goose Up
ALTER TYPE app.intent_state ADD VALUE IF NOT EXISTS 'discarded';

-- +goose Down
-- PostgreSQL cannot remove an enum value, the forward-only reality migrations
-- 00009 and 00056 recorded. A row left in 'discarded' would be unreadable by a
-- rolled-back binary, so this moves them somewhere it understands. 'dead' is the
-- existing terminal state; dead_reason says what happened.
--
-- ⚠ THE NO FORCE / FORCE SANDWICH IS LOAD-BEARING, NOT CEREMONY (see 00056 and
-- docs/migrations.md): migrations run as mailroom_owner, which has no BYPASSRLS
-- and no policy of its own, so under FORCE ROW LEVEL SECURITY this UPDATE would
-- match ZERO rows, silently, on every real database.
ALTER TABLE app.writeback_intents NO FORCE ROW LEVEL SECURITY;

UPDATE app.writeback_intents
   SET state = 'dead',
       dead_reason = COALESCE(dead_reason,
                              'discarded by the operator (ADR-028); state rolled back by migration 00099 Down')
 WHERE state = 'discarded';

ALTER TABLE app.writeback_intents FORCE ROW LEVEL SECURITY;
