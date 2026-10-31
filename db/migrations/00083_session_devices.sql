-- SPDX-License-Identifier: AGPL-3.0-only
--
-- SIGNED-IN DEVICES AND THE SESSION SWEEP (#937, ADR-048 sections 6 and 7,
-- increment 3).
--
-- EXPAND-ONLY (ADR-008): one nullable column, one index, two owner policies and
-- one function. Nothing existing changes.
--
--   1. app.push_subscriptions.session_id: which browser session registered the
--      subscription, so signing a device out can delete exactly its push
--      subscriptions. NULL for subscriptions made before this release (a device
--      sign-out does not touch those). ON DELETE SET NULL is only a backstop: the
--      sweep deletes a swept session's subscriptions itself first (a NULLed one
--      would be an unbound subscription that keeps receiving pushes).
--   2. app.sweep_sessions: the daily housekeeping (section 7). It runs as
--      mailroom_owner with no user identity, so it needs owner policies, and it
--      needs a SELECT policy AS WELL AS the DELETE one: without it a
--      DELETE ... WHERE is filtered to zero rows by FORCE ROW LEVEL SECURITY and
--      reports success, a silent no-op (see users_owner_delete in schema.sql).
--      The policies bound what the function MAY delete at the wall clock; the
--      function's own WHERE decides what it does delete, as of p_now.
--
-- No change_log rows: sessions and push subscriptions are operational data, not
-- JMAP-visible objects.

-- +goose Up
ALTER TABLE app.push_subscriptions
  ADD COLUMN session_id app.ulid REFERENCES app.sessions(id) ON DELETE SET NULL;
CREATE INDEX push_subscriptions_user_session ON app.push_subscriptions (user_id, session_id);

-- What the sweep may delete: a row revoked more than 7 days ago, or a live row
-- unseen for more than 45 days (past the provider's 30-day limit, with margin).
-- The SELECT policy is the one that makes the DELETE work at all.
CREATE POLICY sessions_sweep_select ON app.sessions
  FOR SELECT TO mailroom_owner
  USING ((revoked_at IS NOT NULL AND revoked_at < now() - interval '7 days')
      OR (revoked_at IS NULL AND last_seen_at < now() - interval '45 days'));
CREATE POLICY sessions_sweep_delete ON app.sessions
  FOR DELETE TO mailroom_owner
  USING ((revoked_at IS NOT NULL AND revoked_at < now() - interval '7 days')
      OR (revoked_at IS NULL AND last_seen_at < now() - interval '45 days'));
GRANT DELETE ON app.sessions TO mailroom_owner;

-- The sweep also deletes the push subscriptions of the sessions it deletes (a
-- subscription must never outlive its session as an UNBOUND one that keeps
-- receiving pushes, which carry text derived from mail). That needs owner
-- policies on app.push_subscriptions, confined to rows whose session the sweep
-- may delete; the SELECT policy is again what lets the DELETE see any row.
CREATE POLICY push_subscriptions_sweep_select ON app.push_subscriptions
  FOR SELECT TO mailroom_owner
  USING (session_id IS NOT NULL AND EXISTS (
    SELECT 1 FROM app.sessions s
     WHERE s.id = push_subscriptions.session_id
       AND ((s.revoked_at IS NOT NULL AND s.revoked_at < now() - interval '7 days')
         OR (s.revoked_at IS NULL AND s.last_seen_at < now() - interval '45 days'))));
CREATE POLICY push_subscriptions_sweep_delete ON app.push_subscriptions
  FOR DELETE TO mailroom_owner
  USING (session_id IS NOT NULL AND EXISTS (
    SELECT 1 FROM app.sessions s
     WHERE s.id = push_subscriptions.session_id
       AND ((s.revoked_at IS NOT NULL AND s.revoked_at < now() - interval '7 days')
         OR (s.revoked_at IS NULL AND s.last_seen_at < now() - interval '45 days'))));

-- Returns one row per reason, ZERO INCLUDED: a run that deleted nothing must be
-- distinguishable from a report that cannot see a reason at all (CLAUDE.md,
-- per-signal counts). 'unseen' is a live row nobody used for 45 days.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app.sweep_sessions(p_now timestamptz)
RETURNS TABLE (reason text, deleted bigint)
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = app, pg_temp AS $$
DECLARE
  v_push_unseen  bigint;
  v_push_revoked bigint;
BEGIN
  -- Push subscriptions first, while their sessions still exist: counted apart,
  -- so "sessions swept" and "subscriptions swept" are never one number.
  -- push_unseen: the session was live and unseen for 45 days. push_revoked: a
  -- leftover of a session revoked before sign-out deleted its subscriptions.
  WITH gone AS (
    DELETE FROM app.push_subscriptions p USING app.sessions s
     WHERE p.session_id = s.id
       AND ((s.revoked_at IS NOT NULL AND s.revoked_at < p_now - interval '7 days')
         OR (s.revoked_at IS NULL AND s.last_seen_at < p_now - interval '45 days'))
    RETURNING (s.revoked_at IS NULL) AS unseen
  )
  SELECT count(*) FILTER (WHERE unseen), count(*) FILTER (WHERE NOT unseen)
    INTO v_push_unseen, v_push_revoked FROM gone;

  RETURN QUERY
  WITH gone AS (
    DELETE FROM app.sessions s
     WHERE (s.revoked_at IS NOT NULL AND s.revoked_at < p_now - interval '7 days')
        OR (s.revoked_at IS NULL AND s.last_seen_at < p_now - interval '45 days')
    RETURNING CASE WHEN s.revoked_at IS NULL THEN 'unseen' ELSE s.revoked_reason END AS why
  )
  SELECT k.why, count(g.why)::bigint
    FROM (VALUES ('signed_out'), ('signed_out_elsewhere'), ('replaced'),
                 ('refused'), ('expired'), ('unseen')) AS k(why)
    LEFT JOIN gone g ON g.why = k.why
   GROUP BY k.why
  UNION ALL SELECT 'push_unseen', v_push_unseen
  UNION ALL SELECT 'push_revoked', v_push_revoked
  ORDER BY 1;
END
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION app.sweep_sessions(timestamptz) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.sweep_sessions(timestamptz) TO mailroom_app;
ALTER FUNCTION app.sweep_sessions(timestamptz) OWNER TO mailroom_owner;

-- +goose Down
DROP FUNCTION IF EXISTS app.sweep_sessions(timestamptz);
DROP POLICY IF EXISTS push_subscriptions_sweep_delete ON app.push_subscriptions;
DROP POLICY IF EXISTS push_subscriptions_sweep_select ON app.push_subscriptions;
DROP POLICY IF EXISTS sessions_sweep_delete ON app.sessions;
DROP POLICY IF EXISTS sessions_sweep_select ON app.sessions;
REVOKE DELETE ON app.sessions FROM mailroom_owner;
DROP INDEX IF EXISTS app.push_subscriptions_user_session;
ALTER TABLE app.push_subscriptions DROP COLUMN IF EXISTS session_id;
