-- SPDX-License-Identifier: AGPL-3.0-only
--
-- WHICH CORRESPONDENT KEYS MAY BE ENCRYPTED TO (#47, ADR-038). EXPAND-ONLY
-- (ADR-008): one new column with a default, one new table.
--
-- app.contact_keys.authenticated
--   Autocrypt keys arrive in a header on mail whose From can be forged, so a
--   stranger can plant a key against someone else's address. This records, at
--   ingest, whether the message that carried the key was AUTHENTICATED for its
--   From domain (a DMARC pass bound to that domain under the topmost trusted
--   Authentication-Results, or an aligned, displayable local DKIM pass).
--   Encryption ignores Autocrypt keys where this is false.
--   Existing rows default to false, which is the honest value: nobody checked.
--   Keys from other sources do not consult it (a key the user imported, or
--   fetched from the recipient's own domain over HTTPS, is eligible on its own
--   terms).
--
-- app.contact_key_pins
--   Trust on first use. The fingerprint actually encrypted to for each address,
--   per user. A later send whose resolved key differs is REFUSED until the user
--   confirms the change. One row per (user, address); an explicit import may
--   replace it.
--
-- NO change_log ROW: neither is a JMAP-visible object (see
-- store.UpsertContactKey for the same reasoning about contact_keys).

-- +goose Up

ALTER TABLE app.contact_keys ADD COLUMN authenticated boolean NOT NULL DEFAULT false;

-- +goose StatementBegin
CREATE TABLE app.contact_key_pins (
  user_id     app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  address     citext NOT NULL,
  fingerprint text NOT NULL,
  source      text NOT NULL,
  pinned_at   timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, address)
);
-- +goose StatementEnd

ALTER TABLE app.contact_key_pins ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.contact_key_pins FORCE ROW LEVEL SECURITY;
CREATE POLICY contact_key_pins_isolation ON app.contact_key_pins
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());
GRANT SELECT, INSERT, UPDATE, DELETE ON app.contact_key_pins TO mailroom_app;

-- +goose Down
DROP TABLE IF EXISTS app.contact_key_pins;
ALTER TABLE app.contact_keys DROP COLUMN IF EXISTS authenticated;
