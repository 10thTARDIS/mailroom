-- SPDX-License-Identifier: AGPL-3.0-only
--
-- FOLLOW-UP NUDGES (#58). EXPAND-ONLY (ADR-008): three columns with constant
-- defaults and one new table. Nothing is backfilled or rewritten; every ADD
-- COLUMN is catalog-only, so docs/migrations.md's FORCE-RLS backfill rule does
-- not arise.
--
-- Nudges themselves are NOT stored. They are computed when asked for
-- (FollowUp/get) from the mail that already exists, because a nudge is a
-- judgement about a thread's state NOW: a stored nudge would go stale the
-- moment a reply arrived, and keeping it fresh would need a second write path
-- beside every ingest. What is stored is only what the computation cannot
-- derive: the user's settings and the user's own "Done".
--
-- 1. app.desk_preferences follow-up settings, on the per-user singleton that
--    already holds quiet hours and learned placement's switch:
--      nudge_outbound_enabled  "no one answered your mail" — ON by default
--                              (measured on a real mailbox: rare, and no
--                              late answer by day 30 in the observable cases).
--      nudge_inbound_enabled   "you haven't replied to this" — OFF by default
--                              (measured precision was low: opt-in only).
--      nudge_inbound_days      the inbound delay, default 3 days.
--
-- 2. app.nudge_dismissals: the user's "Done" on one nudge. Keyed by message:
--    a dismissed message never resurfaces as a nudge of either kind. ON
--    DELETE CASCADE from the message, so a purged message takes its
--    dismissal with it. The primary key leads with user_id (every hot index
--    does; RLS appends that predicate).

-- +goose Up

ALTER TABLE app.desk_preferences ADD COLUMN nudge_outbound_enabled boolean NOT NULL DEFAULT true;
ALTER TABLE app.desk_preferences ADD COLUMN nudge_inbound_enabled boolean NOT NULL DEFAULT false;
ALTER TABLE app.desk_preferences ADD COLUMN nudge_inbound_days smallint NOT NULL DEFAULT 3
  CHECK (nudge_inbound_days >= 1 AND nudge_inbound_days <= 14);

CREATE TABLE app.nudge_dismissals (
  user_id      app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  message_id   app.ulid NOT NULL REFERENCES app.messages(id) ON DELETE CASCADE,
  kind         text NOT NULL CHECK (kind IN ('outbound', 'inbound')),
  dismissed_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, message_id)
);
ALTER TABLE app.nudge_dismissals ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.nudge_dismissals FORCE ROW LEVEL SECURITY;
CREATE POLICY nudge_dismissals_isolation ON app.nudge_dismissals
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());
-- Written once and gone only with its message: no UPDATE (as category_dismissals).
GRANT SELECT, INSERT, DELETE ON app.nudge_dismissals TO mailroom_app;

-- +goose Down
DROP TABLE IF EXISTS app.nudge_dismissals;
ALTER TABLE app.desk_preferences DROP COLUMN IF EXISTS nudge_inbound_days;
ALTER TABLE app.desk_preferences DROP COLUMN IF EXISTS nudge_inbound_enabled;
ALTER TABLE app.desk_preferences DROP COLUMN IF EXISTS nudge_outbound_enabled;
