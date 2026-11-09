-- SPDX-License-Identifier: AGPL-3.0-only
--
-- The Activity view (ADR-053, #45) reads two logs newest-first for one user, and
-- neither had an index that serves that:
--
--   * app.audit_log has only its identity key, so a per-user read scans the table
--     filtered by the RLS predicate. Purge tombstones make it the largest log.
--   * app.api_key_events is keyed (user_id, key_id, at): by key first, so "this
--     user's events, newest" sorts every row of every key.
--
-- EXPAND-ONLY (ADR-008): two plain CREATE INDEX statements, no column or data
-- change. The api_key_events index is PARTIAL, over exactly the rows the view
-- shows (a refusal or anything not plainly ok, a write, a delete or a send): the
-- view never lists a routine read, and the predicate here must stay textually the
-- one in internal/store/activity.go or the planner will not use it.

-- +goose Up
CREATE INDEX audit_log_user_object_time ON app.audit_log (user_id, object_type, created_at DESC, id DESC);
CREATE INDEX api_key_events_user_activity ON app.api_key_events (user_id, at DESC, id DESC)
  WHERE outcome <> 'ok' OR method LIKE '%/set' OR deletes > 0 OR sends > 0;

-- +goose Down
DROP INDEX IF EXISTS app.api_key_events_user_activity;
DROP INDEX IF EXISTS app.audit_log_user_object_time;
