-- SPDX-License-Identifier: AGPL-3.0-only
--
-- THE CLEAN-UP ASSISTANT (#53): one index for its ranking query
-- (store.SenderStats). EXPAND-ONLY (ADR-008): a new partial index, nothing
-- else.
--
-- feedback_archive_unread: the ranking's "neglect" reads the caller's
-- `archive_unread` feedback events as a set of message ids. The only index
-- on app.feedback_events is (user_id, created_at), which would walk every
-- feedback row the user has (every mark-read, flag, reply, archive) to find
-- the archive_unread ones. Partial, so it costs nothing for the other
-- actions; leads with user_id like every hot index.
--
-- The messages side needs no new index: the windowed ranking (30/90/365
-- days) is a range scan of messages_user_received, and the all-time ranking
-- is one pass over the user's rows either way. The measured plan and time on
-- a seeded 25k-message mailbox are in sender_stats_integration_test.go.

-- +goose Up
CREATE INDEX feedback_archive_unread ON app.feedback_events (user_id, message_id)
  WHERE action = 'archive_unread';

-- +goose Down
DROP INDEX IF EXISTS app.feedback_archive_unread;
