// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"fmt"

	"github.com/oklog/ulid/v2"
)

// RecipientCols is the stored, searchable form of a message's recipients and
// List-Id (ADR-055, migration 00095). internal/rcptproj derives it from a parsed
// message; this package only stores it, so it never imports a parser.
//
// All five values are written together. An empty string means "filled, nothing
// there"; the columns' NULL (a nil *RecipientCols on a MessageRow) means "not
// filled yet".
type RecipientCols struct {
	To, Cc, Bcc string // lowercased names and addresses, one per line
	ToSort      string // first To display name, else address (case kept)
	ListIDKey   string // the id inside List-Id's angle brackets, lowercased
	// Truncated is true when a field held more recipients than the bound and
	// the rest were not stored. Reported by the backfill; never persisted.
	Truncated bool
}

// recipientArgs is the five column values as SQL arguments: strings when filled,
// untyped nils (NULL) when not.
type recipientArgs struct {
	To, Cc, Bcc, ToSort, ListIDKey any
}

func (c *RecipientCols) nullable() recipientArgs {
	if c == nil {
		return recipientArgs{}
	}
	return recipientArgs{To: c.To, Cc: c.Cc, Bcc: c.Bcc, ToSort: c.ToSort, ListIDKey: c.ListIDKey}
}

// RecipientCandidate is a message whose recipient columns are not filled yet.
type RecipientCandidate struct {
	ID          ulid.ULID
	ContentHash []byte
}

// RecipientCensus counts a user's messages by whether their recipient columns
// are filled (the denominator and the "already done" figure of the backfill
// report).
type RecipientCensus struct {
	Total  int
	Filled int
}

// RecipientCensus counts the caller's messages and how many are filled.
func (tx *UserTx) RecipientCensus(ctx context.Context) (RecipientCensus, error) {
	var c RecipientCensus
	if err := tx.tx.QueryRow(ctx, `
		SELECT count(*), count(rcpt_to)
		  FROM app.messages WHERE user_id = $1`, tx.userID.String()).Scan(&c.Total, &c.Filled); err != nil {
		return c, fmt.Errorf("store: recipient census: %w", err)
	}
	return c, nil
}

// MessagesMissingRecipients lists up to limit messages after the cursor, in id
// order, whose recipient columns are NULL. The caller advances the cursor over
// what it was given, so a read-only report and an applying run walk the same
// way.
func (tx *UserTx) MessagesMissingRecipients(ctx context.Context, after ulid.ULID, limit int) ([]RecipientCandidate, error) {
	rows, err := tx.tx.Query(ctx, `
		SELECT id, content_hash
		  FROM app.messages
		 WHERE user_id = $1 AND rcpt_to IS NULL AND id > $2
		 ORDER BY id
		 LIMIT $3`, tx.userID.String(), after.String(), limit)
	if err != nil {
		return nil, fmt.Errorf("store: messages missing recipients: %w", err)
	}
	defer rows.Close()
	var out []RecipientCandidate
	for rows.Next() {
		var id string
		var c RecipientCandidate
		if err := rows.Scan(&id, &c.ContentHash); err != nil {
			return nil, fmt.Errorf("store: scan missing-recipients row: %w", err)
		}
		u, perr := ulid.Parse(id)
		if perr != nil {
			return nil, fmt.Errorf("store: parse message id %q: %w", id, perr)
		}
		c.ID = u
		out = append(out, c)
	}
	return out, rows.Err()
}

// FillRecipients writes the recipient columns of one message that has none yet,
// and reports whether it did. The guard (rcpt_to IS NULL) makes a repeat run, or
// two overlapping ones, change nothing the first did not.
//
// IT APPENDS NO change_log ROW, deliberately: none of these columns is a
// JMAP-visible Email property (to, cc, bcc and header:List-Id are read from the
// message file as before); they only make the existing values filterable
// (ADR-055).
func (tx *UserTx) FillRecipients(ctx context.Context, id ulid.ULID, c RecipientCols) (bool, error) {
	tag, err := tx.tx.Exec(ctx, `
		UPDATE app.messages
		   SET rcpt_to = $3, rcpt_cc = $4, rcpt_bcc = $5, rcpt_to_sort = $6, list_id_key = $7
		 WHERE user_id = $1 AND id = $2 AND rcpt_to IS NULL`,
		tx.userID.String(), id.String(), c.To, c.Cc, c.Bcc, c.ToSort, c.ListIDKey)
	if err != nil {
		return false, fmt.Errorf("store: fill recipients: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}
