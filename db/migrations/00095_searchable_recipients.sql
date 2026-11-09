-- SPDX-License-Identifier: AGPL-3.0-only
--
-- SEARCHABLE RECIPIENTS AND LIST-ID (ADR-055, operator decision Q76).
--
-- Email/query can filter and sort on the sender and the subject because ingest
-- copies them onto the row. Recipients and List-Id lived only in the encrypted
-- message file, so the JMAP `to`/`cc`/`bcc` filters, the `to` sort and the
-- `header` (List-Id) filter could not be answered. These five columns are the
-- stored form, filled at ingest and, for older mail, by
-- `mailroom-admin backfill-recipients`:
--
--   rcpt_to, rcpt_cc, rcpt_bcc  lowercased names and addresses, one per line,
--                               bounded (200 recipients, 128-char names)
--   rcpt_to_sort                first To display name, else address (case kept)
--   list_id_key                 the id inside the <...> of List-Id, lowercased
--
-- NULL means "not filled yet" (and is the backfill's candidate predicate);
-- the empty string means "filled, nothing there". They are set together.
--
-- PRIVACY: these now sit outside the encrypted message file, as from_name and
-- subject already do; a database backup shows who each message went to. This is
-- the operator's informed choice (ADR-055).
--
-- EXPAND-ONLY (ADR-008): five nullable columns, catalog-only, no default, no
-- rewrite and no backfill here (the backfill must decrypt each message, so it is
-- an online admin command). No index: a substring match cannot use a btree and
-- every query already carries user_id; see ADR-055. Nothing JMAP-visible
-- changes, so no change_log row is involved.

-- +goose Up
ALTER TABLE app.messages ADD COLUMN rcpt_to text;
ALTER TABLE app.messages ADD COLUMN rcpt_cc text;
ALTER TABLE app.messages ADD COLUMN rcpt_bcc text;
ALTER TABLE app.messages ADD COLUMN rcpt_to_sort text;
ALTER TABLE app.messages ADD COLUMN list_id_key text;

-- +goose Down
ALTER TABLE app.messages DROP COLUMN IF EXISTS list_id_key;
ALTER TABLE app.messages DROP COLUMN IF EXISTS rcpt_to_sort;
ALTER TABLE app.messages DROP COLUMN IF EXISTS rcpt_bcc;
ALTER TABLE app.messages DROP COLUMN IF EXISTS rcpt_cc;
ALTER TABLE app.messages DROP COLUMN IF EXISTS rcpt_to;
