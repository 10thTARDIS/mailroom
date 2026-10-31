// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"fmt"

	"github.com/oklog/ulid/v2"
)

// MoveBlockedArrival moves a NEW message from a blocked sender (#59) to the
// arriving account's Junk or Trash, at ingest. dest is the contact's
// block_destination ("junk" or "trash", BlockedDestination).
//
// THE MOVE IS THE ORDINARY MOVE. It is applyEmailUpdate — the function behind
// every Email/set move — with MoveToRole "junk" (and the account's own Junk
// mailbox named, as a client move names it) or Trash, so the canonical
// placement, Trash's purge clock and the IMAP writeback intent are exactly
// what the user would get by moving the message by hand. Unlike a rule's
// skip-inbox (skipInboxForNewMessage), it is NOT Mailroom-only: a blocked
// sender's mail leaves the inbox on the server too, so another mail app stops
// showing it as well. That is why the caller runs it AFTER the provenance row
// exists — writeback needs a live location to move the message from.
//
// No change_log row of its own: the message's `created` row, appended by
// ingest right after, already reports where it ended up (the rule every
// ingest-time write follows). Flags are untouched: the message stays unread.
//
// It only moves mail that ARRIVED IN THE INBOX. Mail arriving straight in
// another folder was already filed somewhere by the provider or another
// client and is left where it is. And it leaves the message in the inbox
// when the account has no mailbox with that role — a block must never hide
// mail somewhere the user cannot find it (the skip-inbox rule's rule).
//
// Under its own SAVEPOINT: a failure here (the caller logs it) leaves the
// ingest transaction usable and the message where it arrived. A block can
// cost a junked message its trip to Junk; it can never cost a message.
func (tx *UserTx) MoveBlockedArrival(ctx context.Context, msgID, accountID, arrivingMailboxID ulid.ULID, dest string) (moved bool, err error) {
	if dest != BlockToJunk && dest != BlockToTrash {
		return false, ErrBlockDestinationInvalid
	}
	err = tx.inSavepoint(ctx, func() error {
		role, _, err := tx.MailboxRole(ctx, arrivingMailboxID)
		if err != nil {
			return err
		}
		if role != "inbox" {
			return nil
		}
		mb, ok, err := tx.MailboxForRole(ctx, accountID, dest)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		up := EmailUpdate{MoveToRole: dest, MoveToMailboxID: &mb.ID}
		if dest == BlockToTrash {
			up = EmailUpdate{Trash: true}
		}
		found, _, err := tx.applyEmailUpdate(ctx, msgID, up, false)
		if err != nil {
			return fmt.Errorf("store: move blocked arrival: %w", err)
		}
		moved = found
		return nil
	})
	if err != nil {
		return false, err
	}
	return moved, nil
}
