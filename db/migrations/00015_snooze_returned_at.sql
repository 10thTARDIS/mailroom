-- SPDX-License-Identifier: AGPL-3.0-only

-- When a snoozed message came back (spec §6, mailroom#152).
--
-- Snoozing says "show me this at 9am". Returning it to the position it held
-- before — three days down the list, where its received_at puts it — technically
-- honours the wake and defeats the purpose: the user never sees it. It has to
-- come back at the TOP.
--
-- received_at is NOT rewritten to achieve that, and this column exists precisely
-- so it never has to be. That timestamp is when the mail arrived; it is what the
-- IMAP server says, what threading and every date filter mean, and what a client
-- sorting on RFC 8621's receivedAt is promised. Editing it to move a row up a
-- list would make Mailroom lie about a fact it does not own.
--
-- Instead the wake stamps the moment the message RE-arrived in the user's
-- attention, and an ordering can then ask for "effective arrival" —
-- COALESCE(snooze_woke_at, received_at) — which is true on both counts: the mail
-- arrived when it arrived, and it came back when it came back.
--
-- It is also what lets the UI explain itself. A three-day-old message sitting at
-- the top of an inbox with no explanation is a bug as far as the reader is
-- concerned; knowing it returned at 9am is the difference.
--
-- NULL for every message that was never snoozed, which is nearly all of them, so
-- the partial index stays small. It leads with user_id, per RLS.
--
-- Additive and forward-only (ADR-008): one nullable column, no backfill. Mail
-- that woke before this migration simply has no return time and orders by
-- received_at, as it does today.

-- +goose Up
ALTER TABLE app.messages ADD COLUMN snooze_woke_at timestamptz;

CREATE INDEX messages_user_woke ON app.messages (user_id, snooze_woke_at DESC)
  WHERE snooze_woke_at IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS app.messages_user_woke;
ALTER TABLE app.messages DROP COLUMN snooze_woke_at;
