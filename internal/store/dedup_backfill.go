// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"fmt"

	"github.com/oklog/ulid/v2"
)

// Backfilling dedup_hash onto mail already held (#206, migration 00023).
//
// The column is added nullable and ingest writes it for every new message, so this is
// what makes EXISTING mail participate: the re-delivery lookup matches on the column, and
// a row with NULL matches nothing — so without a backfill the next re-delivery of an old
// message would still insert a second row.
//
// It cannot be a SQL UPDATE. The hash covers the message BODY, which lives in an
// encrypted content-addressed blob, so every row means a blob read and a decrypt. Hence a
// keyset-paged scan the caller drives, exactly like the resnippet backfill.

// DedupBackfillRef is one message's identity for the dedup backfill: its id and the
// content_hash that keys its raw blob.
type DedupBackfillRef struct {
	ID          ulid.ULID
	ContentHash []byte
}

// DedupBackfillRefsAfter walks the caller's messages in ascending id order, returning
// those that still need a dedup_hash. `after` is an exclusive low bound (pass the zero
// ULID to start); ULIDs sort by creation time, so this is a keyset walk over the primary
// key with no offset cost and no long-lived snapshot.
//
// With all=false it returns only rows whose dedup_hash IS NULL, so a resumed or repeated
// run does no redundant decryption. all=true returns every row, for the case where
// ingest.DedupHash itself has changed and every hash must be recomputed — which is a real
// possibility worth supporting explicitly rather than making an operator hand-write an
// UPDATE to NULL the column first.
func (tx *UserTx) DedupBackfillRefsAfter(
	ctx context.Context, after ulid.ULID, limit int, all bool,
) ([]DedupBackfillRef, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := tx.tx.Query(ctx, `
		SELECT id, content_hash
		  FROM app.messages
		 WHERE user_id = $1 AND id > $2
		   AND ($4 OR dedup_hash IS NULL)
		 ORDER BY id
		 LIMIT $3`,
		tx.userID.String(), after.String(), limit, all)
	if err != nil {
		return nil, fmt.Errorf("store: dedup backfill refs: %w", err)
	}
	defer rows.Close()

	var out []DedupBackfillRef
	for rows.Next() {
		var (
			id  string
			ref DedupBackfillRef
		)
		if err := rows.Scan(&id, &ref.ContentHash); err != nil {
			return nil, fmt.Errorf("store: scan dedup backfill ref: %w", err)
		}
		parsed, err := ulid.Parse(id)
		if err != nil {
			return nil, fmt.Errorf("store: dedup backfill id %q: %w", id, err)
		}
		ref.ID = parsed
		out = append(out, ref)
	}
	return out, rows.Err()
}

// SetMessageDedupHash records one message's dedup hash.
//
// NO change_log ROW, and that is not an oversight in the invariant (CLAUDE.md: every
// mutation of a JMAP-visible object appends one). dedup_hash is not a JMAP-visible
// property: it is on no object, in no `properties` set, and readable by no method, so
// there is nothing for a client to resync and a change row would make every client
// refetch a message that is byte-for-byte what they already hold. The same reasoning
// ADR-014 applies to contact re-sighting counters.
func (tx *UserTx) SetMessageDedupHash(ctx context.Context, id ulid.ULID, hash []byte) error {
	if len(hash) == 0 {
		return fmt.Errorf("store: refusing to set an empty dedup hash on %s", id)
	}
	if _, err := tx.tx.Exec(ctx, `
		UPDATE app.messages SET dedup_hash = $3 WHERE user_id = $1 AND id = $2`,
		tx.userID.String(), id.String(), hash); err != nil {
		return fmt.Errorf("store: set dedup hash: %w", err)
	}
	return nil
}

// CountMessagesWithoutDedupHash is how many of the caller's messages do not yet
// participate in re-delivery dedup. Zero is the backfill's success condition, and it is
// worth reporting rather than inferring: "scanned N, wrote N" says nothing about rows the
// scan never reached.
func (tx *UserTx) CountMessagesWithoutDedupHash(ctx context.Context) (int64, error) {
	var n int64
	if err := tx.tx.QueryRow(ctx,
		`SELECT count(*) FROM app.messages WHERE user_id = $1 AND dedup_hash IS NULL`,
		tx.userID.String()).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count messages without dedup hash: %w", err)
	}
	return n, nil
}
