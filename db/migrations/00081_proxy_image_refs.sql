-- SPDX-License-Identifier: AGPL-3.0-only
--
-- WHICH MESSAGES KEEP A CACHED PROXIED IMAGE ALIVE (#922, ADR-026 amendment of
-- 2026-10-27).
--
-- The image proxy used to fetch only when a message was opened. A sender whose
-- image URLs are SIGNED AND EXPIRING (measured: `token-time`/`token-hash` on one
-- host, ADR-026 §7) answers 403 by the time an older message is first opened,
-- so the picture is gone for good. The fix is to fetch at ARRIVAL for senders
-- the proxy's own rule allows, and then to keep the bytes for as long as a
-- message that shows them is kept. This table is that "for as long as".
--
-- One row = "this message shows the image cached under this url_hash". A
-- cached image with at least one row here is exempt from the cache's 30-day
-- unread TTL AND from its size cap (decided 2026-10-27): it lives
-- until the last message that references it is destroyed. The bounds on growth
-- are the per-image byte cap, the per-message image cap, the allowed-senders
-- rule and the arrival window — not eviction.
--
-- ⚠ "DESTROYED" MEANS THE ROW IN app.messages IS DELETED, which is why
-- message_id is a foreign key ON DELETE CASCADE rather than something a sweep
-- has to remember. Every destroy path (the Trash purge's hard delete, account
-- removal, duplicate collapse, a retired draft) is a DELETE FROM app.messages,
-- and every one of them releases these rows with no code of its own. Moving a
-- message to Trash does NOT delete the row, so it does not release anything:
-- Trash is restorable, and an image released at trash time would be gone after
-- an undo.
--
-- ⚠ NO FOREIGN KEY TO app.proxy_image_cache, deliberately. The cache row is
-- written in the same transaction as the reference, and nothing evicts a
-- referenced cache row, so a dangling reference could only follow a manual
-- delete — and then it costs nothing (it protects no bytes). A foreign key
-- would instead make PutProxyImage's ON CONFLICT replace order-sensitive.
--
-- OPERATIONAL, NOT JMAP-VISIBLE: like app.proxy_image_cache, a write here
-- appends no change_log row. Nothing a client syncs changes when an image is
-- cached or released.
--
-- Like the cache, it holds the URL's SHA-256 only, never the URL: an image URL
-- carries a per-recipient token.

-- +goose Up
CREATE TABLE app.proxy_image_refs (
  user_id    app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  -- The cache key (app.proxy_image_cache.url_hash): SHA-256 of the image URL.
  url_hash   app.sha256 NOT NULL,
  message_id app.ulid NOT NULL REFERENCES app.messages(id) ON DELETE CASCADE,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, url_hash, message_id)
);
ALTER TABLE app.proxy_image_refs ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.proxy_image_refs FORCE ROW LEVEL SECURITY;
CREATE POLICY proxy_image_refs_isolation ON app.proxy_image_refs
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());
-- The cascade from app.messages looks rows up by message_id; without this the
-- hard delete of one message would scan the user's whole reference set.
CREATE INDEX proxy_image_refs_by_message ON app.proxy_image_refs (user_id, message_id);

GRANT SELECT, INSERT, UPDATE, DELETE ON app.proxy_image_refs TO mailroom_app;

-- +goose Down
DROP TABLE IF EXISTS app.proxy_image_refs;
