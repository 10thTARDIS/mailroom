-- SPDX-License-Identifier: AGPL-3.0-only

-- REMEMBERING "SHOW REMOTE CONTENT", PER SENDER (#202).
--
-- Remote images are blocked by default (spec §11) and the affordance that loads
-- them was deliberately PER MESSAGE, PER OPEN — it did not persist, and the
-- comment in the client said why: "I trusted this one newsletter once" must not
-- become "this sender may track me forever".
--
-- That reasoning was right about a BLANKET allow and it is what this table is
-- shaped around. The problem it left is that there was nowhere to record a NARROW
-- decision, so the only options were "ask every single time" and "stop asking at
-- all" — and asking every time about the same newsletter is how a safe default
-- becomes something a user routes around. This is the place to record the narrow
-- version.
--
-- TWO MATCHERS, ONE CONCEPT. `kind` is 'address' (exactly this From) or 'domain'
-- (anyone at example.com). Both exist because the annoyance has two shapes: a
-- person is an address, and a publisher is a domain — one mailbox
-- can receive what a human calls one sender from both mail.vendor.example and
-- email.vendor-alt.example, so an address-only allow would ask three times for one
-- decision. Domain is the broader grant, and the UI says so where it is granted.
--
-- WHY NOT A COLUMN ON app.contacts, which already has vip/blocked: contact rows
-- are created automatically from received mail (ADR-014), so a flag there would
-- mix a human's explicit decision in among machine-generated rows, and a DOMAIN is
-- not a contact at all. One table means one revocation path and one list to audit
-- — and being auditable is not a nice-to-have here. An "always allow" nobody can
-- see or take back IS the permanent tracking consent the original comment feared,
-- merely moved out of a click and into a database.
--
-- IT DOES NOT CHANGE WHAT THE SERVER SENDS. internal/api/sanitize.go keeps
-- parking every remote URL in a data-attribute unconditionally; the sanitizer must
-- never branch on a preference, or the safe path stops being the only path. This
-- table is read by the CLIENT to decide what to un-park (docs/hostile-content.md
-- §2.3).
--
-- NO GLOBAL ROW. There is deliberately no kind='all' and no "allow everything"
-- value: the schema cannot express a blanket allow, so no future UI can
-- accidentally offer one.

-- +goose Up

-- A RemoteAllow is a JMAP-visible object, so every create/destroy owes a
-- change_log row in the same transaction (the load-bearing invariant) — and that
-- row's object_type must exist in the enum. Appended at the END, matching
-- db/schema.sql, so a pg_dump built from this history stays byte-identical to one
-- built from the reference DDL (scripts/migrate-check.sh).
--
-- ADD VALUE is safe inside goose's per-migration transaction on PostgreSQL 12+ so
-- long as the new value is not USED in the same transaction. It is not: this
-- migration creates a table and writes no change_log row.
ALTER TYPE app.object_type ADD VALUE IF NOT EXISTS 'RemoteAllow';

CREATE TABLE app.remote_allows (
  id         app.ulid PRIMARY KEY,
  user_id    app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  -- 'address' | 'domain'. CHECKed rather than an enum type: the set is closed and
  -- tiny, and a CHECK is the constraint a reader of this file can see.
  kind       text NOT NULL CHECK (kind IN ('address', 'domain')),
  -- citext: addresses and domains are case-insensitive, and a user who allows
  -- News@Example.com must not be asked again for news@example.com.
  value      citext NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  -- One decision per (kind, value): granting the same allow twice is the same
  -- allow, and the API treats a repeat as a no-op rather than an error.
  UNIQUE (user_id, kind, value)
);

ALTER TABLE app.remote_allows ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.remote_allows FORCE ROW LEVEL SECURITY;
CREATE POLICY remote_allows_isolation ON app.remote_allows
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());

-- The lookup is "does this user allow this value, of this kind" — every hot index
-- leads with user_id because RLS appends that predicate to every plan. The UNIQUE
-- above already provides it; this is the one the ordered list read uses.
CREATE INDEX remote_allows_user_created ON app.remote_allows (user_id, created_at DESC);

GRANT SELECT, INSERT, UPDATE, DELETE ON app.remote_allows TO mailroom_app;

-- +goose Down
-- The table goes; the enum value cannot (PostgreSQL cannot remove one), so that
-- half is forward-only by design, exactly as 00009's was.
DROP TABLE app.remote_allows;
