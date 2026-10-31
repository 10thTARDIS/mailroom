-- SPDX-License-Identifier: AGPL-3.0-only
--
-- LOCAL DKIM AT INGEST (#776, ADR-036). EXPAND-ONLY (ADR-008): one new table,
-- nothing existing touched.
--
-- One row records what Mailroom's OWN DKIM check found on one message that
-- arrived with NO Authentication-Results header from a trusted authserv-id.
-- The check runs at IMAP ingest, minutes after receipt, because verifying
-- later fails every signature carrying an x= expiry (most such
-- signatures in a first real-mailbox measurement).
--
--   method     'dkim' today; the key is (message_id, method) so a later method
--              adds rows rather than columns.
--   result     internal/dkimverify's Status, minus temperror: a transient DNS
--              failure writes NO row, because "not checked" is the honest state
--              and a row would say otherwise.
--   domain     d= of the signature that decided the result ('' when none was
--              readable).
--   aligned    for a pass: d= equals the From domain or is its parent, and is
--              not itself a public suffix. Only an ALIGNED PASS is ever shown;
--              a local fail is stored and never displayed (ADR-036).
--   display_blocked  '' normally. A pass that is NOT displayed because the
--              message carries a duplicate of a header the signature covers
--              (RFC 6376 §8.15: a signature covers the bottom-most instance, so
--              an attacker can prepend a forged From to genuinely signed mail
--              and the signature still verifies). The row is stored with
--              aligned=false and the reason here, e.g. 'duplicate_header:from'.
--   checked_at when the check ran.
--
-- NO change_log ROW OF ITS OWN. The row is written in the same transaction as
-- the message and is part of what the message's Email 'created' row announces;
-- it is never updated afterwards (no backfill), so there is no later mutation
-- for a second row to announce.
--
-- ON DELETE CASCADE from the message and from the user. No UPDATE grant: the
-- verdict is written once and goes only with its message. The primary key's
-- leading column is message_id, which is selective on its own (every read is
-- "these message ids"), and RLS adds user_id = current_user_id() to the plan.

-- +goose Up

-- +goose StatementBegin
CREATE TABLE app.message_local_auth (
  message_id app.ulid NOT NULL REFERENCES app.messages(id) ON DELETE CASCADE,
  user_id    app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  method     text NOT NULL CHECK (method = 'dkim'),
  result     text NOT NULL CHECK (result IN ('pass', 'fail_body', 'fail_signature',
               'key_not_found', 'key_revoked', 'permerror', 'unsupported')),
  domain     text NOT NULL DEFAULT '',
  aligned    boolean NOT NULL,
  display_blocked text NOT NULL DEFAULT '',
  checked_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (message_id, method)
);
-- +goose StatementEnd

ALTER TABLE app.message_local_auth ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.message_local_auth FORCE ROW LEVEL SECURITY;
CREATE POLICY message_local_auth_isolation ON app.message_local_auth
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());
-- Written once and gone only with its message: no UPDATE (as nudge_dismissals).
GRANT SELECT, INSERT, DELETE ON app.message_local_auth TO mailroom_app;

-- +goose Down
DROP TABLE IF EXISTS app.message_local_auth;
