-- SPDX-License-Identifier: AGPL-3.0-only
--
-- SWEEP FINISHED JOB ROWS (#39 on the public tracker).
--
-- Nothing ever deleted from app.jobs: every snooze, notify, scan, purge pass
-- and drawer sweep left a 'done' row behind for good. Measured before writing
-- this: a few weeks of one mailbox is a couple of thousand rows and under a
-- megabyte, almost all of it from the two recurring per-user sweeps, so the
-- table is small but grows without bound. This adds the sweep.
--
-- EXPAND-ONLY (ADR-008): one foreign key action, one function body changed to
-- also stamp a timestamp it should always have stamped, and two new functions.
--
--   1. app.submissions.job_id gains ON DELETE SET NULL. It had no action, so
--      deleting any finished 'submit' job would have failed the whole sweep on a
--      foreign key violation. Nothing reads a finished submission's job_id (the
--      only reader is CancelSubmission, and only while the job is still pending),
--      so NULL after the row is swept is a correct value, not a dangling one.
--      Added NOT VALID and then validated: the existing values are unchanged.
--   2. app.fail_job stamps completed_at when a job dead-letters. It did not, so
--      a dead row carried no time of death, and a sweep keyed on completed_at
--      would have kept every dead row forever. Rows that died before this
--      migration still have NULL; the sweep ages those by locked_at, then
--      created_at.
--   3. app.sweep_terminal_jobs: deletes 'done' rows finished longer ago than one
--      window and 'dead' rows longer ago than another, at most p_limit of each
--      per call. It runs as mailroom_owner, whose ALL policy on app.jobs
--      (jobs_dispatcher_owner) is the deliberate cross-user surface the
--      dispatcher already uses; no new policy is needed.
--      'pending' and 'running' rows are never candidates, so the sweep cannot
--      free an idempotency key a live job holds: jobs_live_idempotency_key only
--      covers those two states.
--   4. app.job_census: the read-only report behind `mailroom-admin jobs-report`,
--      per kind and state, with how many rows the sweep would take today.
--
-- No index: the sweep's scan is bounded by p_limit and runs daily over a table
-- this size; the existing partial index jobs_due keeps the dispatcher's own scan
-- off the terminal rows either way.
--
-- No change_log rows: jobs are operational data, not JMAP-visible objects.

-- +goose Up
ALTER TABLE app.submissions DROP CONSTRAINT submissions_job_id_fkey;
ALTER TABLE app.submissions ADD CONSTRAINT submissions_job_id_fkey
  FOREIGN KEY (job_id) REFERENCES app.jobs(id) ON DELETE SET NULL NOT VALID;
ALTER TABLE app.submissions VALIDATE CONSTRAINT submissions_job_id_fkey;

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app.fail_job(p_id app.ulid, p_worker text,
                                        p_error text, p_retry_at timestamptz)
RETURNS app.job_state
LANGUAGE sql SECURITY DEFINER SET search_path = app, pg_temp AS $$
  UPDATE app.jobs SET
    state     = CASE WHEN attempts >= max_attempts THEN 'dead'::app.job_state
                     ELSE 'pending'::app.job_state END,
    fire_at   = CASE WHEN attempts >= max_attempts THEN fire_at
                     ELSE p_retry_at END,
    completed_at = CASE WHEN attempts >= max_attempts THEN now()
                        ELSE completed_at END,
    last_error = p_error,
    locked_by = NULL, locked_at = NULL
  WHERE id = p_id AND locked_by = p_worker AND state = 'running'
  RETURNING state;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app.sweep_terminal_jobs(p_now timestamptz,
    p_done_keep interval, p_dead_keep interval, p_limit int)
RETURNS TABLE (state text, deleted bigint)
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = app, pg_temp AS $$
DECLARE
  v_done bigint;
  v_dead bigint;
BEGIN
  WITH gone AS (
    DELETE FROM app.jobs j
     WHERE j.id IN (SELECT id FROM app.jobs
                     WHERE app.jobs.state = 'done'
                       AND app.jobs.completed_at < p_now - p_done_keep
                     LIMIT p_limit
                     FOR UPDATE SKIP LOCKED)
    RETURNING 1)
  SELECT count(*) INTO v_done FROM gone;

  WITH gone AS (
    DELETE FROM app.jobs j
     WHERE j.id IN (SELECT id FROM app.jobs
                     WHERE app.jobs.state = 'dead'
                       AND COALESCE(app.jobs.completed_at, app.jobs.locked_at, app.jobs.created_at)
                           < p_now - p_dead_keep
                     LIMIT p_limit
                     FOR UPDATE SKIP LOCKED)
    RETURNING 1)
  SELECT count(*) INTO v_dead FROM gone;

  -- One row per state, zero included: a run that deleted nothing must read
  -- differently from a report that cannot see a state at all.
  RETURN QUERY VALUES ('done'::text, v_done), ('dead'::text, v_dead);
END;
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION app.sweep_terminal_jobs(timestamptz, interval, interval, int) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.sweep_terminal_jobs(timestamptz, interval, interval, int) TO mailroom_app;
ALTER FUNCTION app.sweep_terminal_jobs(timestamptz, interval, interval, int) OWNER TO mailroom_owner;

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app.job_census(p_now timestamptz,
    p_done_keep interval, p_dead_keep interval)
RETURNS TABLE (kind text, state text, total bigint, sweepable bigint, oldest_finished timestamptz)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = app, pg_temp AS $$
  SELECT j.kind, j.state::text, count(*),
         count(*) FILTER (WHERE
           (j.state = 'done' AND j.completed_at < p_now - p_done_keep) OR
           (j.state = 'dead' AND COALESCE(j.completed_at, j.locked_at, j.created_at) < p_now - p_dead_keep)),
         min(CASE WHEN j.state = 'dead' THEN COALESCE(j.completed_at, j.locked_at, j.created_at)
                  ELSE j.completed_at END)
    FROM app.jobs j
   GROUP BY j.kind, j.state
   ORDER BY j.kind, j.state;
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION app.job_census(timestamptz, interval, interval) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.job_census(timestamptz, interval, interval) TO mailroom_app;
ALTER FUNCTION app.job_census(timestamptz, interval, interval) OWNER TO mailroom_owner;

-- +goose Down
DROP FUNCTION IF EXISTS app.job_census(timestamptz, interval, interval);
DROP FUNCTION IF EXISTS app.sweep_terminal_jobs(timestamptz, interval, interval, int);
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app.fail_job(p_id app.ulid, p_worker text,
                                        p_error text, p_retry_at timestamptz)
RETURNS app.job_state
LANGUAGE sql SECURITY DEFINER SET search_path = app, pg_temp AS $$
  UPDATE app.jobs SET
    state     = CASE WHEN attempts >= max_attempts THEN 'dead'::app.job_state
                     ELSE 'pending'::app.job_state END,
    fire_at   = CASE WHEN attempts >= max_attempts THEN fire_at
                     ELSE p_retry_at END,
    last_error = p_error,
    locked_by = NULL, locked_at = NULL
  WHERE id = p_id AND locked_by = p_worker AND state = 'running'
  RETURNING state;
$$;
-- +goose StatementEnd
ALTER TABLE app.submissions DROP CONSTRAINT submissions_job_id_fkey;
ALTER TABLE app.submissions ADD CONSTRAINT submissions_job_id_fkey
  FOREIGN KEY (job_id) REFERENCES app.jobs(id);
