// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"
)

// The read surface of `mailroom-admin nudge-report` (#58): a keyset walk over
// the caller's messages carrying exactly what the follow-up-nudge measurement
// needs to decide DIRECTION (did the user send this?) and GROUND TRUTH (did
// anyone reply, and when?). READ-ONLY: nothing here writes, so there is no
// change_log consequence.
//
// The content-derived features (To vs Cc, question marks, the kind verdict)
// live in the message blob, not on app.messages, and the report parses them
// itself; this surface carries only the row facts and the content_hash that
// keys the blob.

// NudgeMessage is one message as the nudge report sees it.
type NudgeMessage struct {
	ID ulid.ULID
	// ThreadID is the zero ULID when the message has no thread.
	ThreadID   ulid.ULID
	ReceivedAt time.Time
	// SentAt is the Date header (nil when absent). The report uses
	// ReceivedAt as its clock and SentAt only to audit it: a Sent folder
	// imported in bulk carries the IMPORT time as INTERNALDATE, which would
	// make every reply look instant or late.
	SentAt      *time.Time
	FromAddress string
	ContentHash []byte
	// MessageIDHint and InReplyTo are the threading hints verbatim; the report
	// uses them to audit whether replies actually land in their parent's thread.
	MessageIDHint string
	InReplyTo     string
	// IsAnswered is the canonical \Answered flag — set by whichever client
	// replied, with no timestamp.
	IsAnswered bool
	// IsJunk is the provider's verdict OR membership of a junk-role mailbox,
	// the same predicate search's default exclusion uses (junkSQL).
	IsJunk    bool
	IsTrashed bool
	// InSentMailbox is true when the message has ever had provenance in, or
	// is locally placed in, a mailbox whose role is "sent" — the user's
	// provider Sent folder, which the report treats as the primary evidence
	// that the user sent it. ANY provenance state counts, not just live: a
	// sent message later moved to an archive was still sent.
	InSentMailbox bool
	// ViaSubmission is true when a Mailroom submission for this message
	// reached state 'sent'.
	ViaSubmission bool
}

// NudgeMessagesAfter walks the caller's non-draft messages in ascending id
// order (exclusive `after`; pass the zero ULID to start). RLS and the explicit
// user_id scope it to the caller: another user's rows are never returned.
func (tx *UserTx) NudgeMessagesAfter(ctx context.Context, after ulid.ULID, limit int) ([]NudgeMessage, error) {
	if limit <= 0 {
		limit = 1000
	}
	return tx.queryNudgeMessages(ctx, func(bind func(any) string) string {
		return `m.id > ` + bind(after.String()) + ` ORDER BY m.id LIMIT ` + bind(limit)
	})
}

// queryNudgeMessages runs the one NudgeMessage projection with a caller's
// condition (and any ORDER/LIMIT) appended after `WHERE m.user_id = $1 AND
// NOT m.is_draft AND`. $1 is the caller and $2 their sent-role mailboxes;
// cond binds its own values through bind. One projection, so the report's
// walk and the live nudge's reads (follow_up.go) describe a message the same
// way.
func (tx *UserTx) queryNudgeMessages(ctx context.Context, cond func(bind func(any) string) string) ([]NudgeMessage, error) {
	sent, err := tx.roleMailboxIDs(ctx, "sent")
	if err != nil {
		return nil, err
	}
	args := []any{tx.userID.String(), ulidStrings(sent)}
	bind := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	junk, err := tx.junkSQL(ctx, bind)
	if err != nil {
		return nil, err
	}
	where := cond(bind)
	rows, err := tx.tx.Query(ctx, `
		SELECT m.id, COALESCE(m.thread_id::text, ''), m.received_at, m.sent_at,
		       lower(COALESCE(m.from_address::text, '')), m.content_hash,
		       COALESCE(m.message_id_hint, ''), COALESCE(m.in_reply_to, ''),
		       m.is_answered, `+junk+`, m.trashed_at IS NOT NULL,
		       (m.local_mailbox_id = ANY($2)
		        OR m.id IN (SELECT p.message_id FROM app.message_provenance p
		                     WHERE p.user_id = $1 AND p.mailbox_id = ANY($2))),
		       m.id IN (SELECT s.message_id FROM app.submissions s
		                 WHERE s.user_id = $1 AND s.state = 'sent')
		  FROM app.messages m
		 WHERE m.user_id = $1 AND NOT m.is_draft AND `+where, args...)
	if err != nil {
		return nil, fmt.Errorf("store: nudge messages: %w", err)
	}
	defer rows.Close()
	var out []NudgeMessage
	for rows.Next() {
		var (
			r             NudgeMessage
			idStr, thrStr string
			inSent        *bool
		)
		if err := rows.Scan(&idStr, &thrStr, &r.ReceivedAt, &r.SentAt, &r.FromAddress, &r.ContentHash,
			&r.MessageIDHint, &r.InReplyTo, &r.IsAnswered, &r.IsJunk, &r.IsTrashed,
			&inSent, &r.ViaSubmission); err != nil {
			return nil, fmt.Errorf("store: scan nudge message: %w", err)
		}
		r.InSentMailbox = inSent != nil && *inSent
		if r.ID, err = ulid.Parse(idStr); err != nil {
			return nil, fmt.Errorf("store: parse message id %q: %w", idStr, err)
		}
		if thrStr != "" {
			if r.ThreadID, err = ulid.Parse(thrStr); err != nil {
				return nil, fmt.Errorf("store: parse thread id %q: %w", thrStr, err)
			}
		}
		r.ReceivedAt = r.ReceivedAt.UTC()
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: nudge messages rows: %w", err)
	}
	return out, nil
}

// NudgeIdentities is "me" for the nudge report: the caller's fixed identity
// addresses and wildcard identity domains, lower-cased, plus the number of
// sent-role mailboxes — the three facts that decide whether DIRECTION can be
// measured at all on this mailbox. An empty set is a finding, not an error.
type NudgeIdentities struct {
	Addresses     []string
	Domains       []string
	SentMailboxes int
}

// NudgeIdentities returns the caller's NudgeIdentities.
func (tx *UserTx) NudgeIdentities(ctx context.Context) (NudgeIdentities, error) {
	var out NudgeIdentities
	var err error
	out.Addresses, out.Domains, err = tx.myAddressesAndDomains(ctx)
	if err != nil {
		return out, err
	}
	sent, err := tx.roleMailboxIDs(ctx, "sent")
	if err != nil {
		return out, err
	}
	out.SentMailboxes = len(sent)
	return out, nil
}
