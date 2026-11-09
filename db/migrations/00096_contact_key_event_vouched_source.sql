-- ADR-054 (key rotation): a `pinned` event the user made for a key the previous
-- pin vouched for records the voucher in its source, as "vouched:" followed by the
-- voucher's fingerprint. app.contact_key_events.source is a closed set; this
-- widens it by exactly that one prefix. ADR-054 expected the column to be
-- unconstrained; it is not, and this is the whole change. The form is exactly
-- 'vouched:' plus an UPPERCASE hex fingerprint (40 characters for a v4 key, 64
-- for a v6), as the code writes it, so the column cannot carry free text.
--
-- LOOSENING ONLY (ADR-008): no row changes, every existing value still passes.
-- Down puts the old set back after rewriting the new form to 'user', which is what
-- the event would have said without the voucher.

-- +goose Up
ALTER TABLE app.contact_key_events DROP CONSTRAINT contact_key_events_source_check;
ALTER TABLE app.contact_key_events ADD CONSTRAINT contact_key_events_source_check
  CHECK (source IN ('autocrypt', 'wkd', 'manual', 'keyserver', 'user') OR source ~ '^vouched:[0-9A-F]{40}([0-9A-F]{24})?$');

-- +goose Down
ALTER TABLE app.contact_key_events NO FORCE ROW LEVEL SECURITY;
UPDATE app.contact_key_events SET source = 'user' WHERE source LIKE 'vouched:%';
ALTER TABLE app.contact_key_events FORCE ROW LEVEL SECURITY;
ALTER TABLE app.contact_key_events DROP CONSTRAINT contact_key_events_source_check;
ALTER TABLE app.contact_key_events ADD CONSTRAINT contact_key_events_source_check
  CHECK (source IN ('autocrypt', 'wkd', 'manual', 'keyserver', 'user'));
