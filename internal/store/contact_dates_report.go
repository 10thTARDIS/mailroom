// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"
)

// The read surface of `mailroom-admin contact-dates-report` (#816). READ-ONLY:
// nothing here writes, so there is no change_log consequence.
//
// WHY. contact_addresses.first_seen_at / last_seen_at default to now() at insert
// and are bumped to now() at ingest, so they record when Mailroom INGESTED mail,
// not when it was sent. A "last mail" column on a contacts page needs real
// message dates; this measures how far apart the two are before anything is
// backfilled.
//
// FUTURE DATES: an archive import falls back to the sender-controlled Date
// header when it has no received time of its own, so messages.received_at can
// hold a forged future date. Rows later than now()+1 day are ignored here (and
// so by the backfill, which reads this), or one forged message would pin a
// contact's last mail in the future (#829).
//
// WHICH CLOCK: messages.received_at, not sent_at. received_at is NOT NULL, it is
// the column every date surface keys on (the messages_user_received index, the
// Email/query sort, the search index's date buckets), and it is the provider's
// INTERNALDATE, so it survives a bulk import. sent_at is the Date header:
// nullable and sender-controlled ("may be absent/lying", schema comment).

// ContactMailDate is one contact address with its stored sighting dates and the
// message dates derived from the From side of the user's mail. The sent-to side
// is not here: recipients are not stored as columns (only in the blob), so the
// report derives it from the user's own sent messages (ContactSentRefsAfter).
type ContactMailDate struct {
	// ContactID owns the address (contact-dates-backfill groups by it).
	ContactID   ulid.ULID
	Address     string
	FirstSeenAt time.Time
	LastSeenAt  time.Time
	// FromCount is how many non-draft messages are From this address. 0 means
	// FromFirst and FromLast are nil, never zero times.
	FromCount int
	FromFirst *time.Time
	FromLast  *time.Time
}

// ContactMailDates returns every contact address of the caller with the
// earliest and latest received_at among non-draft messages From it.
//
// One aggregate: a GROUP BY over messages joined once to contact_addresses,
// never a per-address loop. Both sides are user_id-led (messages_user_bulk on
// (user_id, from_address); UNIQUE (user_id, address) on the contacts); RLS adds
// the same predicate. An address with no From mail is still returned (LEFT
// JOIN), so the caller can report it rather than lose it.
func (tx *UserTx) ContactMailDates(ctx context.Context) ([]ContactMailDate, error) {
	rows, err := tx.tx.Query(ctx, contactMailDatesSQL, tx.userID.String())
	if err != nil {
		return nil, fmt.Errorf("store: contact mail dates: %w", err)
	}
	defer rows.Close()
	var out []ContactMailDate
	for rows.Next() {
		var (
			r   ContactMailDate
			cid string
		)
		if err := rows.Scan(&cid, &r.Address, &r.FirstSeenAt, &r.LastSeenAt, &r.FromCount, &r.FromFirst, &r.FromLast); err != nil {
			return nil, fmt.Errorf("store: scan contact mail date: %w", err)
		}
		if r.ContactID, err = ulid.Parse(cid); err != nil {
			return nil, fmt.Errorf("store: parse contact id %q: %w", cid, err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// contactMailDatesSQL is ContactMailDates' one query, a constant so EXPLAIN can
// be pointed at exactly what runs.
const contactMailDatesSQL = `
	SELECT ca.contact_id, ca.address::text, ca.first_seen_at, ca.last_seen_at,
	       COALESCE(f.n, 0), f.first_at, f.last_at
	  FROM app.contact_addresses ca
	  LEFT JOIN (
	        SELECT m.from_address, count(*) AS n,
	               min(m.received_at) AS first_at, max(m.received_at) AS last_at
	          FROM app.messages m
	         WHERE m.user_id = $1 AND NOT m.is_draft AND m.from_address IS NOT NULL
	           AND m.received_at <= now() + interval '1 day'
	         GROUP BY m.from_address) f
	    ON f.from_address = ca.address
	 WHERE ca.user_id = $1
	 ORDER BY ca.address`

// ContactSentRef is one message the caller may have sent: enough to decide that
// (FromAddress) and to find its recipients in the blob (ContentHash).
type ContactSentRef struct {
	ID          ulid.ULID
	ContentHash []byte
	FromAddress string
	ReceivedAt  time.Time
}

// ContactSentRefsAfter walks the caller's non-draft messages in ascending id
// order (exclusive `after`; the zero ULID starts). It returns EVERY non-draft
// message: whether the From is the user's own is decided by the caller with
// ResolveSendIdentity, the rule ingest uses to set outbound_count, so the two
// cannot disagree.
func (tx *UserTx) ContactSentRefsAfter(ctx context.Context, after ulid.ULID, limit int) ([]ContactSentRef, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := tx.tx.Query(ctx, `
		SELECT m.id, m.content_hash, COALESCE(m.from_address::text, ''), m.received_at
		  FROM app.messages m
		 WHERE m.user_id = $1 AND m.id > $2 AND NOT m.is_draft
		   AND m.received_at <= now() + interval '1 day'
		 ORDER BY m.id
		 LIMIT $3`,
		tx.userID.String(), after.String(), limit)
	if err != nil {
		return nil, fmt.Errorf("store: walk contact sent refs: %w", err)
	}
	defer rows.Close()
	var out []ContactSentRef
	for rows.Next() {
		var (
			r     ContactSentRef
			idStr string
		)
		if err := rows.Scan(&idStr, &r.ContentHash, &r.FromAddress, &r.ReceivedAt); err != nil {
			return nil, fmt.Errorf("store: scan contact sent ref: %w", err)
		}
		if r.ID, err = ulid.Parse(idStr); err != nil {
			return nil, fmt.Errorf("store: parse message id %q: %w", idStr, err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
