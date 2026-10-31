// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/oklog/ulid/v2"
)

// ObjectOutOfOffice is one mail account's out-of-office reply (#50,
// app.out_of_office, migration 00065). Its change_log object id is the
// ACCOUNT id: there is at most one per account.
const ObjectOutOfOffice ObjectType = "OutOfOffice"

// OutOfOffice mirrors one app.out_of_office row. A Get for an account with no
// row returns the defaults with Stored false.
type OutOfOffice struct {
	AccountID        ulid.ULID
	Stored           bool
	Enabled          bool
	From, To         *time.Time
	Subject          string
	Body             string
	ReplyDays        int
	FromAddress      string
	ContactsOnly     bool
	InstalledSHA256  []byte
	ServerState      string
	ServerDetail     string
	CheckedAt        *time.Time
	ContactsIncluded *int
	ContactsTotal    *int
	UpdatedAt        time.Time
}

// DefaultOutOfOffice is the form before anything is saved.
func DefaultOutOfOffice(accountID ulid.ULID) OutOfOffice {
	return OutOfOffice{AccountID: accountID, ReplyDays: 7, ServerState: "off"}
}

const outOfOfficeColumns = `account_id, enabled, from_at, to_at, subject, body, reply_days, from_address,
	contacts_only, installed_sha256, server_state, server_detail, checked_at, contacts_included,
	contacts_total, updated_at`

func scanOutOfOffice(row pgx.Row) (OutOfOffice, error) {
	var (
		o   OutOfOffice
		acc string
	)
	if err := row.Scan(&acc, &o.Enabled, &o.From, &o.To, &o.Subject, &o.Body, &o.ReplyDays, &o.FromAddress,
		&o.ContactsOnly, &o.InstalledSHA256, &o.ServerState, &o.ServerDetail, &o.CheckedAt,
		&o.ContactsIncluded, &o.ContactsTotal, &o.UpdatedAt); err != nil {
		return OutOfOffice{}, err
	}
	id, err := ulid.Parse(acc)
	if err != nil {
		return OutOfOffice{}, fmt.Errorf("store: out_of_office account id: %w", err)
	}
	o.AccountID = id
	o.Stored = true
	return o, nil
}

// GetOutOfOffice returns the account's out-of-office settings, or the defaults
// (Stored false) when none were saved. An account that is not this user's is
// ErrAccountNotFound, never another user's row (RLS).
func (tx *UserTx) GetOutOfOffice(ctx context.Context, accountID ulid.ULID) (OutOfOffice, error) {
	if _, err := tx.GetAccount(ctx, accountID); err != nil {
		return OutOfOffice{}, err
	}
	o, err := scanOutOfOffice(tx.tx.QueryRow(ctx, `
		SELECT `+outOfOfficeColumns+`
		  FROM app.out_of_office
		 WHERE user_id = $1 AND account_id = $2`, tx.userID.String(), accountID.String()))
	if errors.Is(err, pgx.ErrNoRows) {
		return DefaultOutOfOffice(accountID), nil
	}
	if err != nil {
		return OutOfOffice{}, fmt.Errorf("store: get out of office: %w", err)
	}
	return o, nil
}

// ListOutOfOffice returns every saved row of this user's.
func (tx *UserTx) ListOutOfOffice(ctx context.Context) ([]OutOfOffice, error) {
	rows, err := tx.tx.Query(ctx, `
		SELECT `+outOfOfficeColumns+`
		  FROM app.out_of_office
		 WHERE user_id = $1
		 ORDER BY account_id`, tx.userID.String())
	if err != nil {
		return nil, fmt.Errorf("store: list out of office: %w", err)
	}
	defer rows.Close()
	var out []OutOfOffice
	for rows.Next() {
		o, err := scanOutOfOffice(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list out of office: %w", err)
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

func sameInt(a, b *int) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// sameOutOfOffice compares everything the row stores except updated_at.
func sameOutOfOffice(a, b OutOfOffice) bool {
	return a.Enabled == b.Enabled && sameTime(a.From, b.From) && sameTime(a.To, b.To) &&
		a.Subject == b.Subject && a.Body == b.Body && a.ReplyDays == b.ReplyDays &&
		a.FromAddress == b.FromAddress && a.ContactsOnly == b.ContactsOnly &&
		bytes.Equal(a.InstalledSHA256, b.InstalledSHA256) && a.ServerState == b.ServerState &&
		a.ServerDetail == b.ServerDetail && sameTime(a.CheckedAt, b.CheckedAt) &&
		sameInt(a.ContactsIncluded, b.ContactsIncluded) && sameInt(a.ContactsTotal, b.ContactsTotal)
}

// PutOutOfOffice writes the whole row for o.AccountID and appends ONE
// OutOfOffice change_log row (created or updated) in the same transaction. A
// write identical to what is stored writes nothing and appends nothing.
// It reports whether anything changed.
func (tx *UserTx) PutOutOfOffice(ctx context.Context, o OutOfOffice) (bool, error) {
	current, err := tx.GetOutOfOffice(ctx, o.AccountID) // also proves the account is ours
	if err != nil {
		return false, err
	}
	if current.Stored && sameOutOfOffice(current, o) {
		return false, nil
	}
	if _, err := tx.tx.Exec(ctx, `
		INSERT INTO app.out_of_office (user_id, account_id, enabled, from_at, to_at, subject, body,
		       reply_days, from_address, contacts_only, installed_sha256, server_state, server_detail,
		       checked_at, contacts_included, contacts_total, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, now())
		ON CONFLICT (user_id, account_id) DO UPDATE SET
		       enabled = EXCLUDED.enabled, from_at = EXCLUDED.from_at, to_at = EXCLUDED.to_at,
		       subject = EXCLUDED.subject, body = EXCLUDED.body, reply_days = EXCLUDED.reply_days,
		       from_address = EXCLUDED.from_address, contacts_only = EXCLUDED.contacts_only,
		       installed_sha256 = EXCLUDED.installed_sha256, server_state = EXCLUDED.server_state,
		       server_detail = EXCLUDED.server_detail, checked_at = EXCLUDED.checked_at,
		       contacts_included = EXCLUDED.contacts_included, contacts_total = EXCLUDED.contacts_total,
		       updated_at = now()`,
		tx.userID.String(), o.AccountID.String(), o.Enabled, o.From, o.To, o.Subject, o.Body,
		o.ReplyDays, o.FromAddress, o.ContactsOnly, o.InstalledSHA256, o.ServerState, o.ServerDetail,
		o.CheckedAt, o.ContactsIncluded, o.ContactsTotal,
	); err != nil {
		return false, fmt.Errorf("store: put out of office: %w", err)
	}
	op := OpUpdated
	if !current.Stored {
		op = OpCreated
	}
	if _, err := tx.AppendChange(ctx, ObjectOutOfOffice, o.AccountID, op); err != nil {
		return false, err
	}
	return true, nil
}

// OutOfOfficeContacts returns the addresses a contacts-only reply answers:
// people the user has written to (outbound_count > 0) or marked VIP, never a
// blocked contact, most recently seen first, at most limit — and how many
// there are in all, so the caller can state the bound.
func (tx *UserTx) OutOfOfficeContacts(ctx context.Context, limit int) ([]string, int, error) {
	const where = `
		  FROM app.contact_addresses ca
		  JOIN app.contacts c ON c.id = ca.contact_id AND c.user_id = ca.user_id
		 WHERE ca.user_id = $1 AND NOT c.blocked AND (ca.outbound_count > 0 OR c.vip)`
	var total int
	if err := tx.tx.QueryRow(ctx, `SELECT count(*)`+where, tx.userID.String()).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("store: count out-of-office contacts: %w", err)
	}
	rows, err := tx.tx.Query(ctx, `SELECT ca.address::text`+where+`
		 ORDER BY ca.last_seen_at DESC, ca.address
		 LIMIT $2`, tx.userID.String(), limit)
	if err != nil {
		return nil, 0, fmt.Errorf("store: out-of-office contacts: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, 0, err
		}
		out = append(out, a)
	}
	return out, total, rows.Err()
}
