-- SPDX-License-Identifier: AGPL-3.0-only
--
-- RULE-SUGGESTION DISMISSALS. EXPAND-ONLY (ADR-008): one new table, nothing
-- backfilled or rewritten.
--
-- After the user files mail from one sender into an area by hand, again and
-- again, the move confirmation offers "Always put mail from this sender in
-- <area>?" (Area/ruleSuggestion). "Not now" is stored here so the offer does
-- not come back on the very next move: one row per (user, area, sender), the
-- time of the last "Not now". A row older than 30 days no longer suppresses
-- the offer; the server compares the time, nothing deletes old rows.
--
-- Keyed by area and ON DELETE CASCADE from it, so deleting an area takes its
-- dismissals with it. The address is citext (as messages.from_address is), so
-- a sender's capitalisation is not a second sender. The primary key leads with
-- user_id (every hot index does; RLS appends that predicate).
--
-- No change_log row: a dismissal is a private memory of "not now", not a
-- JMAP-visible object (no /get, no /changes, no state string), so the
-- load-bearing change_log invariant does not apply to it.
--
-- Grants include UPDATE: re-dismissing is an upsert that refreshes the time.

-- +goose Up

CREATE TABLE app.rule_suggestion_dismissals (
  user_id      app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  area_id      app.ulid NOT NULL REFERENCES app.areas(id) ON DELETE CASCADE,
  address      citext NOT NULL,
  dismissed_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, area_id, address)
);
ALTER TABLE app.rule_suggestion_dismissals ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.rule_suggestion_dismissals FORCE ROW LEVEL SECURITY;
CREATE POLICY rule_suggestion_dismissals_isolation ON app.rule_suggestion_dismissals
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());
GRANT SELECT, INSERT, UPDATE ON app.rule_suggestion_dismissals TO mailroom_app;

-- +goose Down
DROP TABLE IF EXISTS app.rule_suggestion_dismissals;
