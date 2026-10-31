-- SPDX-License-Identifier: AGPL-3.0-only
--
-- AGENT ACCESS: API KEYS IN USE, THEIR AUDIT, AND THEIR PENDING APPROVALS
-- (#49, spec §13a, ADR-042).
--
-- EXPAND-ONLY (ADR-008). app.api_keys has existed since the baseline but nothing
-- has ever read or written it, so the CHECK constraints added here validate
-- against an empty table in every deployment (a seeded test row satisfies
-- them). No existing column changes meaning; confirm_mode is SUPERSEDED by
-- confirm_scopes and left in place, unread, for a later contract step.
--
-- app.api_keys (new columns, appended in this order)
--   confirm_scopes      which DESTRUCTIVE scopes wait for a human ('mail:send',
--                       'mail:delete'). Per scope rather than one boolean
--                       because "ask me before sending" and "ask me before
--                       deleting" are separate decisions (#49).
--   token_hint          the first characters of the token body, so a list of
--                       keys can say which is which. 35 bits of a 256-bit token.
--   suspended_at/_reason  set by the anomaly rules (mass delete) or by hand.
--                       Unlike revoked_at it can be cleared.
--   suspend_on_anomaly  default ON (ADR-042 §6).
--
-- app.api_key_events   one row per method call a key made (admitted or not),
--   per anomaly, and per approval decision. user_id-led, FORCE RLS. NEVER
--   message content: ids and counts only. cost/deletes/sends are COLUMNS
--   because the limits and the anomaly rules query them (JSONB is for payloads,
--   never for queryable state). Rows are written once; the only column the app
--   may change afterwards is error_type, the outcome of a call that was
--   admitted before it ran (a column-level UPDATE grant, nothing wider).
--
-- app.api_key_drafts   which drafts each key wrote: a key submits only its own.
--
-- app.api_key_suppressed_refusals  refusals past a key's per-minute caps,
--   counted per (minute, outcome, method, reason) instead of written one by one.
--
-- app.api_key_pending  a destructive call held for approval. payload is the
--   canonical request (ids and a draft content hash, never bodies) and is read
--   back whole; summary is built server-side for the person deciding.
--
-- Two SECURITY DEFINERs, both owned by mailroom_owner and both returning ids
-- only:
--   app.api_key_for_token(hash)        bearer authentication, before any
--                                      identity exists (the login-time
--                                      user_id_for_subject pattern).
--   app.api_key_pending_due_users()    which users have an expired pending
--                                      request, for the sweep (the
--                                      dequeue_due_jobs pattern).
-- Each needs an owner SELECT policy, written as narrowly as its body.

-- +goose Up
-- +goose StatementBegin
ALTER TABLE app.api_keys
  ADD COLUMN confirm_scopes text[] NOT NULL DEFAULT '{}',
  ADD COLUMN token_hint text NOT NULL DEFAULT '',
  ADD COLUMN suspended_at timestamptz,
  ADD COLUMN suspended_reason text,
  ADD COLUMN suspend_on_anomaly boolean NOT NULL DEFAULT true;
-- +goose StatementEnd

ALTER TABLE app.api_keys
  ADD CONSTRAINT api_keys_scopes_closed CHECK (
    cardinality(scopes) > 0 AND scopes <@ ARRAY['mail:read','mail:send','mail:draft','mail:move',
      'mail:delete','labels:read','labels:write','search:read','contacts:read','contacts:write']::text[]),
  ADD CONSTRAINT api_keys_confirm_destructive CHECK (
    confirm_scopes <@ ARRAY['mail:send','mail:delete']::text[]),
  ADD CONSTRAINT api_keys_rate_bounds CHECK (rate_per_min BETWEEN 1 AND 600),
  ADD CONSTRAINT api_keys_budget_bounds CHECK (daily_budget BETWEEN 1 AND 20000),
  ADD CONSTRAINT api_keys_hash_is_sha256 CHECK (octet_length(token_hash) = 32);

COMMENT ON COLUMN app.api_keys.confirm_mode IS
  'SUPERSEDED by confirm_scopes (migration 00074, ADR-042) and never read. '
  'Kept until a contract step drops it (ADR-008).';

-- +goose StatementBegin
CREATE TABLE app.api_key_events (
  id            app.ulid PRIMARY KEY,
  user_id       app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  key_id        app.ulid NOT NULL REFERENCES app.api_keys(id) ON DELETE CASCADE,
  at            timestamptz NOT NULL DEFAULT now(),
  method        text NOT NULL,
  outcome       text NOT NULL CHECK (outcome IN ('ok', 'denied', 'rate_limited', 'over_budget',
                  'pending', 'approved', 'denied_by_user', 'expired', 'anomaly')),
  cost          int NOT NULL DEFAULT 0 CHECK (cost >= 0),
  deletes       int NOT NULL DEFAULT 0 CHECK (deletes >= 0),
  sends         int NOT NULL DEFAULT 0 CHECK (sends >= 0),
  summary       jsonb NOT NULL DEFAULT '{}',
  client_ip     inet,
  pending_id    app.ulid,
  error_type    text,
  -- Which anomaly rule an 'anomaly' row records (ADR-042 §6); NULL otherwise.
  anomaly       text CHECK (anomaly IN ('mass_delete', 'mass_send', 'off_hours', 'critical_refusals')),
  -- Why a refused call was refused, from the server's fixed list (#49 review);
  -- NULL for every other row. A column because the per-minute caps count by it.
  reason        text CHECK (reason IN ('method', 'session_only', 'restriction', 'arguments', 'scope', 'rate', 'budget', 'approval', 'mass_delete', 'mass_send')),
  CONSTRAINT api_key_events_anomaly_named CHECK ((outcome = 'anomaly') = (anomaly IS NOT NULL))
);
-- +goose StatementEnd

CREATE INDEX api_key_events_by_key ON app.api_key_events (user_id, key_id, at);

ALTER TABLE app.api_key_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.api_key_events FORCE ROW LEVEL SECURITY;
CREATE POLICY api_key_events_isolation ON app.api_key_events
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());
GRANT SELECT, INSERT ON app.api_key_events TO mailroom_app;
GRANT UPDATE (error_type) ON app.api_key_events TO mailroom_app;

-- +goose StatementBegin
CREATE TABLE app.api_key_pending (
  id            app.ulid PRIMARY KEY,
  user_id       app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  key_id        app.ulid NOT NULL REFERENCES app.api_keys(id) ON DELETE CASCADE,
  kind          text NOT NULL CHECK (kind IN ('send', 'delete')),
  method        text NOT NULL,
  payload       jsonb NOT NULL,
  summary       text NOT NULL,
  created_at    timestamptz NOT NULL DEFAULT now(),
  expires_at    timestamptz NOT NULL,
  state         text NOT NULL DEFAULT 'pending'
                  CHECK (state IN ('pending', 'approved', 'denied', 'expired', 'failed')),
  decided_at    timestamptz,
  detail        text,
  -- When an APPROVED request was admitted to run, written in the same
  -- transaction as its 'approved' audit row. An approved row without it after
  -- ten minutes never ran (the process stopped between the decision and the
  -- run) and the sweep marks it failed (ADR-042 §7).
  ran_at        timestamptz,
  CONSTRAINT api_key_pending_decided CHECK ((state = 'pending') = (decided_at IS NULL))
);
-- +goose StatementEnd

CREATE INDEX api_key_pending_by_state ON app.api_key_pending (user_id, state, expires_at);
CREATE INDEX api_key_pending_by_key ON app.api_key_pending (user_id, key_id, created_at);

ALTER TABLE app.api_key_pending ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.api_key_pending FORCE ROW LEVEL SECURITY;
CREATE POLICY api_key_pending_isolation ON app.api_key_pending
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());
GRANT SELECT, INSERT, UPDATE ON app.api_key_pending TO mailroom_app;

-- The drafts each API key wrote (#49 review, ADR-042 §3): a key may submit
-- ONLY a draft it created itself, so a send key cannot mail out what the user
-- (or another key) has been writing. Written in the transaction that creates
-- the draft; a row goes with its message or its key.
-- +goose StatementBegin
CREATE TABLE app.api_key_drafts (
  user_id       app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  message_id    app.ulid NOT NULL REFERENCES app.messages(id) ON DELETE CASCADE,
  key_id        app.ulid NOT NULL REFERENCES app.api_keys(id) ON DELETE CASCADE,
  created_at    timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, message_id)
);
-- +goose StatementEnd
ALTER TABLE app.api_key_drafts ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.api_key_drafts FORCE ROW LEVEL SECURITY;
CREATE POLICY api_key_drafts_isolation ON app.api_key_drafts
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());
GRANT SELECT, INSERT ON app.api_key_drafts TO mailroom_app;

-- Refusals past a key's per-minute caps (#49 review): a key's refusals are
-- written in full to api_key_events up to rate_per_min routine and
-- 5 x rate_per_min security-meaningful ones a minute (store.CriticalRefusalReasons);
-- past those they are counted here, one row per (minute, outcome, method,
-- reason), so the log stays bounded and complete in aggregate. count is a
-- column because the rate limit sums it.
-- +goose StatementBegin
CREATE TABLE app.api_key_suppressed_refusals (
  user_id       app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  key_id        app.ulid NOT NULL REFERENCES app.api_keys(id) ON DELETE CASCADE,
  minute        timestamptz NOT NULL,
  outcome       text NOT NULL CHECK (outcome IN ('denied', 'rate_limited', 'over_budget')),
  -- The method name only when it is one this server has; otherwise 'unknown'.
  -- Never client text: a key could otherwise mint a row per invented name.
  method        text NOT NULL,
  reason        text NOT NULL CHECK (reason IN ('method', 'session_only', 'restriction', 'arguments', 'scope', 'rate', 'budget', 'approval', 'mass_delete', 'mass_send')),
  count         int NOT NULL CHECK (count > 0),
  PRIMARY KEY (user_id, key_id, minute, outcome, method, reason)
);
-- +goose StatementEnd
ALTER TABLE app.api_key_suppressed_refusals ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.api_key_suppressed_refusals FORCE ROW LEVEL SECURITY;
CREATE POLICY api_key_suppressed_refusals_isolation ON app.api_key_suppressed_refusals
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());
GRANT SELECT, INSERT ON app.api_key_suppressed_refusals TO mailroom_app;
GRANT UPDATE (count) ON app.api_key_suppressed_refusals TO mailroom_app;

-- Bearer authentication (ADR-042 §2). The ONLY cross-user read of
-- app.api_keys: by the hash of a presented token, returning the owning user and
-- the key id, and only for a key that is neither revoked nor suspended and whose
-- user can still sign in. Everything else about the key is then read under the
-- user's own UserTx, so RLS applies to the rest of the request exactly as for a
-- browser session.
CREATE POLICY api_keys_token_lookup ON app.api_keys
  FOR SELECT TO mailroom_owner
  USING (revoked_at IS NULL AND suspended_at IS NULL);

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app.api_key_for_token(p_hash bytea)
RETURNS TABLE (user_id app.ulid, key_id app.ulid)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = app, pg_temp AS $$
  SELECT k.user_id, k.id
    FROM app.api_keys k
    JOIN app.users u ON u.id = k.user_id
   WHERE k.token_hash = p_hash
     AND k.revoked_at IS NULL
     AND k.suspended_at IS NULL
     AND u.disabled_at IS NULL
     AND u.deleted_at IS NULL
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION app.api_key_for_token(bytea) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.api_key_for_token(bytea) TO mailroom_app;
ALTER FUNCTION app.api_key_for_token(bytea) OWNER TO mailroom_owner;
GRANT SELECT ON app.api_keys TO mailroom_owner;

-- The expiry sweep (ADR-042 §7): which users have a pending request past its
-- deadline. User ids only; the expiry itself runs in each user's UserTx.
CREATE POLICY api_key_pending_due_lookup ON app.api_key_pending
  FOR SELECT TO mailroom_owner
  USING ((state = 'pending' AND expires_at <= now())
      OR (state = 'approved' AND ran_at IS NULL AND decided_at <= now() - interval '10 minutes'));

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app.api_key_pending_due_users()
RETURNS SETOF app.ulid
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = app, pg_temp AS $$
  SELECT DISTINCT p.user_id
    FROM app.api_key_pending p
   WHERE (p.state = 'pending' AND p.expires_at <= now())
      OR (p.state = 'approved' AND p.ran_at IS NULL AND p.decided_at <= now() - interval '10 minutes')
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION app.api_key_pending_due_users() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.api_key_pending_due_users() TO mailroom_app;
ALTER FUNCTION app.api_key_pending_due_users() OWNER TO mailroom_owner;
GRANT SELECT ON app.api_key_pending TO mailroom_owner;

-- +goose Down
DROP FUNCTION IF EXISTS app.api_key_pending_due_users();
DROP FUNCTION IF EXISTS app.api_key_for_token(bytea);
DROP POLICY IF EXISTS api_keys_token_lookup ON app.api_keys;
DROP TABLE IF EXISTS app.api_key_suppressed_refusals;
DROP TABLE IF EXISTS app.api_key_drafts;
DROP TABLE IF EXISTS app.api_key_pending;
DROP TABLE IF EXISTS app.api_key_events;
ALTER TABLE app.api_keys
  DROP CONSTRAINT IF EXISTS api_keys_scopes_closed,
  DROP CONSTRAINT IF EXISTS api_keys_confirm_destructive,
  DROP CONSTRAINT IF EXISTS api_keys_rate_bounds,
  DROP CONSTRAINT IF EXISTS api_keys_budget_bounds,
  DROP CONSTRAINT IF EXISTS api_keys_hash_is_sha256,
  DROP COLUMN IF EXISTS suspend_on_anomaly,
  DROP COLUMN IF EXISTS suspended_reason,
  DROP COLUMN IF EXISTS suspended_at,
  DROP COLUMN IF EXISTS token_hint,
  DROP COLUMN IF EXISTS confirm_scopes;
