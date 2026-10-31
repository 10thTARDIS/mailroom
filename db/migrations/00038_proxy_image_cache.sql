-- SPDX-License-Identifier: AGPL-3.0-only

-- THE IMAGE PROXY'S CACHE, AND THE THREE THINGS IT COSTS (ADR-026 §3e/§7, #55).
--
-- Decided: put the cache in the blob store rather than in a
-- separate one. That buys real things: encryption at rest under the user's own
-- DEK for free, content-addressed dedup that matters at this shape (in practice a
-- mailbox's images come from a small number of hosts), and no second store for #54's hard-delete
-- sweep to learn about — the blob store is already in it.
--
-- ⚠ BUT IT IS NOT FREE, AND THIS MIGRATION IS TWO OF THE THREE PREREQUISITES.
-- app.blobs was keyed (user_id, content_hash) with NOTHING distinguishing a
-- stranger's marketing image from the user's own mail, and two properties that
-- had been true by construction stop being true the moment third-party bytes
-- land there:
--
--   1. "Everything in app.blobs is the user's mail." app.users.quota_storage_bytes
--      is enforced against an unconditional sum(size_stored) over this table
--      (internal/store/quota.go). Left alone, a stranger's images would consume
--      the quota that bounds the user's own mail — and a user pushed over it
--      stops being able to RECEIVE OR SEND. That is the cache breaking mail
--      delivery, not a tidiness point. `kind` is what lets the sum exclude them.
--
--   2. "Everything in app.blobs is worth backing up." Third-party images are
--      derived and re-fetchable; the user's mail is not. `kind` is what lets a
--      backup exclude them later, and what lets an operator see at a glance how
--      much of the store is not theirs.
--
-- The third prerequisite is in Go, not here: app.proxy_image_cache is a FOURTH
-- REFERENT for internal/store's blob-reclaim guard, which until now allowlisted
-- exactly messages, attachments and ml_models. A cache entry is referenced by
-- none of those, so a hash the cache happens to share with a purged attachment
-- would have had its app.blobs row reclaimed and its FILE DELETED out from under
-- the cache — and because that row carries the dek_id the ciphertext is opened
-- with, the bytes would not merely leak, they would become PERMANENTLY
-- UNREADABLE. That is #300's ml_models hazard exactly, and finding it a second
-- time by accident would be the expensive way to learn it.

-- +goose Up

-- WHAT KIND OF BYTES THESE ARE. 'mail' is the user's own — message bodies,
-- attachments, model artifacts — and is what every existing row is, which is why
-- the default is a statement of fact rather than an assumption: before this
-- migration there was no other writer.
--
-- 'proxy_cache' is a third-party image fetched on the user's behalf. It is
-- excluded from the storage quota and is safe to delete at any time: the only
-- consequence of losing one is a re-fetch.
--
-- ⚠ THE PROMOTION RULE IS THE SUBTLE PART, and it lives in InsertBlobMeta rather
-- than here. Blobs are content-addressed, so if a cached image's plaintext ever
-- equalled a message's or an attachment's, ONE row would serve both. If the
-- cache got there first, that row would say 'proxy_cache' and the user's own
-- mail would be silently excluded from their quota. So a 'mail' insert PROMOTES
-- an existing row's kind, and a 'proxy_cache' insert never demotes one.
-- Vanishingly unlikely is not the same as impossible, and "it has never happened"
-- was precisely the reasoning #300 found had been protecting a check that could
-- not fire.
ALTER TABLE app.blobs
  ADD COLUMN kind text NOT NULL DEFAULT 'mail';
ALTER TABLE app.blobs
  ADD CONSTRAINT blobs_kind_known CHECK (kind IN ('mail', 'proxy_cache'));

-- Partial index over just the cache rows: eviction and the "how much of this
-- store is not the user's" question both scan only this class, and the class is
-- expected to stay a small minority of the table.
CREATE INDEX blobs_proxy_cache ON app.blobs (user_id, created_at)
  WHERE kind = 'proxy_cache';

-- ONE CACHED IMAGE. Keyed by the URL's hash rather than by (message, image)
-- because the same image genuinely recurs across messages — a publisher's logo
-- appears in every issue — and the whole point of putting this in a
-- content-addressed store is not to hold it twice.
--
-- ⚠ THE URL ITSELF IS NOT STORED, only its SHA-256. A proxied image URL usually
-- carries a per-recipient token in its path or query (in practice a large share of a
-- mailbox's images carry a `token-hash`), so the URL is an identifier for the reader.
-- Keeping the hash is enough to find the entry again and cannot be turned back
-- into the tracking token.
--
-- content_hash is a blob reference with NO foreign key, exactly like
-- app.attachments and app.ml_models.artifact_hash. That is the existing
-- convention here, and it is precisely why the reclaim guard has to name this
-- table explicitly.
CREATE TABLE app.proxy_image_cache (
  user_id      app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  -- SHA-256 of the image URL. Not the URL, and not reversible to it.
  url_hash     app.sha256 NOT NULL,
  -- SHA-256 of the image's PLAINTEXT bytes: the app.blobs row that holds it.
  content_hash app.sha256 NOT NULL,
  -- The type the response was SERVED as, already narrowed to the endpoint's
  -- allowlist. Stored so a cache hit does not have to re-derive it, and so a
  -- type that later leaves the allowlist can be found and evicted.
  content_type text NOT NULL,
  size_bytes   bigint NOT NULL,
  fetched_at   timestamptz NOT NULL DEFAULT now(),
  -- Advanced on every hit, so eviction can be least-recently-USED rather than
  -- least-recently-fetched. An image nobody has looked at in months is the right
  -- one to drop first even if it was fetched recently.
  last_used_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, url_hash)
);
ALTER TABLE app.proxy_image_cache ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.proxy_image_cache FORCE ROW LEVEL SECURITY;
CREATE POLICY proxy_image_cache_isolation ON app.proxy_image_cache
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());
-- Eviction order: oldest use first, within one user.
CREATE INDEX proxy_image_cache_lru ON app.proxy_image_cache (user_id, last_used_at);
-- The reclaim guard asks "does any cache row still reference this hash", so the
-- lookup by content_hash needs to be cheap.
CREATE INDEX proxy_image_cache_by_content ON app.proxy_image_cache (user_id, content_hash);

GRANT SELECT, INSERT, UPDATE, DELETE ON app.proxy_image_cache TO mailroom_app;

-- +goose Down
DROP TABLE IF EXISTS app.proxy_image_cache;
DROP INDEX IF EXISTS app.blobs_proxy_cache;
ALTER TABLE app.blobs DROP CONSTRAINT IF EXISTS blobs_kind_known;
ALTER TABLE app.blobs DROP COLUMN IF EXISTS kind;
