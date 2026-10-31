// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"
)

// Per-account sync-health telemetry (spec §12, mailroom#43). This is the
// OPERATIONAL counterpart to the outbound ledger: the sync runner records the
// outcome of each pass over an account here, and the account-health panel reads
// it on demand through Account/getHealth.
//
// It is deliberately NOT a JMAP-visible object — a write here appends NO
// change_log row and it is not a store ObjectType (CLAUDE.md). It feeds a
// computed read, never /changes, the indexer, or a sync cursor.
//
// All three methods run inside a UserTx, so RLS scopes every read and upsert to
// the caller's own accounts: no identity, no rows (CLAUDE.md, fail closed).

// AccountHealth is the sync-health snapshot for one mail account. The nullable
// timestamps are pointers so "never attempted / never succeeded / no error" is
// distinguishable from a zero time.
type AccountHealth struct {
	AccountID           ulid.ULID
	LastAttemptAt       *time.Time
	LastSuccessAt       *time.Time
	LastError           string // "" when there is no recorded error
	LastErrorAt         *time.Time
	ConsecutiveFailures int
	UpdatedAt           time.Time
	// SendBlockedCode / SendBlockedAt: the account is RECEIVE-ONLY because the
	// provider refuses to take mail from it (SendBlocked*), or "" / nil.
	SendBlockedCode string
	SendBlockedAt   *time.Time
}

// RecordSyncSuccess marks a successful sync pass over accountID at time at: it
// clears any recorded error and resets the consecutive-failure counter to zero,
// while advancing both last_attempt_at and last_success_at to at. It is a single
// upsert keyed on account_id.
func (tx *UserTx) RecordSyncSuccess(ctx context.Context, accountID ulid.ULID, at time.Time) error {
	_, err := tx.tx.Exec(ctx, `
		INSERT INTO app.account_sync_status
			(account_id, user_id, last_attempt_at, last_success_at,
			 last_error, last_error_at, consecutive_failures, updated_at)
		VALUES ($2, $1, $3, $3, NULL, NULL, 0, now())
		ON CONFLICT (account_id) DO UPDATE SET
			last_attempt_at      = $3,
			last_success_at      = $3,
			last_error           = NULL,
			last_error_at        = NULL,
			consecutive_failures = 0,
			updated_at           = now()`,
		tx.userID.String(), accountID.String(), at.UTC())
	if err != nil {
		return fmt.Errorf("store: record sync success: %w", err)
	}
	return nil
}

// RecordSyncFailure marks a failed sync pass over accountID at time at with
// errMsg: it advances last_attempt_at, records the error and when it occurred,
// and increments the consecutive-failure counter. It deliberately does NOT touch
// last_success_at — the panel needs to keep showing when the account last
// actually synced, even while it is currently failing. Single upsert keyed on
// account_id.
func (tx *UserTx) RecordSyncFailure(ctx context.Context, accountID ulid.ULID, errMsg string, at time.Time) error {
	_, err := tx.tx.Exec(ctx, `
		INSERT INTO app.account_sync_status
			(account_id, user_id, last_attempt_at, last_error, last_error_at,
			 consecutive_failures, updated_at)
		VALUES ($2, $1, $3, $4, $3, 1, now())
		ON CONFLICT (account_id) DO UPDATE SET
			last_attempt_at      = $3,
			last_error           = $4,
			last_error_at        = $3,
			consecutive_failures = app.account_sync_status.consecutive_failures + 1,
			updated_at           = now()`,
		tx.userID.String(), accountID.String(), at.UTC(), errMsg)
	if err != nil {
		return fmt.Errorf("store: record sync failure: %w", err)
	}
	return nil
}

// AccountHealth returns the sync-health snapshot for each of accountIDs that has
// one, keyed by account id. Accounts that have never been synced simply have no
// entry (the caller renders them as "never synced"). The read is RLS-scoped, so
// only the caller's own accounts can ever appear.
func (tx *UserTx) AccountHealth(ctx context.Context, accountIDs []ulid.ULID) (map[ulid.ULID]AccountHealth, error) {
	out := make(map[ulid.ULID]AccountHealth, len(accountIDs))
	if len(accountIDs) == 0 {
		return out, nil
	}
	rows, err := tx.tx.Query(ctx, `
		SELECT account_id, last_attempt_at, last_success_at,
		       COALESCE(last_error, ''), last_error_at, consecutive_failures, updated_at,
		       COALESCE(send_blocked_code, ''), send_blocked_at
		  FROM app.account_sync_status
		 WHERE user_id = $1 AND account_id = ANY($2)`,
		tx.userID.String(), ulidStrings(accountIDs))
	if err != nil {
		return nil, fmt.Errorf("store: account health: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			h    AccountHealth
			acct string
		)
		if err := rows.Scan(&acct, &h.LastAttemptAt, &h.LastSuccessAt,
			&h.LastError, &h.LastErrorAt, &h.ConsecutiveFailures, &h.UpdatedAt,
			&h.SendBlockedCode, &h.SendBlockedAt); err != nil {
			return nil, fmt.Errorf("store: scan account health: %w", err)
		}
		if h.AccountID, err = ulid.Parse(acct); err != nil {
			return nil, fmt.Errorf("store: account health id %q: %w", acct, err)
		}
		out[h.AccountID] = h
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: account health rows: %w", err)
	}
	return out, nil
}
