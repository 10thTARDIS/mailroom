-- SPDX-License-Identifier: AGPL-3.0-only
--
-- Ingest-only mode: an account can be synced without Mailroom ever being able
-- to write to it (#521, ADR-028).
--
-- ⚠ THIS IS THE SWITCH THAT MUST EXIST BEFORE A REAL MAILBOX IS
-- ATTACHED. Today there is none: the only account-level control is
-- sync_enabled, which stops ingest too, and mailboxes.sync_scope is a
-- PULL-side, per-mailbox control that the writeback path never reads (verified:
-- sync_scope appears nowhere in internal/writeback/ nor internal/store/
-- writeback.go). So the moment an account is attached, Mailroom can mutate it.
--
-- WHY A NEW COLUMN AND NOT A THIRD sync_scope VALUE. Beyond deviating from
-- ADR-020: a MOVE has two mailboxes. A per-mailbox writeback switch has no
-- defined answer when a message moves from a shadowed folder into a live one.
-- sync_scope is also the pull direction and writeback is the push direction;
-- they are orthogonal axes. Conflating them would recreate exactly the defect
-- ADR-020 closed — an enum value whose name does not describe its behaviour.
-- ADR-020 is untouched by this migration.
--
-- Two booleans on the account give the honest matrix, and "ingest-only" is a
-- named point in it rather than a third thing:
--
--     sync_enabled=t, writeback_enabled=t  -> normal
--     sync_enabled=t, writeback_enabled=f  -> INGEST-ONLY (shadow)
--     sync_enabled=f, writeback_enabled=t  -> detached; queue drains on re-enable
--     sync_enabled=f, writeback_enabled=f  -> fully detached
--
-- DEFAULT true is deliberate and is the behaviour-preserving choice: every
-- existing account is writing back right now, and true is the value that says
-- so. The shadow account is opted IN explicitly, by the operator, once.

-- +goose Up

-- The per-account switch. See ADR-028 §1.
ALTER TABLE app.accounts
  ADD COLUMN IF NOT EXISTS writeback_enabled boolean NOT NULL DEFAULT true;

-- A fifth intent state: TERMINAL, and never claimed.
--
-- ⚠ ADD VALUE runs safely inside goose's per-migration transaction on
-- PostgreSQL 12+ as long as the new value is not USED in the same transaction;
-- it is not. Same pattern and same caveat as migration 00009. The
-- pending -> suppressed transition is runtime code (SuppressAccountIntents),
-- deliberately NOT a backfill here, so this constraint is satisfied.
--
-- WHY A LEDGER RATHER THAN DROPPING THE INTENTS. #521 proposed dropping them as
-- "the honest default". Dropping is the one option that reproduces #520's
-- disease by design: at lift, local state and the server disagree permanently
-- with nothing recording it. A suppressed intent is instead a countable,
-- per-op-class record of every change Mailroom WOULD have made to real mail —
-- which is the evidence the go-live decision should rest on. Lifting the mode
-- drains nothing; replay is an explicit --apply command.
ALTER TYPE app.intent_state ADD VALUE IF NOT EXISTS 'suppressed';

-- +goose Down

-- The column drops cleanly.
ALTER TABLE app.accounts DROP COLUMN IF EXISTS writeback_enabled;

-- The enum value does NOT: PostgreSQL cannot remove one, the same
-- forward-only reality migration 00009 recorded. Any row left in 'suppressed'
-- would be unreadable by a rolled-back binary, so the Down at least moves them
-- somewhere a reader understands. 'dead' is the honest landing place: it is the
-- existing terminal, already-surfaced state, and dead_reason says what happened.
-- ⚠ THE NO FORCE / FORCE SANDWICH IS LOAD-BEARING, NOT CEREMONY. Migrations run
-- as mailroom_owner, which has no BYPASSRLS and no policy of its own, so under
-- FORCE ROW LEVEL SECURITY this UPDATE would match ZERO rows on every real
-- database — silently, with no error and a successful migration (mailroom#505,
-- and the shape CLAUDE.md's measurement discipline is named after). It is
-- caught by TestMigrationDMLRespectsForceRLS, which is how this comment exists.
-- See db/migrations/00051_backfills_under_force_rls.sql and docs/migrations.md.
ALTER TABLE app.writeback_intents NO FORCE ROW LEVEL SECURITY;

UPDATE app.writeback_intents
   SET state = 'dead',
       dead_reason = COALESCE(dead_reason,
                              'writeback suppressed (ADR-028); state rolled back by migration 00056 Down')
 WHERE state = 'suppressed';

ALTER TABLE app.writeback_intents FORCE ROW LEVEL SECURITY;
