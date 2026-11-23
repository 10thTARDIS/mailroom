-- SPDX-License-Identifier: AGPL-3.0-only
--
-- WHAT EVIDENCE A LEARNED PLACEMENT WAS MADE ON (#28, ADR-032 amendment).
-- EXPAND-ONLY (ADR-008): one new column with a constant default (a metadata
-- change in PostgreSQL 11+, no table rewrite); nothing backfilled.
--
-- Learned sorting now acts on weaker evidence than two agreeing filings: a
-- single filing of an address, and a recent run of filings on a key whose older
-- filings disagree. Those placements need (a) a stricter undo, so that a
-- correction of a single-filing placement blocks its key until two more
-- agreeing filings arrive, and (b) their own row in placement-report, so their
-- precision is read apart from the stronger rule's and cannot hide inside a
-- good aggregate. Both need the basis recorded on the placement itself, which
-- is the one thing that cannot be re-derived later (the filings it was made on
-- are current state and move when the user re-files).
--
-- basis: 'evidence' (the ordinary rule: two or more agreeing filings, as
-- before; every existing row), 'single' (one filing of an address) or
-- 'recency' (a recent run into one area on a mixed key). Text with a CHECK, as
-- the outcome column is.

-- +goose Up
ALTER TABLE app.learned_placements
  ADD COLUMN basis text NOT NULL DEFAULT 'evidence' CHECK (basis IN ('evidence', 'single', 'recency'));

-- +goose Down
ALTER TABLE app.learned_placements DROP COLUMN IF EXISTS basis;
