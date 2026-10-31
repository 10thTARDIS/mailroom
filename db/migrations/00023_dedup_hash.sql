-- SPDX-License-Identifier: AGPL-3.0-only

-- CATCHING RE-DELIVERED MAIL (#206, ADR-018 amending ADR-001).
--
-- ADR-001 makes a message's identity SHA-256 of its raw RFC 5322 bytes, and states
-- the consequence out loud: "a transport that mutates bytes creates a second
-- message, which is correct." That is right for IDENTITY — content_hash is the blob
-- address and the FK hub of this schema, and nothing here touches it.
--
-- But it means the case dedup most obviously exists for is the one it cannot see.
-- When a mail path delivers the same message twice, each delivery is stamped with
-- its own trace metadata, so the bytes differ, so it becomes two messages. A
-- real inbox showed some mail three and four times: a small share of the mailbox
-- was redundant rows.
--
-- WHAT WAS MEASURED, because the fix depended on it. `mailroom-admin
-- diff-duplicates` decrypted every duplicate group and diffed it. The BODIES WERE
-- BYTE-IDENTICAL IN ALL 18. `Received:` differed in 16; the other two also differed
-- in X-Gm-Message-State, X-Google-Dkim-Signature, X-Forwarded-Encrypted,
-- X-Migadu-Queue-Id, X-Migadu-Spam-Score, X-Migadu-Flow, X-Migadu-Reason,
-- X-Report-Abuse and a re-signed Dkim-Signature — per-delivery stamping from one
-- forwarder and one provider.
--
-- So dedup_hash is an ALLOWLIST, not the bytes minus a denylist:
--
--     SHA-256(Message-ID, Date, From, To, Cc, Subject + body)   [ingest.DedupHash]
--
-- Subtracting provider headers would have worked today and rotted on the next
-- provider — two of them contributed nine fields between them, from ONE mailbox.
-- Hashing what identifies a message needs no per-provider entry, ever.
--
-- WHY NULLABLE. Forward-only expand-and-contract (ADR-008): the column is added
-- nullable, existing rows are backfilled online by `mailroom-admin
-- backfill-dedup-hash` (which must decrypt each blob, so it cannot be a SQL
-- UPDATE), and nothing depends on it being populated. A row with a NULL dedup_hash
-- simply does not participate in re-delivery dedup, which is exactly how every
-- message behaved before this migration.
--
-- WHY NOT UNIQUE. Two rows sharing a dedup_hash is precisely the condition this
-- exists to detect, and such rows can already exist in a live database — a
-- unique index could not be created and, if it could, would turn existing data into
-- an error. Once those rows are collapsed (#206 part 3, which touches the FK hub and
-- so is deliberately a separate, dry-run-first step), a follow-up migration can add
-- the unique index and make the guarantee structural. Until then ingest's lookup is
-- what enforces it, and a lost race inserts a duplicate rather than failing a
-- delivery — the safe direction for mail.

-- +goose Up
ALTER TABLE app.messages ADD COLUMN dedup_hash bytea;

-- Partial: only populated rows are ever looked up, and while the backfill is still
-- running that is most of the table. Leads with user_id like every other hot index
-- here, because RLS appends that predicate to every plan.
CREATE INDEX messages_user_dedup_hash ON app.messages (user_id, dedup_hash)
  WHERE dedup_hash IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS app.messages_user_dedup_hash;
ALTER TABLE app.messages DROP COLUMN IF EXISTS dedup_hash;
