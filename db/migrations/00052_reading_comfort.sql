-- SPDX-License-Identifier: AGPL-3.0-only
--
-- READING COMFORT: A TEXT SIZE AND A ROW LAYOUT, PER CLIENT FAMILY
-- (mailroom#514).
--
-- The user complaint was one sentence: "no matter what we do, we're
-- cutting off the subject with the first line." Two settings answer it, and
-- both are DISPLAY preferences of a client family rather than facts about the
-- account — a phone that shows three lines per row and a 27-inch browser that
-- shows one are both right, which is exactly what app.client_preferences is
-- for (migration 00046: "a phone and a browser legitimately want different
-- density and pane defaults").
--
--   text_size   the whole app's root font size, five steps with today's size
--               ('m') in the middle, so an existing login sees no change.
--   row_layout  'oneLine' (the redrawn single line: sender, subject, then the
--               snippet in whatever room the subject leaves) or 'multiLine'
--               (sender + marks + time, subject, snippet).
--
-- ⚠ EXPAND-ONLY, AND DELIBERATELY NO DML. docs/migrations.md's "Backfilling a
-- FORCE-RLS table" rule (#505) does not apply here BECAUSE there is nothing to
-- backfill: both columns are NOT NULL with a DEFAULT, so every existing row
-- gets the default from the ALTER itself — which is a catalog-only rewrite in
-- PostgreSQL 11+ and needs no UPDATE. Had this needed one it would have had to
-- be wrapped in NO FORCE/FORCE, because migrations run as mailroom_owner and a
-- bare UPDATE on a FORCE-RLS table silently matches zero rows. Also the reason
-- the defaults are the CURRENT behaviour rather than the new one: a migration
-- must not change what an established client shows.
--
-- CHECK constraints rather than enums, following client_preferences' own three
-- text columns (00046) — ordinary DDL to extend, and internal/api/desk.go's
-- closed vocabularies (deskTextSizes, deskRowLayouts) refuse a bad value with a
-- named property long before Postgres sees it. The CHECK stays the last line of
-- defence.
--
-- ⚠ ADD COLUMN APPENDS, and db/schema.sql must mirror the resulting column
-- ORDER exactly — migrate-check diffs a pg_dump of this history against one of
-- the reference schema, column for column. So both columns are declared LAST in
-- schema.sql's app.client_preferences, after updated_at (the note 00047, 00049
-- and 00050 all leave).

-- +goose Up
ALTER TABLE app.client_preferences
  ADD COLUMN text_size  text NOT NULL DEFAULT 'm'       CHECK (text_size  IN ('xs','s','m','l','xl')),
  ADD COLUMN row_layout text NOT NULL DEFAULT 'oneLine' CHECK (row_layout IN ('oneLine','multiLine'));

-- +goose Down
ALTER TABLE app.client_preferences
  DROP COLUMN row_layout,
  DROP COLUMN text_size;
