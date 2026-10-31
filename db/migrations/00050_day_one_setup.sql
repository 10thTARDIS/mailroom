-- SPDX-License-Identifier: AGPL-3.0-only
--
-- DAY-ONE SETUP: A FOLDER-SORTED MAILBOX BECOMES A DESK WITHOUT MOVING
-- ANYTHING BY ACCIDENT (mailroom#446, Direction 7 "Desk and Drawers", #372).
--
-- The desk assumes mail arrives in an inbox and is triaged there. A real
-- mailbox arriving from another client has years of mail already sorted into
-- folders, and nothing in Mailroom knew what to do with it: feeds_desk
-- (migration 00046) could make every folder count as unactioned, which turns
-- a tidy mailbox into a four-thousand-message desk on first boot. #446 is the
-- four steps that turn the one into the other, each of them a REPORT FIRST
-- (CLAUDE.md "build the report before the fix"), and this migration is the
-- durable state those steps need.
--
-- # desk_archived_at: "archived in Mailroom", which is NOT a move
--
-- The decision this whole issue turns on: mail older than the window (default
-- 30 days) gets its area and its labels and then LEAVES THE DESK — but
-- NOTHING MOVES ON THE SERVER. The bytes stay in the folder the provider put
-- them in; every other client sees exactly what it saw before; the message is
-- reachable in All mail and by label and by search. Only Mailroom's own desk
-- stops counting it as unactioned.
--
-- That is a third, distinct state, and the two that already exist cannot
-- express it:
--
--   moved_mailbox_id  = the user moved this message to a folder. A REAL move:
--                       a writeback intent is enqueued and the IMAP server is
--                       told. Using it here would move four thousand messages
--                       on somebody's provider on first boot — the exact event
--                       #446 exists to avoid.
--   drawer_archives   = the drawer sweep archived this message (migration
--                       00048). ALSO a real move (to Archive), and keyed to
--                       an AREA and a dwell PERIOD, neither of which day-one
--                       has: this is one operator decision about history, not
--                       an area's ongoing policy.
--
-- So: a nullable timestamp on the message, read by exactly ONE SQL expression
-- (store.deskLiveSQL, which gains "m.desk_archived_at IS NULL"), which is what
-- makes AreaSummary counts, Email/query onDesk, and the drawer sweep candidate
-- scan agree that a day-one archived message is off the desk — including the
-- sweep, which must NEVER server-move mail that day-one archived precisely
-- BECAUSE day-one promised nothing would move.
--
-- Catalog-only ADD COLUMN: a plain timestamptz, not a domain type and with no
-- CHECK, so this is the ~1 ms case docs/migrations.md measured even on
-- app.messages, the one table it warns to keep clear of. NO INDEX: the column
-- is only ever read as IS NULL inside a per-row predicate already bounded by
-- user_id and a provenance EXISTS — an index on a mostly-NULL column no query
-- starts from would be write cost for nothing.
--
-- # setup_completed_at: no established desk is ever interrupted by a wizard
--
-- NULL means "this login has not been through day-one; the desk opens to
-- setup". Every login that ALREADY has desk preferences has already arranged a
-- desk, so the backfill sets it to updated_at — inline, in this migration,
-- deliberately: app.desk_preferences holds exactly ONE ROW PER LOGIN (it is a
-- singleton keyed on user_id), so this is a handful of rows, not the
-- expand-and-contract case docs/migrations.md reserves for large tables. The
-- alternative — leaving them NULL and backfilling later — would show a
-- day-one wizard to every existing user the moment this deploys, which is the
-- one outcome the column exists to prevent.
--
-- # setup_runs / setup_journal: the undo journal is the reason both exist
--
-- Step 2 is "undoable as a whole". That is a promise about a bulk write, and a
-- promise no counter can keep: to reverse the run you have to know, PER
-- MESSAGE, exactly what it did — whether the label it wanted was already there
-- (in which case removing it would take away the user own filing), whether the
-- message was already archived in Mailroom, and which folder it came out of if
-- it was moved. app.setup_journal is that record, one row per message per run,
-- and SetupUndoChunk replays the inverse from it. The alternative, deriving
-- the inverse from the plan, cannot distinguish "this run wrote it" from "it
-- was already like that", so undo would destroy state the run never created.
--
-- app.setup_runs is the run itself, and it is A JMAP-VISIBLE OBJECT
-- (SetupRun below): the client polls it while the chunked job drains, which
-- means status and counters have to reach other devices through the change log
-- like every other syncable object (spec §13/§14). Both PLANNED and ACTUAL
-- counters, as separate columns rather than one pair: "we said N and wrote
-- N" is the only way the report promise and the run outcome can be
-- compared at all, and a single set of columns cannot say both.
--
-- plan IS JSONB AND THAT IS ALLOWED HERE: CLAUDE.md forbids JSONB for
-- QUERYABLE state, and nothing ever queries this by field. It is the operator
-- per-folder choices, read back whole by the worker and shown back to the
-- user; the queryable facts (status, window, every counter) are columns. Same
-- reasoning app.jobs.payload already carries.

-- +goose Up

-- The run object joins the change log vocabulary. Forward-only: PostgreSQL
-- cannot drop an enum value, exactly as 00046 left 'Area'.
ALTER TYPE app.object_type ADD VALUE IF NOT EXISTS 'SetupRun';

-- +goose StatementBegin
CREATE TABLE app.setup_runs (
  id             app.ulid PRIMARY KEY,
  user_id        app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  -- One kind today, named rather than implied: the census, the preview and the
  -- rule induction all write nothing, so 'import' is the only step of #446
  -- that has a run at all. A CHECK rather than an enum, following
  -- mailboxes.sync_scope (00026): ordinary DDL to extend, and the Go constant
  -- set already validates before SQL sees it.
  kind           text NOT NULL CHECK (kind IN ('import')),
  -- The lifecycle the client polls. 'undoing' is a state and not a flag
  -- because the SAME job kind drains both directions and reads this column to
  -- decide which way it is going (internal/setup).
  status         text NOT NULL CHECK (status IN ('queued','running','done','failed','undoing','undone')),
  -- The window this run treated as "newly arrived" — mail inside it was left
  -- completely alone. Recorded on the run because the report denominator is
  -- meaningless without it.
  window_days    int NOT NULL CHECK (window_days > 0),
  -- The per-folder choices. A PAYLOAD, never queried by field — see the header.
  plan           jsonb NOT NULL,
  -- PLANNED (what the preview promised) beside ACTUAL (what the run did), so
  -- the two can be compared. left_for_later and unclassified are planned-side
  -- only: they describe mail the run deliberately did not touch, so there is
  -- no "actual" for them to differ from.
  planned_labels   int NOT NULL DEFAULT 0,
  labels_written   int NOT NULL DEFAULT 0,
  planned_archived int NOT NULL DEFAULT 0,
  archived         int NOT NULL DEFAULT 0,
  planned_moves    int NOT NULL DEFAULT 0,
  moved            int NOT NULL DEFAULT 0,
  left_for_later   int NOT NULL DEFAULT 0,
  -- Mail the census could not classify (no From address). COUNTED, NEVER
  -- HIDDEN (CLAUDE.md: every report states its denominator).
  unclassified     int NOT NULL DEFAULT 0,
  error          text,
  created_at     timestamptz NOT NULL DEFAULT now(),
  started_at     timestamptz,
  finished_at    timestamptz,
  undone_at      timestamptz
);
-- +goose StatementEnd

-- The client lists a login runs newest first, and Setup/apply asks "is one
-- already outstanding" before creating another. Leads with user_id because RLS
-- appends that predicate to every plan (CLAUDE.md).
CREATE INDEX setup_runs_user_created ON app.setup_runs (user_id, created_at);

ALTER TABLE app.setup_runs ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.setup_runs FORCE ROW LEVEL SECURITY;
CREATE POLICY setup_runs_isolation ON app.setup_runs
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());
GRANT SELECT, INSERT, UPDATE, DELETE ON app.setup_runs TO mailroom_app;

-- ONE ROW PER MESSAGE PER RUN, recording what the run ACTUALLY wrote — see the
-- header for why the inverse cannot be derived from the plan instead.
--
-- ⚠ A ROW IS WRITTEN EVEN WHEN NOTHING CHANGED (all three facts false/NULL).
-- The chunk cursor IS "has this message a journal row for this run yet", so a
-- message the run examined and found already labelled and already archived
-- must still get a row or every subsequent chunk would select it again and the
-- job would never finish.
--
-- NOT a JMAP-visible object of its own: app.object_type carries no
-- 'SetupJournal' value and no change_log row is appended for this table. It is
-- the run internal record; what a client syncs is the SetupRun and the Emails
-- the run touched.
-- +goose StatementBegin
CREATE TABLE app.setup_journal (
  id                    app.ulid PRIMARY KEY,
  user_id               app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  run_id                app.ulid NOT NULL REFERENCES app.setup_runs(id) ON DELETE CASCADE,
  message_id            app.ulid NOT NULL REFERENCES app.messages(id) ON DELETE CASCADE,
  -- The label THIS RUN inserted, or NULL when the message already carried it
  -- (or wanted none). Undo removes only a label named here, and only while it
  -- is still attributed to the setup — a label the user has re-applied by hand
  -- since is theirs.
  label_id              app.ulid REFERENCES app.labels(id) ON DELETE CASCADE,
  -- Whether THIS RUN set desk_archived_at. False for a message that was
  -- already archived in Mailroom before the run, so undo leaves it archived.
  desk_archived         boolean NOT NULL DEFAULT false,
  -- The folder a bringToDesk message came OUT of, so undo can put it back.
  -- SET NULL rather than CASCADE: losing the folder must not delete the record
  -- that the run moved this message, the same reasoning
  -- drawer_archives.area_id takes (00048) — undo then simply cannot move it
  -- back, which is the honest outcome when the destination no longer exists.
  moved_from_mailbox_id app.ulid REFERENCES app.mailboxes(id) ON DELETE SET NULL,
  created_at            timestamptz NOT NULL DEFAULT now()
);
-- +goose StatementEnd

-- ⚠ "ONE ROW PER MESSAGE PER RUN" IS ENFORCED HERE, NOT TRUSTED TO THE CHUNK
-- QUERY. The chunk query skips journaled messages, so in a single drainer the
-- constraint can never fire — but a run can legitimately have TWO drainers at
-- once: `mailroom-admin day-one --apply` drives chunks inline while the job it
-- also enqueued is sitting in the dispatcher. Two concurrent chunks can both
-- select a message whose journal row neither has committed yet, and the loser
-- would otherwise write a second row and double-count its own counters. The
-- unique index makes the loser's whole chunk abort and roll back — including the
-- counter increments — and its retry then finds the row and skips the message.
-- Fail loud and retry, rather than a run whose journal quietly says it touched
-- more mail than it did. Leading with (user_id, run_id) it also serves the chunk
-- cursor's own lookup, so it replaces a plain index rather than adding to one.
CREATE UNIQUE INDEX setup_journal_user_run_message
  ON app.setup_journal (user_id, run_id, message_id);

ALTER TABLE app.setup_journal ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.setup_journal FORCE ROW LEVEL SECURITY;
CREATE POLICY setup_journal_isolation ON app.setup_journal
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());
GRANT SELECT, INSERT, UPDATE, DELETE ON app.setup_journal TO mailroom_app;

-- ⚠ ADD COLUMN APPENDS, and db/schema.sql must mirror the resulting column
-- ORDER exactly — migrate-check diffs a pg_dump of this history against one of
-- the reference schema, column for column (the note 00047 and 00049 both
-- leave).
ALTER TABLE app.desk_preferences ADD COLUMN setup_completed_at timestamptz;

-- The inline backfill, safe BECAUSE this table is a per-login singleton — see
-- the header. It exists so no established desk is interrupted by a wizard.
UPDATE app.desk_preferences SET setup_completed_at = updated_at;

ALTER TABLE app.messages ADD COLUMN desk_archived_at timestamptz;

-- +goose Down
ALTER TABLE app.messages DROP COLUMN desk_archived_at;
ALTER TABLE app.desk_preferences DROP COLUMN setup_completed_at;
DROP TABLE IF EXISTS app.setup_journal;
DROP TABLE IF EXISTS app.setup_runs;
-- app.object_type SetupRun value is deliberately NOT removed: PostgreSQL
-- cannot drop an enum value and a change_log row referencing it may already
-- exist. Forward-only (ADR-008), exactly as 00046 left 'Area'.
