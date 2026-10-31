-- SPDX-License-Identifier: AGPL-3.0-only

-- app.submissions.message_id is the ONLY foreign key into app.messages that
-- declares no ON DELETE action (mailroom#148). Its eight siblings all do:
-- provenance, attachments, labels, writeback intents, triage scores and feedback
-- CASCADE; dsn_events SET NULLs, because a bounce is evidence that outlives the
-- message it is about. This one was simply missed, and the default — NO ACTION —
-- makes any message with a send record PERMANENTLY UNDELETABLE.
--
-- That is not a theoretical hole. It surfaced as a raw `SQLSTATE 23503` in the
-- client when a draft-flagged sent message was destroyed, and the same wall sits
-- in front of two paths that have not hit it yet only because they are slow:
--
--   * the Trash purge (ADR-009) hard-deletes a message 30 days after it is
--     trashed. Trash a message you sent and the sweep fails on it, every night,
--     for as long as the row exists — a stuck purge that also strands its blob.
--   * account deletion hard-deletes that account's messages.
--
-- CASCADE, matching the six siblings that chose it, rather than SET NULL:
--
--   * the submission is the record of sending THIS message. Once the user has
--     hard-deleted the message — a deliberate act, 30 days after trashing it, or
--     by deleting the whole account — a send record pointing at nothing is not
--     an audit trail, it is a dangling row nothing can render.
--   * what is genuinely worth keeping outlives it independently: app.dsn_events
--     keeps bounce evidence (its own SET NULL), and app.outbound_ledger keeps
--     per-account quota accounting, which never referenced the message at all.
--   * SET NULL would mean making the column nullable and threading "a submission
--     with no message" through the send path, for a state that only exists after
--     the message is gone and the submission is terminal.
--
-- A scheduled send whose draft is deleted therefore takes its submission with
-- it, which is the intuitive reading of deleting the message. The durable job
-- survives, fires, finds no submission, and no-ops — a path the send worker
-- already handles, because a cancelled submission looks the same to it.
--
-- Constraint-only change: no data is read or rewritten, and the constraint is
-- re-added already VALID because the rows it covers are unchanged.

-- +goose Up
ALTER TABLE app.submissions DROP CONSTRAINT submissions_message_id_fkey;
ALTER TABLE app.submissions
  ADD CONSTRAINT submissions_message_id_fkey
  FOREIGN KEY (message_id) REFERENCES app.messages(id) ON DELETE CASCADE;

-- +goose Down
ALTER TABLE app.submissions DROP CONSTRAINT submissions_message_id_fkey;
ALTER TABLE app.submissions
  ADD CONSTRAINT submissions_message_id_fkey
  FOREIGN KEY (message_id) REFERENCES app.messages(id);
