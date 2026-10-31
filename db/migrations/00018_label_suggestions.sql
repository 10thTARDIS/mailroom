-- SPDX-License-Identifier: AGPL-3.0-only

-- Where the classifier's ABSTENTIONS land (spec §8, mailroom#169).
--
-- The per-label classifier has three outcomes, not two: apply, reject, and "I do
-- not know". The third is the one that makes the feature trustworthy — a system
-- that abstains 10% of the time is more useful than one confidently wrong 3% of
-- the time, because a wrong label is SILENT. Mail ends up somewhere the user
-- does not look and they discover it weeks later, or never.
--
-- But an abstention only helps if it goes somewhere. Without a place to put it,
-- "I do not know" degrades to "do nothing", which is indistinguishable from the
-- model never having run, and the whole point is lost.
--
-- WHY A TABLE, rather than deriving it at read time. Two reasons:
--
--   1. Cost: answering "what needs a category?" would mean scoring every message
--      against every label's model on every query.
--   2. Stability: models retrain. A derived list would silently change what it
--      shows between one look and the next, so a message could vanish from the
--      review queue without anyone deciding anything. A stored suggestion is a
--      decision the system made at a known moment, and it stays until a human
--      or a retrain resolves it.
--
-- Suggestions are NOT labels. Nothing here affects mailbox membership, keywords,
-- or what any other client sees — this table is a queue of questions. The
-- answers, when they come, go through the ordinary label write path with its
-- change_log row.
--
-- CONFIDENCE IS STORED so the surface can order by it: the messages the model
-- came closest to being sure about are the ones where a human's answer teaches
-- it the most.

-- +goose Up
CREATE TABLE app.label_suggestions (
  user_id     app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  message_id  app.ulid NOT NULL REFERENCES app.messages(id) ON DELETE CASCADE,
  label_id    app.ulid NOT NULL REFERENCES app.labels(id) ON DELETE CASCADE,
  -- The model's probability that this label applies, in [0,1]. Within the
  -- abstention band by construction: a confident yes became a label, and a
  -- confident no was dropped.
  confidence  double precision NOT NULL,
  -- The model version that said so, so a suggestion can be attributed and stale
  -- ones recognised after a retrain.
  model_version int NOT NULL DEFAULT 0,
  created_at  timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, message_id, label_id)
);
ALTER TABLE app.label_suggestions ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.label_suggestions FORCE ROW LEVEL SECURITY;
CREATE POLICY label_suggestions_isolation ON app.label_suggestions
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());

-- "What needs a category?", newest and most-confident first. Leads with user_id
-- per RLS.
CREATE INDEX label_suggestions_by_confidence
  ON app.label_suggestions (user_id, confidence DESC, message_id);

-- The app plane needs CRUD, RLS-constrained like every other user-scoped table.
-- WITHOUT THIS the table exists, RLS is correct, and every access is denied —
-- which surfaces as ingest failing on exactly the messages that touch it, not as
-- anything resembling a permissions problem. migrate-check does NOT cover this:
-- it compares the migration against db/schema.sql, so a grant missing from both
-- looks like agreement.
GRANT SELECT, INSERT, UPDATE, DELETE ON app.label_suggestions TO mailroom_app;

-- +goose Down
DROP TABLE IF EXISTS app.label_suggestions;
