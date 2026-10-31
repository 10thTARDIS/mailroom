-- SPDX-License-Identifier: AGPL-3.0-only

-- In-UI account management (mailroom#98/#45, spec §12): a user can now add,
-- edit, and remove their own IMAP/SMTP accounts through JMAP (Account/get,
-- Account/query, Account/changes, Account/set) rather than only through the
-- operator's boot-time accounts.json.
--
-- An Account is therefore now a JMAP-VISIBLE object: every create/update/destroy
-- of one MUST append a change_log row in the same transaction as the mutation
-- (the load-bearing invariant — CLAUDE.md), so /changes, the indexer, and client
-- sync cursors see account mutations like any other object. That row's
-- object_type is 'Account', which the app.object_type enum did not carry until
-- now. This migration adds exactly that value.
--
-- Additive and forward-only (ADR-008): a new enum value cannot be dropped, so
-- the Down is a documented no-op (removing an enum value is unsupported by
-- PostgreSQL and would require rewriting every dependent column). ADD VALUE is
-- appended at the END of the enum so a pg_dump of a database built from this
-- history is byte-identical to one built from db/schema.sql, where 'Account' is
-- likewise the last object_type value (scripts/migrate-check.sh).
--
-- ADD VALUE runs safely inside goose's per-migration transaction on PostgreSQL
-- 12+ as long as the new value is not USED in the same transaction; it is not.

-- +goose Up
ALTER TYPE app.object_type ADD VALUE IF NOT EXISTS 'Account';

-- +goose Down
-- No-op: PostgreSQL cannot remove an enum value. Left forward-only by design.
SELECT 1;
