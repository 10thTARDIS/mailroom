-- SPDX-License-Identifier: AGPL-3.0-only
--
-- TWO SUBMISSION FIXES FROM THE v1.80.0 BETA, both columns on app.submissions.
--
-- 1. SEND & ARCHIVE ARCHIVES ONLY WHAT WAS ACTUALLY SENT (mailroom#648).
--
-- v1.80.0's Send & archive archived the conversation from the CLIENT, the
-- moment EmailSubmission/set accepted the send. Accepting a send is not
-- sending it: the submission is only QUEUED at that point (the Undo-Send
-- window), and it can still fail. Observed in practice: the archive
-- move landed two seconds after the click, the provider then refused the reply
-- permanently (550 5.7.1, spam detected), and the conversation sat in Archive
-- while the reply never left.
--
-- The archive therefore moves to the server, and to the one moment that knows
-- the answer: the send worker's transition of the submission to 'sent'.
-- archive_on_success is what the client asks for at create time (the Mailroom
-- extension `archiveOnSuccess` on EmailSubmission/set create) and what the
-- worker reads back at that moment. A submission that fails, is cancelled by
-- Undo Send, or exhausts its retries never reaches that transition, so it
-- never archives anything.
--
-- app.ulid[] rather than a join table: it is a request carried by exactly one
-- submission, read exactly once, by id, alongside the row it belongs to. It is
-- NOT a foreign key and cannot be one (Postgres has no element FKs), which is
-- deliberate: a message named here may be deleted before the send fires, and
-- that must not block deleting it. The worker re-resolves every id through the
-- user's own RLS-scoped transaction and skips what it cannot see.
--
-- NULLABLE, NO BACKFILL: NULL means "archive nothing", which is exactly what
-- every existing submission asked for. Bounded so a hostile client cannot park
-- an unbounded array on a row the worker walks; the API refuses the same bound
-- first, with a readable error.
--
-- 2. A FAILED SUBMISSION CAN BE DISMISSED.
--
-- A submission the provider refused for good stayed in the Outbox, and in the
-- rail's Outbox count, forever: EmailSubmission/set destroy is refused (a
-- submission is the permanent record of a send), and discarding the draft
-- only soft-trashes it, which changes nothing the Outbox reads. #589's rule —
-- a failure stays visible until the user acts on it — had no act.
--
-- dismissed_at is that act: set by the Mailroom extension patch
-- `dismissed: true` on EmailSubmission/set update, legal ONLY while the
-- submission is 'failed'. The record of the send attempt is kept (nothing is
-- deleted, the draft included); the Outbox stops listing it. The CHECK makes
-- "dismissed" unrepresentable on any other state, so a dismissed row can never
-- be a pending send the Outbox has stopped showing.
--
-- NULLABLE, NO BACKFILL: no failure has been dismissed before this migration.
--
-- Both columns: app.submissions is user-scoped; the existing
-- submissions_isolation policy and the table-level GRANT to mailroom_app cover
-- them, and no SECURITY DEFINER function reads or writes them. Setting
-- dismissed_at appends the EmailSubmission change_log row (the invariant);
-- archive_on_success rides on the create, which already appends one.

-- +goose Up

ALTER TABLE app.submissions ADD COLUMN archive_on_success app.ulid[];

ALTER TABLE app.submissions
  ADD CONSTRAINT submissions_archive_on_success_bounded
  CHECK (archive_on_success IS NULL OR cardinality(archive_on_success) BETWEEN 1 AND 100);

ALTER TABLE app.submissions ADD COLUMN dismissed_at timestamptz;

ALTER TABLE app.submissions
  ADD CONSTRAINT submissions_dismissed_only_failed
  CHECK (dismissed_at IS NULL OR state = 'failed');

-- +goose Down

ALTER TABLE app.submissions DROP CONSTRAINT IF EXISTS submissions_dismissed_only_failed;
ALTER TABLE app.submissions DROP COLUMN IF EXISTS dismissed_at;
ALTER TABLE app.submissions DROP CONSTRAINT IF EXISTS submissions_archive_on_success_bounded;
ALTER TABLE app.submissions DROP COLUMN IF EXISTS archive_on_success;
