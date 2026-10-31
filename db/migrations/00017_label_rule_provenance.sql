-- SPDX-License-Identifier: AGPL-3.0-only

-- Record WHICH rule applied a label (spec §8 T0, mailroom#155).
--
-- THE STATE THIS FIXES: app.message_labels.applied_by has existed since the
-- baseline schema with the vocabulary 'user'|'rule'|'ml'|'agent', and NOTHING
-- has ever written anything but the default. Ingest applies a rule's addLabel
-- through the ordinary ApplyEmailUpdate(AddLabelIDs) path, which takes
-- DEFAULT 'user'. So every automatically-applied label on a user's mail
-- currently asserts the user applied it by hand.
--
-- That is worse than an absent column. A user who finds a surprising label has
-- no way to tell an automation did it, and the one question they will ask —
-- "why is this here?" — is answered with a confident falsehood. #155's last
-- requirement ("labelled by rule X", so a surprise is traceable to its cause)
-- has nothing to read.
--
-- applied_by alone is not enough to answer it: knowing "a rule did this" does
-- not say WHICH, and with several labelling rules that is the whole question.
--
-- ON DELETE SET NULL is the deliberate asymmetry: deleting a rule must never
-- delete mail's labels (CASCADE here would silently unlabel every message that
-- rule ever touched — destroying user-visible state as a side effect of tidying
-- an automation). The label survives and loses its attribution, degrading to
-- exactly what we have today. Never the reverse.
--
-- NO BACKFILL (ADR-008: forward-only). Existing rows keep applied_by='user',
-- which for labels the user really did apply is correct, and for rule-applied
-- ones is the pre-existing inaccuracy — unknowable now, since nothing recorded
-- it at the time. Inventing attribution retroactively would be a guess wearing
-- a fact's clothes.

-- +goose Up
-- The constraint is named explicitly to match db/schema.sql, where the FK is a
-- separate ALTER (message_labels is created long before app.rules): an
-- auto-generated *_fkey name here would diff against the reference DDL.
ALTER TABLE app.message_labels ADD COLUMN applied_by_rule_id app.ulid;
ALTER TABLE app.message_labels
  ADD CONSTRAINT message_labels_applied_by_rule_fk
  FOREIGN KEY (applied_by_rule_id) REFERENCES app.rules(id) ON DELETE SET NULL;

-- "What has this rule labelled?" — the query the rules editor asks to show a
-- rule's real effect, and the one an operator asks before deleting a rule.
-- Leads with user_id per RLS; partial, because the overwhelming majority of
-- label rows are hand-applied and carry NULL here.
CREATE INDEX message_labels_by_rule ON app.message_labels (user_id, applied_by_rule_id)
  WHERE applied_by_rule_id IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS app.message_labels_by_rule;
ALTER TABLE app.message_labels DROP COLUMN applied_by_rule_id;
