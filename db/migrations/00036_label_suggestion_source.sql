-- SPDX-License-Identifier: AGPL-3.0-only

-- WHICH SYSTEM ASKED THE QUESTION (#242).
--
-- app.label_suggestions has had exactly one writer since it was created: the
-- per-label classifier, through ingest's classifyMessage. #242 adds a second —
-- the RULE INDUCER, whose accepted candidates become ordinary app.rules rows
-- carrying a `suggestLabel` action that applyRules executes at ingest. Two
-- producers, one table, and from the row alone there is no way to tell them
-- apart.
--
-- ⚠ THEY MUST BE TOLD APART, AND THE REASON IS confidence. For a model row that
-- column is a calibrated-ish probability: 0.63 means the model put 63% on this
-- label, and a client rendering "63%" is repeating what the model said. For a
-- rule row it is rules.EvidenceConfidence(n) — a pure ORDERING RANK derived from
-- how many messages the user filed under that label, with no probability
-- semantics whatsoever and no measurement behind it (see internal/rules/suggest.go,
-- which says so at length). Rendering 0.8 from four filings as "80% confident"
-- would be this codebase inventing a statistic and putting it in front of a user,
-- which is precisely the failure CLAUDE.md's measurement discipline exists to
-- prevent.
--
-- So `source` is not metadata for a future report. It is what makes the existing
-- confidence column safe to keep sharing: the client branches on it and renders
-- an evidence COUNT for rules and a percentage for models. Without it the only
-- honest option would have been to stop rendering confidence at all.
--
-- ⚠ THE DEFAULT IS A FACT, NOT A GUESS. Every row already in this table was
-- written by classifyMessage — it is the only code path that has ever called
-- PutLabelSuggestion (verified by grep, and by migration 00019's own note that
-- "PutLabelSuggestion has exactly one caller and it runs at ingest"). So
-- backfilling existing rows to 'model' does not assume anything about them; it
-- records what is already true, which is why this can be a plain NOT NULL DEFAULT
-- rather than the nullable "we do not know" column 00019 needed for `reasons`.
-- There is no third state here to lose.
--
-- Forward-only and expand-and-contract (ADR-008): the column is additive, the
-- default backfills existing rows in place, and every reader that does not select
-- it keeps working unchanged. Nothing has to be rewritten and nothing has to be
-- deployed in a particular order.
--
-- NO INDEX CHANGE, and the case is worth putting on the record rather than
-- skipping in silence, since label_suggestions_by_confidence (user_id,
-- confidence DESC, message_id) is the obvious candidate to widen.
--
--   FOR adding source: a future "show me only what the inducer proposed" report
--   would filter on it, and (user_id, source, confidence DESC) would serve that
--   in one range scan. The table is also append-mostly, so a wider index is cheap
--   to maintain relative to reads.
--
--   AGAINST, which wins: no query filters on source today, and — more to the
--   point — neither of the two queries that read this table by confidence can use
--   that index anyway. The needs-a-category ORDER BY is a correlated
--   `(SELECT max(ls.confidence) ... WHERE ls.user_id = m.user_id AND
--   ls.message_id = m.id)` (internal/store/jmap_email.go), and
--   SuggestionsForMessages filters `user_id = $1 AND message_id = ANY($2)`; both
--   probe (user_id, message_id), which is the PRIMARY KEY's own prefix. Adding a
--   column to an index the live query shapes do not use would be paying a write
--   cost on every ingested suggestion for a plan nobody executes. If a
--   source-filtered report ever lands, the right answer is a PARTIAL index
--   (`WHERE source = 'rule'`) sized to that query, decided with its EXPLAIN in
--   hand — not a speculative widening now. (This mirrors 00029's reasoning for
--   declining an index whose query genuinely exists; here the query does not even
--   exist yet.)

-- +goose Up
ALTER TABLE app.label_suggestions
  ADD COLUMN source text NOT NULL DEFAULT 'model';

-- Named explicitly, like mailboxes_sync_scope_known (00026), so the constraint
-- has the same name whether a database was built from the migrations or straight
-- from db/schema.sql — migrate-check diffs pg_dump output of both and a
-- system-generated name is one more thing that has to happen to agree by luck.
ALTER TABLE app.label_suggestions
  ADD CONSTRAINT label_suggestions_source_known
  CHECK (source IN ('model', 'rule'));

-- +goose Down
ALTER TABLE app.label_suggestions
  DROP CONSTRAINT IF EXISTS label_suggestions_source_known;
ALTER TABLE app.label_suggestions DROP COLUMN IF EXISTS source;
