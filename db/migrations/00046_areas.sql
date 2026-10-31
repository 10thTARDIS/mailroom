-- SPDX-License-Identifier: AGPL-3.0-only

-- THE DESK'S AREAS, AS PERSISTENT STATE (mailroom#442 increment 1).
--
-- Increment 0 (merged) shipped the resolver (internal/areas) and a read-only
-- report over it — "build the report before the fix" (CLAUDE.md): before
-- anything durable existed, an operator could already see where a plan sent
-- every message and where it left gaps. This migration is the object the
-- report was rehearsing for: the desk's rebuild groups every message into
-- exactly one AREA, and that grouping has to be SERVER-SIDE STATE for three
-- reasons, none of which is "it would be nice to persist it":
--
--   1. SETTINGS THAT MUST AGREE BETWEEN DEVICES. Which tabs and labels feed
--      which area, the auto-archive period, whether an area is pinned to the
--      rail — these are exactly the kind of per-user arrangement spec §13/§14
--      already sync through the change log (saved searches, category tabs,
--      compose snippets). An area plan that lived only in one client's local
--      storage would be re-built by hand on every other device.
--   2. COUNTS THAT MUST BE ONE QUERY. The desk's rail shows a Surface/Drawer
--      count per area on every render. Recomputing internal/areas.Resolve in
--      application code for every live message, on every request, to answer
--      "how many" is the kind of cost a SQL aggregate exists to avoid — hence
--      areaExprSQL below and its use in AreaSummary (areas_resolve.go).
--   3. THE RESOLVER MUST BE ONE IMPLEMENTATION FOR TWO CONSUMERS. Both
--      Email/query's area filter and the summary counts have to agree with
--      whatever internal/areas.Resolve would say about the SAME message, or a
--      client filtering to an area could see a count that disagrees with the
--      list it renders (#155's "a preview that can disagree with the engine is
--      worse than none", restated for a third resolver).
--
-- FEEDS ARE ROWS, NOT JSONB, for the same reason category_tabs.tab and
-- labels.tab are columns rather than a mapping blob (migration 00028): a
-- "feeds at most one area" invariant enforced by application code is a bug
-- that has not been triggered yet, while a UNIQUE partial index enforces it
-- for every writer, present and future, including a script run directly
-- against the database. CLAUDE.md's "JSONB is never used for queryable
-- state" names exactly this shape.
--
-- THE AREA OWNS A LABEL, per #442's decision ("areas are labels"): placing a
-- message in an area is applying that area's own label. This is why
-- app.areas.label_id is NOT NULL and ON DELETE RESTRICT rather than SET NULL
-- or CASCADE — an area with no label could not receive a message at all (the
-- write path IS "apply this label"), so a label deletion that silently
-- orphaned an area would leave a rail entry nothing could ever file into.
-- Deleting the area is the correct way to give up the label; deleting the
-- label out from under a live area is refused by the database, not merely by
-- convention.
--
-- FEEDS_DESK IS A MAILBOX COLUMN, not an areas table, because it answers a
-- narrower and unrelated question: "folders feed the desk" (#446) — whether
-- mail with live provenance in THIS folder counts as unactioned exactly like
-- INBOX, for the purposes of AreaSummary's own live-mail predicate. It has
-- nothing to do with which area a message falls into once it counts, so it
-- rides on the object it actually describes (the same reasoning sync_scope and
-- accepts_keywords already follow) rather than growing a second, parallel
-- "which mailboxes matter" table.
--
-- DESK_PREFERENCES AND CLIENT_PREFERENCES ride along in this migration because
-- they are the other two persistent objects the desk rebuild needs and they
-- share every one of the reasons above: settings (the Surface/Drawer score
-- threshold, per-client density and pane layout) that must agree across a
-- person's own devices. DESK_PREFERENCES IS A SINGLETON keyed on user_id
-- itself — there is exactly one desk per user in 0.5 — so its change_log
-- object_id is the user's own id rather than a manufactured row id.
-- CLIENT_PREFERENCES IS NOT a singleton: a phone and a browser want different
-- density and pane defaults, so it is keyed on (user_id, family) instead.

-- +goose Up

ALTER TYPE app.object_type ADD VALUE IF NOT EXISTS 'Area';
ALTER TYPE app.object_type ADD VALUE IF NOT EXISTS 'DeskPreferences';
ALTER TYPE app.object_type ADD VALUE IF NOT EXISTS 'ClientPreferences';

-- Which surface asked, for CLIENT_PREFERENCES below. A closed vocabulary for
-- the same reason app.category_tab is one: a sixth kind of client needs code
-- to render it regardless of how the row is stored.
CREATE TYPE app.client_family AS ENUM ('web', 'phone', 'desktop');

-- +goose StatementBegin
CREATE TABLE app.areas (
  id                app.ulid PRIMARY KEY,
  user_id           app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  name              text NOT NULL,
  -- Explicit ordering: the rail is a place a person arranges, and it is also
  -- the tie-break Resolve/areaExprSQL use when a message's labels feed more
  -- than one area (earlier position wins) — the identical role
  -- tabs.Precedence() plays for the tab layer, stated as data here because
  -- area order is the operator's own arrangement with no protocol fact behind
  -- it (same reasoning as saved_searches.position).
  position          int NOT NULL DEFAULT 0,
  -- THE AREA'S OWN LABEL (#442, "areas are labels"): placing a message in the
  -- area applies this label, which is also how a labelled message resolves
  -- BACK to its area through areaExprSQL's label-fed arm. ON DELETE RESTRICT,
  -- not CASCADE: an area with no label could not receive a message at all, so
  -- a label deletion that silently orphaned an area would leave a rail entry
  -- nothing could ever file into. Delete the area instead.
  label_id          app.ulid NOT NULL REFERENCES app.labels(id) ON DELETE RESTRICT,
  -- NULL = the drawer never auto-archives; otherwise the dwell period in days
  -- before the drawer half of this area's mail is swept. Bounded to positive
  -- values because zero or negative would mean "immediately", which is not a
  -- period at all and is better expressed as "off" (NULL).
  auto_archive_days int CHECK (auto_archive_days IS NULL OR auto_archive_days > 0),
  -- An area a person keeps for its counts or its automation but does not want
  -- cluttering the rail. Never affects resolution: a hidden area still owns
  -- its feeds and its label exactly as a visible one does.
  hidden_from_rail  boolean NOT NULL DEFAULT false,
  created_at        timestamptz NOT NULL DEFAULT now(),
  updated_at        timestamptz NOT NULL DEFAULT now(),
  -- One area per label: the same "at most one" invariant area_feeds enforces
  -- for a label reused as an ordinary feed, extended to the label an area
  -- itself owns (which is otherwise indistinguishable from any other feed once
  -- messages are labelled with it).
  UNIQUE (user_id, label_id)
);
-- +goose StatementEnd

-- Case-insensitive name uniqueness, the same shape as materialiseCategoryTabs'
-- reasoning for label reuse below: two areas that differ only by case would
-- read as duplicates on the rail, and the default materialisation itself
-- depends on being able to ask "does an area named this already exist"
-- unambiguously.
CREATE UNIQUE INDEX areas_user_lower_name ON app.areas (user_id, lower(name));

ALTER TABLE app.areas ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.areas FORCE ROW LEVEL SECURITY;
CREATE POLICY areas_isolation ON app.areas
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());
GRANT SELECT, INSERT, UPDATE, DELETE ON app.areas TO mailroom_app;

-- +goose StatementBegin
CREATE TABLE app.area_feeds (
  id         app.ulid PRIMARY KEY,
  user_id    app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  area_id    app.ulid NOT NULL REFERENCES app.areas(id) ON DELETE CASCADE,
  tab        app.category_tab,
  label_id   app.ulid REFERENCES app.labels(id) ON DELETE CASCADE,
  created_at timestamptz NOT NULL DEFAULT now(),
  -- A feed is a tab OR a label, never both and never neither — a row that
  -- named neither would feed nothing, and one naming both would make "which
  -- one decided this" ambiguous for areaExprSQL's label-vs-tab precedence.
  CONSTRAINT area_feeds_exactly_one CHECK ((tab IS NULL) <> (label_id IS NULL))
);
-- +goose StatementEnd

-- "A LABEL OR TAB MAY FEED AT MOST ONE AREA" (internal/areas' own doc comment,
-- #442), enforced HERE rather than trusted to application code. A tab or label
-- feeding two areas would make a message's area depend on which area's row the
-- resolver happened to see first — exactly the ambiguity internal/areas.
-- Validate refuses in the file-based plan, extended to the persisted one so a
-- concurrent write, a bulk script, or a bug cannot recreate the state Validate
-- exists to reject.
CREATE UNIQUE INDEX area_feeds_tab_once   ON app.area_feeds (user_id, tab)      WHERE tab IS NOT NULL;
CREATE UNIQUE INDEX area_feeds_label_once ON app.area_feeds (user_id, label_id) WHERE label_id IS NOT NULL;
-- The store's own listing/summary access path: every feed of one area.
CREATE INDEX area_feeds_area ON app.area_feeds (user_id, area_id);

ALTER TABLE app.area_feeds ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.area_feeds FORCE ROW LEVEL SECURITY;
CREATE POLICY area_feeds_isolation ON app.area_feeds
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());
GRANT SELECT, INSERT, UPDATE, DELETE ON app.area_feeds TO mailroom_app;

-- A feed is PART OF THE AREA OBJECT for JMAP purposes, not an object of its
-- own: app.object_type carries no 'AreaFeed' value, and the store appends the
-- Area change_log row for the OWNING area when a feed changes, never a row of
-- its own. A client that fetches Area/get sees the whole feed list on one
-- object; there is nothing a separate AreaFeed type would let it sync that the
-- Area object does not already carry.

-- +goose StatementBegin
CREATE TABLE app.desk_preferences (
  -- A SINGLETON per user (0.5 has exactly one desk), so the change_log
  -- object_id for it is the user_id itself rather than a manufactured row id
  -- — the same shape category_tabs would need if a user could have only one.
  user_id             app.ulid PRIMARY KEY REFERENCES app.users(id) ON DELETE CASCADE,
  -- The Surface/Drawer split: a message scores >= this is Surface, below it is
  -- Drawer (AreaSummary). Bounded to [0,1] because the underlying triage score
  -- is itself a probability (spec §8) and a threshold outside that range could
  -- never be crossed either way, which is a silently broken desk rather than
  -- an unusual one.
  line_threshold      real NOT NULL DEFAULT 0.5 CHECK (line_threshold >= 0 AND line_threshold <= 1),
  -- How long the Drawer holds a message before an area's own auto-archive (if
  -- any) sweeps it, absent a per-area override. Bounded to positive: zero
  -- would mean "instantly", which is not a dwell period.
  drawer_default_days int  NOT NULL DEFAULT 30 CHECK (drawer_default_days > 0),
  -- How long a Tray item sits before the desk nags about it. Same positivity
  -- reasoning as the column above.
  tray_nag_days       int  NOT NULL DEFAULT 7  CHECK (tray_nag_days > 0),
  updated_at          timestamptz NOT NULL DEFAULT now()
);
-- +goose StatementEnd

ALTER TABLE app.desk_preferences ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.desk_preferences FORCE ROW LEVEL SECURITY;
CREATE POLICY desk_preferences_isolation ON app.desk_preferences
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());
GRANT SELECT, INSERT, UPDATE, DELETE ON app.desk_preferences TO mailroom_app;

-- +goose StatementBegin
CREATE TABLE app.client_preferences (
  id             app.ulid PRIMARY KEY,
  user_id        app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  -- NOT a singleton like desk_preferences: a phone and a browser legitimately
  -- want different density and pane defaults, so the row is keyed on which
  -- KIND of client asked, not on the user alone.
  family         app.client_family NOT NULL,
  theme          text NOT NULL DEFAULT 'system' CHECK (theme IN ('system', 'light', 'dark')),
  density        text NOT NULL DEFAULT 'normal' CHECK (density IN ('compact', 'normal', 'relaxed')),
  message_opens  text NOT NULL DEFAULT 'beside' CHECK (message_opens IN ('beside', 'focused')),
  pane_split_pct int  NOT NULL DEFAULT 58 CHECK (pane_split_pct BETWEEN 20 AND 80),
  updated_at     timestamptz NOT NULL DEFAULT now(),
  UNIQUE (user_id, family)
);
-- +goose StatementEnd

ALTER TABLE app.client_preferences ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.client_preferences FORCE ROW LEVEL SECURITY;
CREATE POLICY client_preferences_isolation ON app.client_preferences
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());
GRANT SELECT, INSERT, UPDATE, DELETE ON app.client_preferences TO mailroom_app;

-- "FOLDERS FEED THE DESK" (#446): mail with live provenance in a mailbox
-- flagged here counts as unactioned exactly like INBOX, for AreaSummary's own
-- live-mail predicate. A Mailbox property, not an areas-side table, because it
-- answers a question about the MAILBOX (does its mail count as unactioned) that
-- has nothing to do with which area a message falls into once it does — the
-- same reasoning that keeps sync_scope and accepts_keywords on this table
-- rather than a parallel one. Catalog-only ADD COLUMN (plain boolean, not a
-- domain type — see docs/migrations.md's ADD COLUMN warning), so this is cheap
-- even on a large mailboxes table. The store appends the Mailbox change_log
-- row when it flips (SetMailboxFeedsDesk, mirroring SetMailboxSyncScope).
--
-- Declared last to match the column order this ALTER produces; migrate-check
-- diffs db/schema.sql against the migration history and column order counts.
ALTER TABLE app.mailboxes ADD COLUMN feeds_desk boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE app.mailboxes DROP COLUMN feeds_desk;
DROP TABLE IF EXISTS app.client_preferences;
DROP TABLE IF EXISTS app.desk_preferences;
DROP TABLE IF EXISTS app.area_feeds;
DROP TABLE IF EXISTS app.areas;
DROP TYPE IF EXISTS app.client_family;
-- app.object_type's 'Area', 'DeskPreferences' and 'ClientPreferences' are
-- deliberately NOT removed: PostgreSQL cannot drop an enum value, and a
-- change_log row referencing one may already exist. Forward-only (ADR-008),
-- exactly as 00028 left 'CategoryTab' and 00024 left 'SavedSearch'.
