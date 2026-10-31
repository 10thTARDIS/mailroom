-- SPDX-License-Identifier: AGPL-3.0-only
--
-- SAVED CONTACTS, THE USER'S OWN NAME AND NOTES, MERGE AND SEPARATE (#816,
-- ADR-041). EXPAND-ONLY (ADR-008): nullable columns and two CHECKs. No
-- existing column changes meaning and no row is rewritten; there is NO
-- backfill here (`mailroom-admin contact-dates-backfill` is the report-first,
-- opt-in backfill for saved_at and the sighting dates).
--
-- app.contacts.saved_at
--   Non-NULL = the contact is SAVED (listed on the Contacts page), and when.
--   Set by ingest when the user sends mail to one of its addresses, by
--   Contact/set isSaved:true, and by isVip:true; cleared only by
--   Contact/set isSaved:false, which deletes nothing else.
--
-- app.contacts.user_display_name
--   The user's own name for the contact. Ingest NEVER writes it; the
--   displayed name is COALESCE(user_display_name, display_name). At most 200
--   characters (the API trims and strips control characters first).
--
-- app.contacts.merged_into
--   A TOMBSTONE marker: non-NULL means this contact was merged into that one
--   (Contact/merge) and is gone from every JMAP read (it was reported
--   destroyed). The row is kept because it holds the merged-away contact's
--   own state (names, notes, VIP, block, saved), which Contact/separate
--   restores. It holds no addresses and no keys. ON DELETE CASCADE: contacts
--   are deleted only with their user, or by the own-address clean-up when the
--   last address goes, and a tombstone of a deleted contact has nothing left
--   to restore into.
--
-- app.contact_addresses.merged_from_contact / app.contact_keys.merged_from_contact
--   The contact the row came from when it was merged in: its home contact,
--   now a tombstone. NULL = the row is in its home contact. Set only when
--   NULL, so a merge of a merge keeps each row's original home. Contact/
--   separate moves an address (and, with the last address of that home, its
--   keys) into a new contact made from the tombstone's state.
--
-- notes: capped at 4,000 characters (the API refuses longer). Existing rows
-- are all '' (checked on a live database, #816), so the CHECK validates cheaply.
--
-- RLS: no new table; the three tables are already FORCE + deny by default.
-- ⚠ COLUMNS ARE APPENDED, so db/schema.sql declares them LAST in each table
-- (migrate-check compares column order).

-- +goose Up
ALTER TABLE app.contacts ADD COLUMN saved_at timestamptz;
ALTER TABLE app.contacts ADD COLUMN user_display_name text;
ALTER TABLE app.contacts ADD COLUMN merged_into app.ulid REFERENCES app.contacts(id) ON DELETE CASCADE;
ALTER TABLE app.contacts ADD CONSTRAINT contacts_notes_length CHECK (char_length(notes) <= 4000);
ALTER TABLE app.contacts ADD CONSTRAINT contacts_user_display_name_length
  CHECK (char_length(user_display_name) <= 200);
ALTER TABLE app.contact_addresses ADD COLUMN merged_from_contact app.ulid
  REFERENCES app.contacts(id) ON DELETE SET NULL;
ALTER TABLE app.contact_keys ADD COLUMN merged_from_contact app.ulid
  REFERENCES app.contacts(id) ON DELETE SET NULL;

-- +goose Down
ALTER TABLE app.contact_keys DROP COLUMN IF EXISTS merged_from_contact;
ALTER TABLE app.contact_addresses DROP COLUMN IF EXISTS merged_from_contact;
ALTER TABLE app.contacts DROP CONSTRAINT IF EXISTS contacts_user_display_name_length;
ALTER TABLE app.contacts DROP CONSTRAINT IF EXISTS contacts_notes_length;
ALTER TABLE app.contacts DROP COLUMN IF EXISTS merged_into;
ALTER TABLE app.contacts DROP COLUMN IF EXISTS user_display_name;
ALTER TABLE app.contacts DROP COLUMN IF EXISTS saved_at;
