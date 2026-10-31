-- SPDX-License-Identifier: AGPL-3.0-only
--
-- RECEIVE-ONLY ACCOUNTS (#52 increment 6, ADR-046 §12).
--
-- EXPAND-ONLY (ADR-008). Two nullable columns appended to
-- app.account_sync_status (catalog-only, no rewrite, no backfill).
--
-- send_blocked_code / send_blocked_at: the account's provider refuses to take
-- mail from it as a policy — today only Microsoft's `535 5.7.139`, SMTP AUTH
-- disabled for the tenant or mailbox ('smtp_auth_disabled') — and since when.
-- While set, EmailSubmission/set refuses a new send up front and a queued
-- submission fails with a readable cause instead of dialling again. NULL = the
-- account can send. Like the rest of this table it is OPERATIONAL state, not a
-- JMAP-visible object: a write appends no change_log row, and
-- Account/getHealth reads it on demand.

-- +goose Up
ALTER TABLE app.account_sync_status ADD COLUMN send_blocked_code text;
ALTER TABLE app.account_sync_status ADD COLUMN send_blocked_at timestamptz;

-- +goose Down
ALTER TABLE app.account_sync_status DROP COLUMN send_blocked_at;
ALTER TABLE app.account_sync_status DROP COLUMN send_blocked_code;
