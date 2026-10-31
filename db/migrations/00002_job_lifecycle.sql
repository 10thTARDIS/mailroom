-- SPDX-License-Identifier: AGPL-3.0-only

-- Job-state transition functions for the durable dispatcher (mailroom#13).
-- Additive: two SECURITY DEFINER functions over the existing app.jobs table,
-- following the same owner-pinned narrow-surface pattern as dequeue_due_jobs
-- (mailroom#16). No table or policy changes. db/schema.sql carries the same
-- definitions; scripts/migrate-check.sh proves they agree.

-- +goose Up
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app.complete_job(p_id app.ulid, p_worker text)
RETURNS boolean
LANGUAGE sql SECURITY DEFINER SET search_path = app AS $$
  UPDATE app.jobs SET state = 'done', completed_at = now(),
                      locked_by = NULL, locked_at = NULL
  WHERE id = p_id AND locked_by = p_worker AND state = 'running'
  RETURNING true;
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION app.complete_job(app.ulid, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.complete_job(app.ulid, text) TO mailroom_app;
ALTER FUNCTION app.complete_job(app.ulid, text) OWNER TO mailroom_owner;

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app.fail_job(p_id app.ulid, p_worker text,
                                        p_error text, p_retry_at timestamptz)
RETURNS app.job_state
LANGUAGE sql SECURITY DEFINER SET search_path = app AS $$
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
REVOKE ALL ON FUNCTION app.fail_job(app.ulid, text, text, timestamptz) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.fail_job(app.ulid, text, text, timestamptz) TO mailroom_app;
ALTER FUNCTION app.fail_job(app.ulid, text, text, timestamptz) OWNER TO mailroom_owner;

-- +goose Down
DROP FUNCTION IF EXISTS app.fail_job(app.ulid, text, text, timestamptz);
DROP FUNCTION IF EXISTS app.complete_job(app.ulid, text);
