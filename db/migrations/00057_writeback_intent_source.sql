-- SPDX-License-Identifier: AGPL-3.0-only

-- WHAT ASKED FOR THIS WRITEBACK (#554 item 3, #555, #557).
--
-- app.writeback_intents records what Mailroom wants to do to the mail server,
-- and from the row alone there is no way to tell what asked for it. Seven
-- production paths enqueue intents and every one funnels through a single
-- writer (EnqueueIntent, internal/store/writeback.go — verified by grep for
-- `INSERT INTO app.writeback_intents`, which has exactly one non-test hit), so
-- the rows arrive indistinguishable: a label a person clicked and a label a
-- rule applied at ingest are byte-identical by the time they land here.
--
-- ⚠ THIS IS NOT METADATA FOR A FUTURE REPORT. Three open defects are blocked on
-- it, and all three want the same missing field:
--
--   #555  Restoring a message from Trash does not cancel its queued purge
--         EXPUNGE, and the engine destroys the server copy anyway. The durable
--         fix is to re-check purge eligibility at APPLY time rather than trust
--         a decision made at enqueue -- but the engine cannot re-check, because
--         it cannot tell a purge EXPUNGE from a draft-retirement EXPUNGE. They
--         are the same op with the same shape, and re-checking "is this message
--         still trashed and still due" against a draft retirement would refuse
--         to retire drafts. `source = 'purge'` is the discriminator.
--
--   #554  The ingest-only ledger counts a draft autosave identically to a
--   item 3 deliberate user action, so `--apply`'s headline number cannot be read
--         as "things the user asked for". Measured on a live instance: most
--         intents are '+kw', the op ingest-time rule labelling produces
--         in bulk.
--
--   #557  A superseded draft revision's APPEND is cancelled only while
--         'pending', so during a shadow run it survives as 'suppressed' with
--         its blob already reclaimed, and dead-letters at replay.
--
-- ⚠ NULLABLE, AND NULL IS NOT 'user' -- THE OPPOSITE CHOICE TO 00036's.
--
-- 00036 added label_suggestions.source as NOT NULL DEFAULT 'model' and said so
-- plainly: that table had had exactly ONE writer, so the default "records what
-- is already true". The situation here is the reverse and the same reasoning
-- gives the opposite answer.
--
-- The idempotency key does encode which PATH a historical row came from --
-- 'purge-expunge:' for the sweep, 'append:' and 'expunge:' for the draft
-- machinery, a leading action ULID for everything routed through
-- ApplyEmailUpdate -- and a classification over a live corpus matched every
-- row with none left over. So a backfill is derivable and it is still
-- wrong, because the key proves the PATH and the question is the ACTOR. Every
-- rule-, ml-, desk- and setup-applied change goes down the identical
-- enqueueEmailIntent path as a hand-applied one. Backfilling those '+kw'
-- rows as 'user' would assert provenance nobody recorded.
--
-- That is precisely the falsehood internal/store/label_source.go exists to end,
-- in its own words: applied_by "has carried this vocabulary since the baseline
-- schema and nothing ever wrote to it -- every row said 'user' by default,
-- including the ones a rule created. That made the one question a surprising
-- label provokes ('why is this here?') answerable only with a falsehood."
-- Repeating it one table over, in the migration that exists to make provenance
-- answerable, would be an odd way to start.
--
-- NULL therefore means "enqueued before 00057; the actor is unrecoverable", the
-- same contract app.user_keys.origin states in its COMMENT ON COLUMN (NULL must
-- never be read as 'imported'). Readers must treat NULL as unknown, never as a
-- value. It costs nothing measurable: every pre-00057 row is already in a
-- terminal state ('applied' on the live instance checked: none pending, none
-- suppressed, none dead), and source is only ever READ for rows still awaiting
-- application. There is no report of historical actors to lose, because there
-- was never a record to report.
--
-- ⚠ WHAT THE MEASUREMENT COULD NOT SEE, stated because a clean result invites
-- over-reading: that corpus contained ZERO 'purge-expunge:' rows. The purge
-- classification above is derived from the key format in
-- EnqueuePurgeExpungeIntents (internal/store/purge.go), not confirmed against a
-- single real row. That is a bound on the evidence, not a pass -- an empty
-- bucket cannot distinguish "the pattern is right" from "this path has never
-- run here". It does not affect this migration (those rows would be NULL either
-- way), but it does mean #555's apply-time check must be proven by a test that
-- creates the row, not by trusting this note.
--
-- THE VOCABULARY reuses app.message_labels.applied_by's strings where they mean
-- the same thing, so 'rule' denotes the same actor in both tables and a reader
-- learns one vocabulary rather than two. Two subsystem values are added for the
-- paths no person initiates: 'purge' (the Trash sweep) and 'draft' (autosave
-- and retirement). They are not actors in the applied_by sense, which is why
-- they are new strings rather than a stretched reading of an existing one.
--
-- Forward-only and expand-and-contract (ADR-008): additive, nullable, no
-- backfill, no rewrite, and every reader that does not select it keeps working.
-- Deploy order does not matter -- an old binary writing NULL is exactly what an
-- unattributed row should say.
--
-- NO INDEX, and the case is recorded rather than skipped, since 00036 set that
-- precedent for this table shape.
--
--   FOR: #555's apply-time check filters expunge intents on source = 'purge',
--   and the ledger report will group by (op, source).
--
--   AGAINST, which wins: both queries are already scoped to a single account's
--   claimed batch or a single account's suppressed ledger, which
--   writeback_pending (account_id, created_at) WHERE state IN
--   ('pending','inflight') and writeback_dead already serve. source is a filter
--   applied to a handful of rows already fetched, not a selectivity problem. A
--   partial index sized to a real query, with its EXPLAIN in hand, is the right
--   answer if one ever appears -- the same conclusion 00036 reached, and for
--   the same reason: paying a write cost on every enqueue for a plan nobody
--   executes.

-- +goose Up
ALTER TABLE app.writeback_intents
  ADD COLUMN source text;

-- Named explicitly, like label_suggestions_source_known (00036) and
-- mailboxes_sync_scope_known (00026), so the constraint has the same name
-- whether a database was built from the migrations or straight from
-- db/schema.sql -- migrate-check diffs pg_dump output of both and a
-- system-generated name is one more thing that has to agree by luck.
--
-- `source IS NULL OR ...` rather than a bare IN: a CHECK evaluates to NULL for
-- a NULL input and NULL is not a violation, so the bare form would admit NULL
-- anyway. Saying it out loud costs one clause and stops a reader having to
-- recall three-valued logic to know whether NULL was intended.
ALTER TABLE app.writeback_intents
  ADD CONSTRAINT writeback_intents_source_known
  CHECK (source IS NULL OR source IN
    ('user', 'rule', 'ml', 'agent', 'desk', 'setup', 'purge', 'draft'));

-- +goose Down
ALTER TABLE app.writeback_intents
  DROP CONSTRAINT IF EXISTS writeback_intents_source_known;
ALTER TABLE app.writeback_intents DROP COLUMN IF EXISTS source;
