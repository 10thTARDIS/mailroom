// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/pgpkeys"
)

// MERGE AND SEPARATE (#816, ADR-041 §4).
//
// Merge moves every address and key of `from` into `into`, combines the user's
// state by fixed rules, and leaves `from` behind as a TOMBSTONE
// (contacts.merged_into = into): hidden from every read, reported destroyed,
// and still holding its own pre-merge state. Each moved row records its home
// (merged_from_contact, set only when NULL so a merge of a merge keeps the
// original home). Separate moves addresses back out into a NEW contact made
// from their home tombstone's state; a home's keys go with the last of its
// addresses. Each merge also writes a record of what it borrowed
// (app.contact_merges, #834), and a separate that takes back every address a
// merge moved undoes it (contact_merge_record.go).
//
// Pins (app.contact_key_pins) are keyed by address, so they need nothing:
// an address keeps its pin wherever it goes. Key events hang off the key row,
// so they follow their key.
//
// Each operation is ONE savepoint around every statement and its change rows
// (the delegate-review rule): a caller recovering from an error cannot commit
// half a merge.

// Errors for merge and separate.
var (
	ErrMergeSelf = errors.New("store: a contact cannot be merged with itself")
	// ErrMergeNotesTooLong: the combined notes would pass MaxContactNotes. The
	// merge is refused rather than cutting the user's text.
	ErrMergeNotesTooLong = fmt.Errorf("store: the two contacts' notes together would pass %d characters; shorten one first", MaxContactNotes)
	// ErrContactAddressNotFound: an address id that is not one of the contact's.
	ErrContactAddressNotFound = errors.New("store: that address is not one of the contact's")
	// ErrSeparateAll: separating every address would leave the contact empty.
	ErrSeparateAll = errors.New("store: a contact keeps at least one address; separate the others")
	// ErrSeparateNone: no addresses named.
	ErrSeparateNone = errors.New("store: name at least one address to separate")
)

// mergeNotesSeparator joins two contacts' notes on merge.
const mergeNotesSeparator = "\n\n---\n\n"

// contactState is one contact's own columns, as locked for a merge or a
// separate.
type contactState struct {
	id          ulid.ULID
	displayName string
	userName    *string
	notes       string
	vip         bool
	blocked     bool
	dest        string
	blockedAt   *time.Time
	savedAt     *time.Time
}

const contactStateColumns = `id, display_name, user_display_name, notes, vip, blocked, block_destination, blocked_at, saved_at`

func scanContactState(row interface{ Scan(...any) error }) (contactState, error) {
	var (
		c   contactState
		ids string
	)
	if err := row.Scan(&ids, &c.displayName, &c.userName, &c.notes, &c.vip, &c.blocked, &c.dest, &c.blockedAt, &c.savedAt); err != nil {
		return c, err
	}
	id, err := ulid.Parse(ids)
	if err != nil {
		return c, fmt.Errorf("store: parse contact id %q: %w", ids, err)
	}
	c.id = id
	return c, nil
}

// lockLiveContacts locks the named live (not merged-away) contacts of the
// caller FOR UPDATE, in id order so two merges of the same pair cannot
// deadlock. A missing id (absent, a tombstone, or another user's: RLS makes
// them one case) is ErrContactNotFound.
func (tx *UserTx) lockLiveContacts(ctx context.Context, ids ...ulid.ULID) (map[ulid.ULID]contactState, error) {
	rows, err := tx.tx.Query(ctx, `
		SELECT `+contactStateColumns+`
		  FROM app.contacts
		 WHERE user_id = $1 AND id = ANY($2) AND merged_into IS NULL
		 ORDER BY id
		 FOR UPDATE`,
		tx.userID.String(), ulidStrings(ids))
	if err != nil {
		return nil, fmt.Errorf("store: lock contacts: %w", err)
	}
	defer rows.Close()
	out := map[ulid.ULID]contactState{}
	for rows.Next() {
		c, err := scanContactState(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan contact: %w", err)
		}
		out[c.id] = c
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: lock contacts: %w", err)
	}
	for _, id := range ids {
		if _, ok := out[id]; !ok {
			return nil, ErrContactNotFound
		}
	}
	return out, nil
}

// combineBlock is the block rule shared by merge and separate: blocked if
// either is; the EARLIER blocked_at; the stricter destination (trash > junk)
// among the blocked sides. Neither blocked: a's destination preference stays.
func combineBlock(a, b contactState) (blocked bool, dest string, at *time.Time) {
	blocked = a.blocked || b.blocked
	if !blocked {
		return false, a.dest, nil
	}
	dest = BlockToJunk
	for _, s := range []contactState{a, b} {
		if !s.blocked {
			continue
		}
		if s.dest == BlockToTrash {
			dest = BlockToTrash
		}
		if s.blockedAt != nil && (at == nil || s.blockedAt.Before(*at)) {
			t := *s.blockedAt
			at = &t
		}
	}
	if at == nil { // unreachable while the blocked_at CHECK holds
		now := time.Now().UTC()
		at = &now
	}
	return true, dest, at
}

// combineContacts is the merged state of into and from (ADR-041 §4): saved if
// either (the earlier saved_at); VIP if either; the block rule above; notes
// joined with a separator (into's first); the user's name from into unless it
// has none; the learned name from into unless it is empty.
func combineContacts(into, from contactState) (contactState, error) {
	out := into
	switch {
	case into.savedAt == nil:
		out.savedAt = from.savedAt
	case from.savedAt != nil && from.savedAt.Before(*into.savedAt):
		out.savedAt = from.savedAt
	}
	out.vip = into.vip || from.vip
	out.blocked, out.dest, out.blockedAt = combineBlock(into, from)
	switch {
	case into.notes == "":
		out.notes = from.notes
	case from.notes != "":
		out.notes = into.notes + mergeNotesSeparator + from.notes
	}
	if len([]rune(out.notes)) > MaxContactNotes {
		return out, ErrMergeNotesTooLong
	}
	if into.userName == nil || *into.userName == "" {
		out.userName = from.userName
	}
	if into.displayName == "" {
		out.displayName = from.displayName
	}
	return out, nil
}

// writeContactState writes c's own columns over its row.
func (tx *UserTx) writeContactState(ctx context.Context, c contactState) error {
	if _, err := tx.tx.Exec(ctx, `
		UPDATE app.contacts
		   SET display_name = $3, user_display_name = $4, notes = $5, vip = $6,
		       blocked = $7, block_destination = $8, blocked_at = $9, saved_at = $10
		 WHERE user_id = $1 AND id = $2`,
		tx.userID.String(), c.id.String(), c.displayName, c.userName, c.notes, c.vip,
		c.blocked, c.dest, c.blockedAt, c.savedAt); err != nil {
		return fmt.Errorf("store: write contact: %w", err)
	}
	return nil
}

// ContactMergeResult reports what MergeContacts moved.
type ContactMergeResult struct {
	Addresses int
	// Keys moved to into; KeysCombined were keys into already held (same
	// fingerprint): from's copy was merged into into's row, its history moved
	// onto it, and its row deleted.
	Keys, KeysCombined int
}

// MergeContacts merges contact from into contact into (ADR-041 §4). Change
// rows: Contact `updated` for into and Contact `destroyed` for from, in the
// same savepoint as every write, with the merge record (into's state before
// and after, and the moved addresses) that a separate undoes it from.
func (tx *UserTx) MergeContacts(ctx context.Context, into, from ulid.ULID) (ContactMergeResult, error) {
	var res ContactMergeResult
	if into == from {
		return res, ErrMergeSelf
	}
	err := tx.inSavepoint(ctx, func() error {
		locked, err := tx.lockLiveContacts(ctx, into, from)
		if err != nil {
			return err
		}
		before := locked[into]
		combined, err := combineContacts(before, locked[from])
		if err != nil {
			return err
		}
		if err := tx.writeContactState(ctx, combined); err != nil {
			return err
		}

		moved, err := tx.moveAddressesForMerge(ctx, into, from)
		if err != nil {
			return err
		}
		res.Addresses = len(moved)

		combinedKeys, err := tx.combineCollidingKeys(ctx, into, from)
		if err != nil {
			return err
		}
		res.KeysCombined = combinedKeys
		tag, err := tx.tx.Exec(ctx, `
			UPDATE app.contact_keys
			   SET contact_id = $2, merged_from_contact = COALESCE(merged_from_contact, $3)
			 WHERE user_id = $1 AND contact_id = $3`,
			tx.userID.String(), into.String(), from.String())
		if err != nil {
			return fmt.Errorf("store: merge keys: %w", err)
		}
		res.Keys = int(tag.RowsAffected())

		// The tombstone, and every older tombstone that pointed at it, now
		// point at the live contact.
		if _, err := tx.tx.Exec(ctx, `
			UPDATE app.contacts SET merged_into = $2
			 WHERE user_id = $1 AND (id = $3 OR merged_into = $3)`,
			tx.userID.String(), into.String(), from.String()); err != nil {
			return fmt.Errorf("store: tombstone merged contact: %w", err)
		}
		// What this merge borrowed, so a separate can give it back (#834).
		if err := tx.insertMergeRecord(ctx, contactMergeRecord{id: NewULID(), into: into, from: from,
			addresses: moved, before: before, after: combined}); err != nil {
			return err
		}
		if _, err := tx.AppendChange(ctx, ObjectContact, into, OpUpdated); err != nil {
			return err
		}
		_, err = tx.AppendChange(ctx, ObjectContact, from, OpDestroyed)
		return err
	})
	if err != nil {
		return ContactMergeResult{}, err
	}
	return res, nil
}

// moveAddressesForMerge moves from's addresses into into, keeping each one's
// original home, and returns the moved address ids.
func (tx *UserTx) moveAddressesForMerge(ctx context.Context, into, from ulid.ULID) ([]ulid.ULID, error) {
	rows, err := tx.tx.Query(ctx, `
		UPDATE app.contact_addresses
		   SET contact_id = $2, merged_from_contact = COALESCE(merged_from_contact, $3)
		 WHERE user_id = $1 AND contact_id = $3
		RETURNING id`,
		tx.userID.String(), into.String(), from.String())
	if err != nil {
		return nil, fmt.Errorf("store: merge addresses: %w", err)
	}
	defer rows.Close()
	var moved []ulid.ULID
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("store: merge addresses: %w", err)
		}
		a, err := ulid.Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("store: parse address id %q: %w", raw, err)
		}
		moved = append(moved, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: merge addresses: %w", err)
	}
	return moved, nil
}

// combineCollidingKeys handles the keys from holds that into already holds
// (UNIQUE (user, contact, fingerprint)): the same person's two addresses
// carrying one key is the ordinary case. from's copy is merged into into's
// with pgpkeys.Merge (so a revocation or renewal only from's copy carried is
// kept), the stricter refused, the authenticated of either, removed only if
// both were; from's history moves onto into's row; from's row is deleted. It
// returns how many keys it combined. Such a key stays with into on a later
// separate (ADR-041 §4).
func (tx *UserTx) combineCollidingKeys(ctx context.Context, into, from ulid.ULID) (int, error) {
	rows, err := tx.tx.Query(ctx, `
		SELECT f.id, f.public_key, f.refused, f.authenticated, f.removed_at IS NOT NULL,
		       i.id, i.public_key, i.removed_at IS NOT NULL
		  FROM app.contact_keys f
		  JOIN app.contact_keys i ON i.user_id = f.user_id AND i.contact_id = $2 AND i.fingerprint = f.fingerprint
		 WHERE f.user_id = $1 AND f.contact_id = $3
		 FOR UPDATE`,
		tx.userID.String(), into.String(), from.String())
	if err != nil {
		return 0, fmt.Errorf("store: read colliding keys: %w", err)
	}
	type pair struct {
		fromID, intoID         string
		fromPub, intoPub       []byte
		refused, authenticated bool
		fromRemoved            bool
		intoRemoved            bool
	}
	var pairs []pair
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.fromID, &p.fromPub, &p.refused, &p.authenticated, &p.fromRemoved,
			&p.intoID, &p.intoPub, &p.intoRemoved); err != nil {
			rows.Close()
			return 0, fmt.Errorf("store: scan colliding key: %w", err)
		}
		pairs = append(pairs, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("store: read colliding keys: %w", err)
	}
	for _, p := range pairs {
		pub := p.intoPub
		if merged, differs, merr := pgpkeys.Merge(p.intoPub, p.fromPub); merr == nil && differs && len(merged) <= maxPublicKey {
			pub = merged
		}
		if _, err := tx.tx.Exec(ctx, `
			UPDATE app.contact_keys
			   SET public_key = $3, refused = refused OR $4, authenticated = authenticated OR $5,
			       removed_at = CASE WHEN $6 THEN removed_at END
			 WHERE user_id = $1 AND id = $2`,
			tx.userID.String(), p.intoID, pub, p.refused, p.authenticated, p.fromRemoved && p.intoRemoved); err != nil {
			return 0, fmt.Errorf("store: combine key: %w", err)
		}
		if _, err := tx.tx.Exec(ctx, `
			UPDATE app.contact_key_events SET contact_key_id = $3
			 WHERE user_id = $1 AND contact_key_id = $2`,
			tx.userID.String(), p.fromID, p.intoID); err != nil {
			return 0, fmt.Errorf("store: move key history: %w", err)
		}
		if _, err := tx.tx.Exec(ctx, `
			DELETE FROM app.contact_keys WHERE user_id = $1 AND id = $2`,
			tx.userID.String(), p.fromID); err != nil {
			return 0, fmt.Errorf("store: drop combined key: %w", err)
		}
	}
	return len(pairs), nil
}

// SeparateResult reports where Contact/separate put each address.
type SeparateResult struct {
	// Created lists the new contacts, in creation order.
	Created []ulid.ULID
	// AddressContact maps each separated address id to its new contact.
	AddressContact map[ulid.ULID]ulid.ULID
	// Keys is how many keys moved with their home's last address.
	Keys int
	// Restored: the separate took back every address of each merge the
	// separated addresses came in by, so those merges were undone on this
	// contact (#834). False for a partial separate, for addresses whose home
	// is this contact, and for a merge made before merges were recorded:
	// nothing of this contact's own state was restored then.
	Restored bool
	// KeptFields are this contact's fields an undone merge had written that
	// were changed after it (by the user, or by mail filling the learned
	// name), so they were kept rather than restored. Property names, in a
	// fixed order; never nil.
	KeptFields []string
}

// SeparateContact moves the named addresses of contact id out into new
// contacts (ADR-041 §4): one new contact per home. Addresses merged in from
// home H (a tombstone) go to a new contact made from H's own state — its
// names, notes, VIP, saved and block. Addresses whose home is this contact go
// to one new contact that copies this contact's learned name, saved, VIP and
// block, without its notes or the user's name. H's keys go with them when no
// address from H is left behind.
//
// UNDOING THE MERGE (#834). When the separated addresses include every
// address a recorded merge moved (that is still here), that merge is undone
// on the contact it merged into (contact_merge_record.go): each field it
// wrote goes back to its pre-merge value unless it was changed since, which
// is kept and reported. A partial separate undoes nothing.
//
// THE BLOCK goes back with the address that had it. A new contact takes the
// home's own block; it ALSO takes the block of the contact it leaves (the
// block rule, combineBlock) unless the merge that brought its addresses here
// was undone with the block restored, i.e. the block on this contact was
// exactly what the merges made of the contacts' own blocks. So a block the
// user set after the merge, or one left by a partial separate, carries over:
// separating never unblocks what the user blocked.
//
// Change rows: Contact `created` for each new contact, `updated` for this
// one, and `updated` for another live contact an undone merge restored, when
// it changed (a nested merge whose middle contact an earlier separate
// recreated), in one savepoint with every write. A tombstone's restored state
// writes no row: it was reported destroyed and is invisible.
func (tx *UserTx) SeparateContact(ctx context.Context, id ulid.ULID, addressIDs []ulid.ULID) (SeparateResult, error) {
	res := SeparateResult{AddressContact: map[ulid.ULID]ulid.ULID{}, KeptFields: []string{}}
	if len(addressIDs) == 0 {
		return res, ErrSeparateNone
	}
	err := tx.inSavepoint(ctx, func() error {
		locked, err := tx.lockLiveContacts(ctx, id)
		if err != nil {
			return err
		}
		cur := locked[id]

		// The contact's addresses and their homes.
		rows, err := tx.tx.Query(ctx, `
			SELECT id, merged_from_contact FROM app.contact_addresses
			 WHERE user_id = $1 AND contact_id = $2
			 FOR UPDATE`,
			tx.userID.String(), id.String())
		if err != nil {
			return fmt.Errorf("store: read contact addresses: %w", err)
		}
		home := map[ulid.ULID]ulid.ULID{} // address id -> home (zero = this contact)
		for rows.Next() {
			var aid string
			var h *string
			if err := rows.Scan(&aid, &h); err != nil {
				rows.Close()
				return fmt.Errorf("store: scan contact address: %w", err)
			}
			a, err := ulid.Parse(aid)
			if err != nil {
				rows.Close()
				return fmt.Errorf("store: parse address id %q: %w", aid, err)
			}
			var hid ulid.ULID
			if h != nil {
				if hid, err = ulid.Parse(*h); err != nil {
					rows.Close()
					return fmt.Errorf("store: parse home id %q: %w", *h, err)
				}
			}
			home[a] = hid
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("store: read contact addresses: %w", err)
		}

		wanted := map[ulid.ULID]bool{}
		for _, a := range addressIDs {
			if _, ok := home[a]; !ok {
				return ErrContactAddressNotFound
			}
			wanted[a] = true
		}
		if len(wanted) >= len(home) {
			return ErrSeparateAll
		}

		// Group by home, in the order the caller named them.
		var homes []ulid.ULID
		groups := map[ulid.ULID][]ulid.ULID{}
		for _, a := range addressIDs {
			h := home[a]
			if _, seen := groups[h]; !seen {
				homes = append(homes, h)
			}
			if !containsULID(groups[h], a) {
				groups[h] = append(groups[h], a)
			}
		}

		// Undo the merges these addresses complete, and write what changed.
		u := &mergeUndo{tx: tx, x: id, cur: cur, self: cur, home: home, wanted: wanted}
		if err := u.run(ctx); err != nil {
			return err
		}
		if !sameState(u.self, cur) {
			if err := tx.writeContactState(ctx, u.self); err != nil {
				return err
			}
		}
		res.Restored, res.KeptFields = u.restored, u.keptProperties()

		for _, h := range homes {
			newState, err := u.separatedState(ctx, h, groups[h])
			if err != nil {
				return err
			}
			newState.id = NewULID()
			if _, err := tx.tx.Exec(ctx, `
				INSERT INTO app.contacts (id, user_id, display_name, user_display_name, notes, vip,
				                          blocked, block_destination, blocked_at, saved_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
				newState.id.String(), tx.userID.String(), newState.displayName, newState.userName,
				newState.notes, newState.vip, newState.blocked, newState.dest, newState.blockedAt,
				newState.savedAt); err != nil {
				return fmt.Errorf("store: create separated contact: %w", err)
			}
			if _, err := tx.tx.Exec(ctx, `
				UPDATE app.contact_addresses SET contact_id = $3, merged_from_contact = NULL
				 WHERE user_id = $1 AND contact_id = $2 AND id = ANY($4)`,
				tx.userID.String(), id.String(), newState.id.String(), ulidStrings(groups[h])); err != nil {
				return fmt.Errorf("store: move separated addresses: %w", err)
			}
			// A merged-in home's keys go with the last of its addresses, and
			// the merges INTO that home follow it to its new contact.
			if h != (ulid.ULID{}) {
				left := 0
				for a, ah := range home {
					if ah == h && !wanted[a] {
						left++
					}
				}
				if left == 0 {
					tag, err := tx.tx.Exec(ctx, `
						UPDATE app.contact_keys SET contact_id = $3, merged_from_contact = NULL
						 WHERE user_id = $1 AND contact_id = $2 AND merged_from_contact = $4`,
						tx.userID.String(), id.String(), newState.id.String(), h.String())
					if err != nil {
						return fmt.Errorf("store: move separated keys: %w", err)
					}
					res.Keys += int(tag.RowsAffected())
					if _, err := tx.tx.Exec(ctx, `
						UPDATE app.contact_merges SET into_contact = $3
						 WHERE user_id = $1 AND into_contact = $2`,
						tx.userID.String(), h.String(), newState.id.String()); err != nil {
						return fmt.Errorf("store: repoint merge records: %w", err)
					}
				}
			}
			res.Created = append(res.Created, newState.id)
			for _, a := range groups[h] {
				res.AddressContact[a] = newState.id
			}
		}
		var updated []ulid.ULID
		for _, t := range u.order {
			if tg := u.targets[t]; tg.live && !sameState(tg.pre, tg.state) {
				updated = append(updated, t)
			}
		}
		if err := tx.AppendChanges(ctx, ObjectContact, res.Created, OpCreated); err != nil {
			return err
		}
		if len(updated) > 0 {
			if err := tx.AppendChanges(ctx, ObjectContact, updated, OpUpdated); err != nil {
				return err
			}
		}
		_, err = tx.AppendChange(ctx, ObjectContact, id, OpUpdated)
		return err
	})
	if err != nil {
		return SeparateResult{}, err
	}
	return res, nil
}

// mergeUndo is one separate's undo of the merges its addresses complete.
type mergeUndo struct {
	tx     *UserTx
	x      ulid.ULID               // the contact separated from
	cur    contactState            // x before the separate
	self   contactState            // x after the undo
	home   map[ulid.ULID]ulid.ULID // x's addresses -> home
	wanted map[ulid.ULID]bool      // the separated addresses

	touched  []contactMergeRecord // records that moved any separated address
	complete map[ulid.ULID]bool   // touched records none of whose addresses stay in x
	undone   map[ulid.ULID]bool
	blockOK  map[ulid.ULID]bool // undone with the block restored
	order    []ulid.ULID        // targets other than x, in the order found
	targets  map[ulid.ULID]*mergeTarget
	kept     map[contactField]bool // on x
	restored bool
	states   map[ulid.ULID]contactState // other contacts' states, read once
}

// mergeTarget is a contact other than x that an undone merge restores: a
// tombstone (a nested merge), or a live contact an earlier separate made from
// one.
type mergeTarget struct {
	live       bool
	pre, state contactState
}

func (u *mergeUndo) run(ctx context.Context) error {
	tx := u.tx
	u.complete, u.undone, u.blockOK = map[ulid.ULID]bool{}, map[ulid.ULID]bool{}, map[ulid.ULID]bool{}
	u.targets, u.kept, u.states = map[ulid.ULID]*mergeTarget{}, map[contactField]bool{}, map[ulid.ULID]contactState{}
	sep := make([]string, 0, len(u.wanted))
	for a := range u.wanted {
		sep = append(sep, a.String())
	}
	touched, err := tx.mergeRecords(ctx, `addresses::text[] && $2::text[]`, sep)
	if err != nil {
		return err
	}
	u.touched = touched
	onX := false
	for _, r := range touched {
		done := true
		for _, a := range r.addresses {
			if _, in := u.home[a]; in && !u.wanted[a] {
				done = false
				break
			}
		}
		u.complete[r.id] = done
		if done && r.into == u.x {
			onX = true
		}
		if done && r.into != u.x && u.targets[r.into] == nil {
			st, mergedInto, found, err := tx.readContactStateForUpdate(ctx, r.into)
			if err != nil {
				return err
			}
			if !found {
				continue
			}
			u.targets[r.into] = &mergeTarget{live: mergedInto == nil, pre: st, state: st}
			u.order = append(u.order, r.into)
		}
	}
	// Other targets first (a nested merge's tombstone is the `from` of the
	// newer merge into x), then x.
	for _, t := range u.order {
		tg := u.targets[t]
		if _, err := u.undoOn(ctx, t, &tg.state); err != nil {
			return err
		}
		if !sameState(tg.pre, tg.state) {
			if err := tx.writeContactState(ctx, tg.state); err != nil {
				return err
			}
		}
	}
	if onX {
		kept, err := u.undoOn(ctx, u.x, &u.self)
		if err != nil {
			return err
		}
		for _, f := range kept {
			u.kept[f] = true
		}
	}

	// Restored: every merge into x that the separated addresses came by was
	// undone, and every separated merged-in address came by one.
	allOnX, anyOnX := true, false
	for _, r := range touched {
		if r.into != u.x {
			continue
		}
		if u.undone[r.id] {
			anyOnX = true
		} else {
			allOnX = false
		}
	}
	covered := true
	for a := range u.wanted {
		if u.home[a] == (ulid.ULID{}) {
			continue
		}
		found := false
		for _, r := range touched {
			if r.into == u.x && containsULID(r.addresses, a) {
				found = true
				break
			}
		}
		if !found {
			covered = false
			break
		}
	}
	u.restored = anyOnX && allOnX && covered
	return nil
}

// undoOn undoes, newest first, each complete record whose target is t, on
// *state; undone records are deleted and rebased newer ones rewritten. It
// returns the fields kept.
func (u *mergeUndo) undoOn(ctx context.Context, t ulid.ULID, state *contactState) ([]contactField, error) {
	tx := u.tx
	recs, err := tx.mergeRecords(ctx, `into_contact = $2`, t.String())
	if err != nil {
		return nil, err
	}
	var kept []contactField
	for i := len(recs) - 1; i >= 0; i-- {
		if !u.complete[recs[i].id] {
			continue
		}
		froms := make([]contactState, 0, len(recs)-i-1)
		for _, n := range recs[i+1:] {
			fs, err := u.stateOf(ctx, n.from)
			if err != nil {
				return nil, err
			}
			froms = append(froms, fs)
		}
		var k []contactField
		var blockOK bool
		*state, k, blockOK = undoMerge(*state, recs, i, froms)
		kept = append(kept, k...)
		u.undone[recs[i].id], u.blockOK[recs[i].id] = true, blockOK
		if _, err := tx.tx.Exec(ctx, `DELETE FROM app.contact_merges WHERE user_id = $1 AND id = $2`,
			tx.userID.String(), recs[i].id.String()); err != nil {
			return nil, fmt.Errorf("store: delete merge record: %w", err)
		}
		recs = append(recs[:i], recs[i+1:]...)
	}
	for _, r := range recs {
		if r.rebased {
			if err := tx.updateMergeRecordStates(ctx, r); err != nil {
				return nil, err
			}
		}
	}
	return kept, nil
}

// stateOf is contact c's current state in this separate: x's, a target's,
// or the row's (read once).
func (u *mergeUndo) stateOf(ctx context.Context, c ulid.ULID) (contactState, error) {
	if c == u.x {
		return u.self, nil
	}
	if tg := u.targets[c]; tg != nil {
		return tg.state, nil
	}
	if st, ok := u.states[c]; ok {
		return st, nil
	}
	st, _, found, err := u.tx.readContactStateForUpdate(ctx, c)
	if err != nil {
		return contactState{}, err
	}
	if !found {
		st = contactState{id: c, dest: BlockToJunk}
	}
	u.states[c] = st
	return st, nil
}

// preState is contact c's state before this separate.
func (u *mergeUndo) preState(ctx context.Context, c ulid.ULID) (contactState, error) {
	if c == u.x {
		return u.cur, nil
	}
	if tg := u.targets[c]; tg != nil {
		return tg.pre, nil
	}
	return u.stateOf(ctx, c)
}

func (u *mergeUndo) keptProperties() []string {
	out := []string{}
	for _, f := range contactFields {
		if u.kept[f] {
			out = append(out, f.Property())
		}
	}
	return out
}

// separatedState is the state a new contact made by separate starts from.
// Home zero (the addresses' home is x itself): a copy of x as the undo left
// it, without its notes or user name. A merged-in home: the home tombstone's
// own state (after any undo of merges into it), with the block of every
// contact the addresses were merged through whose block is not the merges'
// own product (see SeparateContact).
func (u *mergeUndo) separatedState(ctx context.Context, h ulid.ULID, addrs []ulid.ULID) (contactState, error) {
	self := u.self
	base := contactState{displayName: self.displayName, vip: self.vip, dest: self.dest, savedAt: self.savedAt,
		blocked: self.blocked, blockedAt: self.blockedAt}
	if h == (ulid.ULID{}) {
		return base, nil
	}
	row := u.tx.tx.QueryRow(ctx, `
		SELECT `+contactStateColumns+`
		  FROM app.contacts
		 WHERE user_id = $1 AND id = $2`,
		u.tx.userID.String(), h.String())
	t, err := scanContactState(row)
	switch {
	case err == nil:
		base = t
	case isNoRows(err):
		// The home is gone (cannot happen while tombstones are kept; the FK
		// nulls the pointer first): fall back to the copy of x, and to the
		// block rule against x below.
	default:
		return contactState{}, fmt.Errorf("store: read home contact: %w", err)
	}
	seen := map[ulid.ULID]bool{}
	for _, a := range addrs {
		covered := false
		for _, r := range u.touched {
			if !containsULID(r.addresses, a) {
				continue
			}
			if r.into == u.x {
				covered = true
			}
			if (u.undone[r.id] && u.blockOK[r.id]) || seen[r.into] {
				continue
			}
			seen[r.into] = true
			src, err := u.preState(ctx, r.into)
			if err != nil {
				return contactState{}, err
			}
			base.blocked, base.dest, base.blockedAt = combineBlock(base, src)
		}
		if !covered && !seen[u.x] {
			// No record of how it came here (a merge made before #834): the
			// block rule against x, as before.
			seen[u.x] = true
			base.blocked, base.dest, base.blockedAt = combineBlock(base, u.cur)
		}
	}
	return base, nil
}

// readContactStateForUpdate reads and locks contact c (live or a tombstone),
// with its merged_into.
func (tx *UserTx) readContactStateForUpdate(ctx context.Context, c ulid.ULID) (contactState, *string, bool, error) {
	var (
		st         contactState
		ids        string
		mergedInto *string
	)
	err := tx.tx.QueryRow(ctx, `
		SELECT `+contactStateColumns+`, merged_into
		  FROM app.contacts
		 WHERE user_id = $1 AND id = $2
		 FOR UPDATE`,
		tx.userID.String(), c.String()).Scan(&ids, &st.displayName, &st.userName, &st.notes, &st.vip,
		&st.blocked, &st.dest, &st.blockedAt, &st.savedAt, &mergedInto)
	if isNoRows(err) {
		return contactState{}, nil, false, nil
	}
	if err != nil {
		return contactState{}, nil, false, fmt.Errorf("store: read contact: %w", err)
	}
	st.id = c
	return st, mergedInto, true, nil
}

func containsULID(xs []ulid.ULID, x ulid.ULID) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
