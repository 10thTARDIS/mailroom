// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"
)

// FOLLOW-UP NUDGES (#58 phase 2), the store half. A nudge is computed on read
// (internal/nudge.Due) from mail that already exists, so this file only READS
// that mail — in three bounded queries — and stores the one thing the
// computation cannot derive: the user's "Done" (app.nudge_dismissals).
//
// THE COST, BOUNDED. Every query leads with user_id on an existing index:
//   1. candidates: messages in a window of at most FollowUpMaxAge, through
//      messages_user_received; at most FollowUpMaxCandidates rows.
//   2. their conversations: messages_user_thread for the candidates' threads,
//      plus In-Reply-To children (at most FollowUpMaxRelated rows).
//   3. only when the inbound nudge is on: the user's sent mail (the as-of
//      "wrote to them before" signal), newest FollowUpMaxHistory within
//      FollowUpHistoryWindow.
// On a mailbox of tens of thousands of messages with a typical sent volume
// that is a few hundred rows; every bound that is hit is reported back
// (FollowUpSet.Truncated) rather than silently cutting the answer.

const (
	// FollowUpMaxAge is nudge.MaxAge as the store applies it: mail older than
	// this is never a candidate. internal/api's test pins the two equal.
	FollowUpMaxAge = 30 * 24 * time.Hour
	// FollowUpMaxCandidates bounds query 1.
	FollowUpMaxCandidates = 1000
	// FollowUpMaxRelated bounds query 2.
	FollowUpMaxRelated = 5000
	// FollowUpMaxHistory and FollowUpHistoryWindow bound query 3.
	FollowUpMaxHistory    = 2000
	FollowUpHistoryWindow = 2 * 365 * 24 * time.Hour
)

// FollowUpQuery is what the caller wants considered.
type FollowUpQuery struct {
	Now time.Time
	// MinAge is the youngest a candidate may be: the smaller of the two
	// delays in force (outbound 7 days, inbound the user's setting).
	MinAge time.Duration
	// Inbound includes inbound candidates and the sent-mail history.
	Inbound bool
}

// FollowUpSet is everything nudge.Due needs, deduplicated by message id.
type FollowUpSet struct {
	Messages []NudgeMessage
	// Candidates marks the messages that may be nudged themselves. Everything
	// else in Messages is CONTEXT — a later message in a conversation, or sent
	// mail read only for "wrote to them before" — and may be trashed, snoozed
	// or out of the window, which is exactly why it must not be nudged.
	Candidates map[ulid.ULID]bool
	Dismissed  map[ulid.ULID]bool
	// Truncated names any bound that was hit ("candidates", "related",
	// "history"); empty when the answer is complete.
	Truncated []string
}

// FollowUpMessages reads the candidates for follow-up nudges, their
// conversations, and (for the inbound rule) the user's sent mail. Candidates
// exclude drafts, trashed and currently-snoozed mail (snoozing a nudge hides
// it until it wakes) and anything the user already marked Done; inbound ones
// also exclude junk, \Answered, and mail whose stored headers already say
// list or bulk (a prefilter only — the shipped rule re-decides from the
// parsed message, and these columns can only rule OUT what it would too).
func (tx *UserTx) FollowUpMessages(ctx context.Context, q FollowUpQuery) (FollowUpSet, error) {
	set := FollowUpSet{Candidates: map[ulid.ULID]bool{}, Dismissed: map[ulid.ULID]bool{}}
	addrs, domains, err := tx.myAddressesAndDomains(ctx)
	if err != nil {
		return set, err
	}
	from := q.Now.Add(-FollowUpMaxAge)
	to := q.Now.Add(-q.MinAge)

	// outbound is "the user sent it", on any of the three kinds of evidence
	// the report counts separately (the Go side re-checks with subaddresses).
	outbound := func(bind func(any) string) string {
		a, d := bind(addrs), bind(domains)
		return `(m.local_mailbox_id = ANY($2)
		      OR m.id IN (SELECT p.message_id FROM app.message_provenance p
		                   WHERE p.user_id = $1 AND p.mailbox_id = ANY($2))
		      OR m.id IN (SELECT s.message_id FROM app.submissions s
		                   WHERE s.user_id = $1 AND s.state = 'sent')
		      OR lower(COALESCE(m.from_address::text, '')) = ANY(` + a + `::text[])
		      OR lower(split_part(COALESCE(m.from_address::text, ''), '@', 2)) = ANY(` + d + `::text[]))`
	}

	cands, err := tx.queryNudgeMessages(ctx, func(bind func(any) string) string {
		inbound := "false"
		if q.Inbound {
			inbound = `(NOT m.is_junk AND NOT m.is_answered
			      AND m.list_id IS NULL AND m.list_unsubscribe IS NULL
			      AND m.list_unsubscribe_post IS NULL AND m.feedback_id IS NULL)`
		}
		return `m.trashed_at IS NULL
		  AND m.received_at >= ` + bind(from) + `::timestamptz AND m.received_at <= ` + bind(to) + `::timestamptz
		  AND (m.snoozed_until IS NULL OR m.snoozed_until <= ` + bind(q.Now) + `::timestamptz)
		  AND NOT EXISTS (SELECT 1 FROM app.nudge_dismissals d WHERE d.user_id = $1 AND d.message_id = m.id)
		  AND (` + outbound(bind) + ` OR ` + inbound + `)
		ORDER BY m.received_at DESC LIMIT ` + bind(FollowUpMaxCandidates+1)
	})
	if err != nil {
		return set, err
	}
	if len(cands) > FollowUpMaxCandidates {
		cands = cands[:FollowUpMaxCandidates]
		set.Truncated = append(set.Truncated, "candidates")
	}

	seen := map[ulid.ULID]bool{}
	add := func(ms []NudgeMessage) {
		for _, m := range ms {
			if !seen[m.ID] {
				seen[m.ID] = true
				set.Messages = append(set.Messages, m)
			}
		}
	}
	add(cands)
	for _, c := range cands {
		set.Candidates[c.ID] = true
	}
	if len(cands) == 0 {
		return set, nil
	}

	var threads, msgIDs []string
	for _, c := range cands {
		if c.ThreadID != (ulid.ULID{}) {
			threads = append(threads, c.ThreadID.String())
		}
		if c.MessageIDHint != "" {
			msgIDs = append(msgIDs, c.MessageIDHint)
		}
	}
	related, err := tx.queryNudgeMessages(ctx, func(bind func(any) string) string {
		return `(m.thread_id = ANY(` + bind(threads) + `::text[]) OR m.in_reply_to = ANY(` + bind(msgIDs) + `::text[]))
		ORDER BY m.received_at LIMIT ` + bind(FollowUpMaxRelated+1)
	})
	if err != nil {
		return set, err
	}
	if len(related) > FollowUpMaxRelated {
		related = related[:FollowUpMaxRelated]
		set.Truncated = append(set.Truncated, "related")
	}
	add(related)

	if q.Inbound {
		history, err := tx.queryNudgeMessages(ctx, func(bind func(any) string) string {
			return `m.received_at >= ` + bind(q.Now.Add(-FollowUpHistoryWindow)) + `::timestamptz AND ` + outbound(bind) + `
			ORDER BY m.received_at DESC LIMIT ` + bind(FollowUpMaxHistory+1)
		})
		if err != nil {
			return set, err
		}
		if len(history) > FollowUpMaxHistory {
			history = history[:FollowUpMaxHistory]
			set.Truncated = append(set.Truncated, "history")
		}
		add(history)
	}

	// Dismissals among what was read (a Done on a later message in a
	// conversation must still supersede an earlier one).
	ids := make([]string, 0, len(set.Messages))
	for _, m := range set.Messages {
		ids = append(ids, m.ID.String())
	}
	rows, err := tx.tx.Query(ctx, `
		SELECT message_id FROM app.nudge_dismissals
		 WHERE user_id = $1 AND message_id = ANY($2::text[])`, tx.userID.String(), ids)
	if err != nil {
		return set, fmt.Errorf("store: nudge dismissals: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return set, fmt.Errorf("store: scan nudge dismissal: %w", err)
		}
		id, err := ulid.Parse(s)
		if err != nil {
			return set, fmt.Errorf("store: parse dismissal id %q: %w", s, err)
		}
		set.Dismissed[id] = true
	}
	return set, rows.Err()
}

// FollowUpDisplayRow is what the Follow up strip shows for one due nudge.
type FollowUpDisplayRow struct {
	ID          ulid.ULID
	Subject     string
	FromName    string
	FromAddress string
}

// FollowUpDisplay reads the display columns for the given (due) message ids;
// ids that are not the caller's are simply absent.
func (tx *UserTx) FollowUpDisplay(ctx context.Context, ids []ulid.ULID) ([]FollowUpDisplayRow, error) {
	rows, err := tx.tx.Query(ctx, `
		SELECT id, subject, from_name, COALESCE(from_address::text, '')
		  FROM app.messages WHERE user_id = $1 AND id = ANY($2::text[])`,
		tx.userID.String(), ulidStrings(ids))
	if err != nil {
		return nil, fmt.Errorf("store: follow-up display: %w", err)
	}
	defer rows.Close()
	var out []FollowUpDisplayRow
	for rows.Next() {
		var (
			r FollowUpDisplayRow
			s string
		)
		if err := rows.Scan(&s, &r.Subject, &r.FromName, &r.FromAddress); err != nil {
			return nil, fmt.Errorf("store: scan follow-up display: %w", err)
		}
		if r.ID, err = ulid.Parse(s); err != nil {
			return nil, fmt.Errorf("store: parse message id %q: %w", s, err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ErrNudgeKind is an unknown nudge kind.
var ErrNudgeKind = errors.New("store: nudge kind must be outbound or inbound")

// DismissNudge records the user's "Done" on the nudge for messageID. It
// reports found=false when the message is not the caller's (RLS makes
// another user's message indistinguishable from a missing one), and
// changed=false when it was already dismissed — a no-op that appends nothing.
//
// A dismissal changes what FollowUp/get returns for this Email, so it appends
// exactly one Email `updated` change_log row in the same transaction: every
// client's Email/changes then sees the message move, which is how a Done on
// one device clears the nudge on the others (the load-bearing invariant).
func (tx *UserTx) DismissNudge(ctx context.Context, messageID ulid.ULID, kind string) (found, changed bool, err error) {
	if kind != "outbound" && kind != "inbound" {
		return false, false, ErrNudgeKind
	}
	var exists bool
	if err := tx.tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM app.messages WHERE user_id = $1 AND id = $2)`,
		tx.userID.String(), messageID.String()).Scan(&exists); err != nil {
		return false, false, fmt.Errorf("store: dismiss nudge (lookup): %w", err)
	}
	if !exists {
		return false, false, nil
	}
	tag, err := tx.tx.Exec(ctx, `
		INSERT INTO app.nudge_dismissals (user_id, message_id, kind)
		VALUES ($1, $2, $3)
		ON CONFLICT (user_id, message_id) DO NOTHING`,
		tx.userID.String(), messageID.String(), kind)
	if err != nil {
		return true, false, fmt.Errorf("store: dismiss nudge: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return true, false, nil
	}
	if _, err := tx.AppendChange(ctx, ObjectEmail, messageID, OpUpdated); err != nil {
		return true, false, err
	}
	return true, true, nil
}
