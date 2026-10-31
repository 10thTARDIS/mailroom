-- SPDX-License-Identifier: AGPL-3.0-only

-- COMPLETING #157's PROJECTION FOR #156 (mailroom#156): the rest of
-- internal/kind's header-only Facts that migration 00016 left uncaptured.
--
-- 00016 projected four signals (list_id, list_unsubscribe, recipient_count,
-- auth_fail) off the message BLOB onto the row, specifically so a training/
-- replay path that reads the row (never the bytes) could see the same bulk
-- signals the serve path parses live. internal/kind.Facts (mailroom#175) has
-- SIX more fields with that identical shape — readable only from the raw
-- headers, sender-independent by construction, and therefore exactly what a
-- tab classifier needs to judge a message from a sender it has never seen:
--
--   list_unsubscribe_post  List-Unsubscribe-Post (RFC 8058 one-click)
--   auto_submitted         Auto-Submitted (RFC 3834) — low coverage, high
--                           precision: marketing avoids it deliberately
--   precedence              the pre-standard bulk/list marker
--   feedback_id             presence alone means a high-volume ESP sender
--   dkim_domains            the DKIM-Signature d= values — "the highest-value
--                           signal available", per kind.go's own comment,
--                           because it identifies sending INFRASTRUCTURE
--                           rather than intent
--   return_path             the envelope sender, VERP/bounce-tracking's tell
--
-- Until now these were computed once at ingest (cmd/mailroom-admin's `classify`
-- and `kind-sweep` re-parse the blob directly) and never stored, so nothing
-- could REPLAY the tab classifier over history without re-opening every blob.
-- This migration is that: the write path (internal/ingest) and a
-- `mailroom-admin backfill-function-signals` command populate them exactly the
-- way 00016's four already work.
--
-- RAW, NOT DERIVED. Every column here is the literal header value (or, for
-- DKIM, the literal list of d= domains) — never a classifier VERDICT. auth_fail
-- is 00016's one exception, and it stayed an exception on purpose: it is the
-- conservative output of ml.AuthResultsFail over possibly many
-- Authentication-Results hops, and re-deriving a multi-hop parser in SQL would
-- mean reimplementing it twice. Nothing here needs that argument — every one of
-- these six is a single header (or repeated header) read verbatim. Storing the
-- verdict instead would mean re-migrating every time a classifier weight moves;
-- storing the raw value means history can be re-scored under next month's
-- weights without touching the database again.
--
-- NULLABLE, and — deliberately — NOT distinguishing "header absent" from "not
-- extracted yet". Both read as NULL, exactly matching 00016's list_id /
-- list_unsubscribe / recipient_count on this SAME table (auth_fail, being a
-- verdict rather than a raw value, is NOT NULL DEFAULT false for the opposite
-- reason: "unknown" and "did not fail" cannot be allowed to collapse there).
-- A three-state column (absent / not-yet-extracted / present) was considered
-- and rejected: nothing downstream would ever read the difference — the kind
-- package already treats an absent header and an empty one identically
-- (`f.ListUnsubscribe != ""`), and the ml replay path COALESCEs every one of
-- 00016's nullable columns to '' before use (store.MessageMLFacts). The ONE
-- place the distinction would matter is this backfill's own dry-run report,
-- and that report gets it for free by re-parsing the stored BYTES at scan
-- time rather than by inspecting the column — so the column itself can stay
-- exactly as simple as its four siblings.
--
-- dkim_domains is the one column where the two states are distinguishable
-- for free and it is called out here so a future reader does not assume an
-- inconsistency: an array has no empty-string ambiguity, so in principle NULL
-- (not extracted) and '{}' (extracted, no DKIM-Signature header present)
-- COULD be told apart. This migration does not exploit that: the write path
-- (ingest and the backfill) collapses an empty extraction to NULL as well, for
-- the same reason as the five text columns above — uniform treatment across
-- one signal block beats a one-off exception that a future reader has to
-- remember, and no consumer asks for the extra state today. If one ever does,
-- the column already supports it; only the write path would need to change.
--
-- NO BACKFILL HERE (ADR-008: forward-only, online backfills). Existing rows
-- get NULL, which reads as "no such header" — wrong for whichever of these a
-- message actually carried, and deliberately not fixed in a migration that
-- would have to open every blob. `mailroom-admin backfill-function-signals`
-- re-derives them from the stored bytes, online, in batches, and reports
-- per-signal presence so an operator can see the real shape before writing.
--
-- No new index. 00016 added messages_user_bulk because the classifier already
-- had a concrete query in hand ("everything from this sender that carries
-- List-Unsubscribe"). No equivalent query exists yet for these six, and
-- speculatively indexing an array column and five sparse text columns with no
-- caller in mind is exactly the kind of thing that should arrive with the
-- query that needs it, not ahead of it.

-- +goose Up
ALTER TABLE app.messages ADD COLUMN list_unsubscribe_post text;
ALTER TABLE app.messages ADD COLUMN auto_submitted text;
ALTER TABLE app.messages ADD COLUMN precedence text;
ALTER TABLE app.messages ADD COLUMN feedback_id text;
ALTER TABLE app.messages ADD COLUMN dkim_domains text[];
ALTER TABLE app.messages ADD COLUMN return_path text;

-- +goose Down
ALTER TABLE app.messages DROP COLUMN list_unsubscribe_post;
ALTER TABLE app.messages DROP COLUMN auto_submitted;
ALTER TABLE app.messages DROP COLUMN precedence;
ALTER TABLE app.messages DROP COLUMN feedback_id;
ALTER TABLE app.messages DROP COLUMN dkim_domains;
ALTER TABLE app.messages DROP COLUMN return_path;
