// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
)

// The persistent decrypt trail (#47 increment 6b checklist item 4, ADR-037,
// migration 00067).
//
// ⚠ THE ONLY WRITER IS THE CRYPTO PROCESS (internal/pgpkeyd), and that is the
// decision this table exists for. The threat 6b adds is an application
// compromise turning the crypto socket into a decrypt oracle: user_id is
// asserted by the caller (ADR-024 §4 as amended). An audit row written by the
// APPLICATION would be skipped by exactly the attacker it is meant to record.
// Written by the crypto process, in the same request, before any plaintext is
// returned, it cannot be: every decryption leaves a row.
//
// And it cannot be removed afterwards. Both processes connect as mailroom_app,
// so the GRANT is the control: SELECT and INSERT only — no UPDATE, no DELETE —
// the same shape as app.audit_log and app.pgp_root_check. A compromised
// application can add noise; it cannot erase a decryption. Rows go only with
// their user (hard delete's cascade, which runs as the owner).
//
// ⚠ IT CARRIES NO CONTENT. No subject, no plaintext, no ciphertext, no session
// key: a message id, why, whether it opened, and which key opened it. ADR-037's
// rule is that plaintext is never persisted, and an audit trail is a place it
// could leak into by accident.
//
// No change_log row: the trail is not a JMAP-visible object, and no client
// sync cursor can observe it.

// PGPDecryptAudit is one decrypt call.
type PGPDecryptAudit struct {
	MessageID ulid.ULID
	// Purpose is "body" or "attachment" (pgpwire.DecryptPurpose).
	Purpose string
	// Decrypted is the outcome. Failures are recorded too.
	Decrypted bool
	// Fingerprint is the decrypting key's primary fingerprint; set iff
	// Decrypted.
	Fingerprint string
}

// ErrDecryptAuditInvalid is returned for a row the trail will not hold.
var ErrDecryptAuditInvalid = errors.New("store: not a valid decrypt audit row")

// RecordPGPDecrypt appends one row to app.pgp_decrypt_audit.
func (tx *UserTx) RecordPGPDecrypt(ctx context.Context, a PGPDecryptAudit) error {
	if a.MessageID.Compare(ulid.ULID{}) == 0 {
		return fmt.Errorf("%w: a message id is required", ErrDecryptAuditInvalid)
	}
	if a.Purpose != "body" && a.Purpose != "attachment" {
		return fmt.Errorf("%w: purpose %q", ErrDecryptAuditInvalid, a.Purpose)
	}
	outcome := "failed"
	var fpr *string
	if a.Decrypted {
		f := strings.ToUpper(strings.TrimSpace(a.Fingerprint))
		if f == "" {
			return fmt.Errorf("%w: a successful decrypt must name its key", ErrDecryptAuditInvalid)
		}
		outcome = "decrypted"
		fpr = &f
	}
	_, err := tx.tx.Exec(ctx, `
		INSERT INTO app.pgp_decrypt_audit
		  (id, user_id, message_id, purpose, outcome, key_fingerprint)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		NewULID().String(), tx.userID.String(), a.MessageID.String(),
		a.Purpose, outcome, fpr)
	if err != nil {
		return fmt.Errorf("store: record pgp decrypt: %w", err)
	}
	return nil
}

// PGPDecryptSummary is "decrypted N messages for user U", reconstructed from the
// trail.
type PGPDecryptSummary struct {
	// Calls counts rows by outcome ("decrypted", "failed").
	Calls map[string]int
	// Messages is the number of DISTINCT messages successfully decrypted. A
	// message opened ten times is one message and ten calls; both numbers are
	// reported because each answers a different question.
	Messages int
}

// PGPDecryptSummary reads the calling user's trail since a point in time.
func (tx *UserTx) PGPDecryptSummary(ctx context.Context, since time.Time) (PGPDecryptSummary, error) {
	out := PGPDecryptSummary{Calls: map[string]int{}}
	rows, err := tx.tx.Query(ctx, `
		SELECT outcome, count(*)
		  FROM app.pgp_decrypt_audit
		 WHERE user_id = $1 AND decrypted_at >= $2
		 GROUP BY outcome`,
		tx.userID.String(), since)
	if err != nil {
		return out, fmt.Errorf("store: pgp decrypt summary: %w", err)
	}
	for rows.Next() {
		var outcome string
		var n int
		if err := rows.Scan(&outcome, &n); err != nil {
			rows.Close()
			return out, fmt.Errorf("store: pgp decrypt summary: %w", err)
		}
		out.Calls[outcome] = n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("store: pgp decrypt summary: %w", err)
	}
	if err := tx.tx.QueryRow(ctx, `
		SELECT count(DISTINCT message_id)
		  FROM app.pgp_decrypt_audit
		 WHERE user_id = $1 AND decrypted_at >= $2 AND outcome = 'decrypted'`,
		tx.userID.String(), since).Scan(&out.Messages); err != nil {
		return out, fmt.Errorf("store: pgp decrypt summary: %w", err)
	}
	return out, nil
}
