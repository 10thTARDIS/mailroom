-- SPDX-License-Identifier: AGPL-3.0-only

-- THE TAB CLASSIFIER'S VERDICT, STORED — so Email/query can filter by tab
-- (mailroom#228, ADR-022, amending ADR-021).
--
-- ⚠ THIS IS NOT A `tab` COLUMN, and the distinction is the whole design. A
-- message's TAB stays derived:
--
--     resolved_tab := COALESCE(mapping_tab, classified_tab)
--
-- where mapping_tab is read LIVE through the label→tab join. So ADR-021's
-- load-bearing guarantee is untouched: re-pointing one label still moves all of
-- its mail on the next read, with no backfill and nothing left stale. What is
-- stored here is only the CLASSIFIER'S OPINION about a message, which is a
-- different thing and changes on a different cadence.
--
-- WHY STORE ANYTHING AT ALL. The tab strip has to filter, filtering happens in
-- SQL, and the classifier is a first-match decision list in Go
-- (internal/tabs.Classify). Re-expressing those rules in SQL would be two
-- implementations of one thing — #155 settled that for Rule/preview ("a preview
-- that can disagree with the engine is worse than none"), and here the
-- disagreement would be between what the strip COUNTS and what the list SHOWS.
--
-- THE CADENCE ARGUMENT, which is what makes storing it acceptable: a mapping
-- edit is a USER ACTION and must be instant, so the mapping half stays a join. A
-- classifier change is a RELEASE, so a bounded backfill at release time is an
-- ordinary migration concern rather than latency anybody feels.
--
-- classifier_version is per row, not a single instance-wide marker, so staleness
-- is DETECTABLE rather than inferred: `WHERE classifier_version IS DISTINCT FROM
-- $current` is exactly the set a re-classify must visit. A bare updated_at would
-- have made "is this row current?" a question about clocks.
--
-- NULLABLE, and the honest failure mode is written down rather than hidden: a
-- message with NULL classified_tab AND no mapped label is INVISIBLE to a tab
-- filter. That is why `mailroom-admin tabs` reports the NULL count — a half-run
-- backfill must not read as a complete answer (#155's bounded-scan rule).
--
-- NO BACKFILL HERE (ADR-008: forward-only, online backfills). Existing rows get
-- NULL; `mailroom-admin classify-tabs` fills them in batches, dry-run by default
-- and reporting per-tab counts.
--
-- NO INDEX, deliberately, and this one is worth stating because the query DOES
-- exist (unlike migration 00027, which declined an index for want of a caller).
-- The predicate is `COALESCE(<correlated subquery>, classified_tab) = ANY(...)`,
-- and a COALESCE over a subquery is not something an index on classified_tab can
-- serve — the planner has to evaluate the mapping half per row regardless. An
-- index here would be paid for on every write and used on nothing. If tab
-- filtering ever becomes hot enough to need help, the answer is a different
-- query shape, not a speculative index on this column.

-- +goose Up
ALTER TABLE app.messages ADD COLUMN classified_tab app.category_tab;
ALTER TABLE app.messages ADD COLUMN classifier_version int;

-- +goose Down
ALTER TABLE app.messages DROP COLUMN classified_tab;
ALTER TABLE app.messages DROP COLUMN classifier_version;
