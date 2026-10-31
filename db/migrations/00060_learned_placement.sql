-- SPDX-License-Identifier: AGPL-3.0-only
--
-- LEARNED PLACEMENT (#714, ADR-032): Mailroom learns where mail goes from the
-- user's own filings and files new mail from a known sender into that area at
-- ingest. Every part of this migration is EXPAND-ONLY: two new tables, one
-- new column with a default that keeps today's behaviour ON (the chosen
-- default: it acts immediately), and one partial index. Nothing is backfilled
-- and no existing row changes.
--
-- 1. app.learned_placements: THE PLACEMENT LOG. One row per learned
--    placement, written in the ingest transaction that applies the label, so
--    no placement can exist without its record. It is the measurement the
--    correction gate reads (kept vs corrected per area), and it is what a
--    correction is recorded against. The key (address or domain) is stored so
--    a correction can block exactly the key that decided.
--
--    One placement per message at most (PRIMARY KEY (user_id, message_id)):
--    learned placement only ever acts on a NEW message at ingest.
--
--    outcome is a CHECK, not an enum, like client_preferences' vocabularies:
--      open       decided, not yet resolved
--      kept       not corrected within placement.KeptAfter (kept_by 'age'),
--                 or archived from the area it was placed in ('archive')
--      corrected  the user moved it out of that area (took the learned label
--                 off); corrected_at is when
--    The two timestamp CHECKs make a half-resolved row unrepresentable.
--
--    NOT a JMAP-visible object: no client syncs it, so it owns no change_log
--    rows. The label it describes is on app.message_labels, and that write
--    rides the ingest's Email `created` row like the desk's own label does.
--
-- 2. app.learned_placement_gate_resets: THE OPERATOR'S RE-ENABLE. The gate is
--    recomputed from the log at every decision over the placements decided
--    AFTER the area's reset (if any). A reset therefore starts the count
--    afresh without rewriting a single outcome. One row per (user, area).
--
-- 3. app.desk_preferences.learned_placement_enabled: THE OFF SWITCH, per user,
--    enforced at ingest by the store. DEFAULT true is a deliberate decision
--    (#714) and is also what every existing row gets from the ALTER itself,
--    which is catalog-only for a plain boolean with a constant default (no
--    UPDATE, so docs/migrations.md's FORCE-RLS backfill rule does not arise).
--    Declared LAST in db/schema.sql to match ADD COLUMN's append order.
--
-- 4. message_labels_user_filings: THE EVIDENCE INDEX. The evidence query
--    reads only rows with applied_by = 'user' on an area's own label. On a
--    real mailbox (#714) those are a small fraction of the message_labels rows; the
--    existing (user_id, label_id) index would walk all of the desk rows of the
--    area labels at every ingest to find them. Partial, so it costs nothing
--    for the desk's own rows. Leads with user_id like every hot index.
--
-- applied_by gains the value 'learned'. app.message_labels.applied_by has no
-- CHECK (a comment lists the vocabulary), so that needs no DDL here. The
-- writeback_intents source CHECK is deliberately NOT widened: a learned label
-- is written at ingest before the message has a live account, so it enqueues
-- no intent, and an area's own label has no keyword atom anyway (ADR-031).
--
-- RLS: both new tables are user-scoped with FORCE ROW LEVEL SECURITY and the
-- deny-by-default isolation policy every user table carries; the app role
-- gets CRUD and nothing else. No SECURITY DEFINER function reads them.

-- +goose Up

-- +goose StatementBegin
CREATE TABLE app.learned_placements (
  user_id      app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  message_id   app.ulid NOT NULL REFERENCES app.messages(id) ON DELETE CASCADE,
  key_kind     text NOT NULL CHECK (key_kind IN ('address', 'domain')),
  key_value    text NOT NULL,
  area_id      app.ulid NOT NULL REFERENCES app.areas(id) ON DELETE CASCADE,
  decided_at   timestamptz NOT NULL DEFAULT now(),
  outcome      text NOT NULL DEFAULT 'open' CHECK (outcome IN ('open', 'kept', 'corrected')),
  kept_at      timestamptz,
  kept_by      text CHECK (kept_by IN ('age', 'archive')),
  corrected_at timestamptz,
  PRIMARY KEY (user_id, message_id),
  CONSTRAINT learned_placements_kept_stamped
    CHECK ((outcome = 'kept') = (kept_at IS NOT NULL AND kept_by IS NOT NULL)),
  CONSTRAINT learned_placements_corrected_stamped
    CHECK ((outcome = 'corrected') = (corrected_at IS NOT NULL))
);
-- +goose StatementEnd

-- The gate's per-area counts.
CREATE INDEX learned_placements_by_area ON app.learned_placements (user_id, area_id, outcome);
-- The contradiction lookup at every decision: the newest correction on a key.
CREATE INDEX learned_placements_corrections ON app.learned_placements (user_id, key_kind, key_value)
  WHERE outcome = 'corrected';
-- Ageing open placements into kept.
CREATE INDEX learned_placements_open ON app.learned_placements (user_id, decided_at)
  WHERE outcome = 'open';

ALTER TABLE app.learned_placements ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.learned_placements FORCE ROW LEVEL SECURITY;
CREATE POLICY learned_placements_isolation ON app.learned_placements
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());
GRANT SELECT, INSERT, UPDATE, DELETE ON app.learned_placements TO mailroom_app;

-- +goose StatementBegin
CREATE TABLE app.learned_placement_gate_resets (
  user_id  app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  area_id  app.ulid NOT NULL REFERENCES app.areas(id) ON DELETE CASCADE,
  reset_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, area_id)
);
-- +goose StatementEnd

ALTER TABLE app.learned_placement_gate_resets ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.learned_placement_gate_resets FORCE ROW LEVEL SECURITY;
CREATE POLICY learned_placement_gate_resets_isolation ON app.learned_placement_gate_resets
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());
GRANT SELECT, INSERT, UPDATE, DELETE ON app.learned_placement_gate_resets TO mailroom_app;

ALTER TABLE app.desk_preferences ADD COLUMN learned_placement_enabled boolean NOT NULL DEFAULT true;

CREATE INDEX message_labels_user_filings ON app.message_labels (user_id, label_id)
  WHERE applied_by = 'user';

-- +goose Down
DROP INDEX IF EXISTS app.message_labels_user_filings;
ALTER TABLE app.desk_preferences DROP COLUMN IF EXISTS learned_placement_enabled;
DROP TABLE IF EXISTS app.learned_placement_gate_resets;
DROP TABLE IF EXISTS app.learned_placements;
