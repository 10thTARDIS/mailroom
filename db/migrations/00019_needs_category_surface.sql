-- SPDX-License-Identifier: AGPL-3.0-only

-- WHY a suggestion is being made, not just how confident it is (#176, #169).
--
-- The classifier already computes this: ml.Decide fills LabelVerdict.Reasons
-- with the top per-feature contributions behind the confidence, and the doc
-- comment on that field calls it "the same explainability contract the priority
-- score carries (#34)". It was then DROPPED at the ingest boundary — there was
-- no column to put it in, so internal/ingest read the verdict, stored the
-- number, and threw the explanation away.
--
-- That made the needs-a-category surface unbuildable as specified. A queue of
-- "this might be Shopping (0.63)" with no way to ask WHY is a queue of
-- assertions, and #156's whole position is that an automation nobody can
-- interrogate is one nobody should trust. Worse here than for the priority
-- score: the priority score orders an inbox the user can still read past, while
-- an answer given in this queue becomes training data.
--
-- SHAPE IS DELIBERATELY IDENTICAL to app.triage_scores.top_features: the raw
-- json.Marshal of a []ml.FeatureContribution ({key, weight}), decoded and mapped
-- through ml.FeatureLabel at render time. Two panels asking "why?" about two
-- different models should not be two different payloads, and rendering them
-- through one function is what stops them drifting.
--
-- NULLABLE, with no default. Three states that must stay distinguishable:
--   NULL  — a suggestion written before this column existed, or by a model that
--           could not explain itself. "We do not know why" is the honest answer
--           and the client says so.
--   '[]'  — the model ran, explained, and had nothing above the noise floor.
--   [...] — real contributions.
-- A DEFAULT of '[]' would collapse the first into the second and turn "no record"
-- into a claim of "no reasons".

-- ----------------------------------------------------------------------------
-- "This message does not need a category."
-- ----------------------------------------------------------------------------
--
-- WHY THIS TABLE EXISTS, measured rather than assumed. On a real
-- mailbox at the time this was written: most messages unlabelled, only a few
-- hand-labelled, no rows in app.ml_models, no rows in app.label_suggestions. The needs-a-category
-- queue, defined strictly as "the classifier abstained", is therefore EMPTY —
-- and stays empty until a model exists AND new mail arrives, because
-- PutLabelSuggestion has exactly one caller and it runs at ingest.
--
-- A review surface that renders zero rows on the only mailbox anyone has is not
-- a review surface; it is a page whose empty state congratulates the user while
-- hundreds of uncategorised messages sit in their inbox.
--
-- So `needsCategory` means what its name says: NOTHING HAS CATEGORISED THIS
-- MESSAGE. An open suggestion counts (the system asked and is waiting), and so
-- does the plain absence of any label at all — which is #176's own second listed
-- source, "mail no rule and no kind-binding covers". The queue is then non-empty
-- on day one for every user, which is exactly when supervision is worth the
-- most and exactly when there is no model to produce it.
--
-- THAT CHANGE IS WHAT FORCES THIS TABLE. While the queue was only abstentions,
-- "none of these" could delete the suggestion rows and the message left the
-- queue as a side effect. Once absence-of-a-label puts a message in the queue,
-- deleting suggestions no longer removes anything — the message is still
-- unlabelled — so a dismissal would visibly do nothing, and the queue could
-- never be emptied by any means except labelling every message in it.
--
-- A tombstone, not a flag on app.messages: it is sparse (most mail is never
-- dismissed), it belongs to the review surface rather than to the message, and
-- it is durable against a future retrain — which was the open question logged
-- against deleting suggestion rows. A re-classified message must not re-ask a
-- question the user has already declined to answer.
--
-- Dismissing records NOTHING about any label being wrong. A user clearing a
-- first-run backlog is saying "not now" at least as often as "not this", and the
-- two are indistinguishable from here; training on a guess about which is how a
-- corpus acquires labels nobody meant.

-- +goose Up
ALTER TABLE app.label_suggestions ADD COLUMN reasons jsonb;

CREATE TABLE app.category_dismissals (
  user_id      app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  message_id   app.ulid NOT NULL REFERENCES app.messages(id) ON DELETE CASCADE,
  dismissed_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, message_id)
);
ALTER TABLE app.category_dismissals ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.category_dismissals FORCE ROW LEVEL SECURITY;
CREATE POLICY category_dismissals_isolation ON app.category_dismissals
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());

-- The primary key already leads with user_id, which is what the needsCategory
-- NOT EXISTS probes on, so no second index is needed.

-- WITHOUT THIS GRANT the table exists, RLS is correct, and every access is
-- denied — which surfaces as the review queue failing rather than as anything
-- resembling a permissions problem. migrate-check does not cover it: it compares
-- the migration against db/schema.sql, so a grant missing from both looks like
-- agreement. (The same footgun 00018 documents.)
GRANT SELECT, INSERT, DELETE ON app.category_dismissals TO mailroom_app;

-- +goose Down
DROP TABLE IF EXISTS app.category_dismissals;
ALTER TABLE app.label_suggestions DROP COLUMN reasons;
