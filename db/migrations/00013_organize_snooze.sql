-- SPDX-License-Identifier: AGPL-3.0-only

-- Snooze (spec §6, mailroom#51): "wake me about this later". The third and last
-- of the local-first organization primitives, after pin and mute (00012).
--
-- snoozed_until is the wake TIME, and it is the whole state — there is no
-- separate boolean. A message is snoozed exactly while snoozed_until is set and
-- in the future; waking is therefore a single UPDATE to NULL, and a message can
-- never be "snoozed but with no wake time", which a two-column design would
-- allow and then have to defend against.
--
-- JMAP-visible, like pin and mute: a change appends an Email change_log row, so
-- every device learns that a message left (or rejoined) the inbox view.
--
-- LOCAL-ONLY, no IMAP move. Spec §6 says "with writeback where representable",
-- and a snooze COULD be represented as a move out of INBOX and back. It
-- deliberately is not:
--
--   * a move is the one operation that interacts with canonical placement, the
--     expunge reaper and provenance generations all at once — the machinery
--     where a bug loses mail rather than merely misfiling it;
--   * a snooze is a VIEW-level statement ("not now"), and expressing it by
--     relocating the user's mail on the server is a much larger claim than the
--     user made;
--   * the wake is a timer, and a timer that must also undo a remote move has
--     two ways to fail instead of one.
--
-- The honest cost: another IMAP client still shows a snoozed message in INBOX.
-- Consistent with pin and mute, which are local-only for the same reason.
--
-- The wake itself rides the durable job model (spec §16/§14) as kind
-- 'snooze_wake' — the kind the baseline migration's comment already reserved.
-- internal/jobs' dispatcher runs a catch-up pass on boot, so a wake whose time
-- passed while the server was down fires on the next start rather than being
-- lost. That is what makes the timer durable in the sense §6 requires.
--
-- The partial index leads with user_id (RLS appends that predicate) and covers
-- the two hot reads: "is this snoozed" and the Snoozed view's ordering.
--
-- Additive and forward-only (ADR-008): one nullable column, no backfill.

-- +goose Up
ALTER TABLE app.messages ADD COLUMN snoozed_until timestamptz;

CREATE INDEX messages_user_snoozed ON app.messages (user_id, snoozed_until)
  WHERE snoozed_until IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS app.messages_user_snoozed;
ALTER TABLE app.messages DROP COLUMN snoozed_until;
