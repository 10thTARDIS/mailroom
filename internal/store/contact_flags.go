// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"errors"
	"fmt"
)

// VIP AND BLOCK — THE USER'S OWN MARKERS ON A CONTACT (#59, spec §10a).
//
// app.contacts has carried `vip` and `blocked` since the baseline with nothing
// reading either. They now do three things:
//
//   - BLOCKED: new mail whose From is one of a blocked contact's addresses is
//     moved to Junk (the default) or Trash at ingest, BEFORE rules and learned
//     placement (ingest.blockedArrival, MoveBlockedArrival below). Mail that is
//     already stored is moved only when the user asks, by the client, through
//     the ordinary Email/set move — never here.
//   - VIP: `is:vip` in search (search_predicate.go) and the VIP push modes and
//     the quiet-hours exception (internal/notify, PushCounts).
//
// Both are SYNCED Contact state (ADR-014's list grows by these four columns):
// a change appends exactly one Contact change_log row, a no-op appends none.
//
// MATCHING IS BY EXACT ADDRESS, and the From header can be forged. That is
// harmless HERE, and the reason is the direction of the harm: a block only
// ever moves mail OUT of sight, into Junk or Trash. Someone forging a blocked
// address on their mail gets their own mail junked; someone forging an
// UNblocked address to evade a block is in exactly the position they were in
// before blocks existed (and the provider's own spam filter, plus the sender
// warning, still apply). The worst case a spoofed From can cause is a
// legitimate message going to Junk — recoverable with "Not junk". VIP is the
// one marker a forger would want, and it only ever ADDS a notification; it
// never moves, hides or trusts anything.

// ErrContactNotFound is returned for a contact id that does not exist for the
// caller (another user's id included — RLS makes the two indistinguishable).
var ErrContactNotFound = errors.New("store: contact not found")

// ErrBlockDestinationInvalid is returned for a destination other than the two
// the schema accepts, before any statement runs.
var ErrBlockDestinationInvalid = errors.New("store: block destination must be junk or trash")

// Block destinations (app.contacts.block_destination).
const (
	BlockToJunk  = "junk"
	BlockToTrash = "trash"
)

// BlockedDestination reports whether address belongs to a blocked contact and,
// if so, where its new mail goes ("junk" or "trash"). address is normalised the
// way contacts store it; an empty or malformed one is simply not blocked. One
// probe of contact_addresses' UNIQUE (user_id, address) plus the contacts
// primary key — cheap enough to run on every ingested message.
//
// ⚠ UNDER ITS OWN SAVEPOINT, because its caller is the ingest transaction
// and promises to be fail-soft: on PostgreSQL a failed statement aborts the
// whole transaction (every later statement fails with 25P02), so a lookup
// error without a savepoint would fail the ingest of the message rather than
// treat it as not blocked. Rolled back to the savepoint, the error is
// returned and the transaction stays usable.
func (tx *UserTx) BlockedDestination(ctx context.Context, address string) (dest string, blocked bool, err error) {
	addr := NormalizeAddress(address)
	if addr == "" {
		return "", false, nil
	}
	err = tx.inSavepoint(ctx, func() error {
		e := tx.tx.QueryRow(ctx, `
			SELECT c.block_destination
			  FROM app.contact_addresses ca
			  JOIN app.contacts c ON c.user_id = ca.user_id AND c.id = ca.contact_id
			 WHERE ca.user_id = $1 AND ca.address = $2 AND c.blocked`,
			tx.userID.String(), addr).Scan(&dest)
		switch {
		case e == nil:
			blocked = true
			return nil
		case isNoRows(e):
			return nil
		}
		return fmt.Errorf("store: blocked destination: %w", e)
	})
	if err != nil {
		return "", false, err
	}
	if !blocked {
		return "", false, nil
	}
	return dest, true, nil
}

// vipAddressesSQL is the set of the caller's VIP addresses, as a subquery
// bound to $1 (the caller's own user id — the emailQueryWhere convention).
// The ONE definition `is:vip` and the push counter share, so a search for VIP
// mail and a VIP push can never disagree about who is a VIP. Non-correlated:
// Postgres builds it once as a hashed SubPlan (contacts_user_vip finds the few
// VIP contacts; contact_addresses_by_contact their addresses).
const vipAddressesSQL = `(SELECT vca.address FROM app.contacts vc
	JOIN app.contact_addresses vca ON vca.user_id = vc.user_id AND vca.contact_id = vc.id
	WHERE vc.user_id = $1 AND vc.vip)`

// fromVIPSQL is "this message is from a VIP": two-valued (IS TRUE turns the
// NULL of a message with no From, or of IN over an empty set, into false), so
// its NOT is the exact complement — search_predicate.go's rule for every leaf.
const fromVIPSQL = `((m.from_address IN ` + vipAddressesSQL + `) IS TRUE)`
