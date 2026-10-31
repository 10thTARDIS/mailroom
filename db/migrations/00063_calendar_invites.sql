-- SPDX-License-Identifier: AGPL-3.0-only
--
-- CALENDAR INVITATIONS (#725, ADR-033). EXPAND-ONLY (ADR-008): two new
-- tables and their indexes; nothing existing is altered, backfilled or
-- rewritten, so docs/migrations.md's FORCE-RLS backfill rule does not arise.
--
-- 1. app.calendar_invites: ONE ROW PER INVITATION OR CANCELLATION MESSAGE,
--    written by ingest in the message's own transaction. It is an INDEX of
--    the message bytes (which stay the authority: the card is parsed from
--    them on every Email/get), kept for exactly one question the bytes of a
--    single message cannot answer: "is there a newer version of this event,
--    or has it been cancelled?" — i.e. the other messages carrying the same
--    UID. A row is matched only on (uid, recurrence_id, organizer): an
--    update or cancellation from a DIFFERENT organiser is not treated as one,
--    so a stranger who learns a UID cannot cancel the user's meeting on the
--    card by mailing a CANCEL of their own. REPLY messages are not indexed;
--    nothing asks about them across messages.
--
--    recurrence_id is '' for the series and the instance's RECURRENCE-ID
--    otherwise: a cancelled Tuesday is not a cancelled series.
--
--    verified is whether the message is AUTHENTICATED as coming from that
--    organiser (internal/ical Verified: its one From field equals ORGANIZER —
--    Sender is never honoured — and the topmost trusted Authentication-Results carries an
--    aligned DMARC pass). Only a verified CANCEL or update may change ANOTHER
--    message's card; an unverified one shows its own card, marked, and
--    changes nothing else. A UID is not a secret, so (uid, organiser) alone
--    let anyone who had seen an invitation cancel it on the card.
--
--    Mail that arrived before this migration has no row. Its card still
--    renders (from the bytes); it just cannot be linked to a later update
--    until a backfill exists. None is shipped here: the read-only report
--    comes first (CLAUDE.md), and the cost of the gap is a missing
--    "updated" note on an old invitation, not a wrong one.
--
-- 2. app.calendar_responses: THE USER'S OWN ANSWERS. One row per Accept /
--    Maybe / Decline sent, bound to the EmailSubmission that carries the
--    iTIP REPLY. The current answer for an event is the newest row whose
--    submission was not cancelled — so Undo Send takes the answer back with
--    it, and a deleted Sent copy (submission_id SET NULL) does not. Rows are
--    never updated; a second answer is a second row.
--
--    The JMAP-visible half of a response is Email.calendarInvite.response on
--    every message carrying that UID, so writing a row (and cancelling its
--    submission) appends an Email `updated` change for each of them in the
--    same transaction (store/calendar.go).
--
-- RLS: both tables are user-scoped with FORCE ROW LEVEL SECURITY and the
-- deny-by-default isolation policy every user table carries; the app role
-- gets SELECT/INSERT/DELETE on the index (ingest writes it, a re-ingest may
-- replace it) and SELECT/INSERT on the answers (never edited). No SECURITY
-- DEFINER function reads either.

-- +goose Up

-- +goose StatementBegin
CREATE TABLE app.calendar_invites (
  user_id       app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  message_id    app.ulid NOT NULL REFERENCES app.messages(id) ON DELETE CASCADE,
  uid           text NOT NULL CHECK (length(uid) BETWEEN 1 AND 512),
  recurrence_id text NOT NULL DEFAULT '' CHECK (length(recurrence_id) <= 64),
  sequence      integer NOT NULL CHECK (sequence >= 0),
  method        text NOT NULL CHECK (method IN ('REQUEST', 'CANCEL')),
  organizer     citext NOT NULL CHECK (length(organizer) BETWEEN 3 AND 320),
  verified      boolean NOT NULL DEFAULT false,
  recorded_at   timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, message_id)
);
-- +goose StatementEnd

CREATE INDEX calendar_invites_by_uid ON app.calendar_invites (user_id, uid);

ALTER TABLE app.calendar_invites ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.calendar_invites FORCE ROW LEVEL SECURITY;
CREATE POLICY calendar_invites_isolation ON app.calendar_invites
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());
GRANT SELECT, INSERT, DELETE ON app.calendar_invites TO mailroom_app;

-- +goose StatementBegin
CREATE TABLE app.calendar_responses (
  id            app.ulid PRIMARY KEY,
  user_id       app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  message_id    app.ulid REFERENCES app.messages(id) ON DELETE SET NULL,
  uid           text NOT NULL CHECK (length(uid) BETWEEN 1 AND 512),
  recurrence_id text NOT NULL DEFAULT '' CHECK (length(recurrence_id) <= 64),
  organizer     citext NOT NULL CHECK (length(organizer) BETWEEN 3 AND 320),
  sequence      integer NOT NULL CHECK (sequence >= 0),
  partstat      text NOT NULL CHECK (partstat IN ('ACCEPTED', 'TENTATIVE', 'DECLINED')),
  submission_id app.ulid REFERENCES app.submissions(id) ON DELETE SET NULL,
  responded_at  timestamptz NOT NULL DEFAULT now()
);
-- +goose StatementEnd

CREATE INDEX calendar_responses_by_uid ON app.calendar_responses (user_id, uid, responded_at);
CREATE INDEX calendar_responses_by_submission ON app.calendar_responses (user_id, submission_id)
  WHERE submission_id IS NOT NULL;

ALTER TABLE app.calendar_responses ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.calendar_responses FORCE ROW LEVEL SECURITY;
CREATE POLICY calendar_responses_isolation ON app.calendar_responses
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());
GRANT SELECT, INSERT ON app.calendar_responses TO mailroom_app;

-- +goose Down
DROP TABLE IF EXISTS app.calendar_responses;
DROP TABLE IF EXISTS app.calendar_invites;
