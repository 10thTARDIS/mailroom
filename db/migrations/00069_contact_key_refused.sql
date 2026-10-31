-- SPDX-License-Identifier: AGPL-3.0-only
--
-- THE USER'S "KEEP THE OLD KEY" DECISION (#47, #626 section E, ADR-039).
-- EXPAND-ONLY (ADR-008): one column with a default, no new table.
--
-- app.contact_keys.refused
--   Set when the user, shown a signature by a key that differs from the one
--   pinned for the address, chose to keep the old key. A refused key is never
--   used to encrypt (compose.EligibleSource) and a signature by it keeps
--   showing as a changed key. "Trust this key" clears it and promotes the key
--   to source 'manual'. A column on the key row, rather than a table, because
--   the decision is about one held key and shares its lifetime (cascade).
--
-- The decision is written together with a Contact change_log row: Contact.keys
-- (source, refused) is part of the synced Contact object.

-- +goose Up
ALTER TABLE app.contact_keys ADD COLUMN refused boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE app.contact_keys DROP COLUMN IF EXISTS refused;
