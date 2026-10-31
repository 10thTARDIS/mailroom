-- SPDX-License-Identifier: AGPL-3.0-only
--
-- THE GMAIL READ PATH'S BOOKKEEPING (#52, ADR-047, increment 5a).
--
-- EXPAND-ONLY (ADR-008). Seven nullable columns appended
-- to three existing tables (catalog-only changes, no rewrite, no backfill)
-- and one partial index.
-- None of it is JMAP-visible, so none of it carries a change_log row (like
-- the #620 poll/observation baselines on app.mailboxes).
--
-- 1. app.mailboxes.backfill_floor_uid — the NEWEST-FIRST import's floor. A
--    Gmail account's first sync starts its cursor at UIDNEXT, so new mail
--    flows at once, and walks history downward under the daily byte budget.
--    Every UID at or above the floor (and at or below last_seen_uid) has been
--    fetched; everything below it is history not yet imported. NULL = no
--    floor: the mailbox was synced oldest-first and everything below the
--    cursor is owed (every non-Gmail mailbox, unchanged). It is also what
--    keeps the #93 gap backfill from reading all of un-imported history as
--    "gaps" and fetching it unpaced: a gap is a UID at or above the floor.
--
-- 2. app.mailboxes.labels_swept_at — when the Gmail label sweep last read
--    this mailbox's X-GM-LABELS (the fallback for label changes CONDSTORE
--    does not report; ADR-047 §4). NULL = never. label_sweep_next_uid is
--    where an interrupted sweep resumes (NULL = none in progress), so a
--    failure part-way does not restart a full sweep every pass.
--
-- 3. app.message_provenance.gm_labels — the X-GM-LABELS Gmail last reported
--    for this physical copy, decoded (`\Inbox`, `Work`). It is what a label
--    change is diffed against, so Mailroom applies Gmail's CHANGES rather
--    than re-asserting Gmail's whole state over the user's own edits, and it
--    keeps the record of which labels came from Gmail. NULL = never observed
--    (every non-Gmail row). gm_thrid is X-GM-THRID, beside the existing
--    gm_msgid and in the same text form: ingest's threading hint when no
--    Message-ID hint matched (store.ThreadByGmailThrID), indexed by a partial
--    index that leads with user_id. Every existing row has gm_thrid NULL, so
--    the build scans the table once and indexes nothing (the 00060 shape).
--
-- 4. app.account_sync_status.download_hour / download_hours — the per-account
--    download budget's meter, a ROLLING 24 HOURS: download_hours is a ring of
--    25 one-hour buckets of message-body bytes (indexed by hour-since-epoch
--    mod 25) and download_hour the start of the hour last written. 25, not
--    24, so a byte stays counted until at least 24 hours after the END of its
--    hour: the window is conservative by up to one hour. Rolling
--    because whether Gmail's daily limit is a calendar day or a rolling 24
--    hours is UNVERIFIED, and a calendar-day meter would allow twice the
--    budget across its midnight. Gmail suspends an account's IMAP everywhere
--    past its limit, so this is persisted per FETCH command and survives a
--    restart. Per Mailroom account, not per Google identity.
--
-- The index is a plain (not CONCURRENTLY) CREATE INDEX, inside goose's
-- per-file transaction like every migration here; 00060 built one on a larger
-- table the same way. On existing rows it indexes nothing (all NULL).

-- +goose Up
ALTER TABLE app.mailboxes ADD COLUMN backfill_floor_uid bigint;
ALTER TABLE app.mailboxes ADD COLUMN labels_swept_at timestamptz;
ALTER TABLE app.mailboxes ADD COLUMN label_sweep_next_uid bigint;
ALTER TABLE app.message_provenance ADD COLUMN gm_labels text[];
ALTER TABLE app.message_provenance ADD COLUMN gm_thrid text;
CREATE INDEX provenance_gm_thrid ON app.message_provenance (user_id, account_id, gm_thrid)
  WHERE gm_thrid IS NOT NULL;
ALTER TABLE app.account_sync_status ADD COLUMN download_hour timestamptz;
ALTER TABLE app.account_sync_status ADD COLUMN download_hours bigint[];

-- +goose Down
ALTER TABLE app.account_sync_status DROP COLUMN download_hours;
ALTER TABLE app.account_sync_status DROP COLUMN download_hour;
DROP INDEX app.provenance_gm_thrid;
ALTER TABLE app.message_provenance DROP COLUMN gm_thrid;
ALTER TABLE app.message_provenance DROP COLUMN gm_labels;
ALTER TABLE app.mailboxes DROP COLUMN label_sweep_next_uid;
ALTER TABLE app.mailboxes DROP COLUMN labels_swept_at;
ALTER TABLE app.mailboxes DROP COLUMN backfill_floor_uid;
