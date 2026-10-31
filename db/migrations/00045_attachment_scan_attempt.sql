-- SPDX-License-Identifier: AGPL-3.0-only
--
-- A row the scanner could not process must still leave the queue
-- (mailroom#409, #407, #415; ADR-027 §5c).
--
-- # The defect these columns close
--
-- The re-scan predicate asks "does this row still need a pass?" and had no way
-- to record that a pass had already HAPPENED and produced nothing. Three
-- populations were therefore due forever:
--
--   1. a part that decoded to zero bytes — `attachscan.PartHash` returns nil for
--      it BY DESIGN (hashing nothing yields one identity every empty part would
--      share, and a single release against it would open the gate on all of
--      them), so `part_hash` stays NULL on every pass and the digest arm, which
--      deliberately bypasses the tier gate, matches it every time;
--   2. a row whose message blob would not read;
--   3. a row whose message the parser now finds no attachment parts in.
--
-- ⚠ THE DAMAGE WAS NOT THE WASTED WORK. `MessagesDueForRescan` is
-- `ORDER BY m.id LIMIT 50` over ingest-ordered ULIDs, so once fifty permanently
-- stuck messages precede a genuinely due one, THE SAME FIFTY RUN FOREVER and
-- nothing else is ever re-scanned — while the job logs `messages=50` every
-- thirty seconds, which reads as progress.
--
-- # Why a new pair of columns rather than reusing scanned_at
--
-- ⚠ BECAUSE `scanned_at` IS A SAFETY SURFACE, NOT A BOOKKEEPING FIELD. ADR-027
-- §1a makes the age of a verdict the thing that justifies the `> 6 months` tier
-- never being re-scanned at all, and `attachment-report` prints it. Refreshing
-- it when nothing was scanned would tell the operator a verdict is fresh in
-- precisely the surface that exists to stop that being said. So the verdict
-- stamp keeps meaning "when this verdict was MADE", and the attempt stamp below
-- means "when a pass last VISITED this row, whatever came of it".
--
-- ⚠ ADD COLUMN IS CATALOG-ONLY HERE, AND THAT IS NOT LUCK. A column of a domain
-- type carrying a CHECK (`app.sha256`, `app.ulid`) forces a FULL TABLE REWRITE
-- under ACCESS EXCLUSIVE — measured at 2.0 s per 300k rows against 2 ms for a
-- plain type. `timestamptz` and `integer` are plain, so this is the 2 ms case.
--
-- ⚠ DECLARED AFTER part_hash BECAUSE ALTER TABLE ADD COLUMN APPENDS. db/schema.sql
-- and this chain must produce byte-identical schemas and CI's migrate-check
-- diffs them INCLUDING COLUMN ORDER — the failure v1.60.0 shipped, which no
-- local run could catch because schema-apply and the integration suite both
-- apply schema.sql, the file that can be wrong.
--
-- No backfill. Both NULL means "no pass has visited this row", which is the
-- honest state for every existing row and is what keeps them due.

-- +goose Up
ALTER TABLE app.attachments
  ADD COLUMN attempted_at         timestamptz,  -- when a pass last visited this row
  ADD COLUMN attempted_db_version integer;      -- clamd signature DB version at that visit

-- The predicate's new leading factor: "we have not already tried this row at
-- this signature version". Leads with user_id because RLS appends that to every
-- plan (CLAUDE.md). NULLs are ordinary btree entries, so the IS NULL arm — the
-- one that matches every row that has never been visited — is served here too.
--
-- ⚠ Anticipatory on the instance that motivated it (268 rows, where a sequential
-- scan wins), and increasingly the selective one as a mailbox is caught up:
-- once most rows carry the current version, this factor is what excludes them.
CREATE INDEX attachments_user_attempted_version
  ON app.attachments (user_id, attempted_db_version);

-- +goose Down
DROP INDEX app.attachments_user_attempted_version;
ALTER TABLE app.attachments
  DROP COLUMN attempted_db_version,
  DROP COLUMN attempted_at;
