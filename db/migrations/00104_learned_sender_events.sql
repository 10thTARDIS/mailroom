-- SPDX-License-Identifier: AGPL-3.0-only
--
-- WHERE THE ADDRESS BEHIND A LEARNED-SORTING ACTIVITY ENTRY LIVES. EXPAND-ONLY
-- (ADR-008): one new table, nothing backfilled.
--
-- Forgetting, trusting and untrusting a sender each leave an Activity entry
-- ("Learned sorting forgot <sender>"). The Activity view reads app.audit_log,
-- whose user_id is ON DELETE SET NULL on purpose: audit rows SURVIVE a hard
-- delete of the user. A sender address written into an audit row would
-- therefore outlive the deletion of the account it belongs to, which is the
-- opposite of what a hard delete is for.
--
-- So the audit row carries only this row's id (object_id) and the scope
-- (detail); the address lives HERE, in a user-scoped table that cascades with
-- the user. Activity resolves the address by joining through this table under
-- RLS; when the row is gone it shows "a sender".
--
-- No change_log row: a private activity record, not a JMAP-visible object.
-- Written once, never updated or deleted by the app (it goes with the user).

-- +goose Up

CREATE TABLE app.learned_sender_events (
  id         app.ulid PRIMARY KEY,
  user_id    app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  address    text NOT NULL CHECK (length(address) BETWEEN 3 AND 320),
  kind       text NOT NULL CHECK (kind IN ('forget', 'trust', 'untrust')),
  scope      text NOT NULL CHECK (scope IN ('address', 'domain')),
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX learned_sender_events_by_user ON app.learned_sender_events (user_id, created_at);
ALTER TABLE app.learned_sender_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.learned_sender_events FORCE ROW LEVEL SECURITY;
CREATE POLICY learned_sender_events_isolation ON app.learned_sender_events
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());
GRANT SELECT, INSERT ON app.learned_sender_events TO mailroom_app;

-- +goose Down
DROP TABLE IF EXISTS app.learned_sender_events;
