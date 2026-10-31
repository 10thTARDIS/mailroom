-- SPDX-License-Identifier: AGPL-3.0-only

-- SUPERSEDED CONTENT HASHES (Forgejo #260, refs #47 increment 5, ADR-001).
--
-- Compose-time signing rewrites a message's bytes in place at submit
-- (store.ReplaceMessageBytes). The copy Mailroom had already APPENDed to the
-- account's IMAP Drafts folder still holds the OLD, unsigned bytes — and it is
-- the local row's content hash that dedup is keyed on.
--
-- ⚠ THE CONSEQUENCE, OBSERVED ON A REAL INSTANCE RATHER THAN PREDICTED. A
-- sync pass that reads the Drafts folder before the expunge lands finds bytes
-- matching nothing, and ingests them as a NEW message: a stray, unsigned,
-- already-sent draft sitting in the user's mailbox. Measured timing on the
-- first real signed send:
--
--     19:50:36  expunge queued for the draft copy
--     19:50:37  sync ingests Drafts uid 4  ->  stray row created
--     19:50:44  writeback applies the expunge
--
-- Sync won by seven seconds. A second send lost the race the other way and was
-- clean — which is what makes this the kind of defect that reaches a user and
-- not a test run.
--
-- Before signing this could not happen: the local row and the IMAP copy were
-- byte-identical, so dedup on content_hash matched and no row was created.
-- Dedup worked because the bytes agreed. Signing is what made them disagree.
--
-- WHY A TOMBSTONE RATHER THAN A TIGHTER ORDERING. Moving the expunge earlier
-- narrows the window; it does not close it, because a slow writeback still
-- loses to a fast sync. Recording the hash we USED to hold turns the race into
-- a lookup: however late the stale copy arrives, it is recognised.
--
-- WHY IT MAPS TO A MESSAGE ID RATHER THAN BEING A BARE DENYLIST. The message
-- was mutated IN PLACE, so its id is unchanged and still correct. Returning it
-- lets ingest record the stale delivery as another provenance row against the
-- real message — which is exactly what the existing dedup paths do (#206), and
-- what lets the expunge machinery find that copy later. A bare "ignore these
-- bytes" rule would drop the delivery on the floor and lose that.
--
-- LIFETIME. One row per signed send, deleted with its message by the cascade
-- below. That is a natural bound rather than an unbounded log, so no separate
-- pruning job is added here; if signed volume ever makes it worth reclaiming
-- sooner, the purge sweep is the place.

-- +goose Up

CREATE TABLE app.superseded_content (
  user_id      app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  content_hash bytea NOT NULL,
  message_id   app.ulid NOT NULL REFERENCES app.messages(id) ON DELETE CASCADE,
  created_at   timestamptz NOT NULL DEFAULT now(),
  -- Leads with user_id (the house rule: RLS appends that predicate to every
  -- plan) and is the lookup index — ingest asks exactly this question.
  PRIMARY KEY (user_id, content_hash)
);

COMMENT ON TABLE app.superseded_content IS
  'Content hashes a message USED to have, after its bytes were replaced in '
  'place (currently only compose-time PGP signing). Ingest consults this so a '
  'stale IMAP copy of the pre-replacement bytes is recognised as the same '
  'message rather than ingested as a new one (#260).';

ALTER TABLE app.superseded_content ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.superseded_content FORCE ROW LEVEL SECURITY;
CREATE POLICY superseded_content_isolation ON app.superseded_content
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());

-- No UPDATE: a tombstone is written once and removed, never edited. Same shape
-- as app.category_dismissals, and for the same reason — there is nothing about
-- "these bytes are superseded" that can later become half-true.
GRANT SELECT, INSERT, DELETE ON app.superseded_content TO mailroom_app;

-- +goose Down
DROP TABLE IF EXISTS app.superseded_content;
