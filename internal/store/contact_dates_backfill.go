// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"
)

// The write surface of `mailroom-admin contact-dates-backfill` (#816,
// ADR-041 §5). The command decides WHAT to change from the same measurement
// contact-dates-report prints; this file only applies one batch of it, under
// one savepoint with its change rows.
//
// CHANGE ROWS: yes. Contact.firstMailAt / lastMailAt are properties now, so a
// corrected date is a visible change, and so is a contact becoming saved. One
// Contact `updated` row per contact the batch actually changed, however many
// of its addresses moved (AppendChanges: one range, one lock hold). A fix
// whose row no longer holds the dates it was computed from (ingest moved it
// in between) is skipped and counted as stale, never forced.

// ContactDateFix is one address's correction: the stored dates it was
// computed against, and the true dates to write for the signals that differ.
type ContactDateFix struct {
	ContactID         ulid.ULID
	Address           string
	OldFirst, OldLast time.Time
	NewFirst, NewLast time.Time
	FixFirst, FixLast bool
}

// ContactBackfillBatch is what one ApplyContactBackfillBatch call did.
type ContactBackfillBatch struct {
	// FirstFixed / LastFixed count addresses whose first/last date was set.
	FirstFixed, LastFixed int
	// Stale counts fixes skipped because the stored dates had moved.
	Stale int
	// Saved counts contacts that became saved.
	Saved int
	// Contacts is how many distinct contacts changed (= change rows written).
	Contacts int
}

// UnsavedWrittenToContacts lists the live contacts that are not saved and have
// at least one address the user has written to (outbound_count > 0): the
// contacts the backfill saves.
func (tx *UserTx) UnsavedWrittenToContacts(ctx context.Context) ([]ulid.ULID, error) {
	rows, err := tx.tx.Query(ctx, `
		SELECT c.id FROM app.contacts c
		 WHERE c.user_id = $1 AND c.saved_at IS NULL AND c.merged_into IS NULL
		   AND EXISTS (SELECT 1 FROM app.contact_addresses ca
		                WHERE ca.user_id = c.user_id AND ca.contact_id = c.id AND ca.outbound_count > 0)
		 ORDER BY c.id`,
		tx.userID.String())
	if err != nil {
		return nil, fmt.Errorf("store: unsaved written-to contacts: %w", err)
	}
	defer rows.Close()
	var out []ulid.ULID
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, fmt.Errorf("store: scan contact id: %w", err)
		}
		id, err := ulid.Parse(s)
		if err != nil {
			return nil, fmt.Errorf("store: parse contact id %q: %w", s, err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ApplyContactBackfillBatch writes one batch of date fixes and saves, and one
// Contact `updated` row per contact that actually changed, all in one
// savepoint.
func (tx *UserTx) ApplyContactBackfillBatch(ctx context.Context, fixes []ContactDateFix, save []ulid.ULID) (ContactBackfillBatch, error) {
	var res ContactBackfillBatch
	err := tx.inSavepoint(ctx, func() error {
		changed := map[ulid.ULID]bool{}
		var order []ulid.ULID
		mark := func(id ulid.ULID) {
			if !changed[id] {
				changed[id] = true
				order = append(order, id)
			}
		}
		for _, f := range fixes {
			if !f.FixFirst && !f.FixLast {
				continue
			}
			var cid string
			err := tx.tx.QueryRow(ctx, `
				UPDATE app.contact_addresses
				   SET first_seen_at = CASE WHEN $5 THEN $7::timestamptz ELSE first_seen_at END,
				       last_seen_at  = CASE WHEN $6 THEN $8::timestamptz ELSE last_seen_at END
				 WHERE user_id = $1 AND address = $2 AND first_seen_at = $3 AND last_seen_at = $4
				 RETURNING contact_id`,
				tx.userID.String(), f.Address, f.OldFirst, f.OldLast, f.FixFirst, f.FixLast,
				f.NewFirst, f.NewLast).Scan(&cid)
			if isNoRows(err) {
				res.Stale++
				continue
			}
			if err != nil {
				return fmt.Errorf("store: backfill contact dates: %w", err)
			}
			id, err := ulid.Parse(cid)
			if err != nil {
				return fmt.Errorf("store: parse contact id %q: %w", cid, err)
			}
			if f.FixFirst {
				res.FirstFixed++
			}
			if f.FixLast {
				res.LastFixed++
			}
			mark(id)
		}
		if len(save) > 0 {
			rows, err := tx.tx.Query(ctx, `
				UPDATE app.contacts c SET saved_at = now()
				 WHERE c.user_id = $1 AND c.id = ANY($2) AND c.saved_at IS NULL AND c.merged_into IS NULL
				   AND EXISTS (SELECT 1 FROM app.contact_addresses ca
				                WHERE ca.user_id = c.user_id AND ca.contact_id = c.id AND ca.outbound_count > 0)
				 RETURNING c.id`,
				tx.userID.String(), ulidStrings(save))
			if err != nil {
				return fmt.Errorf("store: backfill saved contacts: %w", err)
			}
			for rows.Next() {
				var s string
				if err := rows.Scan(&s); err != nil {
					rows.Close()
					return fmt.Errorf("store: scan saved contact: %w", err)
				}
				id, err := ulid.Parse(s)
				if err != nil {
					rows.Close()
					return fmt.Errorf("store: parse contact id %q: %w", s, err)
				}
				res.Saved++
				mark(id)
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return fmt.Errorf("store: backfill saved contacts: %w", err)
			}
		}
		res.Contacts = len(order)
		return tx.AppendChanges(ctx, ObjectContact, order, OpUpdated)
	})
	if err != nil {
		return ContactBackfillBatch{}, err
	}
	return res, nil
}
