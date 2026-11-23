-- SPDX-License-Identifier: AGPL-3.0-only
--
-- FORGET WHAT LEARNED SORTING KNOWS ABOUT A SENDER (#167, ADR-032 amendment).
-- EXPAND-ONLY (ADR-008): one new table, nothing backfilled or rewritten.
--
-- Learned sorting reads its evidence fresh at every decision, from the user's
-- own filings and the corrections of its own placements. "Forget this sender"
-- therefore cannot be a stored model to clear. It is a MARK: one row per
-- (user, key kind, key) recording the moment the user asked, and the evidence
-- reads ignore any filing or correction OLDER than the mark. Nothing is
-- deleted (the filings are the user's own labels, and the corrections are the
-- gate's record), and a filing made after the mark counts again, so a sender
-- the user forgot can be learned afresh.
--
-- key_kind and key_value are the placement log's own vocabulary
-- (app.learned_placements): 'address' with the lowercased From address, or
-- 'domain' with its domain. A second "forget" of the same key moves the mark
-- forward (an upsert), so the grant includes UPDATE.
--
-- No change_log row: a private per-user mark, not a JMAP-visible object (no
-- /get, no /changes, no state string), so the load-bearing change_log
-- invariant does not apply (like 00085 and 00101).

-- +goose Up

CREATE TABLE app.learned_sender_forgotten (
  user_id      app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  key_kind     text NOT NULL CHECK (key_kind IN ('address', 'domain')),
  key_value    text NOT NULL CHECK (length(key_value) BETWEEN 1 AND 320 AND key_value = lower(btrim(key_value))),
  forgotten_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, key_kind, key_value)
);
ALTER TABLE app.learned_sender_forgotten ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.learned_sender_forgotten FORCE ROW LEVEL SECURITY;
CREATE POLICY learned_sender_forgotten_isolation ON app.learned_sender_forgotten
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());
GRANT SELECT, INSERT, UPDATE ON app.learned_sender_forgotten TO mailroom_app;

-- +goose Down
DROP TABLE IF EXISTS app.learned_sender_forgotten;
