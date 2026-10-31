// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"
)

// The read side of a per-user export (#54, spec §10).
//
// ⚠ IT IS PAGED, NOT A SINGLE QUERY, and that is the whole reason this exists
// rather than a `SELECT *`. The importer streams — "a 25 GB Takeout costs the
// memory of ONE message, not of the archive" — and an exporter that materialised
// every row first would be the asymmetric half of a round trip: fine on the
// a small mailbox, and fatal on the archive the import path was
// deliberately built to survive.
//
// Paging is by ULID keyset rather than OFFSET. ULIDs are monotonic and the
// primary key is (user_id, id), so `id > $after ORDER BY id` is an index range
// scan whose cost does not grow with how far in you are, and it cannot skip or
// repeat a row when a concurrent write lands mid-export.

// ExportMessage is one message's exportable metadata. The BYTES are not here:
// they live in the blob store and are streamed one at a time by the exporter.
type ExportMessage struct {
	ID          ulid.ULID
	ContentHash []byte
	ReceivedAt  time.Time
	SizeBytes   int64

	// The canonical flags. Maildir encodes Seen/Answered/Flagged/Draft and
	// trashed in the FILENAME, so those survive a round trip through the format
	// itself rather than needing a sidecar.
	Seen     bool
	Flagged  bool
	Answered bool
	Draft    bool
	// Junk has NO maildir representation and is carried in a sidecar.
	Junk bool
	// Keywords are the non-label IMAP keywords. Maildir's info section has no
	// room for arbitrary atoms, so these are sidecar data too.
	Keywords []string
	// Trashed is when the message was trashed, or nil. Exported as maildir's
	// 'T' flag — see the exporter for what an importer does with it.
	Trashed *time.Time
}

// ExportMessagePage returns up to limit messages with an id greater than after,
// in id order. Pass a zero ULID to start.
//
// RLS scopes it to the calling user, so — as with the census — there is no
// hand-written user_id predicate that could disagree with the policy.
func (tx *UserTx) ExportMessagePage(ctx context.Context, after ulid.ULID, limit int) ([]ExportMessage, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("store: export page limit must be positive, got %d", limit)
	}
	rows, err := tx.tx.Query(ctx, `
		SELECT id, content_hash, received_at, size_bytes,
		       is_seen, is_flagged, is_answered, is_draft, is_junk,
		       keywords, trashed_at
		  FROM app.messages
		 WHERE id > $1
		 ORDER BY id
		 LIMIT $2`, after.String(), limit)
	if err != nil {
		return nil, fmt.Errorf("store: export message page: %w", err)
	}
	defer rows.Close()

	var out []ExportMessage
	for rows.Next() {
		var (
			id string
			m  ExportMessage
		)
		if err := rows.Scan(&id, &m.ContentHash, &m.ReceivedAt, &m.SizeBytes,
			&m.Seen, &m.Flagged, &m.Answered, &m.Draft, &m.Junk,
			&m.Keywords, &m.Trashed); err != nil {
			return nil, fmt.Errorf("store: scan export message: %w", err)
		}
		parsed, err := ulid.Parse(id)
		if err != nil {
			return nil, fmt.Errorf("store: export message id %q: %w", id, err)
		}
		m.ID = parsed
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: export message page: %w", err)
	}
	return out, nil
}
