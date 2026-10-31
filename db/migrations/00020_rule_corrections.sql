-- SPDX-License-Identifier: AGPL-3.0-only

-- WHEN A USER TAKES BACK WHAT A RULE DID (#170).
--
-- Rules apply labels automatically at ingest. Until now, correcting one was a
-- dead end: removing a label is a plain DELETE from app.message_labels, so the
-- row goes and `applied_by` / `applied_by_rule_id` go with it. The rule that put
-- it there keeps firing, on every future message that matches, and nothing
-- anywhere records that a human disagreed.
--
-- That is the difference between a system you can correct and one you have to
-- fight. It matters more the more automation there is: a real mailbox
-- can go from a handful of rules to dozens in a day.
--
-- ONE ROW PER (RULE, MESSAGE, LABEL), not per event. The question this answers
-- is "how many messages has this rule misfiled?", and a user who removes a
-- label, gets it reapplied by the same rule, and removes it again has been
-- wronged about ONE message, twice. Counting events would inflate a rule's
-- apparent badness by how stubborn it is rather than how wrong it is. A repeat
-- refreshes the timestamp.
--
-- IT IS EVIDENCE, NOT AN ACTION. Nothing here disables, edits or narrows a rule.
-- A rule is explicit state the user wrote or accepted, and changing it without
-- asking would make the rules list stop describing what actually happens (#170's
-- own constraint). What this enables is the OFFER: "this rule has misfiled 2
-- messages — narrow it?", with the induction machinery from #168 proposing the
-- tightened form and its blast radius.
--
-- WHY THE MESSAGE ID IS KEPT rather than just a counter: to induce a narrower
-- rule you need to know WHICH messages were wrong, so the proposal can be
-- checked against them. A bare count would say a rule is bad without giving
-- anyone the means to fix it.
--
-- CASCADES, all three: a deleted rule's corrections are meaningless, a purged
-- message cannot be evidence, and a deleted label takes its mistakes with it.

-- +goose Up
CREATE TABLE app.rule_corrections (
  user_id      app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  rule_id      app.ulid NOT NULL REFERENCES app.rules(id) ON DELETE CASCADE,
  message_id   app.ulid NOT NULL REFERENCES app.messages(id) ON DELETE CASCADE,
  label_id     app.ulid NOT NULL REFERENCES app.labels(id) ON DELETE CASCADE,
  corrected_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, rule_id, message_id, label_id)
);
ALTER TABLE app.rule_corrections ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.rule_corrections FORCE ROW LEVEL SECURITY;
CREATE POLICY rule_corrections_isolation ON app.rule_corrections
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());

-- "How many messages has each rule misfiled?" — the count the rules list shows,
-- and the only query this table exists to answer quickly. Leads with user_id per
-- RLS.
CREATE INDEX rule_corrections_by_rule
  ON app.rule_corrections (user_id, rule_id);

-- WITHOUT THIS GRANT the table exists, RLS is correct, and every access is
-- denied — which surfaces as label REMOVAL failing, i.e. the user cannot correct
-- a mistake, which is precisely the situation this table was added to fix.
-- migrate-check does not cover it: it compares the migration against
-- db/schema.sql, so a grant missing from both looks like agreement.
GRANT SELECT, INSERT, UPDATE, DELETE ON app.rule_corrections TO mailroom_app;

-- +goose Down
DROP TABLE IF EXISTS app.rule_corrections;
