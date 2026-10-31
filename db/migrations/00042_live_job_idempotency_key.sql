-- SPDX-License-Identifier: AGPL-3.0-only
--
-- Scope app.jobs.idempotency_key uniqueness to jobs that are still outstanding
-- (mailroom#357).
--
-- ⚠ THE KEY OUTLIVED THE JOB, AND THE REPAIR PATH ASKED THE KEY. Jobs are never
-- deleted, so an unqualified UNIQUE on idempotency_key consumes that key for the
-- lifetime of the database — including for a job that DEAD-LETTERED and will
-- therefore never run.
--
-- That is fatal for snooze specifically, because the snooze key is derived from
-- data rather than chosen: internal/store/snooze.go keys the wake on
-- `snooze_wake:<user>:<wake time truncated to the minute>` so that snoozing
-- fifty messages to "tomorrow morning" costs ONE timer instead of fifty. The
-- collapse is right. Its lifetime was not.
--
-- Follow the sequence for a wake job that exhausts max_attempts:
--
--   1. The job dead-letters. The row stays, holding the key.
--   2. snooze.EnsureScheduled — which describes itself as "the repair path for a
--      job queue that lost one" — runs at every boot. It asks NextSnoozeWake for
--      the earliest outstanding wake, which is the message that never woke, so
--      the time it gets back is that same past instant.
--   3. That instant truncates to the same minute, which yields THE SAME KEY.
--      ON CONFLICT DO NOTHING fires against the dead row, nothing is inserted,
--      and EnsureScheduled returns scheduled=false.
--
-- So the repair path runs forever and silently declines to repair, and the mail
-- the user deliberately deferred never comes back. Nothing surfaces it: the
-- dead-letter list users can see is per-account WRITEBACK, not jobs; the repair
-- path logs only when it DOES schedule; and Email/get still reports the message
-- as snoozed, which reads as working.
--
-- This is the third instance of one shape in this table — a repair path whose
-- precondition is satisfied by the very corruption it exists to undo. The other
-- two were EnsureRecurringUserJob counting a stranded 'running' job as "already
-- scheduled", and dequeue_due_jobs never ageing out the lock that stranded it
-- (both migration 00041).
--
-- THE FIX MAKES THE KEY MEAN WHAT THE CALL SITE ASKS IT. Every ON CONFLICT on
-- this key is asking "is there already an outstanding job for this?", not "has
-- there ever been one". A partial unique index over the non-terminal states says
-- exactly that: the collapse is preserved in full while a job that is done or
-- dead releases its key.
--
-- 'pending' AND 'running', not 'pending' alone. Narrowing it further would also
-- fix #357 and would additionally close a smaller window (a wake rescheduled
-- into the same minute as the job currently running it), but it would let a
-- second job be enqueued while the first is mid-flight — and app.jobs is shared
-- with submit, where that key is the last line between a retry and a double
-- send. The states this index covers are precisely the states
-- EnsureRecurringUserJob already treats as "outstanding".
--
-- No backfill: the constraint being replaced was unique, so no duplicate keys
-- can exist to conflict with the new index.
--
-- ⚠ Callers must name the predicate. `ON CONFLICT (idempotency_key) DO NOTHING`
-- can no longer infer an arbiter and raises 42P10; the conflict target has to
-- carry the index predicate too (see internal/store/snooze.go). A bare INSERT is
-- unaffected.

-- +goose Up
ALTER TABLE app.jobs DROP CONSTRAINT jobs_idempotency_key_key;
CREATE UNIQUE INDEX jobs_live_idempotency_key ON app.jobs (idempotency_key)
  WHERE state IN ('pending', 'running');

-- +goose Down
DROP INDEX app.jobs_live_idempotency_key;
ALTER TABLE app.jobs ADD CONSTRAINT jobs_idempotency_key_key UNIQUE (idempotency_key);
