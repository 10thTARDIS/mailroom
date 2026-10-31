-- SPDX-License-Identifier: AGPL-3.0-only

-- Per-account sync-health telemetry (mailroom#43, spec §12 account-health panel).
-- One row per mail account records the outcome of the sync runner's last pass
-- over it: when it last attempted, when it last SUCCEEDED, the most recent error
-- and when it occurred, and a running count of consecutive failures. The panel
-- reads this on demand to show "synced 3h ago" or a stall/error.
--
-- This is OPERATIONAL telemetry, exactly like app.outbound_ledger: it is NOT a
-- JMAP-visible object, so a write here appends NO change_log row and it is NOT a
-- store.ObjectType. It is never fed to /changes, the indexer, or sync cursors;
-- it is read through a computed method (Account/getHealth), not synced. The sync
-- runner writes it fail-soft — a telemetry write must never fail a sync pass.
--
-- account_id is the PRIMARY KEY (one status row per account); the runner upserts
-- on it. user_id carries RLS the same way every user table does (FORCE RLS,
-- deny-by-default), and the (user_id, account_id) index leads with user_id so the
-- RLS-appended predicate is served by an index (CLAUDE.md).
--
-- Additive and forward-only (ADR-008): a brand-new table with no backfill. The
-- Down drops it cleanly.

-- +goose Up
CREATE TABLE app.account_sync_status (
  account_id           app.ulid PRIMARY KEY REFERENCES app.accounts(id) ON DELETE CASCADE,
  user_id              app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  last_attempt_at      timestamptz,
  last_success_at      timestamptz,
  last_error           text,
  last_error_at        timestamptz,
  consecutive_failures int NOT NULL DEFAULT 0,
  updated_at           timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE app.account_sync_status ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.account_sync_status FORCE ROW LEVEL SECURITY;
CREATE POLICY account_sync_status_isolation ON app.account_sync_status
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());
CREATE INDEX account_sync_status_user ON app.account_sync_status (user_id, account_id);

-- App-plane CRUD, RLS-constrained (mirrors the grant block in db/schema.sql).
-- Grants are per-table here (no ALL TABLES default), so a new table needs its own.
GRANT SELECT, INSERT, UPDATE, DELETE ON app.account_sync_status TO mailroom_app;

-- +goose Down
DROP TABLE app.account_sync_status;
