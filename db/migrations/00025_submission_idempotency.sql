-- SPDX-License-Identifier: AGPL-3.0-only

-- MAKING A SEND SAFE TO RETRY (#62 — offline compose + queued send).
--
-- The offline action queue has existed since v1.0 and works: an action is applied
-- optimistically, appended to a durable queue, and replayed until the server agrees. It
-- carries pins, snoozes, keyword changes and trashes — all `Email/set` PATCHES, which are
-- safe to replay because applying the same patch twice is a no-op.
--
-- A SEND IS NOT. `EmailSubmission/set create` has no natural idempotency: there is no
-- unique constraint on `submissions.message_id`, so submitting the same draft twice creates
-- two submissions and SENDS THE MESSAGE TWICE. That is why compose has never gone through
-- the queue, and it is the one thing that has to be fixed before it can.
--
-- The failing sequence is ordinary, not exotic: the client posts the submission, the
-- server creates it and sends, and the RESPONSE is lost — a dropped connection, a closed
-- laptop, a proxy timeout. The client cannot tell that from "the request never arrived",
-- so it must retry, and a retry without this column mails the recipient a second copy.
--
-- WHY A CLIENT-SUPPLIED KEY RATHER THAN A CONSTRAINT ON message_id:
--
--   * `UNIQUE (user_id, message_id)` would forbid a DELIBERATE re-send of the same draft.
--     Undo Send cancels a submission (state='canceled'); re-sending afterwards is a
--     reasonable thing to do and that constraint would refuse it.
--   * A partial unique index over the live states would allow that, but it still conflates
--     two different questions: "has this draft been sent?" and "is this the same ATTEMPT I
--     already made?". Only the second one is what a retry needs to know.
--   * A key the client generates once per queued action answers exactly the second
--     question, and answers it without constraining send semantics at all.
--
-- `app.jobs` already carries an `idempotency_key` for the same reason, so this is the
-- established shape in this schema rather than a new idea.
--
-- NULLABLE, and the index is PARTIAL: every submission created before this migration has
-- none, and a submission made by any caller that does not supply one (the CLI, a future
-- non-JMAP path) is still perfectly legal — it simply is not retry-safe, which is the
-- behaviour everything had until now. A NOT NULL column would have required inventing keys
-- for historical rows that no client will ever retry.

-- +goose Up
ALTER TABLE app.submissions ADD COLUMN idempotency_key text;

ALTER TABLE app.submissions
  ADD CONSTRAINT submissions_idempotency_key_bounded
  CHECK (idempotency_key IS NULL OR length(idempotency_key) BETWEEN 8 AND 200);

-- Partial, so the many NULL rows do not collide with each other. Leads with user_id like
-- every other index here (RLS appends that predicate anyway), and a key is scoped to its
-- user: two users' clients generating the same key must not collide.
CREATE UNIQUE INDEX submissions_user_idempotency_key
  ON app.submissions (user_id, idempotency_key)
  WHERE idempotency_key IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS app.submissions_user_idempotency_key;
ALTER TABLE app.submissions DROP CONSTRAINT IF EXISTS submissions_idempotency_key_bounded;
ALTER TABLE app.submissions DROP COLUMN IF EXISTS idempotency_key;
