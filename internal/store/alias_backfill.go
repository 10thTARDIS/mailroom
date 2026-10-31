// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"fmt"

	"github.com/oklog/ulid/v2"
)

// This file holds the store surface the alias/contacts repair commands need
// (mailroom#177): re-deriving `addressed_address` over existing mail once a
// catchall identity exists, and undoing the two things whose absence made the
// alias story inert.
//
// WHY A BACKFILL EXISTS AT ALL. The alias a message was delivered to is resolved
// AT INGEST against the identities that existed THEN (ingest.deriveAddressedIdentity
// -> MatchVerifiedAddress). A user who runs a catchall but has no domain_wildcard
// identity therefore has every message resolve to the one fixed address they do
// have, and nothing re-resolves it later. Provisioning the wildcard fixes mail
// arriving from then on; only a backfill fixes the mail already stored.
//
// CHANGE-LOG DISCIPLINE. `addressed_address`/`addressed_identity_id` ARE
// JMAP-visible (Email.deliveredTo / Email.identityId, urn:mailroom:compose), and
// so are contacts. So every actual change here appends exactly one change_log row
// in the SAME UserTx as the mutation — the load-bearing invariant (CLAUDE.md).
// Every write is guarded by IS DISTINCT FROM, so a re-run of a backfill that has
// already converged writes nothing and appends nothing: these commands are safe
// to run repeatedly, which is the only way an operator can trust running them at
// all.

// MessageAliasRef is one message's identity for the alias backfill scan: its id,
// the content_hash keying its raw blob (the alias lives in headers, so the blob
// must be opened), and the alias state currently stored. Carrying the current
// state lets a dry run report exactly what WOULD change without a second
// decryption pass or any write.
type MessageAliasRef struct {
	ID                  ulid.ULID
	ContentHash         []byte
	AddressedAddress    string
	AddressedIdentityID ulid.ULID // zero when no identity resolved
}

// MessageAliasRefsAfter walks the caller's messages in ascending id order,
// returning each message's blob reference and current alias state. `after` is an
// exclusive low bound (pass the zero ULID to start); ULIDs sort by creation time,
// so this is a keyset walk over the primary key with no offset cost and no
// long-lived snapshot. RLS scopes the scan to the caller.
func (tx *UserTx) MessageAliasRefsAfter(ctx context.Context, after ulid.ULID, limit int) ([]MessageAliasRef, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := tx.tx.Query(ctx, `
		SELECT id, content_hash, COALESCE(addressed_address, ''),
		       COALESCE(addressed_identity_id, '')
		  FROM app.messages
		 WHERE user_id = $1 AND id > $2
		 ORDER BY id
		 LIMIT $3`,
		tx.userID.String(), after.String(), limit)
	if err != nil {
		return nil, fmt.Errorf("store: walk message alias refs: %w", err)
	}
	defer rows.Close()
	var out []MessageAliasRef
	for rows.Next() {
		var (
			id, ident string
			ref       MessageAliasRef
			hash      []byte
		)
		if err := rows.Scan(&id, &hash, &ref.AddressedAddress, &ident); err != nil {
			return nil, fmt.Errorf("store: scan message alias ref: %w", err)
		}
		if ref.ID, err = ulid.Parse(id); err != nil {
			return nil, fmt.Errorf("store: parse message id %q: %w", id, err)
		}
		if ident != "" {
			if ref.AddressedIdentityID, err = ulid.Parse(ident); err != nil {
				return nil, fmt.Errorf("store: parse addressed identity id %q: %w", ident, err)
			}
		}
		ref.ContentHash = hash
		out = append(out, ref)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: walk message alias refs: %w", err)
	}
	return out, nil
}

// UpdateAddressedAlias re-points a message at the alias it was actually
// delivered to, and the identity that alias resolves to (pass a zero identityID
// for "resolved to nothing" — the honest state for an alias at a domain this
// user is not verified for, which ingest also records).
//
// It is a guarded UPDATE: the IS DISTINCT FROM pair writes no row when the alias
// state is already what we would set, and the same no-row outcome covers a
// message that does not exist for the caller (RLS). Both are true no-ops that
// append NOTHING and return changed=false. When something does move, exactly one
// Email `updated` change_log row is appended, so a client re-syncs the new
// deliveredTo/identityId.
//
// There is no writeback intent: the alias is a local derivation over headers we
// already hold, not a remote-mirrored property.
func (tx *UserTx) UpdateAddressedAlias(ctx context.Context, msgID ulid.ULID, address string, identityID ulid.ULID) (changed bool, err error) {
	addr := NormalizeAddress(address)
	// $4 is passed as PLAIN TEXT ("" for "no identity") and cast to app.ulid
	// exactly once, in the SET. Handing pgx a param that is a bare app.ulid in one
	// position and compared as text in another is what produced 42P08 in
	// InsertModelVersion; keeping both uses textual removes the ambiguity instead
	// of papering over it with more casts.
	ident := ""
	if identityID != (ulid.ULID{}) {
		ident = identityID.String()
	}
	var touched string
	err = tx.tx.QueryRow(ctx, `
		UPDATE app.messages
		   SET addressed_address = NULLIF($3, ''),
		       addressed_identity_id = NULLIF($4, '')::app.ulid
		 WHERE user_id = $1 AND id = $2
		   AND (COALESCE(addressed_address::text, ''), COALESCE(addressed_identity_id::text, ''))
		       IS DISTINCT FROM ($3, $4)
		RETURNING id`,
		tx.userID.String(), msgID.String(), addr, ident).Scan(&touched)
	if err != nil {
		if isNoRows(err) {
			return false, nil
		}
		return false, fmt.Errorf("store: update addressed alias: %w", err)
	}
	if _, err := tx.AppendChange(ctx, ObjectEmail, msgID, OpUpdated); err != nil {
		return false, err
	}
	return true, nil
}

// OwnAddressContact is one contact_addresses row that turned out to be one of
// the USER's own addresses rather than a correspondent's — the wreckage a
// catchall user accumulates before their wildcard identity exists (mailroom#177).
// SoleAddress reports whether it is the only address on its contact, which
// decides whether removing it also removes an now-empty contact.
type OwnAddressContact struct {
	AddressID   ulid.ULID
	ContactID   ulid.ULID
	Address     string
	SeenCount   int
	SoleAddress bool
}

// OwnAddressContacts lists the caller's contact addresses that are actually
// their OWN — every address a currently-enabled identity authorizes them to send
// as. "Me" is defined here EXACTLY as upsertContactAddress defines it and as the
// send path defines it (a fixed identity's address, or any address under a
// domain_wildcard), so this finds precisely the rows that today's ingest would
// no longer create.
//
// It is read-only: an operator sees the list before anything is deleted.
func (tx *UserTx) OwnAddressContacts(ctx context.Context) ([]OwnAddressContact, error) {
	rows, err := tx.tx.Query(ctx, `
		SELECT ca.id, ca.contact_id, ca.address::text, ca.seen_count,
		       (SELECT count(*) FROM app.contact_addresses sib
		         WHERE sib.user_id = ca.user_id AND sib.contact_id = ca.contact_id) = 1
		  FROM app.contact_addresses ca
		 WHERE ca.user_id = $1
		   AND EXISTS (
		       SELECT 1 FROM app.identities i
		        WHERE i.user_id = ca.user_id
		          AND i.enabled = true
		          AND ( (i.kind = 'fixed'           AND i.address = ca.address)
		             OR (i.kind = 'domain_wildcard' AND i.domain  = split_part(ca.address::text, '@', 2)) ))
		 ORDER BY ca.seen_count DESC, ca.address`,
		tx.userID.String())
	if err != nil {
		return nil, fmt.Errorf("store: list own-address contacts: %w", err)
	}
	defer rows.Close()
	var out []OwnAddressContact
	for rows.Next() {
		var (
			c            OwnAddressContact
			addrID, cnID string
		)
		if err := rows.Scan(&addrID, &cnID, &c.Address, &c.SeenCount, &c.SoleAddress); err != nil {
			return nil, fmt.Errorf("store: scan own-address contact: %w", err)
		}
		if c.AddressID, err = ulid.Parse(addrID); err != nil {
			return nil, fmt.Errorf("store: parse contact address id %q: %w", addrID, err)
		}
		if c.ContactID, err = ulid.Parse(cnID); err != nil {
			return nil, fmt.Errorf("store: parse contact id %q: %w", cnID, err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list own-address contacts: %w", err)
	}
	return out, nil
}

// DeleteOwnAddressContact removes one wrongly-derived own-address row. When it
// was the contact's only address the now-empty contact goes too, and the change
// is reported as a Contact `destroyed`; otherwise the contact survives with one
// fewer address and the change is a Contact `updated`. Either way exactly one
// Contact change_log row is appended in the same tx as the delete (the
// invariant), because a client holding the contact must learn it changed.
//
// A row that no longer exists for the caller is a no-op appending nothing, so
// the repair is re-runnable.
func (tx *UserTx) DeleteOwnAddressContact(ctx context.Context, addressID ulid.ULID) (removedContact bool, err error) {
	var contactID string
	err = tx.tx.QueryRow(ctx, `
		DELETE FROM app.contact_addresses
		 WHERE user_id = $1 AND id = $2
		RETURNING contact_id`,
		tx.userID.String(), addressID.String()).Scan(&contactID)
	if err != nil {
		if isNoRows(err) {
			return false, nil
		}
		return false, fmt.Errorf("store: delete own-address contact: %w", err)
	}
	cid, err := ulid.Parse(contactID)
	if err != nil {
		return false, fmt.Errorf("store: parse contact id %q: %w", contactID, err)
	}

	var remaining int
	if err := tx.tx.QueryRow(ctx, `
		SELECT count(*) FROM app.contact_addresses
		 WHERE user_id = $1 AND contact_id = $2`,
		tx.userID.String(), cid.String()).Scan(&remaining); err != nil {
		return false, fmt.Errorf("store: count remaining contact addresses: %w", err)
	}
	if remaining > 0 {
		if _, err := tx.AppendChange(ctx, ObjectContact, cid, OpUpdated); err != nil {
			return false, err
		}
		return false, nil
	}
	if _, err := tx.tx.Exec(ctx, `
		DELETE FROM app.contacts WHERE user_id = $1 AND id = $2`,
		tx.userID.String(), cid.String()); err != nil {
		return false, fmt.Errorf("store: delete empty contact: %w", err)
	}
	if _, err := tx.AppendChange(ctx, ObjectContact, cid, OpDestroyed); err != nil {
		return false, err
	}
	return true, nil
}

// SetOutboundCount SETS (does not increment) how many times the user has written
// to an address. The distinction is the whole point: the live path in
// upsertContactAddress increments as mail is ingested, which cannot be used to
// repair history without double-counting every re-run. Recomputing the total
// from the stored mail and assigning it is idempotent, so the repair can be run
// as often as the operator likes and converges to the same numbers.
//
// outbound_count is NOT JMAP-visible (it is a scoring input, not a Contact
// property), so no change_log row is owed — and deliberately so: re-deriving a
// hidden signal must not fabricate sync traffic for every client.
func (tx *UserTx) SetOutboundCount(ctx context.Context, address string, count int) (changed bool, err error) {
	addr := NormalizeAddress(address)
	if addr == "" {
		return false, nil
	}
	var touched string
	err = tx.tx.QueryRow(ctx, `
		UPDATE app.contact_addresses
		   SET outbound_count = $3
		 WHERE user_id = $1 AND address = $2
		   AND outbound_count IS DISTINCT FROM $3
		RETURNING id`,
		tx.userID.String(), addr, count).Scan(&touched)
	if err != nil {
		if isNoRows(err) {
			return false, nil
		}
		return false, fmt.Errorf("store: set outbound count: %w", err)
	}
	return true, nil
}
