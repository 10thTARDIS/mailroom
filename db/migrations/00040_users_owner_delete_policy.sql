-- SPDX-License-Identifier: AGPL-3.0-only

-- THE POLICY WITHOUT WHICH A HARD DELETE SILENTLY DOES NOTHING (#54).
--
-- app.users is owned by mailroom_owner, FORCE ROW LEVEL SECURITY is on, and
-- until now no policy granted that owner DELETE. The consequence is not an
-- error — it is the absence of one:
--
--   SET ROLE mailroom_owner;
--   DELETE FROM app.users WHERE id = '…';
--   DELETE 0                  <- no error
--   rows_remaining = 1        <- the user is still there
--
-- ⚠ THE REASONING THAT SAYS "THE MISSING GRANT WOULD CATCH IT LOUDLY" IS WRONG,
-- and it is worth writing down because it is the natural thing to assume.
-- db/schema.sql grants app.users only SELECT, INSERT and UPDATE to
-- mailroom_owner — but a table OWNER holds DELETE implicitly, which is why
-- information_schema reports a grant this file never writes. Nothing loud
-- stands in the way; only the silent RLS filter, which reduces the statement to
-- zero rows and reports success.
--
-- Measured on the deployed instance and reproduced in the integration harness
-- before this migration was written (v1.46.0):
--
--   users_owner               = mailroom_owner
--   rls_forced                = t
--   delete_policies_for_owner = 0
--
-- ⚠ THIS MIGRATION IS EXPECTED TO TURN EXACTLY ONE TEST RED, AND THAT IS ITS
-- ACCEPTANCE CRITERION. TestHardDeleteOfAUserSilentlyRemovesNothingToday
-- asserts the no-op on app.users itself and says in its failure message that a
-- red result means the prerequisite has been met. It was measured at 1 of 1,425
-- tests before this landed, which is how we know the policy breaks nothing else.
--
-- # Why USING (true) and not something narrower
--
-- The policy governs which rows mailroom_owner may delete, and the answer is
-- "whichever one the delete names". The narrowing that matters happens above
-- this line: only a SECURITY DEFINER running as this owner can reach the
-- statement at all, and mailroom_app — the role the application connects as —
-- has no DELETE grant on app.users and no policy here. A predicate like
-- `deleted_at IS NOT NULL` was considered and rejected: it would make hard
-- delete depend on a soft-delete step that nothing in Go performs today (#300
-- found `users.deleted_at` is never written), so the first real deletion would
-- fail in exactly the silent way this migration exists to end.
--
-- ⚠ AND IT WORKS ONLY BECAUSE THE OWNER CAN ALREADY *SEE* app.users. A
-- `DELETE ... WHERE` needs its target rows to be SELECTABLE under RLS, not
-- merely deletable: with a FOR DELETE policy and no SELECT policy the statement
-- still removes zero rows. `users_subject_lookup` (FOR SELECT TO mailroom_owner
-- USING (true), migration 00034) is what supplies that, so these two policies
-- are load-bearing TOGETHER. Narrowing users_subject_lookup later silently
-- breaks hard delete, and internal/store/delete_policy_integration_test.go
-- asserts that dependency on app.users itself so the pair cannot drift apart
-- unnoticed.
--
-- Forward-only (ADR-008): a policy is additive, nothing is rewritten, and no
-- reader changes behaviour.

-- +goose Up
CREATE POLICY users_owner_delete ON app.users
  FOR DELETE TO mailroom_owner
  USING (true);

COMMENT ON POLICY users_owner_delete ON app.users IS
  'Lets the table owner actually delete a user. Without it FORCE ROW LEVEL '
  'SECURITY filters the statement to zero rows and DELETE reports success — a '
  'silent no-op, because an owner holds DELETE implicitly and no permission '
  'error is raised. Works only in combination with users_subject_lookup, which '
  'gives the owner the SELECT visibility a DELETE ... WHERE requires (#54).';


-- ─────────────────────────────────────────────────────────────────────────────
-- The hard delete itself.
--
-- ⚠ IT MUST RUN AS THE TABLE OWNER, which is why it is a SECURITY DEFINER and
-- not a statement the application issues. mailroom_app has no DELETE grant on
-- app.users and no policy there — deliberately, per ADR-009 — so the delete has
-- exactly one narrow, auditable way in, exactly as provisioning does.
--
-- ⚠ THE TOMBSTONE IS WRITTEN BEFORE THE DELETE, AND ITS user_id COLUMN IS NOT
-- WHERE THE USER ID SURVIVES. app.audit_log.user_id REFERENCES app.users ON
-- DELETE SET NULL, so the cascade would blank it microseconds later and the
-- record of WHOSE data was destroyed would be gone — the one fact the row exists
-- to preserve. The id is therefore written into object_id (plain text, no
-- foreign key) as well, which is what keeps the derived blob prefix computable
-- for a later re-sweep.
--
-- The cascade does the rest: all 42 user-scoped tables carry an FK to
-- app.users, forty of them ON DELETE CASCADE and audit_log ON DELETE SET NULL,
-- so one DELETE reaches everything. Referential actions run with the owner's
-- privileges and are not filtered by RLS.
--
-- Returns the number of rows deleted from app.users: 1 normally, 0 if the id
-- does not exist. ⚠ A CALLER MUST CHECK IT. Zero is exactly what the missing
-- policy used to produce, and "it ran without erroring" has never been evidence
-- of anything here.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app.hard_delete_user(p_id app.ulid, p_actor text)
RETURNS integer
LANGUAGE plpgsql SECURITY DEFINER SET search_path = app, pg_temp AS $$
DECLARE
  v_deleted integer;
BEGIN
  DELETE FROM app.users WHERE id = p_id;
  GET DIAGNOSTICS v_deleted = ROW_COUNT;

  -- ⚠ THE TOMBSTONE IS WRITTEN AFTER THE DELETE, AND ITS user_id IS NULL BY
  -- CONSTRUCTION. Both were learned from a test rather than from review.
  --
  -- Writing it first, with user_id = p_id, fails two ways at once. For an id
  -- that does not exist the FK rejects the INSERT, so the function errors
  -- before it can report the far more useful "removed no rows". And for an id
  -- that DOES exist, app.audit_log.user_id REFERENCES app.users ON DELETE SET
  -- NULL, so the cascade blanks the column microseconds later anyway — the
  -- value was never going to survive.
  --
  -- So the id lives in object_id (plain text, no foreign key) and in detail.
  -- That is what keeps the derived blob prefix computable for a later re-sweep,
  -- which is the whole reason the row exists.
  IF v_deleted > 0 THEN
    INSERT INTO app.audit_log (user_id, actor, action, object_type, object_id, detail)
         VALUES (NULL, p_actor, 'user.hard_delete', 'User', p_id::text,
                 jsonb_build_object('user_id', p_id::text));
  END IF;

  RETURN v_deleted;
END;
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION app.hard_delete_user(app.ulid, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.hard_delete_user(app.ulid, text) TO mailroom_app;
ALTER FUNCTION app.hard_delete_user(app.ulid, text) OWNER TO mailroom_owner;


-- ⚠ THE SAME OWNER-RLS TRAP AS app.users, ONE TABLE OVER (#54).
--
-- app.hard_delete_user runs as mailroom_owner and writes its tombstone into
-- app.audit_log — whose policies name mailroom_app and mailroom_admin and NOT
-- the owner. FORCE ROW LEVEL SECURITY binds the owner too, so the INSERT is
-- refused outright:
--
--   ERROR: new row violates row-level security policy for table "audit_log"
--
-- ⚠ THIS ONE FAILS LOUDLY, WHICH IS THE OPPOSITE OF THE users CASE AND WORTH
-- NOTICING. A missing DELETE policy silently removes zero rows; a missing
-- INSERT policy raises. The delete would therefore have aborted rather than
-- proceeding without a tombstone — the safe direction — but it would have
-- aborted AFTER the blob files and search index were already removed, leaving
-- exactly the detectable-residue state the ordering was designed to produce.
-- Found by the integration test, not in review.
--
-- WITH CHECK (true) rather than a predicate: the definer is the only caller
-- that can reach this policy (mailroom_owner is not a login role in production
-- and the function is the sole EXECUTE-granted path), and the row it writes has
-- user_id set to a user being deleted a statement later.
CREATE POLICY audit_owner_insert ON app.audit_log
  FOR INSERT TO mailroom_owner
  WITH CHECK (true);

-- +goose Down
DROP POLICY IF EXISTS audit_owner_insert ON app.audit_log;
DROP FUNCTION IF EXISTS app.hard_delete_user(app.ulid, text);
DROP POLICY IF EXISTS users_owner_delete ON app.users;
