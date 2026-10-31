-- SPDX-License-Identifier: AGPL-3.0-only
--
-- OUT OF OFFICE (#50): the vacation responder, written to the provider as a
-- ManageSieve script (internal/outofoffice). EXPAND-ONLY (ADR-008): one new
-- table, one new change-log object type, nothing existing touched.
--
-- One row per MAIL ACCOUNT, created the first time the user saves the form.
-- The row is JMAP-visible as an OutOfOffice object whose id is the account id
-- (app.object_type 'OutOfOffice'), so every change appends a change_log row
-- in the same transaction, like every other JMAP-visible object.
--
-- What is stored, and why:
--   enabled                the user's intent. True only after the server
--                          accepted and activated the script (a refusal
--                          leaves it false).
--   from_at, to_at         the window, UTC instants; NULL = open-ended. The
--                          script carries the same window as a currentdate
--                          guard, and jobs fire at both ends as a backstop.
--   subject, body,         the reply. Plain text; the body goes into the
--   reply_days,            script's text: block, the rest into quoted
--   from_address,          strings. reply_days is :days (1..30).
--   contacts_only
--   installed_sha256       the SHA-256 of the exact script Mailroom stored;
--                          NULL when none is installed. Comparing it with the
--                          server's copy is how drift is seen.
--   server_state,          what the last look at the server found (off,
--   server_detail,         active, displaced, missing, modified, error) and
--   checked_at             when. Shown in Settings so drift is visible.
--   contacts_included,     with contacts_only: how many contacts the script
--   contacts_total         names and how many there were — the bound, stated.
--
-- The primary key leads with user_id (RLS appends that predicate to every
-- plan). account_id is unique on its own and cascades with the account.

-- +goose Up

ALTER TYPE app.object_type ADD VALUE IF NOT EXISTS 'OutOfOffice';

-- +goose StatementBegin
CREATE TABLE app.out_of_office (
  user_id           app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  account_id        app.ulid NOT NULL UNIQUE REFERENCES app.accounts(id) ON DELETE CASCADE,
  enabled           boolean NOT NULL DEFAULT false,
  from_at           timestamptz,
  to_at             timestamptz,
  subject           text NOT NULL DEFAULT '',
  body              text NOT NULL DEFAULT '',
  reply_days        smallint NOT NULL DEFAULT 7 CHECK (reply_days >= 1 AND reply_days <= 30),
  from_address      text NOT NULL DEFAULT '',
  contacts_only     boolean NOT NULL DEFAULT false,
  installed_sha256  bytea CHECK (installed_sha256 IS NULL OR length(installed_sha256) = 32),
  server_state      text NOT NULL DEFAULT 'off'
                    CHECK (server_state IN ('off', 'active', 'displaced', 'missing', 'modified', 'error')),
  server_detail     text NOT NULL DEFAULT '',
  checked_at        timestamptz,
  contacts_included int,
  contacts_total    int,
  updated_at        timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, account_id),
  CONSTRAINT out_of_office_window CHECK (from_at IS NULL OR to_at IS NULL OR from_at < to_at)
);
-- +goose StatementEnd

ALTER TABLE app.out_of_office ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.out_of_office FORCE ROW LEVEL SECURITY;
CREATE POLICY out_of_office_isolation ON app.out_of_office
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());
GRANT SELECT, INSERT, UPDATE, DELETE ON app.out_of_office TO mailroom_app;

-- +goose Down
DROP TABLE IF EXISTS app.out_of_office;
