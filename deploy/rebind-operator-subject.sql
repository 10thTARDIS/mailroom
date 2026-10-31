-- SPDX-License-Identifier: AGPL-3.0-only
--
-- Move the boot-provisioned operator to the subject your identity provider
-- really issues (#574). Read deploy/README.md §4, "A wrong operator subject",
-- before running it: it is the procedure, and this file is only its one step
-- that touches the database.
--
-- WHY THIS EXISTS. A MAILROOM_OPERATOR_SUBJECT that is not the provider's real
-- `sub` provisions the operator at boot under the wrong subject, and the
-- declared accounts sync the real mail into that user. The fix is NOT to change
-- the variable: the next boot would provision a SECOND user under the new
-- subject, whose email the first still holds, and the server would refuse to
-- start. The user that owns the mail has to be moved to the real subject first,
-- keeping its id and therefore everything it owns.
--
-- ⚠ CONFIRM `new` IS YOUR ACCOUNT, in the identity provider's own admin view,
-- before running this. The log line that names it can be produced by anyone who
-- can put your email address on an account at the provider, and this script
-- hands the user it moves — every account and message — to whoever holds `new`.
--
-- Usage, from the repository root, with the server STOPPED and postgres up:
--
--   docker compose -f deploy/compose.yml stop mailroom
--   docker compose -f deploy/compose.yml exec -T postgres \
--     psql -U postgres -d mailroom -v ON_ERROR_STOP=1 \
--       -v old='<operator_subject from the log line>' \
--       -v new='<token_subject from the log line>' \
--     < deploy/rebind-operator-subject.sql
--
-- It prints `NOTICE: moved user <id> ...` and COMMIT on success. On any refusal
-- it prints the reason as an ERROR and changes nothing.
--
-- If a user ALREADY exists under `new` (the different-email case: your real
-- login created a second, empty user), that user is retired first: disabled, and
-- its subject and email renamed out of the way so the moved user can take them.
-- It is retired only if it owns no accounts and no messages. A non-empty one
-- means two real users, and deciding which to keep is not a script's call.
--
-- psql substitutes :'old' and :'new' as quoted literals; they cannot be read
-- inside the DO body (psql does not interpolate a dollar-quoted string), so
-- they cross over as transaction-local settings. The integration test
-- (internal/store/users_rebind_integration_test.go) runs this exact file.

BEGIN;

SELECT set_config('mailroom.rebind_old', :'old', true),
       set_config('mailroom.rebind_new', :'new', true);

DO $$
DECLARE
  v_old      text := current_setting('mailroom.rebind_old');
  v_new      text := current_setting('mailroom.rebind_new');
  v_id       app.ulid;
  v_disabled timestamptz;
  v_deleted  timestamptz;
  v_squat    app.ulid;
  v_accounts bigint;
  v_messages bigint;
BEGIN
  IF v_old = '' OR v_new = '' OR v_old = v_new THEN
    RAISE EXCEPTION 'rebind: old and new must both be set, and differ (old=%, new=%)', v_old, v_new;
  END IF;

  SELECT id, disabled_at, deleted_at INTO v_id, v_disabled, v_deleted
    FROM app.users WHERE oidc_subject = v_old;
  IF v_id IS NULL THEN
    RAISE EXCEPTION 'rebind: no user has subject % — copy operator_subject from the "login refused" log line exactly', v_old;
  END IF;
  IF v_disabled IS NOT NULL OR v_deleted IS NOT NULL THEN
    RAISE EXCEPTION 'rebind: the user under subject % is disabled or deleted; refusing to move it', v_old;
  END IF;

  SELECT id INTO v_squat FROM app.users WHERE oidc_subject = v_new;
  IF v_squat IS NOT NULL THEN
    SELECT count(*) INTO v_accounts FROM app.accounts WHERE user_id = v_squat;
    SELECT count(*) INTO v_messages FROM app.messages WHERE user_id = v_squat;
    IF v_accounts > 0 OR v_messages > 0 THEN
      RAISE EXCEPTION 'rebind: user % under subject % owns % account(s) and % message(s); refusing to retire it',
        v_squat, v_new, v_accounts, v_messages;
    END IF;
    UPDATE app.users
       SET oidc_subject = 'retired-574:' || v_squat,
           email        = 'retired-' || v_squat || '@retired.invalid',
           disabled_at  = now()
     WHERE id = v_squat;
    RAISE NOTICE 'retired empty user % that held subject %', v_squat, v_new;
  END IF;

  UPDATE app.users SET oidc_subject = v_new WHERE id = v_id;
  RAISE NOTICE 'moved user % from subject % to %', v_id, v_old, v_new;
END;
$$;

COMMIT;
