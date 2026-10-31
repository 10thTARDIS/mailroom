-- SPDX-License-Identifier: AGPL-3.0-only

-- Give the priority scorer facts it can actually use (spec §8, mailroom#157).
--
-- THE BUG THIS FIXES: on a real mailbox, almost everything scored as
-- important and the top of the ranking was a transactional sender. Two causes,
-- one column group each.
--
-- 1. THE BULK SIGNALS WERE INVISIBLE TO HALF THE SYSTEM.
--
-- List-Id, List-Unsubscribe, the recipient count and the Authentication-Results
-- verdict live in the message BLOB, not on the row. The serve path parses them
-- from headers it holds; the training/replay path (ml.factsFromRow) could not,
-- so it left them permanently false and its own comment said so. The consequence
-- is worse than a missing feature: the model can never LEARN that bulk mail is
-- unimportant, because at training time no message has ever been bulk.
--
-- Projecting them onto the row makes both paths see one set of facts. It is also
-- the discriminator automatic classification needs (#156) to tell a retailer's
-- promotion from that retailer's receipt, so this pays three times.
--
-- The VALUES are stored, not booleans: rules already match listId/listUnsubscribe
-- as text, and a classifier wants to distinguish two lists from the same domain.
-- auth_fail is the exception — it is the conservative VERDICT of parsing possibly
-- many Authentication-Results hops (ml.AuthResultsFail), not a header, and
-- re-deriving it per query would mean re-implementing that parser in SQL.
--
-- 2. "KNOWN CONTACT" MEANT "HAS MAILED YOU A LOT".
--
-- app.contact_addresses is populated from the From of every message RECEIVED, so
-- seen_count measures inbound volume. The cold-start heuristic read that as
-- relationship strength and handed it the largest UP weight in the model — which
-- makes a daily newsletter the most important correspondent in the mailbox. In
-- practice a transactional sender had a large seen_count while the user had sent
-- only a handful of messages in total.
--
-- outbound_count is the signal that actually means correspondence: how many times
-- the user has WRITTEN to this address. It is bumped when a message whose From
-- resolves to one of the user's own identities is ingested — which covers live
-- sends AND an imported Sent folder, so a Gmail archive import bootstraps years
-- of real correspondence in one pass rather than starting from nothing.
--
-- NO BACKFILL HERE (ADR-008: forward-only, online backfills). Existing rows get
-- NULL/0, which reads as "not bulk, never corresponded" — wrong for both, and
-- deliberately not fixed in a migration that would have to open every blob.
-- `mailroom-admin rescore` re-derives the signals from the stored bytes and
-- re-scores, online, in batches.

-- +goose Up
ALTER TABLE app.messages ADD COLUMN list_id text;
ALTER TABLE app.messages ADD COLUMN list_unsubscribe text;
ALTER TABLE app.messages ADD COLUMN recipient_count int;
ALTER TABLE app.messages ADD COLUMN auth_fail boolean NOT NULL DEFAULT false;

-- Bulk mail is the common case in a real mailbox, so this is not a selective
-- partial index; it exists for the classifier's "everything from this sender
-- that carries List-Unsubscribe" queries (#156). Leads with user_id, per RLS.
CREATE INDEX messages_user_bulk ON app.messages (user_id, from_address)
  WHERE list_unsubscribe IS NOT NULL OR list_id IS NOT NULL;

ALTER TABLE app.contact_addresses ADD COLUMN outbound_count int NOT NULL DEFAULT 0;

-- +goose Down
DROP INDEX IF EXISTS app.messages_user_bulk;
ALTER TABLE app.messages DROP COLUMN list_id;
ALTER TABLE app.messages DROP COLUMN list_unsubscribe;
ALTER TABLE app.messages DROP COLUMN recipient_count;
ALTER TABLE app.messages DROP COLUMN auth_fail;
ALTER TABLE app.contact_addresses DROP COLUMN outbound_count;
