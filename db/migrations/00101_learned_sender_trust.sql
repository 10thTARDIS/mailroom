-- SPDX-License-Identifier: AGPL-3.0-only
--
-- TRUSTED SENDERS FOR LEARNED SORTING (#169, ADR-032 amendment). EXPAND-ONLY
-- (ADR-008): one new table, nothing backfilled or rewritten.
--
-- Learned sorting acts on a sender only when the sender passes the trusted
-- DMARC check, because a From address is forgeable. Some senders can never pass
-- it: automated mail from the user's own infrastructure arrives with no
-- Authentication-Results header from the trusted server. The user can mark ONE
-- EXACT From address as trusted, and for that address alone the requirement of
-- a trusted DMARC pass is skipped. Every other check still applies (an
-- ambiguous From, the self-spoof and look-alike warnings, the evidence rules,
-- the correction gate). A domain cannot be trusted wholesale, which is why the
-- key is an address and there is no kind column.
--
-- address is stored lowercased and trimmed by the store; the CHECK makes the
-- table refuse any other spelling, so a lookup by equality cannot miss a row
-- that differs only in case.
--
-- No change_log row: this is a private per-user setting, not a JMAP-visible
-- object (no /get, no /changes, no state string of its own), like the
-- rule-suggestion dismissals (00085), so the load-bearing change_log invariant
-- does not apply. Grants include DELETE: untrusting removes the row.

-- +goose Up

CREATE TABLE app.learned_sender_trust (
  user_id    app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  address    text NOT NULL CHECK (length(address) BETWEEN 3 AND 320 AND address = lower(btrim(address))),
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, address)
);
ALTER TABLE app.learned_sender_trust ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.learned_sender_trust FORCE ROW LEVEL SECURITY;
CREATE POLICY learned_sender_trust_isolation ON app.learned_sender_trust
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());
GRANT SELECT, INSERT, DELETE ON app.learned_sender_trust TO mailroom_app;

-- +goose Down
DROP TABLE IF EXISTS app.learned_sender_trust;
