-- SPDX-License-Identifier: AGPL-3.0-only
--
-- THE DRAWER AUTO-ARCHIVE JOB (mailroom#443, Direction 7 "Desk and Drawers",
-- #372): below the line, each area's DRAWER holds mail the desk judged
-- unimportant. A drawer AUTOMATICALLY ARCHIVES what has sat in it longer than
-- the area's period (default 30 days, per-area configurable, decided
-- 2026-09-07). Archive is a MOVE ON THE SERVER to the account's
-- Archive folder, so every other client agrees about what is in the inbox.
-- NEVER DELETE. NEVER MARK AS READ — only an explicit user rule may do either
-- (a firm requirement). The in-tray (flagged) and everything above
-- the line are never touched, whatever their age. The rules engine has no
-- notion of age, so this is a durable job in the shape of the purge sweep
-- (internal/purge), not a rule.
--
-- Measured on a real reference mailbox: a large part of the inbox was
-- older than 30 days — the FIRST run is a large,
-- visible event (an accepted large writeback event), and it must be
-- previewed by a report, never discovered (`mailroom-admin drawer-report`,
-- CLAUDE.md "build the report before the fix").
--
-- # auto_archive vs. auto_archive_days: two questions, not one
--
-- Migration 00046 made auto_archive_days NULL mean "the drawer never
-- auto-archives"; that conflated two questions — "does this drawer sweep at
-- all" and "how long is its period" — and left no way to say "use the login
-- default", which is the setting the design actually promises (default 30 d,
-- per-area overridable). From this migration on:
--
--   auto_archive        = whether this drawer sweeps at all (default ON)
--   auto_archive_days   = the period; NULL = inherit desk_preferences'
--                         drawer_default_days rather than "never"
--
-- CATALOG-ONLY, per docs/migrations.md's ADD COLUMN warning: a plain boolean,
-- not a domain type, so this is the 2 ms case even on app.areas at scale.
--
-- # drawer_sweep_enabled: the master switch, because auto_archive is not enough
--
-- auto_archive above is a PER-AREA switch, but the plan this same increment
-- ships materialises five areas, all on by default (#443). Left as is,
-- deploying this migration starts moving mail within the hour of first
-- boot — EnsureScheduled seeds the job at boot, and the job is on an hourly
-- timer — before the operator has ever run `mailroom-admin drawer-report`.
-- That is exactly the failure "build the report before the fix" exists to
-- prevent (CLAUDE.md; #443's own report doc comment: "the first run is a
-- large, visible event ... must be previewed, not discovered").
--
-- So there is also ONE login-level switch, default OFF, that gates the whole
-- sweep regardless of any area's own auto_archive: internal/drawer's Sweep
-- reads it FIRST and archives nothing while it is false, but keeps
-- rescheduling on the ordinary hourly interval so the chain never stops
-- ticking — the moment the switch flips, the very next run does the work.
-- `mailroom-admin drawer-report` names the current state and `--enable`
-- flips it; a new login's day-one setup (#446) is where turning it on by
-- default, for a fresh account with no history to preview, will eventually
-- live. Until then: nothing moves until someone flips it deliberately.

-- +goose Up

ALTER TABLE app.areas ADD COLUMN auto_archive boolean NOT NULL DEFAULT true;

ALTER TABLE app.desk_preferences ADD COLUMN drawer_sweep_enabled boolean NOT NULL DEFAULT false;

-- The sweep's own audit table and idempotency guard, both at once.
--
-- WHY A TABLE AND NOT A COLUMN ON app.messages:
--
--   1. "archived by drawer rule" must be answerable per message in All Mail
--      (Email.drawerArchivedAt) without a column nobody else on the message
--      row would ever read.
--   2. Settings needs "archived N so far" per area (Area/getSummary's
--      archivedByDrawer) — one aggregate query, not a scan of every message.
--   3. IDEMPOTENCY. A message the user moves back to the inbox after a sweep
--      would otherwise be old enough again and get swept a second time; a row
--      here means "swept once, never again" — the same reasoning
--      app.superseded_content and app.drawer_archives' own sibling tables
--      already give for a fact that must survive the state it describes
--      changing back.
--
-- NOT a JMAP-visible object of its own: app.object_type carries no
-- 'DrawerArchive' value, and no change_log row is appended for this table.
-- It surfaces only as a derived Email property and a per-area count.
-- +goose StatementBegin
CREATE TABLE app.drawer_archives (
  user_id      app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  message_id   app.ulid NOT NULL REFERENCES app.messages(id) ON DELETE CASCADE,
  -- SET NULL, not CASCADE: deleting an area must not erase the record that its
  -- drawer archived this message — area_name (below) keeps the history
  -- legible after the area itself is gone, the same reasoning
  -- rule_corrections' rule_id takes for a deleted rule.
  area_id      app.ulid REFERENCES app.areas(id) ON DELETE SET NULL,
  area_name    text NOT NULL,
  period_days  int  NOT NULL CHECK (period_days > 0),
  archived_at  timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, message_id)
);
-- +goose StatementEnd

-- Settings' "archived N so far" per area, and the ordering a client would want
-- for a "recently archived" view. Leads with user_id so RLS's appended
-- predicate is served by the same scan (CLAUDE.md).
CREATE INDEX drawer_archives_by_area ON app.drawer_archives (user_id, area_id, archived_at);

ALTER TABLE app.drawer_archives ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.drawer_archives FORCE ROW LEVEL SECURITY;
CREATE POLICY drawer_archives_isolation ON app.drawer_archives
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());
GRANT SELECT, INSERT, UPDATE, DELETE ON app.drawer_archives TO mailroom_app;

-- +goose Down
DROP TABLE IF EXISTS app.drawer_archives;
ALTER TABLE app.desk_preferences DROP COLUMN drawer_sweep_enabled;
ALTER TABLE app.areas DROP COLUMN auto_archive;
