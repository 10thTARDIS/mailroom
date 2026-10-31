-- SPDX-License-Identifier: AGPL-3.0-only

-- GMAIL-STYLE CATEGORY TABS, AS EDITABLE PER-USER STATE (mailroom#219).
--
-- #219 reverses #156's "explicitly not Gmail's tabs", deliberately and within
-- that epic's own logic: #156 rejected "a content classifier bootstrapped from a
-- corpus we do not have", and its own text says the global tier needs PUBLIC
-- KNOWLEDGE, which spec §8 permits. Gmail's five tabs are all FUNCTION
-- categories — what a message DOES — which is legible from RFC-defined headers
-- and public sending infrastructure. No corpus, no cross-user model, no
-- learning. Tabs and labels are two ORTHOGONAL axes: tabs are function, labels
-- are topic, and asking one mechanism to serve both is why the epic never
-- shipped.
--
-- THIS MIGRATION IS THE STATE, NOT THE CLASSIFIER. Two things become editable
-- per-user data here, because "I can rearrange label mapping later if I need to"
-- is a requirement, not an aside:
--
--   1. WHICH TABS EXIST FOR THIS USER (app.category_tabs) — Gmail treats tabs as
--      individually enableable and that is the right model. All five ship,
--      Forums included and empty, because a typical mapping routes
--      nothing to it and expects to rearrange later.
--   2. WHERE EACH LABEL GOES (app.labels.tab) — the label→tab mapping.
--
-- Neither is a Go map. A compiled mapping would need a release to change, and
-- the whole point is that the operator rearranges it themselves.
--
-- A MESSAGE'S TAB IS DERIVABLE, NEVER STORED. Labelled mail resolves through the
-- mapping (a join), so an edit takes effect on the next read with no backfill
-- and no stale rows; unlabelled mail is classified from the function signals
-- migration 00027 persisted. That is the reason there is no `tab` column on
-- app.messages here and must not become one: a stored tab would have to be
-- re-derived for every message each time the operator moved one label, which is
-- exactly the maintenance burden this shape exists to avoid.

-- +goose Up

-- The tab vocabulary. An ENUM rather than free text with a CHECK because the set
-- is genuinely closed: a tab is only useful if something can decide it, the
-- decider is code, so a sixth tab needs a release regardless — and at that point
-- ALTER TYPE ... ADD VALUE is the cheaper half of the change. Values are
-- lowercase to match app.change_op / app.job_state; app.object_type is the
-- outlier because its values are JMAP type names.
CREATE TYPE app.category_tab AS ENUM
  ('primary', 'social', 'promotions', 'updates', 'forums');

-- A CategoryTab is JMAP-visible: enabling one on the laptop must reach the
-- phone, or it is a setting the user makes again on every device. Same argument
-- as RemoteAllow (00021) and SavedSearch (00024).
ALTER TYPE app.object_type ADD VALUE IF NOT EXISTS 'CategoryTab';

-- +goose StatementBegin
CREATE TABLE app.category_tabs (
  -- A ULID PRIMARY KEY even though (user_id, tab) already identifies the row.
  -- app.change_log.object_id is app.ulid NOT NULL, so a table keyed only by its
  -- natural key could not append a change_log row at all — and a JMAP-visible
  -- object that cannot append one has no legitimate write path (CLAUDE.md).
  id         app.ulid PRIMARY KEY,
  user_id    app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  tab        app.category_tab NOT NULL,
  enabled    boolean NOT NULL DEFAULT true,
  -- Where it sits in the strip. A person arranges a tab bar, so the order is
  -- theirs; ties break by the enum's own order so the result is total and
  -- stable rather than planner-dependent (same reasoning as
  -- saved_searches.position).
  position   int NOT NULL DEFAULT 0,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  -- PRIMARY IS NOT DISABLEABLE, and this is a real invariant rather than
  -- deference to Gmail. A message whose tab is disabled folds into Primary —
  -- that is what unchecking a category does — so a disabled Primary would be a
  -- tab that is switched off and still holding every folded message, plus every
  -- message that resolved to Primary directly. There is no coherent rendering of
  -- that state, so it is made unrepresentable instead of handled.
  CONSTRAINT category_tabs_primary_always_enabled CHECK (tab <> 'primary' OR enabled),
  UNIQUE (user_id, tab)
);
-- +goose StatementEnd

ALTER TABLE app.category_tabs ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.category_tabs FORCE ROW LEVEL SECURITY;
CREATE POLICY category_tabs_isolation ON app.category_tabs
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());

-- NO EXTRA INDEX. The only query is "every tab for this user, in position
-- order", and UNIQUE (user_id, tab) already provides a user_id-leading index
-- over what is at most five rows per user. Indexing five rows for an ORDER BY is
-- the speculative indexing migration 00027 declined for its six columns, and it
-- would be less defensible here because the row count is bounded by the enum.
GRANT SELECT, INSERT, UPDATE, DELETE ON app.category_tabs TO mailroom_app;

-- THE LABEL→TAB MAPPING, as a column on the label rather than a join table.
--
-- A label's tab is a property of the label, exactly like its colour: one row per
-- label, one tab, NULL for "not mapped" (which falls through to the classifier,
-- the same as an unlabelled message). A join table would need its own cascade
-- handling and could hold a mapping for a label that no longer exists; this
-- cannot, because there is no row to hold it in.
--
-- It is JMAP-visible for free: store.UpdateLabel already appends the Label
-- change_log row for any field change, so editing the mapping satisfies the
-- load-bearing invariant through the path that already exists rather than a new
-- one that would have to be kept in step with it.
--
-- NULLABLE AND UNMAPPED BY DEFAULT — no seed of any one user's mapping here.
-- #219 records an example mapping but it is one user's
-- product intent about one user's label names, and a migration that wrote it
-- would be both unportable (it hard-codes an operator's labels into the schema)
-- and wrong for the second user. It is applied as data, through the API.
--
-- No index: the resolver reaches labels through app.message_labels and the label
-- set is single digits. An index on a nine-row table is noise.
ALTER TABLE app.labels ADD COLUMN tab app.category_tab;

-- +goose Down
ALTER TABLE app.labels DROP COLUMN tab;
DROP TABLE IF EXISTS app.category_tabs;
DROP TYPE IF EXISTS app.category_tab;
-- app.object_type's 'CategoryTab' is deliberately NOT removed: PostgreSQL cannot
-- drop an enum value, and a change_log row referencing it may already exist.
-- Forward-only (ADR-008), exactly as 00024 left 'SavedSearch'.
