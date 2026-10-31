-- SPDX-License-Identifier: AGPL-3.0-only

-- Per-account presentation: a user-chosen color for an account (mailroom#26,
-- multi-account unified inbox). The color lets a client tint an account's mail
-- in a unified view so a message's origin account is visible at a glance; it is
-- purely presentational and carries no sync semantics.
--
-- Sync SCOPE is deliberately NOT duplicated here: it is already per-mailbox
-- (app.mailboxes.sync_scope) with a per-account on/off in app.accounts
-- .sync_enabled, so there is nothing to add on the account for scope. Only the
-- color is new.
--
-- Additive and nullable (NULL = "no color chosen"): no backfill, no rewrite
-- (ADR-008 expand-and-contract). The column is appended at the END of
-- app.accounts so this history and the reference DDL in db/schema.sql produce
-- byte-identical pg_dump output (scripts/migrate-check.sh).

-- +goose Up
ALTER TABLE app.accounts ADD COLUMN color text;

-- +goose Down
ALTER TABLE app.accounts DROP COLUMN color;
