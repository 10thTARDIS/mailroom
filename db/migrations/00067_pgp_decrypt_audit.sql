-- SPDX-License-Identifier: AGPL-3.0-only
--
-- THE PERSISTENT DECRYPT TRAIL (#47 increment 6b, checklist item 4, ADR-037).
-- EXPAND-ONLY (ADR-008): one new table, nothing existing touched.
--
-- One row per call to the crypto process's `decrypt` method: which message,
-- why (rendering the body, or re-deriving an attachment), whether it opened,
-- and which key opened it. "Decrypted N messages for user U" is a query over
-- this table.
--
-- ⚠ WRITTEN BY THE CRYPTO PROCESS, NOT BY THE APPLICATION. user_id on the
-- crypto socket is asserted by the caller (ADR-024 §4 as amended), so an
-- application compromise makes `decrypt` an oracle; an audit row the
-- application wrote is one that attacker would simply not write. The crypto
-- process writes it in the same request, and refuses to return plaintext if
-- the row cannot be written.
--
-- ⚠ APPEND-ONLY FOR mailroom_app: SELECT and INSERT, no UPDATE, no DELETE —
-- the shape of app.audit_log and app.pgp_root_check. Both processes share the
-- role, so the grant is what stops a compromised application erasing a
-- decryption. Rows go only with their user (the hard-delete cascade, which runs
-- as the table owner).
--
-- ⚠ NO CONTENT. No subject, no plaintext, no ciphertext, no session key
-- (ADR-037: plaintext is never persisted, and a trail is somewhere it could
-- leak into by accident).
--
-- message_id carries NO foreign key on purpose: the trail outlives the message.
-- A deleted message's decryptions are still decryptions that happened.
--
-- NO change_log ROW: not a JMAP-visible object; no sync cursor observes it.

-- +goose Up

-- +goose StatementBegin
CREATE TABLE app.pgp_decrypt_audit (
  id              app.ulid PRIMARY KEY,
  user_id         app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
  message_id      app.ulid NOT NULL,
  purpose         text NOT NULL CHECK (purpose IN ('body', 'attachment')),
  outcome         text NOT NULL CHECK (outcome IN ('decrypted', 'failed')),
  key_fingerprint text,
  decrypted_at    timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT pgp_decrypt_audit_key_iff_decrypted
    CHECK ((outcome = 'decrypted') = (key_fingerprint IS NOT NULL))
);
-- +goose StatementEnd

CREATE INDEX pgp_decrypt_audit_user_time ON app.pgp_decrypt_audit (user_id, decrypted_at);

ALTER TABLE app.pgp_decrypt_audit ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.pgp_decrypt_audit FORCE ROW LEVEL SECURITY;
CREATE POLICY pgp_decrypt_audit_isolation ON app.pgp_decrypt_audit
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());

-- Append-only: no UPDATE, no DELETE (see above).
GRANT SELECT, INSERT ON app.pgp_decrypt_audit TO mailroom_app;

-- +goose Down
DROP TABLE IF EXISTS app.pgp_decrypt_audit;
