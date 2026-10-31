-- SPDX-License-Identifier: AGPL-3.0-only
--
-- Reclaim jobs stranded in 'running' by a process that died holding the lock
-- (#351 B3).
--
-- ⚠ THE COLUMN EXISTED; THE LOGIC DID NOT. app.jobs.locked_at has been written
-- on every dequeue and cleared on every completion since the baseline, and read
-- by NOTHING — it appears in no WHERE clause anywhere in db/, internal/ or cmd/.
-- Meanwhile internal/jobs/dispatcher.go states the assumption out loud:
--
--     Shutting down: leave the job 'running' with its lock; the next
--     dispatcher (or this one on restart) reclaims it after the lock ages out.
--
-- There was no ageing. dequeue_due_jobs selected `state = 'pending'` only, and
-- complete_job/fail_job are both guarded on `locked_by = p_worker` with a worker
-- id that is a fresh ULID per process — so after a restart no live worker could
-- ever match, and the row stayed 'running' forever.
--
-- What that cost, because it is worse than one lost job:
--
--   * EnsureRecurringUserJob (internal/store/jobs.go) treats
--     `state IN ('pending','running')` as "already scheduled". Every boot found
--     the zombie, returned scheduled=false, and logged nothing at all. So
--     purge_trash and ml_train were not merely delayed — their RECURRING CHAINS
--     were dead, silently, until someone noticed Trash never emptying.
--   * purge_trash reschedules itself AFTER sweeping, so dying mid-sweep also
--     stops blob reclamation. That is the tail that eventually fills the disk.
--   * submit may already have handed the message to SMTP; the submission then
--     sits in 'sending' with no retry and nothing user-visible.
--
-- Any restart during a job did this: a deploy, an OOM kill, a host reboot,
-- `docker compose restart`.
--
-- ⚠ THE LEASE IS A TRADE, STATED RATHER THAN HIDDEN. Reclaiming after a fixed
-- interval means a job still legitimately running after that interval can be
-- claimed a second time. 15 minutes is chosen to sit far above every handler we
-- ship (submit, snooze_wake and ml_train are seconds; purge_trash is bounded by
-- its own batch size), and re-running a handler is survivable because the
-- expensive one — submit — carries an idempotency key. The alternative, a
-- heartbeat that renews the lease while work is in flight, is strictly better
-- and strictly larger; this is the version that makes the stated behaviour true
-- today. Revisit if a handler ever grows a long tail.
--
-- The reclaim rides INSIDE dequeue_due_jobs rather than arriving as its own
-- periodic job, for three reasons: it is atomic with the claim it repairs, it
-- needs no scheduling of its own, and a reclaimer implemented AS a job could be
-- stranded by the very defect it exists to fix.
--
-- attempts is incremented for a reclaimed job exactly as for a fresh claim: the
-- job WAS attempted, and letting it burn retries is what stops a handler that
-- reliably kills its process from being retried forever.
--
-- The signature is deliberately unchanged. `app.dequeue_due_jobs(text, int)` is
-- pinned by name in the isolation suite (it is the one sanctioned cross-user
-- definer), and a security-sensitive function is the wrong place to widen a
-- signature for a tunable nobody has yet needed to tune.

-- +goose Up
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app.dequeue_due_jobs(p_worker text, p_limit int)
RETURNS SETOF app.jobs
LANGUAGE sql SECURITY DEFINER SET search_path = app, pg_temp AS $$
  UPDATE app.jobs j SET state = 'running', locked_by = p_worker, locked_at = now(),
                        attempts = attempts + 1
  WHERE j.id IN (
    SELECT id FROM app.jobs
    WHERE (state = 'pending' AND fire_at <= now())
       -- Stranded by a process that died holding the lock (#351 B3):
       -- locked_at was written on every dequeue and read by nothing, so a
       -- job left running by any restart stayed running forever, and
       -- EnsureRecurringUserJob counted the zombie as "already scheduled".
       -- See db/migrations/00041 for the lease trade-off.
       OR (state = 'running' AND locked_at IS NOT NULL
           AND locked_at < now() - interval '15 minutes')
    ORDER BY fire_at
    FOR UPDATE SKIP LOCKED
    LIMIT p_limit
  )
  RETURNING j.*;
$$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app.dequeue_due_jobs(p_worker text, p_limit int)
RETURNS SETOF app.jobs
LANGUAGE sql SECURITY DEFINER SET search_path = app, pg_temp AS $$
  UPDATE app.jobs j SET state = 'running', locked_by = p_worker, locked_at = now(),
                        attempts = attempts + 1
  WHERE j.id IN (
    SELECT id FROM app.jobs
    WHERE state = 'pending' AND fire_at <= now()
    ORDER BY fire_at
    FOR UPDATE SKIP LOCKED
    LIMIT p_limit
  )
  RETURNING j.*;
$$;
-- +goose StatementEnd
