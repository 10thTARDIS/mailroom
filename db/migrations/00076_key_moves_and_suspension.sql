-- SPDX-License-Identifier: AGPL-3.0-only
--
-- API KEYS: AREA MOVES THAT DO NOT TRAIN PLACEMENT, AND SUSPENSION ON BY
-- DEFAULT (#49; ADR-042
-- §3a and §6).
--
-- EXPAND-ONLY (ADR-008). Three changes, each additive:
--
-- 1. app.learned_placements gains the outcome 'withdrawn' and its stamp
--    withdrawn_at. An API key may now file mail into areas, and a key taking a
--    learned label off is neither the user's correction (it would block the
--    sender's key) nor something to leave 'open' (an open placement ages into
--    'kept' and would pad the correction gate with a placement nobody kept).
--    'withdrawn' is counted as neither. The outcome CHECK is widened under the
--    constraint name it already has (db/schema.sql keeps it inline), added
--    NOT VALID and validated in its own statement (docs/migrations.md:
--    "Widening a CHECK on a table that grows"); the stamp CHECK is the same
--    shape as kept_stamped and corrected_stamped. Every existing row has
--    outcome in the old list and withdrawn_at NULL, so both validate.
--
-- 2. app.submissions gains api_key_id: the API key that made the send, NULL
--    for a person. The send worker archives after a send (Send & archive,
--    #648); for a key's send that archive is the key's, and must not keep a
--    learned placement on the user's behalf. Not a foreign key, like
--    archive_on_success: it is provenance, and a key is revoked, never
--    deleted (user deletion cascades the submission itself).
--
-- 3. suspend_on_anomaly is turned ON for every existing key. Its column
--    default and the API's create default have been ON since 00074; the
--    decision is that it applies to every anomaly rule and is on for
--    existing keys too, none of which is in real use yet. app.api_keys holds
--    at most 20 unrevoked keys per user (MaxActiveKeysPerUser), so this is a
--    one-statement update of a tiny table, not a backfill. FORCE RLS is lifted
--    for the statement (mailroom#505).

-- +goose Up
ALTER TABLE app.learned_placements ADD COLUMN withdrawn_at timestamptz;
ALTER TABLE app.learned_placements DROP CONSTRAINT learned_placements_outcome_check;
ALTER TABLE app.learned_placements ADD CONSTRAINT learned_placements_outcome_check
  CHECK (outcome IN ('open', 'kept', 'corrected', 'withdrawn')) NOT VALID;
ALTER TABLE app.learned_placements VALIDATE CONSTRAINT learned_placements_outcome_check;
ALTER TABLE app.learned_placements ADD CONSTRAINT learned_placements_withdrawn_stamped
  CHECK ((outcome = 'withdrawn') = (withdrawn_at IS NOT NULL)) NOT VALID;
ALTER TABLE app.learned_placements VALIDATE CONSTRAINT learned_placements_withdrawn_stamped;

ALTER TABLE app.submissions ADD COLUMN api_key_id app.ulid;

ALTER TABLE app.api_keys NO FORCE ROW LEVEL SECURITY;
UPDATE app.api_keys SET suspend_on_anomaly = true WHERE NOT suspend_on_anomaly;
ALTER TABLE app.api_keys FORCE ROW LEVEL SECURITY;

-- +goose Down
-- A withdrawn placement goes back to 'open' (the closest old outcome: nobody
-- judged it), so the narrower CHECK can be restored. The suspend_on_anomaly
-- update is not reversed: the old value is not recorded, and ON is the
-- column default either way.
ALTER TABLE app.submissions DROP COLUMN IF EXISTS api_key_id;
ALTER TABLE app.learned_placements DROP CONSTRAINT IF EXISTS learned_placements_withdrawn_stamped;
ALTER TABLE app.learned_placements NO FORCE ROW LEVEL SECURITY;
UPDATE app.learned_placements SET outcome = 'open' WHERE outcome = 'withdrawn';
ALTER TABLE app.learned_placements FORCE ROW LEVEL SECURITY;
ALTER TABLE app.learned_placements DROP CONSTRAINT learned_placements_outcome_check;
ALTER TABLE app.learned_placements ADD CONSTRAINT learned_placements_outcome_check
  CHECK (outcome IN ('open', 'kept', 'corrected'));
ALTER TABLE app.learned_placements DROP COLUMN IF EXISTS withdrawn_at;
