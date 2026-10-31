-- SPDX-License-Identifier: AGPL-3.0-only
--
-- ONE MORE REFUSAL REASON: 'pending_cap' (#852, ADR-042 §7).
--
-- EXPAND-ONLY (ADR-008). A key may have at most MaxPendingPerKey requests
-- waiting for a person; a hold past that is refused, and the refusal is
-- audited with its own reason so the per-minute caps on refusals count it and
-- the audit says why. The two reason CHECKs (app.api_key_events and
-- app.api_key_suppressed_refusals) are widened by one value: every existing
-- row satisfies the new list because the old list is a subset of it, so the
-- re-validation scans tables that are small and changes nothing. Same
-- constraint names as the inline CHECKs of 00074, so db/schema.sql keeps them
-- inline and the drift check sees the same dump.

-- +goose Up
ALTER TABLE app.api_key_events DROP CONSTRAINT api_key_events_reason_check;
ALTER TABLE app.api_key_events ADD CONSTRAINT api_key_events_reason_check
  CHECK (reason IN ('method', 'session_only', 'restriction', 'arguments', 'scope', 'rate', 'budget', 'approval', 'mass_delete', 'mass_send', 'pending_cap'));
ALTER TABLE app.api_key_suppressed_refusals DROP CONSTRAINT api_key_suppressed_refusals_reason_check;
ALTER TABLE app.api_key_suppressed_refusals ADD CONSTRAINT api_key_suppressed_refusals_reason_check
  CHECK (reason IN ('method', 'session_only', 'restriction', 'arguments', 'scope', 'rate', 'budget', 'approval', 'mass_delete', 'mass_send', 'pending_cap'));

-- +goose Down
-- The rows that carry the new value are folded into 'arguments' (events) or
-- dropped (suppressed counts) so the narrower CHECK can be restored. Both
-- tables are FORCE RLS and migrations run as mailroom_owner, so the DML runs
-- with FORCE lifted for the statement (mailroom#505, docs/migrations.md).
ALTER TABLE app.api_key_suppressed_refusals NO FORCE ROW LEVEL SECURITY;
DELETE FROM app.api_key_suppressed_refusals WHERE reason = 'pending_cap';
ALTER TABLE app.api_key_suppressed_refusals FORCE ROW LEVEL SECURITY;
ALTER TABLE app.api_key_events NO FORCE ROW LEVEL SECURITY;
UPDATE app.api_key_events SET reason = 'arguments' WHERE reason = 'pending_cap';
ALTER TABLE app.api_key_events FORCE ROW LEVEL SECURITY;
ALTER TABLE app.api_key_suppressed_refusals DROP CONSTRAINT api_key_suppressed_refusals_reason_check;
ALTER TABLE app.api_key_suppressed_refusals ADD CONSTRAINT api_key_suppressed_refusals_reason_check
  CHECK (reason IN ('method', 'session_only', 'restriction', 'arguments', 'scope', 'rate', 'budget', 'approval', 'mass_delete', 'mass_send'));
ALTER TABLE app.api_key_events DROP CONSTRAINT api_key_events_reason_check;
ALTER TABLE app.api_key_events ADD CONSTRAINT api_key_events_reason_check
  CHECK (reason IN ('method', 'session_only', 'restriction', 'arguments', 'scope', 'rate', 'budget', 'approval', 'mass_delete', 'mass_send'));
