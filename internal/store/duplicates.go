// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"
)

// Finding messages that look like the SAME message stored more than once (#206).
//
// ADR-001 makes identity SHA-256(raw RFC 5322 bytes), and says the consequence out
// loud: a transport that mutates bytes creates a second message. Re-delivery is
// exactly that case — the mail path rewrites something per delivery, so the same
// message arriving twice hashes twice and is two rows.
//
// This is the READ side of the diagnosis, and it is read-only by construction: it
// finds candidate groups so the caller can decrypt them and report what actually
// differs. Nothing here writes, and nothing here decides that a group IS duplicate
// — Message-ID is sender-controlled, so a shared one is a HINT and the bytes are
// the evidence.

// DuplicateCopy is one of several messages sharing a Message-ID.
type DuplicateCopy struct {
	ID          ulid.ULID
	ContentHash []byte
	Size        int64
	ReceivedAt  time.Time
	ThreadID    string
}

// DuplicateGroup is every message sharing one Message-ID, oldest first.
type DuplicateGroup struct {
	MessageID string
	Copies    []DuplicateCopy
}

// DuplicateMessageIDGroups returns the caller's Message-IDs held by more than one
// message row, with each copy's blob reference.
//
// Ordered by group size descending so a report shows the worst first, then by
// Message-ID for a stable order between runs (a diagnostic whose output reshuffles
// cannot be diffed against a previous run). Copies within a group are oldest first,
// because "the first one that arrived" is the one a fix would keep.
func (tx *UserTx) DuplicateMessageIDGroups(ctx context.Context, limit int) ([]DuplicateGroup, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := tx.tx.Query(ctx, `
		WITH dup AS (
		  SELECT message_id_hint, count(*) AS copies
		    FROM app.messages
		   WHERE user_id = $1
		     AND message_id_hint IS NOT NULL AND message_id_hint <> ''
		     AND trashed_at IS NULL
		   GROUP BY message_id_hint
		  HAVING count(*) > 1
		   ORDER BY count(*) DESC, message_id_hint
		   LIMIT $2
		)
		SELECT m.message_id_hint, dup.copies, m.id, m.content_hash, m.size_bytes,
		       m.received_at, COALESCE(m.thread_id, '')
		  FROM app.messages m
		  JOIN dup ON dup.message_id_hint = m.message_id_hint
		 WHERE m.user_id = $1 AND m.trashed_at IS NULL
		 ORDER BY dup.copies DESC, m.message_id_hint, m.received_at, m.id`,
		tx.userID.String(), limit)
	if err != nil {
		return nil, fmt.Errorf("store: duplicate message-id groups: %w", err)
	}
	defer rows.Close()

	var out []DuplicateGroup
	for rows.Next() {
		var (
			msgID, id, threadID string
			copies              int
			c                   DuplicateCopy
		)
		if err := rows.Scan(&msgID, &copies, &id, &c.ContentHash, &c.Size,
			&c.ReceivedAt, &threadID); err != nil {
			return nil, fmt.Errorf("store: scan duplicate copy: %w", err)
		}
		parsed, err := ulid.Parse(id)
		if err != nil {
			return nil, fmt.Errorf("store: duplicate copy id %q: %w", id, err)
		}
		c.ID = parsed
		c.ThreadID = threadID
		// The query returns one row per COPY, already grouped and ordered, so the
		// groups are built by watching the Message-ID change rather than with a
		// second query per group.
		if n := len(out); n > 0 && out[n-1].MessageID == msgID {
			out[n-1].Copies = append(out[n-1].Copies, c)
			continue
		}
		out = append(out, DuplicateGroup{MessageID: msgID, Copies: []DuplicateCopy{c}})
	}
	return out, rows.Err()
}
