-- SPDX-License-Identifier: AGPL-3.0-only
--
-- CONTRACT STEP (ADR-008): drop app.sync_jobs.
--
-- The table was designed in the baseline for long-running resumable runs (bulk
-- import and backfill progress). Nothing ever wrote to it or read from it: the
-- importer keeps its own checkpoint, and every backfill is a report-first
-- command that tracks its own progress. A grep of the whole tree finds no
-- production code that names it, so the expand step (stop using it) is already
-- complete and the table holds no rows the application ever created.
--
-- Forward-only. The Down below recreates the empty shape only, for a local
-- rollback; it cannot bring back rows.

-- +goose Up
DROP TABLE IF EXISTS app.sync_jobs;

-- +goose Down
CREATE TABLE app.sync_jobs (
  id            app.ulid PRIMARY KEY,
  user_id       app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  account_id    app.ulid REFERENCES app.accounts(id) ON DELETE CASCADE,
  kind          text NOT NULL,
  source        text,
  state         app.job_state NOT NULL DEFAULT 'pending',
  progress_done bigint NOT NULL DEFAULT 0,
  progress_total bigint,
  cursor        jsonb NOT NULL DEFAULT '{}',
  last_error    text,
  created_at    timestamptz NOT NULL DEFAULT now(),
  completed_at  timestamptz
);
ALTER TABLE app.sync_jobs ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.sync_jobs FORCE ROW LEVEL SECURITY;
CREATE POLICY sync_jobs_isolation ON app.sync_jobs
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());
GRANT SELECT, INSERT, UPDATE, DELETE ON app.sync_jobs TO mailroom_app;
