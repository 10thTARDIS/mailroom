// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"fmt"
	"sort"
	"strings"

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
	// pre holds mailboxes already resolved for messages a bulk write is about to
	// change (PrefetchMailboxCountsBefore), consumed once by NoteMailboxCountsBefore.
	pre map[ulid.ULID][]ulid.ULID
}

func newMailboxCountNotes() *mailboxCountNotes {
	return &mailboxCountNotes{
		mailboxes: map[ulid.ULID]struct{}{},
		messages:  map[ulid.ULID]struct{}{},
		pre:       map[ulid.ULID][]ulid.ULID{},
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
//
// A message whose mailboxes were resolved ahead by PrefetchMailboxCountsBefore
// uses that answer, once; every other message is looked up now, as before.
func (tx *UserTx) NoteMailboxCountsBefore(ctx context.Context, ids ...ulid.ULID) error {
	if len(ids) == 0 {
		return nil
	}
	n := tx.notes()
	var lookup []ulid.ULID
	for _, id := range ids {
		if mbs, ok := n.pre[id]; ok {
			delete(n.pre, id)
			tx.NoteMailboxes(mbs...)
			continue
		}
		lookup = append(lookup, id)
	}
	if len(lookup) == 0 {
		return nil
	}
	mbs, err := tx.mailboxesOfMessages(ctx, lookup)
	if err != nil {
		return err
	}
	tx.NoteMailboxes(mbs...)
	return nil
}

// prefetchChunk bounds one prefetch statement's id list.
const prefetchChunk = 5000

// PrefetchMailboxCountsBefore resolves the mailboxes of many messages in a few
// statements instead of one per message, for a bulk write (Email/set moving or
// deleting thousands) that will call NoteMailboxCountsBefore for each. The
// answer is exactly the one NoteMailboxCountsBefore would have looked up: the
// same derivation (batchMembershipSQL), the same set per message, and so
// the same Mailbox change rows at flush. Each entry is used ONCE and dropped, so
// a message noted a second time is looked up live.
//
// Call it immediately before the loop that writes, in the transaction that
// writes. Between the prefetch and a message's own write only that write
// changes its membership (a flag or trash change never alters a mailbox id), so
// within the transaction the two are the same; against ANOTHER transaction the
// window is the bulk's duration rather than one statement, which the per-message
// lookup never closed either (no row lock is taken before it).
func (tx *UserTx) PrefetchMailboxCountsBefore(ctx context.Context, ids ...ulid.ULID) error {
	n := tx.notes()
	for start := 0; start < len(ids); start += prefetchChunk {
		chunk := ids[start:min(start+prefetchChunk, len(ids))]
		rows, err := tx.tx.Query(ctx, `
			WITH ids AS MATERIALIZED (SELECT unnest($2::text[]) AS id),
			     membership AS (`+batchMembershipSQL+`)
			SELECT DISTINCT message_id, mailbox_id FROM membership`,
			tx.userID.String(), ulidStrings(chunk))
		if err != nil {
			return fmt.Errorf("store: prefetch mailboxes of messages: %w", err)
		}
		got := map[ulid.ULID][]ulid.ULID{}
		for rows.Next() {
			var ms, bs string
			if err := rows.Scan(&ms, &bs); err != nil {
				rows.Close()
				return fmt.Errorf("store: scan prefetched mailbox: %w", err)
			}
			mid, merr := ulid.Parse(ms)
			bid, berr := ulid.Parse(bs)
			if merr != nil || berr != nil {
				rows.Close()
				return fmt.Errorf("store: parse prefetched ids %q %q", ms, bs)
			}
			got[mid] = append(got[mid], bid)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("store: prefetch mailboxes of messages: %w", err)
		}
		// A message with no membership gets an EMPTY answer, not a miss: the
		// live lookup would have found nothing either.
		for _, id := range chunk {
			n.pre[id] = got[id]
		}
	}
	return nil
}

// membershipOfMessagesSQL is effectiveMembershipSQL restricted to $2 (a text
// array of message ids), by rewriting each branch's user predicate rather than
// copying the derivation: there is still ONE statement of "which mailboxes is a
// message in".
//
// ⚠ THE RESTRICTION MUST BE INSIDE EACH BRANCH. Filtering the whole relation
// from outside (`WHERE message_id = ANY($2)` over the CTE) read every one of the
// user's messages per call: MEASURED, a 5,000-message move took 66 s against 31 s
// without the note, because the pre-image query ran once per message. Pushed
// into the branches it is an index lookup by primary key or by message id.
var membershipOfMessagesSQL = strings.NewReplacer(
	"m.user_id = $1", "m.user_id = $1 AND m.id = ANY($2)",
	// The provenance branch joins messages: restrict BOTH sides, or the planner
	// scans every message of the user and probes provenance for each (MEASURED:
	// 7.3 s for 300 ids over 5,000 messages, against 0.4 ms for one).
	"p.user_id = $1", "p.user_id = $1 AND p.message_id = ANY($2) AND m.id = ANY($2)",
).Replace(effectiveMembershipSQL)

// batchMembershipSQL is effectiveMembershipSQL for MANY messages, DRIVEN FROM
// the ids: `ids` (one text column, unnested from $2) is joined to each branch, so
// every branch is an index lookup per id however many there are.
//
// ⚠ NOT membershipOfMessagesSQL, and not `IN (SELECT id FROM ids)` either. The
// `= ANY($2)` form is right for a handful of ids and QUADRATIC for thousands: the
// provenance branch re-tests the whole array for every row it joins. MEASURED on
// a 5,000-message move whose tables had not been analysed yet (a fresh import,
// before autovacuum): 48 s with `= ANY`, 9 s with `IN (SELECT ...)`, 0.3 s with
// the explicit join below, and 16 ms to 70 ms once analysed (whichever form).
//
// It is a second statement of the derivation, so the branches below must stay
// the same as effectiveMembershipSQL's: TestBulkMovePrefetchIsIdentical... runs
// both over moved, local, provenance, two-folder and no-folder messages and
// compares what they note.
const batchMembershipSQL = `
	SELECT m.id AS message_id, m.moved_mailbox_id AS mailbox_id
	  FROM ids JOIN app.messages m ON m.user_id = $1 AND m.id = ids.id
	 WHERE m.moved_mailbox_id IS NOT NULL
	UNION ALL
	SELECT m.id, m.local_mailbox_id
	  FROM ids JOIN app.messages m ON m.user_id = $1 AND m.id = ids.id
	 WHERE m.moved_mailbox_id IS NULL AND m.local_mailbox_id IS NOT NULL
	UNION ALL
	SELECT p.message_id, p.mailbox_id
	  FROM ids
	  JOIN app.message_provenance p ON p.user_id = $1 AND p.message_id = ids.id
	  JOIN app.messages m ON m.id = p.message_id AND m.user_id = p.user_id
	 WHERE p.state = 'live' AND m.moved_mailbox_id IS NULL`

// mailboxesOfMessages resolves the effective mailboxes of messages, over the
// same derivation the counts use (effectiveMembershipSQL, filtered to the ids).
func (tx *UserTx) mailboxesOfMessages(ctx context.Context, ids []ulid.ULID) ([]ulid.ULID, error) {
	rows, err := tx.tx.Query(ctx, `
		WITH membership AS (`+membershipOfMessagesSQL+`)
		SELECT DISTINCT mailbox_id FROM membership`,
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
	defer func() { clear(n.mailboxes); clear(n.messages); clear(n.pre) }()
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
