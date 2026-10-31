-- SPDX-License-Identifier: AGPL-3.0-only
--
-- A TENTH SECURITY DEFINER: app.count_due_jobs, so job-queue backlog can be
-- measured (#524).
--
-- ⚠ ADDING A CROSS-USER DEFINER IS THE THING THIS SCHEMA MOST WANTS REVIEWED,
-- so here is the argument in full rather than a line in a commit message.
--
-- THE PROBLEM. mailroom_job_queue_depth has been declared, registered and
-- served since the metrics package was written, and never once set. It is a
-- plain Gauge, so /metrics has always emitted a constant 0 — an alert on "the
-- job queue is backing up" could not fire, and a scrape of it actively asserts
-- that everything is fine. That matters now because instances are run with real
-- monitoring in front of them, and wiring a dashboard to
-- a metric that is structurally always zero is worse than having no dashboard.
--
-- WHY A PLAIN COUNT DOES NOT WORK, which is the whole reason this exists. The
-- dispatcher is a single global worker running in a SystemTx, where
-- app.current_user_id() is NULL. The app.jobs policy is:
--
--     USING (user_id = app.current_user_id() OR
--            (user_id IS NULL AND app.current_user_id() IS NULL))
--
-- so in that transaction it matches SYSTEM rows only. Every per-user recurring
-- job — purge_trash, ml_train, snooze_wake, submit — is invisible. A
-- `SELECT count(*) FROM app.jobs` there returns a number that looks entirely
-- plausible and silently UNDERCOUNTS, which is precisely the failure mode this
-- project keeps paying for: not an error, just a wrong number nobody can tell
-- is wrong. The alternative — granting mailroom_app a permanent cross-user view
-- of app.jobs — is a far larger surface than this.
--
-- WHY THIS IS A SMALLER SURFACE THAN THE DEFINER IT SITS BESIDE.
-- app.dequeue_due_jobs already hands the same worker whole job ROWS across every
-- user — ids, kinds, payloads, idempotency keys. This function returns ONE
-- BIGINT and nothing else. It cannot name a user, a job, or a payload. Anything
-- it could disclose is strictly implied by what dequeue_due_jobs already
-- returns to the same caller.
--
-- What it does disclose, stated rather than waved past: a single instance-wide
-- integer, from which a caller learns that *somebody* has N jobs due. On a
-- single-user instance that is not a cross-user fact at all. On a shared one it
-- is an aggregate with no attribution, and the role that can call it
-- (mailroom_app) is the application itself, not an end user.
--
-- STABLE, not VOLATILE: it reads and never writes, unlike dequeue_due_jobs.
--
-- DUE, NOT MERELY PENDING. "Queue depth" is meant to be BACKLOG — work that
-- should already have run. app.jobs also holds pending rows scheduled far in the
-- future (a snooze for next week), and counting those would make the gauge climb
-- with normal use and mean nothing when it did. So the predicate is
-- `state = 'pending' AND fire_at <= now()`, and the metric's Help text is
-- corrected in the same change to say so. No dashboard can be broken by that
-- wording, because the metric has never emitted a real value.
--
-- Jobs stranded in 'running' are deliberately NOT counted here: migration 00041
-- gave them a 15-minute lease, so they return to the claimable set on their own
-- and would otherwise be double-counted as backlog while being reclaimed.
--
-- Conventions this follows, all of which are load-bearing (#309, mailroom#16):
--   * SET search_path = app, pg_temp — pg_temp named LAST so an unqualified
--     relation cannot resolve to the caller's temp schema (the definer hijack).
--     Every relation below is schema-qualified anyway; both, deliberately.
--   * REVOKE ALL FROM PUBLIC then GRANT EXECUTE TO mailroom_app. Migration
--     00037's default-privilege revoke should already prevent a PUBLIC grant,
--     but the explicit REVOKE is the established pattern and does not rely on
--     it.
--   * OWNER TO mailroom_owner, pinning the definer deterministically. Applied by
--     a superuser (as in CI/dev) the function would otherwise be owned by that
--     superuser and bypass RLS entirely, hiding the production semantics where
--     the definer is a NON-superuser and IS subject to RLS.
--
-- TestEveryDefinerPinsPgTempLast asserts the definer count and must be bumped
-- from 9 to 10 by this change. That assertion exists so a new cross-user surface
-- cannot slip in unreviewed; this comment is the review it asks for.

-- +goose Up
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app.count_due_jobs()
RETURNS bigint
LANGUAGE sql SECURITY DEFINER STABLE SET search_path = app, pg_temp AS $$
  SELECT count(*) FROM app.jobs
   WHERE state = 'pending' AND fire_at <= now();
$$;
-- +goose StatementEnd

-- +goose StatementBegin
REVOKE ALL ON FUNCTION app.count_due_jobs() FROM PUBLIC;
-- +goose StatementEnd

-- +goose StatementBegin
GRANT EXECUTE ON FUNCTION app.count_due_jobs() TO mailroom_app;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER FUNCTION app.count_due_jobs() OWNER TO mailroom_owner;
-- +goose StatementEnd

-- +goose StatementBegin
-- mailroom_owner holds this implicitly in production (it owns every object); the
-- explicit grant is what makes the definer path work in CI/dev, where the tables
-- are owned by the bootstrap superuser instead.
GRANT SELECT ON app.jobs TO mailroom_owner;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP FUNCTION IF EXISTS app.count_due_jobs();
-- +goose StatementEnd
