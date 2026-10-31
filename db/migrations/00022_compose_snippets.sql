-- SPDX-License-Identifier: AGPL-3.0-only

-- SIGNATURES AND TEMPLATES — named, reusable compose text (#56, spec §5).
--
-- ONE TABLE FOR BOTH, and the reason is that they are the same thing wearing two
-- hats: a named piece of body text the user wrote once and wants to reuse. A
-- SIGNATURE is appended and belongs to an identity; a TEMPLATE seeds a whole
-- message and belongs to the user. Everything else about them — a name, HTML and
-- text bodies, "which one is the default", ordering, editing, deleting — is
-- identical, and two tables would mean two JMAP objects, two /changes streams and
-- two settings surfaces to answer one question.
--
-- `kind` is what differs, and it is CHECKed rather than implied.
--
-- WHY NOT REUSE identities.signature_html, which already exists: that column holds
-- exactly ONE signature per identity, and #56 asks for several to choose between.
-- It stays where it is and keeps working — a user who never makes a second
-- signature sees no change — and it is treated as the identity's default when this
-- table has nothing for that identity. Migrating it away would break the JMAP
-- Identity object's htmlSignature property that clients already read.
--
-- identity_id IS NULLABLE, and the two states mean different things:
--   * kind='signature' with an identity  → offered when sending as that address,
--     which is the whole point of a per-identity signature (spec §5's alias story:
--     the signature for mail from shop@ is not the one for work@).
--   * kind='signature' with NULL         → offered for any identity.
--   * kind='template'                    → always NULL; a template is not tied to
--     an address. CHECKed, so the two cannot be confused.
--
-- ON DELETE CASCADE from identities: a signature for an address that no longer
-- exists is not a signature, it is a trap waiting to be offered for the wrong From.

-- +goose Up
ALTER TYPE app.object_type ADD VALUE IF NOT EXISTS 'ComposeSnippet';

CREATE TABLE app.compose_snippets (
  id          app.ulid PRIMARY KEY,
  user_id     app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  kind        text NOT NULL CHECK (kind IN ('signature', 'template')),
  -- A template is never bound to an identity; a signature may be.
  identity_id app.ulid REFERENCES app.identities(id) ON DELETE CASCADE,
  CONSTRAINT compose_snippets_template_has_no_identity
    CHECK (kind <> 'template' OR identity_id IS NULL),
  name        text NOT NULL,
  -- Both bodies, because compose has both modes (#46). The HTML is what a rich
  -- compose inserts; the text is the plain-mode equivalent and the
  -- multipart/alternative fallback. Either may be empty, but not both — a snippet
  -- with no body is a menu entry that does nothing.
  body_html   text NOT NULL DEFAULT '',
  body_text   text NOT NULL DEFAULT '',
  CONSTRAINT compose_snippets_has_a_body
    CHECK (body_html <> '' OR body_text <> ''),
  -- The one offered without asking. Enforced as "at most one per (user, kind,
  -- identity)" by the partial unique index below rather than by application code,
  -- because two defaults is a state no UI can render honestly.
  is_default  boolean NOT NULL DEFAULT false,
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now(),
  UNIQUE (user_id, kind, name)
);

ALTER TABLE app.compose_snippets ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.compose_snippets FORCE ROW LEVEL SECURITY;
CREATE POLICY compose_snippets_isolation ON app.compose_snippets
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());

-- At most one default per kind per identity. Two partial indexes because NULL is
-- not equal to NULL in a unique index, so the "no identity" case needs its own.
CREATE UNIQUE INDEX compose_snippets_one_default_per_identity
  ON app.compose_snippets (user_id, kind, identity_id)
  WHERE is_default AND identity_id IS NOT NULL;
CREATE UNIQUE INDEX compose_snippets_one_default_no_identity
  ON app.compose_snippets (user_id, kind)
  WHERE is_default AND identity_id IS NULL;

-- The list read: everything of one kind for one user, in a stable order.
CREATE INDEX compose_snippets_user_kind ON app.compose_snippets (user_id, kind, name);

GRANT SELECT, INSERT, UPDATE, DELETE ON app.compose_snippets TO mailroom_app;

-- +goose Down
-- The table goes; the enum value cannot (PostgreSQL cannot remove one), so that
-- half is forward-only by design, as 00009's and 00021's were.
DROP TABLE app.compose_snippets;
