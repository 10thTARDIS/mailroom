-- SPDX-License-Identifier: AGPL-3.0-only
--
-- WHAT A MERGE BORROWED, SO SEPARATE CAN GIVE IT BACK (#834, ADR-041 §4).
-- EXPAND-ONLY (ADR-008): one new table. No existing column changes meaning and
-- no row is rewritten; there is NO backfill (a merge made before this
-- migration has no record, and its separate behaves as it did in 00072).
--
-- app.contact_merges
--   One row per Contact/merge: the contact merged INTO (into_contact), the
--   tombstone merged away (from_contact), the address rows that merge moved
--   (addresses), into's state before the merge (before_state) and the combined
--   state the merge wrote (after_state). A Contact/separate that takes every
--   one of those addresses back restores each of into's fields to before_state
--   if it still equals after_state, and deletes the row. The two states are
--   JSONB SNAPSHOTS read back whole by the store, never queried by field.
--   RLS forced, deny by default, user_id-led index; grants like app.contacts.
--   No change_log row of its own: it is not a JMAP object, and every write to
--   it rides with the Contact change rows of the merge or separate that made
--   it.
--
-- into_contact / from_contact ON DELETE CASCADE: a record is meaningless once
-- either contact is gone (contacts are deleted only with their user, or by the
-- own-address clean-up when the last address goes, and that cascades to the
-- contact's tombstones).

-- +goose Up
-- +goose StatementBegin
CREATE TABLE app.contact_merges (
  id            app.ulid PRIMARY KEY,
  user_id       app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  into_contact  app.ulid NOT NULL REFERENCES app.contacts(id) ON DELETE CASCADE,
  from_contact  app.ulid NOT NULL REFERENCES app.contacts(id) ON DELETE CASCADE,
  addresses     app.ulid[] NOT NULL,
  before_state  jsonb NOT NULL,
  after_state   jsonb NOT NULL,
  merged_at     timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT contact_merges_two_contacts CHECK (into_contact <> from_contact)
);
-- +goose StatementEnd

CREATE INDEX contact_merges_by_into ON app.contact_merges (user_id, into_contact, id);

ALTER TABLE app.contact_merges ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.contact_merges FORCE ROW LEVEL SECURITY;
CREATE POLICY contact_merges_isolation ON app.contact_merges
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());
GRANT SELECT, INSERT, UPDATE, DELETE ON app.contact_merges TO mailroom_app;

-- +goose Down
DROP TABLE IF EXISTS app.contact_merges;
