// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"
)

// RECEIVE-ONLY ACCOUNTS (#52 increment 6, ADR-046 §12). A provider can refuse
// to take mail from an account as a POLICY, for every message, until an
// administrator changes it: Microsoft's `535 5.7.139` (the organisation has
// turned off authenticated SMTP). The account then becomes receive-only:
// EmailSubmission/set refuses a new send up front, and a submission already
// queued fails with SendBlockedReason instead of dialling the provider again.
//
// It is OPERATIONAL state on app.account_sync_status (migration 00080), beside
// the sync telemetry — not a JMAP-visible Account property — so marking and
// clearing it appends no change_log row; Account/getHealth reads it
// (sendBlocked).
//
// It is cleared by what can change the answer: a successful SMTP
// authentication (the connect/reconnect check, or a send that went through),
// and a change of a password account's connection settings.

// SendBlockedSMTPAuthDisabled is the stable code for 5.7.139.
const SendBlockedSMTPAuthDisabled = "smtp_auth_disabled"

// SendBlockedReason is the readable explanation for a send-blocked code — what
// a refused or failed submission says, and what Account/getHealth shows.
func SendBlockedReason(code string) string {
	switch code {
	case SendBlockedSMTPAuthDisabled:
		return "this account can receive mail but not send it: its organisation has turned off " +
			"sending from apps (SMTP AUTH is disabled for the mailbox, Microsoft code 5.7.139). " +
			"Ask your administrator to enable Authenticated SMTP for your mailbox, then reconnect " +
			"the account in Settings, Accounts to check again"
	}
	return "this account cannot send mail: its provider refuses to accept mail from it"
}

// MarkSendBlocked records that accountID is receive-only for code. The first
// mark is kept (send_blocked_at says since when); marking again with the same
// code changes nothing.
func (tx *UserTx) MarkSendBlocked(ctx context.Context, accountID ulid.ULID, code string, at time.Time) error {
	_, err := tx.tx.Exec(ctx, `
		INSERT INTO app.account_sync_status (account_id, user_id, send_blocked_code, send_blocked_at)
		VALUES ($2, $1, $3, $4)
		ON CONFLICT (account_id) DO UPDATE SET
			send_blocked_code = $3,
			send_blocked_at   = COALESCE(app.account_sync_status.send_blocked_at, $4),
			updated_at        = now()
		 WHERE app.account_sync_status.send_blocked_code IS DISTINCT FROM $3`,
		tx.userID.String(), accountID.String(), code, at.UTC())
	if err != nil {
		return fmt.Errorf("store: mark send blocked: %w", err)
	}
	return nil
}

// ClearSendBlocked lifts accountID's receive-only state, if any, and reports
// whether there was one.
func (tx *UserTx) ClearSendBlocked(ctx context.Context, accountID ulid.ULID) (bool, error) {
	tag, err := tx.tx.Exec(ctx, `
		UPDATE app.account_sync_status
		   SET send_blocked_code = NULL, send_blocked_at = NULL, updated_at = now()
		 WHERE user_id = $1 AND account_id = $2 AND send_blocked_code IS NOT NULL`,
		tx.userID.String(), accountID.String())
	if err != nil {
		return false, fmt.Errorf("store: clear send blocked: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// SendBlocked returns accountID's receive-only code, or "" when it can send.
func (tx *UserTx) SendBlocked(ctx context.Context, accountID ulid.ULID) (string, error) {
	var code *string
	err := tx.tx.QueryRow(ctx, `
		SELECT send_blocked_code FROM app.account_sync_status
		 WHERE user_id = $1 AND account_id = $2`,
		tx.userID.String(), accountID.String()).Scan(&code)
	if err != nil {
		if isNoRows(err) {
			return "", nil
		}
		return "", fmt.Errorf("store: send blocked: %w", err)
	}
	if code == nil {
		return "", nil
	}
	return *code, nil
}
