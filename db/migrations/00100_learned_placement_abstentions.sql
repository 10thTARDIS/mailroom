-- SPDX-License-Identifier: AGPL-3.0-only
--
-- WHY LEARNED PLACEMENT HELD BACK, KEPT PER DAY. EXPAND-ONLY (ADR-008): one new
-- table, nothing backfilled or rewritten.
--
-- Learned placement considers every new message at ingest and files only the
-- ones it is sure of. The per-reason counter (mailroom_learned_placement_total)
-- says how often it held back and why, but a counter resets with the process and
-- belongs to the whole instance. To decide what learned placement should do
-- next, the person running a mailbox needs their OWN last thirty days: how many
-- messages had no sender evidence, how many had too little, how many were
-- mixed, how many were refused for authentication. One row per (user, UTC day,
-- reason) with a running count is the lightest form that answers it: at most a
-- dozen rows a day however much mail arrives.
--
-- reason is the counter's closed vocabulary (store.LearnedPlacementLabels,
-- minus "placed", which app.learned_placements already records per message). It
-- is text with a length check rather than an enum or a CHECK list so that a new
-- reason is a code change, not a migration; the store refuses a reason outside
-- the set.
--
-- No change_log row: this is a private tally, not a JMAP-visible object (no
-- /get, no /changes, no state string), so the load-bearing change_log invariant
-- does not apply to it.
--
-- Grants include UPDATE: each arrival increments the day's row (an upsert). The
-- primary key leads with user_id like every hot index; RLS appends that
-- predicate to every plan.

-- +goose Up

CREATE TABLE app.learned_placement_abstentions (
  user_id app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  day     date NOT NULL,
  reason  text NOT NULL CHECK (length(reason) BETWEEN 1 AND 32),
  n       integer NOT NULL CHECK (n > 0),
  PRIMARY KEY (user_id, day, reason)
);
ALTER TABLE app.learned_placement_abstentions ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.learned_placement_abstentions FORCE ROW LEVEL SECURITY;
CREATE POLICY learned_placement_abstentions_isolation ON app.learned_placement_abstentions
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());
GRANT SELECT, INSERT, UPDATE ON app.learned_placement_abstentions TO mailroom_app;

-- +goose Down
DROP TABLE IF EXISTS app.learned_placement_abstentions;
