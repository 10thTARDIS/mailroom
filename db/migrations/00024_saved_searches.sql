-- SPDX-License-Identifier: AGPL-3.0-only

-- SAVED SEARCHES / SMART FOLDERS (#57, matrix §7).
--
-- Mailroom's search understands a real operator syntax — `from:`, `label:`, `is:unread`,
-- `before:`, phrase and prefix matching — and v1.17.0 made it discoverable by suggesting
-- it as you type. What it still could not do was REMEMBER one. A query you retype every
-- day is a folder you do not have.
--
-- SO A SAVED SEARCH IS JUST A NAMED QUERY, and "smart folder" is what it looks like once
-- it is pinned to the sidebar. The matrix lists those as separate rows; they are one
-- feature with a boolean, and building them as two things would mean two objects and two
-- management surfaces answering one question.
--
-- THE QUERY IS STORED AS THE OPERATOR STRING, not as a parsed filter tree, and that is
-- deliberate:
--
--   * It is what the user typed and can read back. A parsed tree would have to be
--     rendered back into text to be editable, and any mismatch between the parser and the
--     renderer becomes a search that silently changes when you open it.
--   * The parser already exists and already runs on Email/query, so a saved search
--     executes through EXACTLY the same path as typing it — there is no second
--     interpretation that could disagree with the first. A saved search that matched
--     different mail than the same text typed by hand would be worse than none.
--   * It survives the syntax gaining operators. A tree would need a migration per
--     operator; a string needs nothing.
--
-- The cost, accepted: a saved search whose query is invalid (a label that was later
-- deleted, say) fails when it runs rather than when it is saved. That is the same failure
-- typing it by hand produces, which is the point — it is not a new class of surprise.
--
-- NO IMAP ANYTHING. A saved search is local Mailroom state, like a label's colour: there
-- is no IMAP concept to mirror it onto and nothing is written back. It syncs between the
-- user's own devices through the change log and nowhere else.

-- +goose Up
ALTER TYPE app.object_type ADD VALUE IF NOT EXISTS 'SavedSearch';

-- +goose StatementBegin
CREATE TABLE app.saved_searches (
  id         app.ulid PRIMARY KEY,
  user_id    app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  name       text NOT NULL,
  -- The operator syntax, exactly as typed. Bounded because it is a search box's worth of
  -- text, not a document, and an unbounded column here would be a place to park a
  -- megabyte that then runs on every sidebar render.
  query      text NOT NULL,
  CONSTRAINT saved_searches_query_not_blank CHECK (btrim(query) <> ''),
  CONSTRAINT saved_searches_query_bounded   CHECK (length(query) <= 2000),
  CONSTRAINT saved_searches_name_not_blank  CHECK (btrim(name) <> ''),
  CONSTRAINT saved_searches_name_bounded    CHECK (length(name) <= 120),
  -- Pinned to the sidebar — i.e. shown as a "smart folder" — or merely saved and
  -- reachable from the search box. One boolean rather than two objects.
  in_sidebar boolean NOT NULL DEFAULT true,
  -- Explicit ordering, because the sidebar is a place a person arranges. Ties break by
  -- name so the order is total and stable rather than whatever the planner returns.
  position   int NOT NULL DEFAULT 0,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  -- A name is how the user refers to it, so two with the same name is a UI that cannot
  -- be reasoned about.
  UNIQUE (user_id, name)
);
-- +goose StatementEnd

ALTER TABLE app.saved_searches ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.saved_searches FORCE ROW LEVEL SECURITY;
CREATE POLICY saved_searches_isolation ON app.saved_searches
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());

-- Leads with user_id like every other hot index here (RLS appends that predicate anyway),
-- then the sidebar's own ordering, so rendering the rail is one index scan.
CREATE INDEX saved_searches_user_sidebar ON app.saved_searches (user_id, position, name);

GRANT SELECT, INSERT, UPDATE, DELETE ON app.saved_searches TO mailroom_app;

-- +goose Down
DROP TABLE IF EXISTS app.saved_searches;
-- The enum value is deliberately NOT removed: PostgreSQL cannot drop one, and a
-- change_log row referencing it may already exist. Forward-only (ADR-008).
