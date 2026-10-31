// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"
)

// MessageFromRef is one message's blob reference plus what
// `mailroom-admin from-field-report` (#754) counts per From shape beside the
// bytes: whether Mailroom wrote it (the sender-auth display is null for our
// own mail) and whether learned placement acted on it.
type MessageFromRef struct {
	ID          ulid.ULID
	ContentHash []byte
	// LocallyAuthored is `local_mailbox_id IS NOT NULL`, as EmailView reads it.
	LocallyAuthored bool
	// LearnedPlaced is whether any label on the message was applied by learned
	// placement (app.message_labels.applied_by = 'learned', #714).
	LearnedPlaced bool
	// FromAddress and ReceivedAt feed the notice classification (#766):
	// whether the message is the user's own, and which date-bounded
	// authserv-id entries apply (received_at, never the Date header).
	FromAddress string
	ReceivedAt  time.Time
	// ProviderDated: see MessageAuthRef.ProviderDated.
	ProviderDated bool
}

// TrustTime is the received time to hand ml.TrustedAt.
func (r MessageFromRef) TrustTime() time.Time { return TrustTime(r.ReceivedAt, r.ProviderDated) }

// MessageFromRefsAfter walks the caller's messages in ascending id order —
// the same keyset walk as MessageAuthRefsAfter. RLS scopes it to the caller,
// and the user_id predicate says so in the query as well.
func (tx *UserTx) MessageFromRefsAfter(ctx context.Context, after ulid.ULID, limit int) ([]MessageFromRef, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := tx.tx.Query(ctx, `
		SELECT m.id, m.content_hash, m.local_mailbox_id IS NOT NULL,
		       EXISTS (SELECT 1 FROM app.message_labels ml
		                WHERE ml.user_id = m.user_id AND ml.message_id = m.id
		                  AND ml.applied_by = 'learned'),
		       COALESCE(m.from_address::text, ''), m.received_at,
		       `+providerDatedExpr("m")+`
		  FROM app.messages m
		 WHERE m.user_id = $1 AND m.id > $2
		 ORDER BY m.id
		 LIMIT $3`,
		tx.userID.String(), after.String(), limit)
	if err != nil {
		return nil, fmt.Errorf("store: walk message from refs: %w", err)
	}
	defer rows.Close()
	var out []MessageFromRef
	for rows.Next() {
		var (
			r     MessageFromRef
			idStr string
		)
		if err := rows.Scan(&idStr, &r.ContentHash, &r.LocallyAuthored, &r.LearnedPlaced, &r.FromAddress, &r.ReceivedAt, &r.ProviderDated); err != nil {
			return nil, fmt.Errorf("store: scan message from ref: %w", err)
		}
		if r.ID, err = ulid.Parse(idStr); err != nil {
			return nil, fmt.Errorf("store: parse message id %q: %w", idStr, err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
