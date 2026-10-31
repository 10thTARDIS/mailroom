// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"
)

// THE MERGE RECORD (#834, ADR-041 §4, migration 00073).
//
// A merge writes one app.contact_merges row: into's own state before the
// merge, the combined state it wrote, and the address rows it moved. A
// separate that takes every one of those addresses back out of the contact
// holding them UNDOES the merge on its target (the record's into_contact): each
// field that still equals what the merge wrote goes back to its pre-merge
// value, and a field that does not (the user edited it after the merge) is
// kept. The record is then deleted.
//
// MERGES OF MERGES. Every merge has its own record, so undoing works from
// each one, newest first. Two shapes:
//
//   - Stacked on one contact (A into C, then B into C). Undoing the older
//     merge while a newer one stands cannot simply restore the older
//     before-state: the newer merge's contribution is in the current value
//     too (B's VIP, B's notes). So the value is REBUILT: the older record's
//     before-state, combined again with each newer merge's `from` in order,
//     by the same combine rules. The newer records are rebased onto the
//     rebuilt chain, so undoing them later restores the right value. A field
//     the user changed anywhere along the chain (between the merges, or
//     after the last) is kept, and nothing is rebuilt for it.
//   - Nested (A into B, then B into C). The first record's target is B, now
//     a tombstone. When its addresses all leave, the tombstone's own state
//     is restored (no change row: a tombstone was reported destroyed and is
//     invisible), so the contact a separate makes from B no longer carries
//     A's name or notes. When B's own addresses leave C, the records that
//     target B follow B's new contact (into_contact is repointed), so a later
//     separate of A's addresses restores that live contact (one Contact
//     `updated` row when something changed).

// contactSnapshot is contactState as stored in before_state and after_state.
type contactSnapshot struct {
	DisplayName      string     `json:"displayName"`
	UserName         *string    `json:"userName"`
	Notes            string     `json:"notes"`
	VIP              bool       `json:"vip"`
	Blocked          bool       `json:"blocked"`
	BlockDestination string     `json:"blockDestination"`
	BlockedAt        *time.Time `json:"blockedAt"`
	SavedAt          *time.Time `json:"savedAt"`
}

func snapshotOf(c contactState) contactSnapshot {
	return contactSnapshot{DisplayName: c.displayName, UserName: c.userName, Notes: c.notes, VIP: c.vip,
		Blocked: c.blocked, BlockDestination: c.dest, BlockedAt: c.blockedAt, SavedAt: c.savedAt}
}

func (s contactSnapshot) state(id ulid.ULID) contactState {
	return contactState{id: id, displayName: s.DisplayName, userName: s.UserName, notes: s.Notes, vip: s.VIP,
		blocked: s.Blocked, dest: s.BlockDestination, blockedAt: s.BlockedAt, savedAt: s.SavedAt}
}

// contactField is one independently combined part of a contact's own state:
// the unit a separate restores or keeps. The block is one field (blocked, its
// destination and its date move together).
type contactField int

const (
	fieldDisplayName contactField = iota
	fieldUserName
	fieldNotes
	fieldVIP
	fieldSaved
	fieldBlock
)

var contactFields = []contactField{fieldDisplayName, fieldUserName, fieldNotes, fieldVIP, fieldSaved, fieldBlock}

// Property is the field's Contact property name, as reported in a separate's
// keptFields.
func (f contactField) Property() string {
	switch f {
	case fieldDisplayName:
		return "displayName"
	case fieldUserName:
		return "userName"
	case fieldNotes:
		return "notes"
	case fieldVIP:
		return "isVip"
	case fieldSaved:
		return "isSaved"
	case fieldBlock:
		return "isBlocked"
	}
	return ""
}

func eqStrPtr(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func eqTimePtr(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

// fieldEqual reports whether a and b agree on f.
func fieldEqual(f contactField, a, b contactState) bool {
	switch f {
	case fieldDisplayName:
		return a.displayName == b.displayName
	case fieldUserName:
		return eqStrPtr(a.userName, b.userName)
	case fieldNotes:
		return a.notes == b.notes
	case fieldVIP:
		return a.vip == b.vip
	case fieldSaved:
		return eqTimePtr(a.savedAt, b.savedAt)
	case fieldBlock:
		return a.blocked == b.blocked && a.dest == b.dest && eqTimePtr(a.blockedAt, b.blockedAt)
	}
	return false
}

// copyField sets dst's f to src's.
func copyField(f contactField, dst *contactState, src contactState) {
	switch f {
	case fieldDisplayName:
		dst.displayName = src.displayName
	case fieldUserName:
		dst.userName = src.userName
	case fieldNotes:
		dst.notes = src.notes
	case fieldVIP:
		dst.vip = src.vip
	case fieldSaved:
		dst.savedAt = src.savedAt
	case fieldBlock:
		dst.blocked, dst.dest, dst.blockedAt = src.blocked, src.dest, src.blockedAt
	}
}

// sameState reports whether a and b agree on every field.
func sameState(a, b contactState) bool {
	for _, f := range contactFields {
		if !fieldEqual(f, a, b) {
			return false
		}
	}
	return true
}

// contactMergeRecord is one app.contact_merges row.
type contactMergeRecord struct {
	id, into, from ulid.ULID
	addresses      []ulid.ULID
	before, after  contactState
	// rebased: before/after were rewritten by the undo of an older merge on
	// the same target, and the row must be updated.
	rebased bool
}

// insertMergeRecord writes rec.
func (tx *UserTx) insertMergeRecord(ctx context.Context, rec contactMergeRecord) error {
	before, err := json.Marshal(snapshotOf(rec.before))
	if err != nil {
		return fmt.Errorf("store: encode merge record: %w", err)
	}
	after, err := json.Marshal(snapshotOf(rec.after))
	if err != nil {
		return fmt.Errorf("store: encode merge record: %w", err)
	}
	// addresses goes in as text[] and is cast to the domain array (pgx has no
	// codec for app.ulid[]; the cast still runs app.ulid's CHECK).
	if _, err := tx.tx.Exec(ctx, `
		INSERT INTO app.contact_merges (id, user_id, into_contact, from_contact, addresses, before_state, after_state)
		VALUES ($1, $2, $3, $4, $5::text[]::app.ulid[], $6::jsonb, $7::jsonb)`,
		rec.id.String(), tx.userID.String(), rec.into.String(), rec.from.String(),
		ulidStrings(rec.addresses), string(before), string(after)); err != nil {
		return fmt.Errorf("store: record merge: %w", err)
	}
	return nil
}

// updateMergeRecordStates rewrites a rebased record's two states.
func (tx *UserTx) updateMergeRecordStates(ctx context.Context, rec contactMergeRecord) error {
	before, err := json.Marshal(snapshotOf(rec.before))
	if err != nil {
		return fmt.Errorf("store: encode merge record: %w", err)
	}
	after, err := json.Marshal(snapshotOf(rec.after))
	if err != nil {
		return fmt.Errorf("store: encode merge record: %w", err)
	}
	if _, err := tx.tx.Exec(ctx, `
		UPDATE app.contact_merges SET before_state = $3::jsonb, after_state = $4::jsonb
		 WHERE user_id = $1 AND id = $2`,
		tx.userID.String(), rec.id.String(), string(before), string(after)); err != nil {
		return fmt.Errorf("store: rebase merge record: %w", err)
	}
	return nil
}

const mergeRecordColumns = `id, into_contact, from_contact, addresses::text[], before_state, after_state`

// mergeRecords reads the caller's merge records matching cond (a WHERE
// fragment over $2), oldest first, locked FOR UPDATE.
func (tx *UserTx) mergeRecords(ctx context.Context, cond string, arg any) ([]contactMergeRecord, error) {
	rows, err := tx.tx.Query(ctx, `
		SELECT `+mergeRecordColumns+`
		  FROM app.contact_merges
		 WHERE user_id = $1 AND `+cond+`
		 ORDER BY id
		 FOR UPDATE`,
		tx.userID.String(), arg)
	if err != nil {
		return nil, fmt.Errorf("store: read merge records: %w", err)
	}
	defer rows.Close()
	var out []contactMergeRecord
	for rows.Next() {
		var (
			id, into, from string
			addrs          []string
			before, after  []byte
		)
		if err := rows.Scan(&id, &into, &from, &addrs, &before, &after); err != nil {
			return nil, fmt.Errorf("store: scan merge record: %w", err)
		}
		var rec contactMergeRecord
		for _, p := range []struct {
			raw string
			dst *ulid.ULID
		}{{id, &rec.id}, {into, &rec.into}, {from, &rec.from}} {
			v, err := ulid.Parse(p.raw)
			if err != nil {
				return nil, fmt.Errorf("store: parse merge record id %q: %w", p.raw, err)
			}
			*p.dst = v
		}
		for _, a := range addrs {
			v, err := ulid.Parse(a)
			if err != nil {
				return nil, fmt.Errorf("store: parse merge record address %q: %w", a, err)
			}
			rec.addresses = append(rec.addresses, v)
		}
		var b, a contactSnapshot
		if err := json.Unmarshal(before, &b); err != nil {
			return nil, fmt.Errorf("store: decode merge record: %w", err)
		}
		if err := json.Unmarshal(after, &a); err != nil {
			return nil, fmt.Errorf("store: decode merge record: %w", err)
		}
		rec.before, rec.after = b.state(rec.into), a.state(rec.into)
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read merge records: %w", err)
	}
	return out, nil
}

// undoMerge undoes records[i] on state (the target's current state), given
// the newer records on the same target (records[i+1:]) and each newer
// record's `from` state. It returns the new state, the fields it kept because
// they were changed along the way (only those the undone merge had written),
// and whether the block field was restored (it matched the merge's chain, so
// the block now on the target is the merges' product, not the user's). Newer
// records are rebased in place.
func undoMerge(state contactState, records []contactMergeRecord, i int, froms []contactState) (contactState, []contactField, bool) {
	r := records[i]
	newer := records[i+1:]
	// chain[0] is the rebuilt state before the first newer merge; chain[j+1]
	// after it.
	chain := []contactState{r.before}
	s := r.before
	for j := range newer {
		// The combined notes can only shrink here, so ErrMergeNotesTooLong
		// cannot apply; the state is used either way.
		s, _ = combineContacts(s, froms[j])
		chain = append(chain, s)
	}
	out := state
	var kept []contactField
	blockRestored := false
	for _, f := range contactFields {
		// Consistent: nobody changed f between the merges or since the last.
		consistent := true
		prev := r.after
		for _, n := range newer {
			if !fieldEqual(f, n.before, prev) {
				consistent = false
				break
			}
			prev = n.after
		}
		if consistent && !fieldEqual(f, state, prev) {
			consistent = false
		}
		if !consistent {
			if !fieldEqual(f, r.before, r.after) {
				kept = append(kept, f)
			}
			continue
		}
		if f == fieldBlock {
			blockRestored = true
		}
		copyField(f, &out, s)
		for j := range newer {
			b, a := newer[j].before, newer[j].after
			copyField(f, &newer[j].before, chain[j])
			copyField(f, &newer[j].after, chain[j+1])
			if !sameState(b, newer[j].before) || !sameState(a, newer[j].after) {
				newer[j].rebased = true
			}
		}
	}
	return out, kept, blockRestored
}
