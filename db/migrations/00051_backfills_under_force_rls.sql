-- SPDX-License-Identifier: AGPL-3.0-only
--
-- BACKFILLS UNDER FORCE RLS SILENTLY MATCH ZERO ROWS (mailroom#505).
--
-- Measured on a deployed instance: migrations run as
-- mailroom_owner (deploy/compose.yml `migrate` service), and mailroom_owner
-- has rolbypassrls = false. Every user table carries ENABLE + FORCE ROW
-- LEVEL SECURITY and exactly one policy, granted to mailroom_app only. Under
-- FORCE, the table owner is subject to RLS same as any other role without
-- BYPASSRLS — and with no policy that applies to mailroom_owner, it sees no
-- rows at all. So 00050's
--
--   UPDATE app.desk_preferences SET setup_completed_at = updated_at;
--
-- matched ZERO rows. goose printed OK (an UPDATE affecting nothing is not an
-- error), and on the instance both logins still read setup_completed_at IS
-- NULL — the exact state the day-one wizard treats as "never set up", which
-- is the one outcome that backfill existed to prevent for an established
-- desk. 00047's
--
--   UPDATE app.messages SET flagged_at = received_at WHERE is_flagged AND flagged_at IS NULL;
--
-- has the identical property; it happened to have no candidate rows on the
-- instance, so the defect was invisible there, not absent.
-- scripts/migrate-check.sh applies migrations to an EMPTY database, where
-- every backfill's true row count is zero anyway — it could never have
-- caught this.
--
-- THE FIX, and why not the other two shapes that look plausible:
--
--   - NOT a policy granted to mailroom_owner. That would be a PERMANENT
--     widening of the schema to cover a ONE-OFF write; the next migration's
--     author would have to remember it exists and reason about whether it
--     applies to them too. The owner should stay policy-free between
--     migrations, the same as it is today.
--   - NOT `SET row_security = off`. That setting only suppresses policy
--     enforcement for a role that WOULD otherwise have a matching policy
--     evaluated (a role the table's RLS design accounts for) — it raises an
--     error for a role with no BYPASSRLS attribute when FORCE is on and no
--     policy would apply, which is exactly mailroom_owner's situation here.
--   - The fix used below: toggle FORCE off for the statement's duration and
--     back on immediately after, inside the SAME transaction as the DML.
--     goose wraps this file in one transaction by default (this file carries
--     none of the opt-out annotations docs/migrations.md describes for
--     splitting a migration out of its transaction), so a failure between
--     the two ALTERs rolls back the whole file — FORCE can never be left
--     off by a partial application of this migration.
--
-- Both UPDATEs below are re-runs of 00050's and 00047's, guarded by the same
-- IS NULL conditions those migrations already had (or, for messages, the
-- same is_flagged AND flagged_at IS NULL predicate) — idempotent, so a login
-- that completed day-one setup between the 00050 deploy and this migration
-- keeps its own timestamp rather than having it overwritten.
--
-- db/schema.sql is UNCHANGED by this migration: FORCE ROW LEVEL SECURITY is
-- the end state for both tables, exactly as schema.sql already declares it,
-- and no column or object changes shape. This file is pure DML wrapped in a
-- transient RLS toggle.

-- +goose Up

ALTER TABLE app.desk_preferences NO FORCE ROW LEVEL SECURITY;
UPDATE app.desk_preferences SET setup_completed_at = updated_at WHERE setup_completed_at IS NULL;
ALTER TABLE app.desk_preferences FORCE ROW LEVEL SECURITY;

ALTER TABLE app.messages NO FORCE ROW LEVEL SECURITY;
UPDATE app.messages SET flagged_at = received_at WHERE is_flagged AND flagged_at IS NULL;
ALTER TABLE app.messages FORCE ROW LEVEL SECURITY;

-- +goose Down
-- Forward-only (ADR-008): re-running the down side of 00047/00050 already
-- documents that down-migrations are not relied on against real data, and
-- this migration is pure re-applied DML with no new object to drop.
