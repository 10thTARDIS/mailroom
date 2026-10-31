-- SPDX-License-Identifier: AGPL-3.0-only
--
-- app.pgp_root_check: the crypto process verifies its PGP root at boot (#525).
--
-- THE DEFECT (#351 B5's runtime half). The crypto process reads its root at
-- start and unseals nothing until somebody signs or decrypts — internal/pgpkeyd
-- unseals lazily. So an instance restored with the WRONG root, or with a freshly
-- generated one (deploy/README.md §2 hands those out for first installs),
-- starts, reports healthy, and serves mail. The failure surfaces the first time
-- a key is used, possibly months later, when every stored private key is
-- already unopenable.
--
-- WHY A CHECK VALUE AND NOT "UNSEAL ONE STORED KEY". The issue asked for the
-- latter, and the crypto process cannot do it: at boot there is no caller, and
-- app.user_keys is readable only under the owning user's identity. Reading "any
-- key" would take an eleventh SECURITY DEFINER returning SEALED KEY MATERIAL
-- across users — a far larger thing to hand out than count_due_jobs' bigint —
-- and it still could not see a wrong root on a database with no keys yet,
-- which is the likeliest shape of a bad restore. Decided on #525.
--
-- So at its first boot against a database the crypto process records
--
--     HKDF(pgp root, salt = none, info = "mailroom-pgp-root-check-v1")
--
-- here, and at every boot after it re-derives the value and refuses to listen
-- on a mismatch. It is a one-way function of a 256-bit random secret under a
-- label no other derivation uses, so it discloses nothing about the root or the
-- per-user wrapping keys (salt = user id, info "mailroom-pgp-v1"). It is not a
-- secret, and it travels in the backup — which is the point: restoring this
-- database and pairing it with a different root is exactly the combination
-- that mismatches.
--
-- ⚠ WHAT IT CLAIMS, precisely: "this is the root this database was built
-- with". Not "this root opens the stored keys". For a restore those coincide;
-- they are not the same statement, and the restore drill's step 5 — which
-- unseals every stored key with the escrowed root — is still what proves the
-- second.
--
-- ⚠ A DATABASE THAT PREDATES THIS MIGRATION HAS NO RECORD, so the first boot
-- after it records whatever root that process was given: trust on first use.
-- The crypto process cannot check that root against keys already stored — RLS
-- hides them from it, by design — so that one boot verifies nothing. An
-- upgrade that is ALSO a restore with a different root, in the same step, is
-- therefore not caught. Any other ordering is.
--
-- WHO MAY WRITE IT. The crypto process connects as mailroom_app, and so does
-- the server (ADR-024 declined a separate crypto role). So the grant is the
-- control, not the role:
--
--   * SELECT and INSERT, and nothing else. With no UPDATE and no DELETE, SQL
--     injection through the application can neither overwrite the value (to
--     wedge the crypto process) nor delete it (so the next boot records
--     whatever root it has). Resetting it is a superuser act, deliberately;
--     docs/backup-restore-runbook.md says when that is right and what it costs.
--   * The policies admit the row only where app.current_user_id() IS NULL — a
--     SystemTx, which is what the crypto process's boot runs. Under any user's
--     identity the row is invisible and cannot be inserted, so no request path
--     can see or pre-empt it.
--   * The singleton key makes a second row impossible, and the length CHECK
--     refuses anything that is not an HKDF-SHA256 output.
--
-- ⚠ IT IS THE SECOND TABLE IN app WITH NO user_id (app.users is the first), and
-- it inverts "no identity, no rows": its one row exists ONLY in the
-- identity-less context. The RLS suite therefore lists it in systemTables
-- rather than appTables — see internal/store/fixtures_test.go.
--
-- No function is added, so TestEveryDefinerPinsPgTempLast's definer count is
-- unchanged.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE app.pgp_root_check (
  singleton   boolean PRIMARY KEY DEFAULT true CHECK (singleton),
  info        text NOT NULL,
  check_value bytea NOT NULL CHECK (octet_length(check_value) = 32),
  recorded_at timestamptz NOT NULL DEFAULT now()
);
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE app.pgp_root_check ENABLE ROW LEVEL SECURITY;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE app.pgp_root_check FORCE ROW LEVEL SECURITY;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE POLICY pgp_root_check_system_read ON app.pgp_root_check
  FOR SELECT TO mailroom_app
  USING (app.current_user_id() IS NULL);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE POLICY pgp_root_check_system_record ON app.pgp_root_check
  FOR INSERT TO mailroom_app
  WITH CHECK (app.current_user_id() IS NULL);
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON TABLE app.pgp_root_check IS
  'One row: HKDF(pgp root, info="mailroom-pgp-root-check-v1"), recorded by the '
  'crypto process at its first boot against this database and compared at '
  'every boot after; a mismatch stops it before it listens (#525). Claims only '
  '"this is the root this database was built with", not "this root opens the '
  'stored keys" — the restore drill proves that. Not a secret. Insert-only for '
  'mailroom_app; resetting it is a superuser act (docs/backup-restore-runbook.md).';
-- +goose StatementEnd

-- +goose StatementBegin
GRANT SELECT, INSERT ON app.pgp_root_check TO mailroom_app;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS app.pgp_root_check;
-- +goose StatementEnd
