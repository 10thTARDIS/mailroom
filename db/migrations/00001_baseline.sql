-- SPDX-License-Identifier: AGPL-3.0-only

-- ============================================================================
-- Mailroom — baseline migration (goose)
-- ============================================================================
-- This migration reproduces db/schema.sql AS OF the first migration (see
-- docs/migrations.md for the schema.sql-agreement rule: this file's Up body
-- and db/schema.sql must describe the same database — CI's drift check
-- (scripts/migrate-check.sh) enforces that).
--
-- Transaction handling: schema.sql wraps itself in an explicit BEGIN/COMMIT
-- because it's meant to be applied directly via `psql -f`. goose ALSO wraps
-- each migration file in its own transaction by default, and nested
-- BEGIN/COMMIT is not valid inside a single connection's transaction block.
-- So here we deliberately DROP the outer BEGIN/COMMIT from schema.sql and
-- rely entirely on goose's default per-file transaction — every statement
-- below is ordinary transactional DDL in Postgres (no CREATE INDEX
-- CONCURRENTLY, no ALTER TYPE ... ADD VALUE outside a transaction), so
-- there is no need for goose's "no transaction" migration annotation.
--
-- goose splits a migration file into statements on top-level `;`. Function
-- bodies and DO blocks contain internal `;` inside `$$...$$` dollar-quoting,
-- so each such block below is wrapped in goose's statement-boundary
-- annotations to force goose to treat it as one statement.
-- ============================================================================

-- +goose Up

CREATE EXTENSION IF NOT EXISTS citext;

-- ----------------------------------------------------------------------------
-- 0. Roles
-- ----------------------------------------------------------------------------
-- mailroom_owner : owns all objects; used ONLY by migrations. Never the app.
-- mailroom_app   : the application role. NOT the owner, no BYPASSRLS, so
--                  FORCE ROW LEVEL SECURITY applies to it.
-- mailroom_admin : admin-plane role. Gets privileges on user management and
--                  operational tables ONLY — deliberately NO grants on mail
--                  content tables. "Admin can't read mail" (spec §10) is
--                  implemented as the absence of grants + RLS, and audited.
--
-- (CREATE ROLE is cluster-level; guarded so the file is re-runnable.)
-- +goose StatementBegin
DO $$
BEGIN
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'mailroom_owner') THEN
    CREATE ROLE mailroom_owner NOLOGIN;
  END IF;
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'mailroom_app') THEN
    CREATE ROLE mailroom_app NOLOGIN;
  END IF;
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'mailroom_admin') THEN
    CREATE ROLE mailroom_admin NOLOGIN;
  END IF;
END $$;
-- +goose StatementEnd

-- ----------------------------------------------------------------------------
-- 1. Schema, domains, helper functions
-- ----------------------------------------------------------------------------
CREATE SCHEMA IF NOT EXISTS app AUTHORIZATION mailroom_owner;

-- ULID: 26 chars, Crockford base32 (no I, L, O, U).
CREATE DOMAIN app.ulid AS text
  CHECK (VALUE ~ '^[0-9A-HJKMNP-TV-Z]{26}$');

-- SHA-256 content hash, raw bytes.
CREATE DOMAIN app.sha256 AS bytea
  CHECK (octet_length(VALUE) = 32);

-- The RLS anchor. Reads the per-transaction setting installed by the tx
-- wrapper. missing_ok=true => NULL when unset => every policy evaluates to
-- NULL => row invisible. FAIL CLOSED: no identity, no rows.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app.current_user_id() RETURNS app.ulid
LANGUAGE sql STABLE PARALLEL SAFE AS $$
  SELECT NULLIF(current_setting('mailroom.user_id', true), '')::app.ulid
$$;
-- +goose StatementEnd

-- Enumerations ---------------------------------------------------------------
CREATE TYPE app.user_role        AS ENUM ('user', 'admin');
CREATE TYPE app.identity_kind    AS ENUM ('fixed', 'domain_wildcard');
CREATE TYPE app.object_type      AS ENUM ('Email', 'Mailbox', 'Thread',
                                          'Identity', 'EmailSubmission',
                                          'Contact', 'Label', 'Rule', 'Key');
CREATE TYPE app.change_op        AS ENUM ('created', 'updated', 'destroyed');
CREATE TYPE app.job_state        AS ENUM ('pending', 'running', 'done',
                                          'failed', 'dead');
CREATE TYPE app.intent_state     AS ENUM ('pending', 'inflight', 'applied',
                                          'dead');
CREATE TYPE app.key_kind         AS ENUM ('pgp', 'smime');
CREATE TYPE app.dsn_action       AS ENUM ('failed', 'delayed', 'delivered',
                                          'relayed', 'expanded');
CREATE TYPE app.provenance_state AS ENUM ('live', 'stale_uidvalidity');

-- ----------------------------------------------------------------------------
-- 2. Users (admin-plane table — the ONE table mailroom_admin can write)
-- ----------------------------------------------------------------------------
CREATE TABLE app.users (
  id            app.ulid PRIMARY KEY,
  oidc_subject  text NOT NULL UNIQUE,          -- Authentik subject claim
  email         citext NOT NULL UNIQUE,        -- login/display email
  display_name  text NOT NULL DEFAULT '',
  role          app.user_role NOT NULL DEFAULT 'user',
  -- quotas: enforced defaults (spec §10); admin UI may lag
  quota_storage_bytes   bigint NOT NULL DEFAULT 107374182400, -- 100 GiB
  quota_sync_connections int   NOT NULL DEFAULT 4,
  quota_llm_daily_tokens bigint NOT NULL DEFAULT 0,           -- 0 = disabled
  created_at    timestamptz NOT NULL DEFAULT now(),
  disabled_at   timestamptz,
  deleted_at    timestamptz                     -- soft; hard delete is a job
);
ALTER TABLE app.users ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.users FORCE ROW LEVEL SECURITY;

-- App plane: a user sees exactly their own row.
CREATE POLICY users_self ON app.users
  FOR SELECT TO mailroom_app
  USING (id = app.current_user_id());
-- Admin plane: full management, but via the admin role only.
CREATE POLICY users_admin_all ON app.users
  FOR ALL TO mailroom_admin
  USING (true) WITH CHECK (true);

-- ----------------------------------------------------------------------------
-- 3. Per-user change sequencing (drives JMAP state strings)
-- ----------------------------------------------------------------------------
-- One counter row per (user, type). Writers take a row lock via
-- UPDATE ... RETURNING, which serializes sequence allocation per user+type —
-- correct and plenty fast at ≤ ~24 users. (A per-user hot row is the known
-- cost; revisit only if contention is ever measured.)
CREATE TABLE app.change_seq (
  user_id     app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  object_type app.object_type NOT NULL,
  last_seq    bigint NOT NULL DEFAULT 0,
  PRIMARY KEY (user_id, object_type)
);
ALTER TABLE app.change_seq ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.change_seq FORCE ROW LEVEL SECURITY;
CREATE POLICY change_seq_isolation ON app.change_seq
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());

-- The change log itself: the single ordered stream consumed by JMAP /changes,
-- the search indexer, and client sync cursors (spec §13/§14).
CREATE TABLE app.change_log (
  user_id     app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  object_type app.object_type NOT NULL,
  seq         bigint NOT NULL,
  object_id   app.ulid NOT NULL,
  op          app.change_op NOT NULL,
  created_at  timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, object_type, seq)
);
-- /changes scans: (user, type, seq > since) — covered by the PK.
-- Collapse step needs per-object lookups within the scanned range only.
ALTER TABLE app.change_log ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.change_log FORCE ROW LEVEL SECURITY;
CREATE POLICY change_log_isolation ON app.change_log
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());

-- Compaction floor: /changes requests with sinceState older than this floor
-- return cannotCalculateChanges (client refetches via /query). One row per
-- (user, type), advanced by the compaction job.
CREATE TABLE app.change_floor (
  user_id     app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  object_type app.object_type NOT NULL,
  floor_seq   bigint NOT NULL DEFAULT 0,
  PRIMARY KEY (user_id, object_type)
);
ALTER TABLE app.change_floor ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.change_floor FORCE ROW LEVEL SECURITY;
CREATE POLICY change_floor_isolation ON app.change_floor
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());

-- ----------------------------------------------------------------------------
-- 4. Accounts, identities, mailboxes
-- ----------------------------------------------------------------------------
CREATE TABLE app.accounts (
  id            app.ulid PRIMARY KEY,
  user_id       app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  name          text NOT NULL,
  provider_profile text NOT NULL DEFAULT 'generic',  -- 'migadu', 'gmail', ...
  imap_host     text NOT NULL,
  imap_port     int  NOT NULL DEFAULT 993,
  smtp_host     text NOT NULL,
  smtp_port     int  NOT NULL DEFAULT 465,
  auth_kind     text NOT NULL DEFAULT 'password',    -- 'password' | 'oauth2'
  -- credentials: AEAD blob under the user's credentials DEK (spec §9)
  credentials_ct  bytea NOT NULL,
  credentials_dek app.ulid NOT NULL,                 -- key id in crypto svc
  envelope_ver  smallint NOT NULL DEFAULT 1,
  -- outbound ledger configuration (spec §5): provider daily cap
  outbound_daily_quota int NOT NULL DEFAULT 100,
  sync_enabled  boolean NOT NULL DEFAULT true,
  created_at    timestamptz NOT NULL DEFAULT now(),
  UNIQUE (user_id, name)
);
ALTER TABLE app.accounts ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.accounts FORCE ROW LEVEL SECURITY;
CREATE POLICY accounts_isolation ON app.accounts
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());

CREATE TABLE app.identities (
  id            app.ulid PRIMARY KEY,
  user_id       app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  account_id    app.ulid NOT NULL REFERENCES app.accounts(id) ON DELETE CASCADE,
  kind          app.identity_kind NOT NULL DEFAULT 'fixed',
  -- fixed:           address = 'me@example.com', domain = NULL
  -- domain_wildcard: address = NULL, domain = 'example.com'
  --                  (validates any derived From under that domain — spec §5)
  address       citext,
  domain        citext,
  display_name  text NOT NULL DEFAULT '',
  signature_html text NOT NULL DEFAULT '',
  default_key_id app.ulid,                    -- FK added after app.user_keys
  enabled       boolean NOT NULL DEFAULT true,
  created_at    timestamptz NOT NULL DEFAULT now(),
  CHECK ( (kind = 'fixed'           AND address IS NOT NULL AND domain IS NULL)
       OR (kind = 'domain_wildcard' AND address IS NULL     AND domain IS NOT NULL) )
);
ALTER TABLE app.identities ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.identities FORCE ROW LEVEL SECURITY;
CREATE POLICY identities_isolation ON app.identities
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());
CREATE UNIQUE INDEX identities_fixed_uq
  ON app.identities (user_id, address) WHERE kind = 'fixed';
CREATE UNIQUE INDEX identities_wildcard_uq
  ON app.identities (user_id, domain) WHERE kind = 'domain_wildcard';

CREATE TABLE app.mailboxes (
  id            app.ulid PRIMARY KEY,
  user_id       app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  account_id    app.ulid NOT NULL REFERENCES app.accounts(id) ON DELETE CASCADE,
  imap_name     text NOT NULL,                -- raw IMAP mailbox name (UTF-7 decoded)
  display_name  text NOT NULL,
  special_use   text,                         -- RFC 6154: \Sent, \Trash, ... or manual mapping
  uidvalidity   bigint NOT NULL DEFAULT 0,
  last_seen_uid bigint NOT NULL DEFAULT 0,
  highest_modseq bigint NOT NULL DEFAULT 0,
  -- PERMANENTFLAGS probe (spec §6): can this mailbox store user keywords?
  accepts_keywords boolean NOT NULL DEFAULT false,
  subscribed    boolean NOT NULL DEFAULT true,
  sync_scope    text NOT NULL DEFAULT 'full', -- 'full' | 'headers' | 'off'
  created_at    timestamptz NOT NULL DEFAULT now(),
  UNIQUE (account_id, imap_name)
);
ALTER TABLE app.mailboxes ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.mailboxes FORCE ROW LEVEL SECURITY;
CREATE POLICY mailboxes_isolation ON app.mailboxes
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());
CREATE INDEX mailboxes_user_account ON app.mailboxes (user_id, account_id);

-- ----------------------------------------------------------------------------
-- 5. Blobs, messages, provenance, threads, attachments
-- ----------------------------------------------------------------------------
-- Content-addressed encrypted blob metadata (spec §9/§14). The bytes live in
-- the BlobStore at a path derived from HMAC(server_key, user_id)/hash — the
-- path is derivable, so it is not stored.
CREATE TABLE app.blobs (
  user_id       app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  content_hash  app.sha256 NOT NULL,          -- hash of PLAINTEXT bytes
  size_plain    bigint NOT NULL,
  size_stored   bigint NOT NULL,              -- ciphertext incl. envelope
  dek_id        app.ulid NOT NULL,            -- per-user DEK reference
  envelope_ver  smallint NOT NULL DEFAULT 1,  -- AEAD envelope format version
  created_at    timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, content_hash)
);
ALTER TABLE app.blobs ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.blobs FORCE ROW LEVEL SECURITY;
CREATE POLICY blobs_isolation ON app.blobs
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());

-- Canonical messages. Identity = (user_id, content_hash) per ADR D1.
CREATE TABLE app.messages (
  id            app.ulid PRIMARY KEY,
  user_id       app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  content_hash  app.sha256 NOT NULL,          -- SHA-256 of raw RFC 5322 bytes
  thread_id     app.ulid,                     -- FK added after app.threads
  -- envelope projection (for list views; authoritative data is the blob)
  subject       text NOT NULL DEFAULT '',
  snippet       text NOT NULL DEFAULT '',
  from_name     text NOT NULL DEFAULT '',
  from_address  citext,
  sent_at       timestamptz,                  -- Date header (may be absent/lying)
  received_at   timestamptz NOT NULL,         -- earliest INTERNALDATE seen
  size_bytes    bigint NOT NULL,
  has_attachments boolean NOT NULL DEFAULT false,
  -- threading hints (NEVER identity — spec §4)
  message_id_hint text,                       -- Message-ID header, verbatim
  in_reply_to   text,
  references_ids text[] NOT NULL DEFAULT '{}',
  -- alias story: which identity this message was addressed to (spec §5)
  addressed_identity_id app.ulid REFERENCES app.identities(id) ON DELETE SET NULL,
  addressed_address citext,                   -- the literal derived alias
  -- canonical flags (merged across provenance; JMAP keywords model)
  is_seen       boolean NOT NULL DEFAULT false,
  is_flagged    boolean NOT NULL DEFAULT false,
  is_answered   boolean NOT NULL DEFAULT false,
  is_draft      boolean NOT NULL DEFAULT false,
  is_junk       boolean NOT NULL DEFAULT false, -- mirror of provider verdict
  keywords      text[] NOT NULL DEFAULT '{}', -- non-label IMAP keywords
  -- deletion lifecycle (ADR D9)
  trashed_at    timestamptz,
  purge_after   timestamptz,                  -- set when trashed; job hard-deletes
  created_at    timestamptz NOT NULL DEFAULT now(),
  UNIQUE (user_id, content_hash),
  FOREIGN KEY (user_id, content_hash)
    REFERENCES app.blobs (user_id, content_hash)
);
ALTER TABLE app.messages ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.messages FORCE ROW LEVEL SECURITY;
CREATE POLICY messages_isolation ON app.messages
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());
-- Hot paths (every index leads with user_id — RLS adds that predicate anyway):
CREATE INDEX messages_user_received  ON app.messages (user_id, received_at DESC);
CREATE INDEX messages_user_thread    ON app.messages (user_id, thread_id);
CREATE INDEX messages_user_msgid     ON app.messages (user_id, message_id_hint);
CREATE INDEX messages_user_trash     ON app.messages (user_id, purge_after)
  WHERE trashed_at IS NOT NULL;

CREATE TABLE app.threads (
  id            app.ulid PRIMARY KEY,
  user_id       app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  -- membership lives on messages.thread_id and is RECOMPUTABLE (spec §4);
  -- denormalized rollups for list views:
  subject       text NOT NULL DEFAULT '',
  message_count int NOT NULL DEFAULT 0,
  last_received_at timestamptz,
  created_at    timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE app.threads ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.threads FORCE ROW LEVEL SECURITY;
CREATE POLICY threads_isolation ON app.threads
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());
CREATE INDEX threads_user_recent ON app.threads (user_id, last_received_at DESC);

ALTER TABLE app.messages
  ADD CONSTRAINT messages_thread_fk
  FOREIGN KEY (thread_id) REFERENCES app.threads(id) ON DELETE SET NULL;

-- Where each canonical message physically lives (spec §4).
CREATE TABLE app.message_provenance (
  id            app.ulid PRIMARY KEY,
  user_id       app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  message_id    app.ulid NOT NULL REFERENCES app.messages(id) ON DELETE CASCADE,
  account_id    app.ulid NOT NULL REFERENCES app.accounts(id) ON DELETE CASCADE,
  mailbox_id    app.ulid NOT NULL REFERENCES app.mailboxes(id) ON DELETE CASCADE,
  uid           bigint NOT NULL,
  uidvalidity   bigint NOT NULL,              -- generation this row belongs to
  state         app.provenance_state NOT NULL DEFAULT 'live',
  gm_msgid      text,                         -- Gmail X-GM-MSGID, metadata only
  remote_flags  text[] NOT NULL DEFAULT '{}', -- last flags seen on the server
  first_seen_at timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE app.message_provenance ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.message_provenance FORCE ROW LEVEL SECURITY;
CREATE POLICY provenance_isolation ON app.message_provenance
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());
-- A UID is unique within a mailbox generation; stale generations keep history.
CREATE UNIQUE INDEX provenance_live_uq
  ON app.message_provenance (mailbox_id, uidvalidity, uid);
CREATE INDEX provenance_user_message ON app.message_provenance (user_id, message_id);

CREATE TABLE app.attachments (
  id            app.ulid PRIMARY KEY,
  user_id       app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  message_id    app.ulid NOT NULL REFERENCES app.messages(id) ON DELETE CASCADE,
  part_path     text NOT NULL,                -- MIME part path, e.g. '2.1'
  filename      text,
  content_type  text NOT NULL DEFAULT 'application/octet-stream',
  size_bytes    bigint NOT NULL,
  content_hash  app.sha256,                   -- present once extracted to blob
  extracted_text_hash app.sha256,             -- opt-in search extraction (§7)
  scan_status   text NOT NULL DEFAULT 'unscanned', -- 'unscanned'|'clean'|'flagged'
  created_at    timestamptz NOT NULL DEFAULT now(),
  UNIQUE (message_id, part_path)
);
ALTER TABLE app.attachments ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.attachments FORCE ROW LEVEL SECURITY;
CREATE POLICY attachments_isolation ON app.attachments
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());
CREATE INDEX attachments_user_message ON app.attachments (user_id, message_id);

-- ----------------------------------------------------------------------------
-- 6. Labels & keyword mapping
-- ----------------------------------------------------------------------------
CREATE TABLE app.labels (
  id            app.ulid PRIMARY KEY,
  user_id       app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  name          text NOT NULL,
  color         text,                          -- '#rrggbb' or named token
  parent_id     app.ulid REFERENCES app.labels(id) ON DELETE SET NULL,
  mirror_to_imap boolean NOT NULL DEFAULT true, -- spec §6 default
  created_at    timestamptz NOT NULL DEFAULT now(),
  UNIQUE (user_id, name)
);
ALTER TABLE app.labels ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.labels FORCE ROW LEVEL SECURITY;
CREATE POLICY labels_isolation ON app.labels
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());

CREATE TABLE app.message_labels (
  user_id       app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  message_id    app.ulid NOT NULL REFERENCES app.messages(id) ON DELETE CASCADE,
  label_id      app.ulid NOT NULL REFERENCES app.labels(id) ON DELETE CASCADE,
  applied_by    text NOT NULL DEFAULT 'user',  -- 'user'|'rule'|'ml'|'agent'
  created_at    timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, message_id, label_id)
);
ALTER TABLE app.message_labels ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.message_labels FORCE ROW LEVEL SECURITY;
CREATE POLICY message_labels_isolation ON app.message_labels
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());
CREATE INDEX message_labels_by_label ON app.message_labels (user_id, label_id);

-- Persisted label↔IMAP-keyword mapping (spec §6): NEVER re-derived from the
-- label name — renames must not orphan keywords.
CREATE TABLE app.label_keyword_map (
  user_id       app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  label_id      app.ulid NOT NULL REFERENCES app.labels(id) ON DELETE CASCADE,
  keyword_atom  text NOT NULL,                 -- e.g. 'MailroomLabel-invoices-2'
  created_at    timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, label_id),
  UNIQUE (user_id, keyword_atom),
  CHECK (keyword_atom ~ '^[!#-''*+\--9;-Z^-~]+$')  -- IMAP atom charset, no specials
);
ALTER TABLE app.label_keyword_map ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.label_keyword_map FORCE ROW LEVEL SECURITY;
CREATE POLICY label_keyword_map_isolation ON app.label_keyword_map
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());

-- ----------------------------------------------------------------------------
-- 7. Writeback intents (spec §4a) and durable jobs (spec §16)
-- ----------------------------------------------------------------------------
-- Intent DELTAS, never target states. Replay-safe via idempotency_key.
CREATE TABLE app.writeback_intents (
  id            app.ulid PRIMARY KEY,
  user_id       app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  message_id    app.ulid REFERENCES app.messages(id) ON DELETE CASCADE,
  account_id    app.ulid NOT NULL REFERENCES app.accounts(id) ON DELETE CASCADE,
  op            text NOT NULL,                 -- '+flag','-flag','+kw','-kw','move','append','expunge'
  payload       jsonb NOT NULL DEFAULT '{}',   -- op arguments (target mailbox, flag name, ...)
  idempotency_key text NOT NULL,
  state         app.intent_state NOT NULL DEFAULT 'pending',
  attempts      int NOT NULL DEFAULT 0,
  last_error    text,
  dead_reason   text,                          -- populated when state='dead'; SURFACED to user
  created_at    timestamptz NOT NULL DEFAULT now(),
  applied_at    timestamptz,
  UNIQUE (user_id, idempotency_key)
);
ALTER TABLE app.writeback_intents ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.writeback_intents FORCE ROW LEVEL SECURITY;
CREATE POLICY writeback_intents_isolation ON app.writeback_intents
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());
CREATE INDEX writeback_pending ON app.writeback_intents (account_id, created_at)
  WHERE state IN ('pending', 'inflight');
CREATE INDEX writeback_dead ON app.writeback_intents (user_id, created_at)
  WHERE state = 'dead';

-- One durable job table for every timer in the product (spec §16):
-- snooze wake-ups, delayed submission (Undo Send / send-later), trash purge,
-- DEK lazy re-encryption, model snapshots, change-log compaction.
CREATE TABLE app.jobs (
  id            app.ulid PRIMARY KEY,
  user_id       app.ulid REFERENCES app.users(id) ON DELETE CASCADE, -- NULL = system job
  kind          text NOT NULL,                 -- 'snooze_wake','submit','purge_trash',...
  fire_at       timestamptz NOT NULL,
  payload       jsonb NOT NULL DEFAULT '{}',
  idempotency_key text NOT NULL UNIQUE,
  state         app.job_state NOT NULL DEFAULT 'pending',
  attempts      int NOT NULL DEFAULT 0,
  max_attempts  int NOT NULL DEFAULT 5,
  locked_by     text,                          -- dispatcher instance id
  locked_at     timestamptz,
  last_error    text,
  created_at    timestamptz NOT NULL DEFAULT now(),
  completed_at  timestamptz
);
-- Jobs are dispatched by a system worker (runs as mailroom_app with a system
-- context), but user rows remain user-visible for e.g. "scheduled sends" UI.
ALTER TABLE app.jobs ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.jobs FORCE ROW LEVEL SECURITY;
CREATE POLICY jobs_user_visible ON app.jobs
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id() OR
         (user_id IS NULL AND app.current_user_id() IS NULL))
  WITH CHECK (user_id = app.current_user_id() OR
              (user_id IS NULL AND app.current_user_id() IS NULL));
-- Dispatcher scan path. The dispatcher fires timers across all users; it runs
-- in a SystemTx ('mailroom.user_id' UNSET) and reaches other users' jobs ONLY
-- through the SECURITY DEFINER app.dequeue_due_jobs below. Jobs are the ONE
-- deliberate, narrow cross-user surface — the function exposes job rows, never
-- mail content.
--
-- That definer runs as the function owner, mailroom_owner (pinned below). In
-- production mailroom_owner owns app.jobs and FORCE ROW LEVEL SECURITY applies
-- to the owner too, so without a policy for it the definer would silently see
-- zero rows and no timer would ever fire (mailroom#16). This policy IS that
-- narrow surface, made explicit: full visibility of job rows for mailroom_owner
-- — a NOLOGIN role reachable only through the definer function.
CREATE POLICY jobs_dispatcher_owner ON app.jobs
  FOR ALL TO mailroom_owner
  USING (true) WITH CHECK (true);
CREATE INDEX jobs_due ON app.jobs (fire_at) WHERE state = 'pending';

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app.dequeue_due_jobs(p_worker text, p_limit int)
RETURNS SETOF app.jobs
LANGUAGE sql SECURITY DEFINER SET search_path = app AS $$
  UPDATE app.jobs j SET state = 'running', locked_by = p_worker, locked_at = now(),
                        attempts = attempts + 1
  WHERE j.id IN (
    SELECT id FROM app.jobs
    WHERE state = 'pending' AND fire_at <= now()
    ORDER BY fire_at
    FOR UPDATE SKIP LOCKED
    LIMIT p_limit
  )
  RETURNING j.*;
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION app.dequeue_due_jobs(text, int) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.dequeue_due_jobs(text, int) TO mailroom_app;
-- Pin the definer deterministically. Applied by a superuser (as in CI/dev), the
-- function would otherwise be owned by that superuser and bypass RLS entirely,
-- hiding the production semantics where the definer is the non-superuser
-- mailroom_owner and IS subject to RLS. Forcing the owner makes CI exercise the
-- real path (mailroom#16).
ALTER FUNCTION app.dequeue_due_jobs(text, int) OWNER TO mailroom_owner;
-- mailroom_owner holds these implicitly in production (it owns every object);
-- the explicit grant is what makes the definer path work in CI/dev, where the
-- tables are owned by the bootstrap superuser instead.
GRANT SELECT, UPDATE ON app.jobs TO mailroom_owner;

-- ----------------------------------------------------------------------------
-- 8. Sending: submissions, outbound ledger, DSNs
-- ----------------------------------------------------------------------------
-- EmailSubmission (JMAP) + the delayed-submission engine (spec §5).
CREATE TABLE app.submissions (
  id            app.ulid PRIMARY KEY,
  user_id       app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  account_id    app.ulid NOT NULL REFERENCES app.accounts(id) ON DELETE CASCADE,
  identity_id   app.ulid NOT NULL REFERENCES app.identities(id),
  message_id    app.ulid NOT NULL REFERENCES app.messages(id), -- the draft/built message
  from_address  citext NOT NULL,               -- validated against identity (spec §5)
  envelope_to   citext[] NOT NULL,
  send_at       timestamptz NOT NULL DEFAULT now(), -- future = undo window / send-later
  job_id        app.ulid REFERENCES app.jobs(id),
  state         text NOT NULL DEFAULT 'scheduled', -- 'scheduled'|'canceled'|'sending'|'sent'|'failed'
  smtp_response text,
  created_at    timestamptz NOT NULL DEFAULT now(),
  sent_at       timestamptz
);
ALTER TABLE app.submissions ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.submissions FORCE ROW LEVEL SECURITY;
CREATE POLICY submissions_isolation ON app.submissions
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());
CREATE INDEX submissions_user_state ON app.submissions (user_id, state, send_at);

-- Per-account, per-day outbound spend vs. provider caps (spec §5).
CREATE TABLE app.outbound_ledger (
  account_id    app.ulid NOT NULL REFERENCES app.accounts(id) ON DELETE CASCADE,
  user_id       app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  day           date NOT NULL,
  sent_count    int NOT NULL DEFAULT 0,
  PRIMARY KEY (account_id, day)
);
ALTER TABLE app.outbound_ledger ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.outbound_ledger FORCE ROW LEVEL SECURITY;
CREATE POLICY outbound_ledger_isolation ON app.outbound_ledger
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());

-- Parsed RFC 3464 DSNs / bounces, linked to the originating message (spec §4).
CREATE TABLE app.dsn_events (
  id            app.ulid PRIMARY KEY,
  user_id       app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  origin_message_id app.ulid REFERENCES app.messages(id) ON DELETE SET NULL,
  origin_submission_id app.ulid REFERENCES app.submissions(id) ON DELETE SET NULL,
  dsn_message_id app.ulid REFERENCES app.messages(id) ON DELETE SET NULL, -- the DSN itself
  recipient     citext NOT NULL,
  action        app.dsn_action NOT NULL,
  status_code   text,                          -- '5.1.1' etc.
  diagnostic    text,
  created_at    timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE app.dsn_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.dsn_events FORCE ROW LEVEL SECURITY;
CREATE POLICY dsn_events_isolation ON app.dsn_events
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());
CREATE INDEX dsn_by_origin ON app.dsn_events (user_id, origin_message_id);

-- ----------------------------------------------------------------------------
-- 9. Triage & ML
-- ----------------------------------------------------------------------------
CREATE TABLE app.triage_scores (
  user_id       app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  message_id    app.ulid NOT NULL REFERENCES app.messages(id) ON DELETE CASCADE,
  score         real NOT NULL,                 -- 0..1 priority
  model_version text NOT NULL,
  top_features  jsonb NOT NULL DEFAULT '[]',   -- explainability (spec §8)
  scored_at     timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, message_id)
);
ALTER TABLE app.triage_scores ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.triage_scores FORCE ROW LEVEL SECURITY;
CREATE POLICY triage_scores_isolation ON app.triage_scores
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());

CREATE TABLE app.feedback_events (
  id            app.ulid PRIMARY KEY,
  user_id       app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  message_id    app.ulid REFERENCES app.messages(id) ON DELETE CASCADE,
  action        text NOT NULL,                 -- 'open','reply','archive_unread','star',...
  context       jsonb NOT NULL DEFAULT '{}',   -- reply latency, surface, etc.
  created_at    timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE app.feedback_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.feedback_events FORCE ROW LEVEL SECURITY;
CREATE POLICY feedback_events_isolation ON app.feedback_events
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());
CREATE INDEX feedback_user_time ON app.feedback_events (user_id, created_at);

-- Model registry: artifacts live in the blob store under blob discipline;
-- snapshots + lineage for rollback/reset (spec §8).
CREATE TABLE app.ml_models (
  id            app.ulid PRIMARY KEY,
  user_id       app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  model_kind    text NOT NULL,                 -- 'priority','labeler'
  version       int NOT NULL,
  parent_id     app.ulid REFERENCES app.ml_models(id),
  artifact_hash app.sha256 NOT NULL,           -- blob ref
  active        boolean NOT NULL DEFAULT false,
  created_at    timestamptz NOT NULL DEFAULT now(),
  UNIQUE (user_id, model_kind, version)
);
ALTER TABLE app.ml_models ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.ml_models FORCE ROW LEVEL SECURITY;
CREATE POLICY ml_models_isolation ON app.ml_models
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());
CREATE UNIQUE INDEX ml_models_one_active
  ON app.ml_models (user_id, model_kind) WHERE active;

CREATE TABLE app.rules (
  id            app.ulid PRIMARY KEY,
  user_id       app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  name          text NOT NULL,
  priority      int NOT NULL DEFAULT 100,
  conditions    jsonb NOT NULL,                -- T0 condition tree (spec §8)
  actions       jsonb NOT NULL,
  enabled       boolean NOT NULL DEFAULT true,
  system_suggested boolean NOT NULL DEFAULT false,
  created_at    timestamptz NOT NULL DEFAULT now(),
  UNIQUE (user_id, name)
);
ALTER TABLE app.rules ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.rules FORCE ROW LEVEL SECURITY;
CREATE POLICY rules_isolation ON app.rules
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());

-- ----------------------------------------------------------------------------
-- 10. Contacts (spec §10a)
-- ----------------------------------------------------------------------------
CREATE TABLE app.contacts (
  id            app.ulid PRIMARY KEY,
  user_id       app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  display_name  text NOT NULL DEFAULT '',
  vip           boolean NOT NULL DEFAULT false,
  blocked       boolean NOT NULL DEFAULT false,
  notes         text NOT NULL DEFAULT '',
  created_at    timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE app.contacts ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.contacts FORCE ROW LEVEL SECURITY;
CREATE POLICY contacts_isolation ON app.contacts
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());

CREATE TABLE app.contact_addresses (
  id            app.ulid PRIMARY KEY,
  user_id       app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  contact_id    app.ulid NOT NULL REFERENCES app.contacts(id) ON DELETE CASCADE,
  address       citext NOT NULL,
  -- the alias story: which of MY addresses this correspondent used (spec §5)
  reached_me_at citext,
  first_seen_at timestamptz NOT NULL DEFAULT now(),
  last_seen_at  timestamptz NOT NULL DEFAULT now(),
  seen_count    int NOT NULL DEFAULT 1,
  UNIQUE (user_id, address)
);
ALTER TABLE app.contact_addresses ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.contact_addresses FORCE ROW LEVEL SECURITY;
CREATE POLICY contact_addresses_isolation ON app.contact_addresses
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());
CREATE INDEX contact_addresses_by_contact ON app.contact_addresses (user_id, contact_id);

-- Discovered public keys for correspondents (WKD/Autocrypt/manual).
CREATE TABLE app.contact_keys (
  id            app.ulid PRIMARY KEY,
  user_id       app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  contact_id    app.ulid NOT NULL REFERENCES app.contacts(id) ON DELETE CASCADE,
  kind          app.key_kind NOT NULL,
  fingerprint   text NOT NULL,
  public_key    bytea NOT NULL,
  source        text NOT NULL,                 -- 'wkd'|'autocrypt'|'manual'|'keyserver'
  fetched_at    timestamptz NOT NULL DEFAULT now(),
  UNIQUE (user_id, contact_id, fingerprint)
);
ALTER TABLE app.contact_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.contact_keys FORCE ROW LEVEL SECURITY;
CREATE POLICY contact_keys_isolation ON app.contact_keys
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());

-- ----------------------------------------------------------------------------
-- 11. User keys, API keys, push, audit, AI providers, sync jobs
-- ----------------------------------------------------------------------------
-- User signing/encryption keys. PRIVATE material is wrapped and usable only
-- via the crypto process (spec §9); this table stores wrapped bytes + refs.
CREATE TABLE app.user_keys (
  id            app.ulid PRIMARY KEY,
  user_id       app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  identity_id   app.ulid REFERENCES app.identities(id) ON DELETE SET NULL,
  kind          app.key_kind NOT NULL,
  fingerprint   text NOT NULL,
  public_key    bytea NOT NULL,
  private_key_ct bytea,                        -- wrapped; NULL for public-only entries
  dek_id        app.ulid,
  envelope_ver  smallint NOT NULL DEFAULT 1,
  decrypt_only  boolean NOT NULL DEFAULT false, -- retired keys kept forever (spec §9)
  wkd_published boolean NOT NULL DEFAULT false,
  created_at    timestamptz NOT NULL DEFAULT now(),
  revoked_at    timestamptz,
  UNIQUE (user_id, fingerprint)
);
ALTER TABLE app.user_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.user_keys FORCE ROW LEVEL SECURITY;
CREATE POLICY user_keys_isolation ON app.user_keys
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());

ALTER TABLE app.identities
  ADD CONSTRAINT identities_default_key_fk
  FOREIGN KEY (default_key_id) REFERENCES app.user_keys(id) ON DELETE SET NULL;

-- Scoped API keys / PATs (spec §13a). Hardened defaults live in app logic;
-- the schema records limits so they are enforceable + auditable.
CREATE TABLE app.api_keys (
  id            app.ulid PRIMARY KEY,
  user_id       app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  name          text NOT NULL,
  token_hash    bytea NOT NULL UNIQUE,         -- hash only; token shown once
  scopes        text[] NOT NULL,               -- 'mail:read','mail:send',...
  account_ids   app.ulid[],                    -- NULL = all the user's accounts
  label_ids     app.ulid[],                    -- optional label restriction
  confirm_mode  boolean NOT NULL DEFAULT false,
  rate_per_min  int NOT NULL DEFAULT 60,       -- mandatory (spec §13a)
  daily_budget  int NOT NULL DEFAULT 500,      -- mandatory (spec §13a)
  created_at    timestamptz NOT NULL DEFAULT now(),
  last_used_at  timestamptz,
  revoked_at    timestamptz,
  UNIQUE (user_id, name)
);
ALTER TABLE app.api_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.api_keys FORCE ROW LEVEL SECURITY;
CREATE POLICY api_keys_isolation ON app.api_keys
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());

CREATE TABLE app.push_subscriptions (
  id            app.ulid PRIMARY KEY,
  user_id       app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  endpoint      text NOT NULL,
  keys_p256dh   text NOT NULL,
  keys_auth     text NOT NULL,
  device_name   text NOT NULL DEFAULT '',
  important_only boolean NOT NULL DEFAULT true, -- triage-aware default (spec §12)
  created_at    timestamptz NOT NULL DEFAULT now(),
  expired_at    timestamptz,
  UNIQUE (user_id, endpoint)
);
ALTER TABLE app.push_subscriptions ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.push_subscriptions FORCE ROW LEVEL SECURITY;
CREATE POLICY push_subscriptions_isolation ON app.push_subscriptions
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());

-- Append-only audit log (spec §11). Users see their own rows; admin sees
-- admin-plane rows (user_id IS NULL) through the admin role.
CREATE TABLE app.audit_log (
  id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  user_id       app.ulid REFERENCES app.users(id) ON DELETE SET NULL,
  actor         text NOT NULL,                 -- 'user'|'api_key:<id>'|'system'|'admin:<id>'
  action        text NOT NULL,                 -- 'message.delete','key.sign','conflict.resolved',...
  object_type   text,
  object_id     text,
  detail        jsonb NOT NULL DEFAULT '{}',
  created_at    timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE app.audit_log ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.audit_log FORCE ROW LEVEL SECURITY;
CREATE POLICY audit_user_read ON app.audit_log
  FOR SELECT TO mailroom_app
  USING (user_id = app.current_user_id());
CREATE POLICY audit_user_insert ON app.audit_log
  FOR INSERT TO mailroom_app
  WITH CHECK (user_id = app.current_user_id() OR user_id IS NULL);
CREATE POLICY audit_admin_read ON app.audit_log
  FOR SELECT TO mailroom_admin
  USING (user_id IS NULL);
-- Append-only is enforced by grants below (no UPDATE/DELETE granted).

CREATE TABLE app.ai_providers (
  id            app.ulid PRIMARY KEY,
  user_id       app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  name          text NOT NULL,
  base_url      text NOT NULL,                 -- OpenAI-compatible endpoint
  api_key_ct    bytea,                         -- wrapped credential
  dek_id        app.ulid,
  model         text NOT NULL DEFAULT '',
  external      boolean NOT NULL DEFAULT false, -- triggers data-egress warning (spec §8)
  daily_token_budget bigint NOT NULL DEFAULT 0,
  enabled       boolean NOT NULL DEFAULT false,
  created_at    timestamptz NOT NULL DEFAULT now(),
  UNIQUE (user_id, name)
);
ALTER TABLE app.ai_providers ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.ai_providers FORCE ROW LEVEL SECURITY;
CREATE POLICY ai_providers_isolation ON app.ai_providers
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());

-- Long-running ingest runs: bulk import + backfill progress (spec §4).
CREATE TABLE app.sync_jobs (
  id            app.ulid PRIMARY KEY,
  user_id       app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  account_id    app.ulid REFERENCES app.accounts(id) ON DELETE CASCADE,
  kind          text NOT NULL,                 -- 'bulk_import','backfill','reindex'
  source        text,                          -- 'mbox'|'maildir'|'takeout'|NULL
  state         app.job_state NOT NULL DEFAULT 'pending',
  progress_done bigint NOT NULL DEFAULT 0,
  progress_total bigint,
  cursor        jsonb NOT NULL DEFAULT '{}',   -- resumability (spec §4)
  last_error    text,
  created_at    timestamptz NOT NULL DEFAULT now(),
  completed_at  timestamptz
);
ALTER TABLE app.sync_jobs ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.sync_jobs FORCE ROW LEVEL SECURITY;
CREATE POLICY sync_jobs_isolation ON app.sync_jobs
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());

-- ----------------------------------------------------------------------------
-- 12. Grants
-- ----------------------------------------------------------------------------
GRANT USAGE ON SCHEMA app TO mailroom_app, mailroom_admin;

-- App plane: CRUD on user-scoped tables, RLS-constrained. No TRUNCATE.
GRANT SELECT, INSERT, UPDATE, DELETE ON
  app.change_seq, app.change_log, app.change_floor,
  app.accounts, app.identities, app.mailboxes,
  app.blobs, app.messages, app.threads, app.message_provenance, app.attachments,
  app.labels, app.message_labels, app.label_keyword_map,
  app.writeback_intents, app.jobs,
  app.submissions, app.outbound_ledger, app.dsn_events,
  app.triage_scores, app.feedback_events, app.ml_models, app.rules,
  app.contacts, app.contact_addresses, app.contact_keys,
  app.user_keys, app.api_keys, app.push_subscriptions,
  app.ai_providers, app.sync_jobs
TO mailroom_app;

-- Users table: app reads self (RLS); INSERT/UPDATE of users is admin-plane.
GRANT SELECT ON app.users TO mailroom_app;
GRANT SELECT, INSERT, UPDATE ON app.users TO mailroom_admin;
-- (no DELETE grant even to admin: user removal is a soft-delete + purge job)

-- Audit: append + read only. Nobody gets UPDATE/DELETE.
GRANT SELECT, INSERT ON app.audit_log TO mailroom_app;
GRANT SELECT ON app.audit_log TO mailroom_admin;

-- mailroom_admin deliberately receives NO grants on mail-content tables:
-- messages, blobs, attachments, labels, contacts, user_keys, etc. Absence of
-- privilege + RLS + audit = the §10 "admin can't read mail" claim, honestly.

-- +goose Down
-- Baseline down = full teardown of the app schema and its contents. This is
-- intentionally the ONLY down migration expected to ever run for real (see
-- docs/migrations.md — the fleet is forward-only in practice); it exists so
-- `goose down`/`goose reset` work cleanly in dev and CI throwaway databases.
DROP SCHEMA IF EXISTS app CASCADE;
