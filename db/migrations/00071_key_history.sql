-- SPDX-License-Identifier: AGPL-3.0-only
--
-- A LASTING KEY HISTORY (#815, ADR-040 amendment §11). Also the refresh
-- bookkeeping column of #818 (§15).
-- EXPAND-ONLY (ADR-008): four nullable columns and one new table. No existing
-- column changes meaning, and no row is rewritten.
--
-- app.user_keys.retired_at
--   When the key stopped being used: set by retire (decryptOnly) and by revoke,
--   whichever came first. NULL means NOT RECORDED: every key retired or revoked
--   before this migration has NULL here, and a reader must show "not recorded"
--   rather than "never retired" (decrypt_only and revoked_at still say WHETHER).
--   The backfill is deliberately none: inventing a date from revoked_at would
--   be a guess for a retire, and a wrong date in a history is worse than none.
--
-- app.user_keys.replaced_by
--   The key that replaced this one (UserKey/replace, #817). Same user by
--   construction (store.ReplaceUserKey). NULL: not replaced, or not recorded.
--
-- app.contact_keys.removed_at
--   ContactKey/set destroy becomes a SOFT delete: the row stays, so its history
--   (the events below) stays attached, and every reader that chooses a key for
--   encryption, verification, Autocrypt, WKD de-duplication or "current"
--   display excludes it (store: WHERE removed_at IS NULL). A re-sighting of a
--   removed key REVIVES the row (removed_at cleared, a first_seen event): see
--   ADR-040 §11.
--
-- app.contact_keys.checked_at
--   When ContactKey/refresh last asked WKD about this key (#818). Bookkeeping
--   for the minimum interval between refreshes; not on the wire.
--
-- app.contact_key_events
--   One row per real change to a held correspondent key, appended in the same
--   savepoint as the change: first_seen, renewed, updated, revoked_seen,
--   pinned, refused, removed. change_log cannot be this history (it is
--   compacted). RLS forced, deny by default, user_id-led index; grants like
--   app.contact_keys. No change_log row of its own: it is read through
--   ContactKey/history, and every event rides with the Contact change row its
--   mutation already writes.
--
-- ⚠ COLUMNS ARE APPENDED, so db/schema.sql declares them LAST in each table
-- (migrate-check compares column order).

-- +goose Up
ALTER TABLE app.user_keys ADD COLUMN retired_at timestamptz;
ALTER TABLE app.user_keys ADD COLUMN replaced_by app.ulid REFERENCES app.user_keys(id) ON DELETE SET NULL;
ALTER TABLE app.contact_keys ADD COLUMN removed_at timestamptz;
ALTER TABLE app.contact_keys ADD COLUMN checked_at timestamptz;

-- +goose StatementBegin
CREATE TABLE app.contact_key_events (
  id             app.ulid PRIMARY KEY,
  user_id        app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  contact_key_id app.ulid NOT NULL REFERENCES app.contact_keys(id) ON DELETE CASCADE,
  kind           text NOT NULL CHECK (kind IN ('first_seen', 'renewed', 'updated', 'revoked_seen',
                                               'pinned', 'refused', 'removed')),
  source         text NOT NULL CHECK (source IN ('autocrypt', 'wkd', 'manual', 'keyserver', 'user')),
  at             timestamptz NOT NULL DEFAULT now(),
  expires_at     timestamptz
);
-- +goose StatementEnd

CREATE INDEX contact_key_events_by_key ON app.contact_key_events (user_id, contact_key_id, at);

ALTER TABLE app.contact_key_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.contact_key_events FORCE ROW LEVEL SECURITY;
CREATE POLICY contact_key_events_isolation ON app.contact_key_events
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());
GRANT SELECT, INSERT, UPDATE, DELETE ON app.contact_key_events TO mailroom_app;

-- +goose Down
DROP TABLE IF EXISTS app.contact_key_events;
ALTER TABLE app.contact_keys DROP COLUMN IF EXISTS checked_at;
ALTER TABLE app.contact_keys DROP COLUMN IF EXISTS removed_at;
ALTER TABLE app.user_keys DROP COLUMN IF EXISTS replaced_by;
ALTER TABLE app.user_keys DROP COLUMN IF EXISTS retired_at;
