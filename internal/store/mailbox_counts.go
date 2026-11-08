// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"fmt"
	"sort"

	"github.com/oklog/ulid/v2"
)

// MAILBOX STATE ON COUNT CHANGES (ADR-052, #115).
//
// A mailbox's totalEmails and unreadEmails are COMPUTED ON READ (GetMailboxes,
// over effectiveMembershipSQL), never stored. Nothing therefore appended a
// change_log row when a message was moved, read or deleted, and a client that
// syncs by state (Mailbox/changes, EventSource) never learned a count had moved.
//
// The fix keeps counts computed and makes the LOG say they may have moved:
//
//   - A write that can change a count NOTES the mailboxes it touches on the
//     transaction (NoteMailboxCountsBefore / After / NoteMailboxes).
//   - Store.UserTx flushes the notes as Mailbox `updated` rows, marked
//     counts_only, in the same transaction, just before commit.
//
// THE NOTES ARE A SET, AND THAT IS THE COST BOUND. Moving 5,000 messages across
// three mailboxes appends three rows, however many statements ran: the rows are
// O(mailboxes touched), never O(messages). A rolled-back transaction flushes
// nothing (the flush runs only when the callback succeeded).
//
// ⚠ WHICH SIDE OF THE WRITE. Membership is DERIVED (effectiveMembershipSQL), so
// a note taken at one instant sees one side of a move. A write that REMOVES a
// message from a mailbox (move, vanish, delete) must note BEFORE it writes
// (NoteMailboxCountsBefore resolves immediately) or the source mailbox is
// invisible afterwards; the destination is noted AFTER (NoteMailboxCountsAfter
// defers resolution to the flush, where all ids resolve in one statement). A
// write that changes seen or trash state in place needs only the After note.
// Flag changes that cannot move a count (flagged, answered, keywords) note
// nothing: otherwise every flag toggle would wake every client.

// mailboxCountNotes is one transaction's pending notes.
type mailboxCountNotes struct {
	mailboxes map[ulid.ULID]struct{} // resolved: note these as they are
	messages  map[ulid.ULID]struct{} // deferred: resolve at flush
}

func newMailboxCountNotes() *mailboxCountNotes {
	return &mailboxCountNotes{
		mailboxes: map[ulid.ULID]struct{}{},
		messages:  map[ulid.ULID]struct{}{},
	}
}

// notes returns the transaction's notes. Store.UserTx allocates them up front,
// and that is load-bearing: a savepoint-scoped writer copies the UserTx by value
// (`inner := *tx`), and a note made through the copy must land in the SAME set
// the flush reads, which only holds if the pointer was shared before the copy.
func (tx *UserTx) notes() *mailboxCountNotes {
	if tx.counts == nil {
		tx.counts = newMailboxCountNotes()
	}
	return tx.counts
}

// NoteMailboxes notes mailboxes whose counts may have moved, by id.
func (tx *UserTx) NoteMailboxes(ids ...ulid.ULID) {
	n := tx.notes()
	for _, id := range ids {
		n.mailboxes[id] = struct{}{}
	}
}

// NoteMailboxCountsAfter notes the mailboxes these messages are in once the
// transaction's writes are done: resolved at flush, in one statement for all of
// them. Call it for a write that adds a message to a mailbox or changes its
// seen or trash state.
func (tx *UserTx) NoteMailboxCountsAfter(ids ...ulid.ULID) {
	n := tx.notes()
	for _, id := range ids {
		n.messages[id] = struct{}{}
	}
}

// NoteMailboxCountsBefore notes the mailboxes these messages are in RIGHT NOW,
// resolved immediately. Call it BEFORE a write that takes a message out of a
// mailbox (a move, a vanish, a delete): afterwards the source is not derivable.
func (tx *UserTx) NoteMailboxCountsBefore(ctx context.Context, ids ...ulid.ULID) error {
	if len(ids) == 0 {
		return nil
	}
	mbs, err := tx.mailboxesOfMessages(ctx, ids)
	if err != nil {
		return err
	}
	tx.NoteMailboxes(mbs...)
	return nil
}

// mailboxesOfMessages resolves the effective mailboxes of messages, over the
// same derivation the counts use (effectiveMembershipSQL, filtered to the ids).
func (tx *UserTx) mailboxesOfMessages(ctx context.Context, ids []ulid.ULID) ([]ulid.ULID, error) {
	rows, err := tx.tx.Query(ctx, `
		WITH membership AS (`+effectiveMembershipSQL+`)
		SELECT DISTINCT mailbox_id FROM membership
		 WHERE message_id = ANY($2)`,
		tx.userID.String(), ulidStrings(ids))
	if err != nil {
		return nil, fmt.Errorf("store: resolve mailboxes of messages: %w", err)
	}
	defer rows.Close()
	var out []ulid.ULID
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, fmt.Errorf("store: scan mailbox of message: %w", err)
		}
		id, err := ulid.Parse(s)
		if err != nil {
			return nil, fmt.Errorf("store: parse mailbox id %q: %w", s, err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// flushMailboxCounts appends the Mailbox change rows for this transaction's
// notes: one `updated` row, counts_only, per DISTINCT mailbox that still
// exists. It is called by Store.UserTx after the callback succeeded and before
// commit, and appends through the one AppendChanges path.
func (tx *UserTx) flushMailboxCounts(ctx context.Context) error {
	n := tx.counts
	if n == nil || (len(n.mailboxes) == 0 && len(n.messages) == 0) {
		return nil
	}
	set := n.mailboxes
	defer func() { clear(n.mailboxes); clear(n.messages) }()
	if len(n.messages) > 0 {
		msgs := make([]ulid.ULID, 0, len(n.messages))
		for id := range n.messages {
			msgs = append(msgs, id)
		}
		mbs, err := tx.mailboxesOfMessages(ctx, msgs)
		if err != nil {
			return err
		}
		for _, id := range mbs {
			set[id] = struct{}{}
		}
	}
	if len(set) == 0 {
		return nil
	}
	cand := make([]ulid.ULID, 0, len(set))
	for id := range set {
		cand = append(cand, id)
	}
	// Only mailboxes that still exist: an account removal destroys its mailboxes
	// in the same transaction and has already appended their `destroyed` rows.
	rows, err := tx.tx.Query(ctx,
		`SELECT id FROM app.mailboxes WHERE user_id = $1 AND id = ANY($2)`,
		tx.userID.String(), ulidStrings(cand))
	if err != nil {
		return fmt.Errorf("store: filter noted mailboxes: %w", err)
	}
	var live []ulid.ULID
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			rows.Close()
			return fmt.Errorf("store: scan noted mailbox: %w", err)
		}
		id, perr := ulid.Parse(s)
		if perr != nil {
			rows.Close()
			return fmt.Errorf("store: parse mailbox id %q: %w", s, perr)
		}
		live = append(live, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: filter noted mailboxes: %w", err)
	}
	sort.Slice(live, func(i, j int) bool { return live[i].Compare(live[j]) < 0 })
	return tx.appendChanges(ctx, ObjectMailbox, live, OpUpdated, true)
}
