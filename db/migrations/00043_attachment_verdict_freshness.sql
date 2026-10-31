-- SPDX-License-Identifier: AGPL-3.0-only
--
-- A verdict is a claim with a timestamp, so record when it was made and what the
-- scanner knew at the time (mailroom#395, ADR-027 §5c, increment 4).
--
-- Until now app.attachments carried `scan_status` and nothing else about a
-- verdict, which is why `attachment-report` prints "NOT YET REPORTABLE: per
-- re-scan tier, oldest verdict, and stamped-vs-current signature version". Those
-- three columns do not exist. These are them.
--
-- ⚠ WHAT IS DELIBERATELY *NOT* A COLUMN: the signature database's PUBLICATION
-- TIMESTAMP. §5c requires it — it is what makes "the DB stopped updating" a
-- fault rather than an invisible no-op — but it is a property of the daemon's
-- current database, not of any row, and clamd hands it over on request. Measured
-- against the deployed daemon, 2026-09-01:
--
--     ClamAV 1.5.4/28110/Tue Sep  1 06:26:54 2026
--             engine  db      publication timestamp
--
-- So it belongs in the live health read next to `searchStalenessSeconds`, not in
-- 257 copies here. Storing it per row would also make it lie the moment a row
-- was not re-scanned.
--
-- ⚠ scanned_engine_version IS NOT REDUNDANT WITH scanned_db_version. §5c
-- requires a re-scan on an engine MAJOR version change, not only on signatures:
-- a new engine can unpack a container the old one could not, so the same bytes
-- and the same signatures can legitimately yield a different verdict. Text
-- rather than an integer because "1.5.4" is not a number.
--
-- ⚠ ALL THREE ARE NULLABLE, AND THE NULL IS LOAD-BEARING RATHER THAN LAZY.
-- A never-scanned attachment genuinely has no verdict time and no stamped
-- version, and there is no honest default: 0 would sort as "scanned by an
-- impossibly old database" and now() would claim a scan that never happened.
--
-- ⚠ AND THAT NULL IS A TRAP FOR THE QUERY THIS MIGRATION EXISTS TO SERVE.
-- ADR-027 §13 and #395 both specify the re-scan predicate as
--
--     WHERE scanned_db_version < current AND tier due
--
-- which SILENTLY MATCHES NOTHING for a never-scanned row, because `NULL < 28110`
-- is NULL rather than true. Verified against this database before writing this
-- file: it would have skipped all 255 unscanned rows — the entire backlog the
-- increment exists to clear — while logging a successful pass. Callers MUST
-- write `(scanned_db_version IS NULL OR scanned_db_version < $1)`.
--
-- No backfill. Every existing row is `unscanned` and correctly gets three NULLs;
-- re-scan picks them up on its first pass precisely because of the IS NULL arm.

-- +goose Up
ALTER TABLE app.attachments
  ADD COLUMN scanned_at             timestamptz,  -- when this verdict was made
  ADD COLUMN scanned_db_version     integer,      -- clamd signature DB version at that moment
  ADD COLUMN scanned_engine_version text;         -- clamd engine version, e.g. '1.5.4'

-- The re-scan job's own predicate, per user, leading with user_id because RLS
-- appends that to every plan (CLAUDE.md). NULLs are ordinary btree entries, so
-- the IS NULL arm is served by this index too.
--
-- ⚠ Anticipatory, and said out loud: this table holds 257 rows on the instance
-- that motivated it, where a sequential scan wins. It is here because the job
-- sweeps every attachment per user on a cadence, so the query is the one thing
-- guaranteed to grow with the mailbox rather than with usage.
CREATE INDEX attachments_user_scanned_version
  ON app.attachments (user_id, scanned_db_version);

-- +goose Down
DROP INDEX app.attachments_user_scanned_version;
ALTER TABLE app.attachments
  DROP COLUMN scanned_engine_version,
  DROP COLUMN scanned_db_version,
  DROP COLUMN scanned_at;
