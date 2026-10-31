// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
)

// The engagement signals the priority model learns from, and the bookkeeping
// around its promotion gate (#842).
//
// WHY THIS FILE EXISTS. Measured on a real instance when #842 was opened: thousands of
// mark_read_no_open and archive_unread events against a handful of positives, and
// ZERO `reply` events although sent messages existed. The only reply seam
// was EmailSubmission/set, so a reply written in any other client — and then
// ingested from Sent — taught the model nothing. Replies are the strongest
// engagement signal a mailbox holds, and they are derivable from the mail
// itself: a message From one of the user's own identities names its parent in
// In-Reply-To/References. This file derives them, records them once, and adds
// the explicit `open` signal the client now sends.
//
// Like the rest of the feedback surface (feedback.go, ml_models.go) these rows
// are OPERATIONAL data, not JMAP-visible objects: no change_log row. Every
// method runs in a UserTx, so RLS scopes every statement to the caller — a
// sent message of user B can never resolve to, or record feedback on, a
// message of user A.

// Feedback tokens this file writes. They MUST match ml.LabelForAction's map;
// internal/api/feedback_action_test.go cross-checks the api package's copies.
const (
	FeedbackActionReply = "reply"
	FeedbackActionOpen  = "open"
)

// ReplyResolution is the outcome of resolving the message a sent message
// answers. Reported per case by `priority-report` and the reply backfill, so a
// zero is distinguishable from "the thing measured is always empty".
type ReplyResolution int

const (
	// ReplyNotOwn: the message's From is not an address this user may send as.
	ReplyNotOwn ReplyResolution = iota
	// ReplyDraft: an unsent draft — not engagement yet.
	ReplyDraft
	// ReplyNoHeaders: no In-Reply-To and no References.
	ReplyNoHeaders
	// ReplyParentMissing: the headers name messages this user does not store.
	ReplyParentMissing
	// ReplyParentOwnOnly: every stored message the headers name is the user's
	// own (a follow-up to their own mail); there is no inbound original.
	ReplyParentOwnOnly
	// ReplyResolved: an inbound original was found.
	ReplyResolved
	// ReplyUnverified: From names one of the user's addresses, but nothing
	// proves the user sent it — it was never in a Sent mailbox and no Mailroom
	// submission sent it. A From header is sender-controlled, so this is the
	// spoof case, and it records nothing (#842 security review).
	ReplyUnverified
)

// String names the resolution for reports.
func (r ReplyResolution) String() string {
	switch r {
	case ReplyNotOwn:
		return "not-own"
	case ReplyDraft:
		return "draft"
	case ReplyNoHeaders:
		return "no-threading-headers"
	case ReplyParentMissing:
		return "parent-not-stored"
	case ReplyParentOwnOnly:
		return "parent-is-own-mail"
	case ReplyResolved:
		return "resolved"
	case ReplyUnverified:
		return "own-address-not-verifiably-sent"
	}
	return "unknown"
}

// IsOwnAddress reports whether addr is one this user may send as — the same
// ResolveSendIdentity test the contacts layer uses to decide "this message is
// one the user sent" (contacts.go), so the two can never disagree.
func (tx *UserTx) IsOwnAddress(ctx context.Context, addr string) (bool, error) {
	if NormalizeAddress(addr) == "" {
		return false, nil
	}
	if _, err := tx.ResolveSendIdentity(ctx, addr); err != nil {
		if errors.Is(err, ErrFromNotVerified) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// THE FROM ADDRESS IS NOT PROOF OF AUTHORSHIP (#842 security review). Anyone
// can mail the user "From: <the user's own address>" with In-Reply-To naming
// the attacker's earlier message; treating that as the user's reply would make
// the attacker's mail a positive training example and teach the model to rank
// it important. auth_notice_self.go reached the same conclusion for the
// authResults notice. So a message counts as the user's reply only when it is
// VERIFIABLY sent by them, from evidence the sender does not control:
//
//   - it was ingested into (has a provenance row in) a Sent-role mailbox — the
//     user's own server filed it there, which an outside sender cannot do; or
//   - it is placed locally in a Sent-role mailbox (Mailroom-composed sends
//     before the APPEND round-trips, and imported Sent folders); or
//   - a Mailroom submission sent it.
//
// A live inbound copy of the user's own mail that passes DMARC for their
// domain is deliberately NOT accepted: the Sent copy of the same message
// already carries the signal, so that path would add risk and no evidence.

// sentRoleSQL is roleForMailbox(...) == "sent" for the mailbox aliased b,
// in SQL: INBOX by name is never Sent; otherwise the SPECIAL-USE (or manual
// mapping) with any leading backslash, case-insensitively.
const sentRoleSQL = `(upper(b.imap_name) <> 'INBOX' AND lower(ltrim(COALESCE(b.special_use, ''), '\')) = 'sent')`

// verifiedSentSQL is the "verifiably the user's own sent mail" predicate for
// the message aliased m (see the block comment above).
const verifiedSentSQL = `(
	EXISTS (SELECT 1 FROM app.message_provenance p
	          JOIN app.mailboxes b ON b.user_id = p.user_id AND b.id = p.mailbox_id
	         WHERE p.user_id = m.user_id AND p.message_id = m.id AND ` + sentRoleSQL + `)
	OR EXISTS (SELECT 1 FROM app.mailboxes b
	            WHERE b.user_id = m.user_id AND b.id = m.local_mailbox_id AND ` + sentRoleSQL + `)
	OR EXISTS (SELECT 1 FROM app.submissions s
	            WHERE s.user_id = m.user_id AND s.message_id = m.id AND s.state = 'sent'))`

// MailboxIsSent reports whether a mailbox has the Sent role. Ingest uses it
// for the mailbox a message is arriving into, before its provenance row exists.
func (tx *UserTx) MailboxIsSent(ctx context.Context, mailboxID ulid.ULID) (bool, error) {
	if mailboxID == (ulid.ULID{}) {
		return false, nil
	}
	var sent bool
	err := tx.tx.QueryRow(ctx, `
		SELECT `+sentRoleSQL+` FROM app.mailboxes b WHERE b.user_id = $1 AND b.id = $2`,
		tx.userID.String(), mailboxID.String()).Scan(&sent)
	if isNoRows(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: mailbox is sent: %w", err)
	}
	return sent, nil
}

// verifiedSent reports whether an already-stored message is verifiably the
// user's own sent mail.
func (tx *UserTx) verifiedSent(ctx context.Context, msgID ulid.ULID) (bool, error) {
	var ok bool
	err := tx.tx.QueryRow(ctx, `
		SELECT `+verifiedSentSQL+` FROM app.messages m WHERE m.user_id = $1 AND m.id = $2`,
		tx.userID.String(), msgID.String()).Scan(&ok)
	if isNoRows(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: verified sent: %w", err)
	}
	return ok, nil
}

// ReplyTarget resolves the INBOUND original a message the user sent answers.
//
// The rule, deliberately ONE original per sent message:
//
//  1. the In-Reply-To parent, if it is stored and is not the user's own mail;
//  2. otherwise the nearest References ancestor (walking from the end) that is.
//
// Not "every inbound ancestor": a reply deep in a thread engages with the
// message it answers, and labelling the whole history positive would hand one
// conversation's sender a dozen positives for one act — sender memorisation
// again (#169, #220). The direct parent is the honest unit.
//
// Message-IDs are stored bare (mime.Header strips angle brackets), but the
// lookup matches either form, as dsn.go does.
func (tx *UserTx) ReplyTarget(ctx context.Context, sentID ulid.ULID) (ulid.ULID, ReplyResolution, error) {
	return tx.replyTarget(ctx, sentID, false)
}

// replyTarget is ReplyTarget; arrivingInSent is ingest's evidence that the
// message is being filed into a Sent mailbox right now, before its provenance
// row exists. Otherwise the stored evidence (verifiedSentSQL) must hold.
func (tx *UserTx) replyTarget(ctx context.Context, sentID ulid.ULID, arrivingInSent bool) (ulid.ULID, ReplyResolution, error) {
	var (
		from      string
		draft     bool
		inReplyTo string
		refs      []string
	)
	err := tx.tx.QueryRow(ctx, `
		SELECT COALESCE(from_address::text, ''), is_draft, COALESCE(in_reply_to, ''), references_ids
		  FROM app.messages
		 WHERE user_id = $1 AND id = $2`,
		tx.userID.String(), sentID.String()).Scan(&from, &draft, &inReplyTo, &refs)
	if isNoRows(err) {
		return ulid.ULID{}, ReplyNotOwn, nil
	}
	if err != nil {
		return ulid.ULID{}, 0, fmt.Errorf("store: reply target (read sent): %w", err)
	}
	own, err := tx.IsOwnAddress(ctx, from)
	if err != nil {
		return ulid.ULID{}, 0, err
	}
	if !own {
		return ulid.ULID{}, ReplyNotOwn, nil
	}
	if draft {
		return ulid.ULID{}, ReplyDraft, nil
	}
	if !arrivingInSent {
		ok, err := tx.verifiedSent(ctx, sentID)
		if err != nil {
			return ulid.ULID{}, 0, err
		}
		if !ok {
			return ulid.ULID{}, ReplyUnverified, nil
		}
	}
	return tx.resolveInboundParent(ctx, sentID, inReplyTo, refs)
}

// resolveInboundParent walks the candidate header ids (In-Reply-To first, then
// References newest-first) and returns the first stored, non-draft message that
// is not the user's own and is not the sent message itself.
func (tx *UserTx) resolveInboundParent(ctx context.Context, selfID ulid.ULID, inReplyTo string, refs []string) (ulid.ULID, ReplyResolution, error) {
	candidates := make([]string, 0, len(refs)+1)
	seen := map[string]bool{}
	add := func(h string) {
		h = strings.Trim(strings.TrimSpace(h), "<>")
		if h != "" && !seen[h] {
			seen[h] = true
			candidates = append(candidates, h)
		}
	}
	add(inReplyTo)
	for i := len(refs) - 1; i >= 0; i-- {
		add(refs[i])
	}
	if len(candidates) == 0 {
		return ulid.ULID{}, ReplyNoHeaders, nil
	}

	sawOwn := false
	for _, hdr := range candidates {
		rows, err := tx.tx.Query(ctx, `
			SELECT id, COALESCE(from_address::text, ''), is_draft
			  FROM app.messages
			 WHERE user_id = $1 AND message_id_hint IN ($2, '<' || $2 || '>')
			 ORDER BY id`,
			tx.userID.String(), hdr)
		if err != nil {
			return ulid.ULID{}, 0, fmt.Errorf("store: reply target (lookup parent): %w", err)
		}
		type hit struct {
			id    string
			from  string
			draft bool
		}
		var hits []hit
		for rows.Next() {
			var h hit
			if err := rows.Scan(&h.id, &h.from, &h.draft); err != nil {
				rows.Close()
				return ulid.ULID{}, 0, fmt.Errorf("store: reply target (scan parent): %w", err)
			}
			hits = append(hits, h)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return ulid.ULID{}, 0, fmt.Errorf("store: reply target (parent rows): %w", err)
		}
		for _, h := range hits {
			if h.id == selfID.String() || h.draft {
				continue
			}
			own, err := tx.IsOwnAddress(ctx, h.from)
			if err != nil {
				return ulid.ULID{}, 0, err
			}
			if own {
				sawOwn = true
				continue
			}
			id, err := ulid.Parse(h.id)
			if err != nil {
				return ulid.ULID{}, 0, fmt.Errorf("store: reply target: parse id %q: %w", h.id, err)
			}
			return id, ReplyResolved, nil
		}
	}
	if sawOwn {
		return ulid.ULID{}, ReplyParentOwnOnly, nil
	}
	return ulid.ULID{}, ReplyParentMissing, nil
}

// feedbackOnceLock serialises the check-then-insert of a once-only feedback
// event for one (user, action, message). A transaction-scoped advisory lock
// rather than a unique index, so this needs no migration; the race it closes
// (ingest of a sent copy and the backfill resolving the same original at the
// same instant) is rare, but "rare" is not "idempotent".
func (tx *UserTx) feedbackOnceLock(ctx context.Context, action string, msgID ulid.ULID) error {
	key := "mailroom.feedback:" + tx.userID.String() + ":" + action + ":" + msgID.String()
	if _, err := tx.tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, key); err != nil {
		return fmt.Errorf("store: feedback lock: %w", err)
	}
	return nil
}

// insertFeedbackOnce inserts one `action` event for msgID unless one already
// exists, dated at `at` (clamped to now). It reports whether it inserted.
// RLS scopes both the existence check and the insert; the EXISTS on the message
// means an id the caller cannot see records nothing.
func (tx *UserTx) insertFeedbackOnce(ctx context.Context, msgID ulid.ULID, action string, contextJSON []byte, at time.Time) (bool, error) {
	if msgID == (ulid.ULID{}) {
		return false, fmt.Errorf("store: feedback once: message id is required")
	}
	if err := tx.feedbackOnceLock(ctx, action, msgID); err != nil {
		return false, err
	}
	if len(contextJSON) == 0 {
		contextJSON = []byte("{}")
	}
	var atArg any
	if !at.IsZero() {
		atArg = at.UTC()
	}
	tag, err := tx.tx.Exec(ctx, `
		INSERT INTO app.feedback_events (id, user_id, message_id, action, context, created_at)
		SELECT $1::app.ulid, $2::app.ulid, $3::app.ulid, $4::text, $5::jsonb,
		       LEAST(now(), COALESCE($6::timestamptz, now()))
		 WHERE EXISTS (SELECT 1 FROM app.messages
		                WHERE user_id = $2::app.ulid AND id = $3::app.ulid)
		   AND NOT EXISTS (SELECT 1 FROM app.feedback_events
		                    WHERE user_id = $2::app.ulid AND message_id = $3::app.ulid
		                      AND action = $4::text)`,
		NewULID().String(), tx.userID.String(), msgID.String(), action, string(contextJSON), atArg)
	if err != nil {
		return false, fmt.Errorf("store: feedback once (%s): %w", action, err)
	}
	return tag.RowsAffected() == 1, nil
}

// RecordReplyFeedback records the `reply` engagement signal on origID, ONCE PER
// ORIGINAL MESSAGE, whichever path sees it first: the EmailSubmission/set seam
// (source "submission"), ingest of the user's own sent copy ("ingest"), or the
// backfill ("backfill"). Per original rather than per (sent, original) pair,
// because one send can reach the store as two rows — the submitted draft and a
// Sent copy another client re-filed — and a per-pair key would count that one
// act twice. Two genuine replies to one message add nothing a per-message label
// can use.
//
// The event is dated at the sent message's received time (never later than
// now), so a backfilled reply sits where it happened in the training window.
func (tx *UserTx) RecordReplyFeedback(ctx context.Context, origID, sentID ulid.ULID, source string) (bool, error) {
	at := time.Time{}
	if sentID != (ulid.ULID{}) {
		// #867: an AI-tool key's reply is the tool's act, not the user's
		// engagement, so it never becomes a signal. This is the single funnel
		// for the submission, ingest and backfill sources.
		byKey, err := tx.SentByAPIKey(ctx, sentID)
		if err != nil {
			return false, err
		}
		if byKey {
			return false, nil
		}
		err = tx.tx.QueryRow(ctx,
			`SELECT received_at FROM app.messages WHERE user_id = $1 AND id = $2`,
			tx.userID.String(), sentID.String()).Scan(&at)
		if err != nil && !isNoRows(err) {
			return false, fmt.Errorf("store: reply feedback (sent time): %w", err)
		}
	}
	payload, err := json.Marshal(map[string]string{"sentMessageId": sentID.String(), "source": source})
	if err != nil {
		return false, err
	}
	return tx.insertFeedbackOnce(ctx, origID, FeedbackActionReply, payload, at)
}

// keyWrittenSQL is "the message aliased k was written or submitted by an API
// key": a draft a key created (app.api_key_drafts) or a submission a key made.
const keyWrittenSQL = `(
	EXISTS (SELECT 1 FROM app.api_key_drafts d
	         WHERE d.user_id = k.user_id AND d.message_id = k.id)
	OR EXISTS (SELECT 1 FROM app.submissions s
	            WHERE s.user_id = k.user_id AND s.message_id = k.id AND s.api_key_id IS NOT NULL))`

// SentByAPIKey reports whether sentID is a message an API key wrote or
// submitted (#867), or a Sent copy of one.
//
// The Sent copy normally lands on the SAME row as the submitted draft: the
// appended bytes are the stored bytes, so ingest's findExisting hits the
// content hash (and failing that superseded_content or dedup_hash) and records
// only a provenance row. But a provider that rewrites the body on the way out
// defeats all three and yields a second row, so a row is also recognised by its
// Message-ID matching a key-written message's (the Message-ID is ours, generated
// under the sending identity's domain, so it is a reliable handle).
func (tx *UserTx) SentByAPIKey(ctx context.Context, sentID ulid.ULID) (bool, error) {
	var byKey bool
	err := tx.tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM app.messages k
			 WHERE k.user_id = $1 AND `+keyWrittenSQL+`
			   AND (k.id = $2
			        OR (COALESCE(k.message_id_hint, '') <> ''
			            AND k.message_id_hint = (SELECT m.message_id_hint FROM app.messages m
			                                      WHERE m.user_id = $1 AND m.id = $2))))`,
		tx.userID.String(), sentID.String()).Scan(&byKey)
	if err != nil {
		return false, fmt.Errorf("store: sent by api key: %w", err)
	}
	return byKey, nil
}

// EmitReplyFeedback is RecordReplyFeedback under its own SAVEPOINT, for the
// best-effort seams (ingest, EmailSubmission/set): a failure here must never
// abort the ingest or the send it rides along with.
func (tx *UserTx) EmitReplyFeedback(ctx context.Context, origID, sentID ulid.ULID, source string) (bool, error) {
	var inserted bool
	err := tx.inSavepoint(ctx, func() error {
		var e error
		inserted, e = tx.RecordReplyFeedback(ctx, origID, sentID, source)
		return e
	})
	return inserted, err
}

// RecordRepliesForSent resolves sentID's inbound original and records the reply
// signal on it once. It returns how the resolution went and whether a row was
// written; everything other than ReplyResolved writes nothing.
func (tx *UserTx) RecordRepliesForSent(ctx context.Context, sentID ulid.ULID, source string) (ReplyResolution, bool, error) {
	orig, res, err := tx.ReplyTarget(ctx, sentID)
	if err != nil || res != ReplyResolved {
		return res, false, err
	}
	inserted, err := tx.RecordReplyFeedback(ctx, orig, sentID, source)
	return res, inserted, err
}

// RecordReplySignalsAtIngest is the ingest seam (#842), under its own
// SAVEPOINT so it can never abort the ingest: for a message VERIFIABLY sent by
// the user (arriving into a Sent mailbox — sourceMailbox — or otherwise
// verifiedSentSQL) it records the reply on the inbound original it answers;
// for an inbound message that joined an existing thread it records the reply
// from any of the user's verifiably-sent messages already in that thread that
// name it (the original arriving after the reply). A message whose From merely
// names the user is NOT the user's reply. It returns how many rows it wrote.
func (tx *UserTx) RecordReplySignalsAtIngest(ctx context.Context, msgID, threadID ulid.ULID, joinedThread bool, fromAddress, messageIDHint string, sourceMailbox ulid.ULID) (int, error) {
	written := 0
	err := tx.inSavepoint(ctx, func() error {
		own, err := tx.IsOwnAddress(ctx, fromAddress)
		if err != nil {
			return err
		}
		if own {
			inSent, err := tx.MailboxIsSent(ctx, sourceMailbox)
			if err != nil {
				return err
			}
			orig, res, err := tx.replyTarget(ctx, msgID, inSent)
			if err != nil || res != ReplyResolved {
				return err
			}
			ok, err := tx.RecordReplyFeedback(ctx, orig, msgID, "ingest")
			if ok {
				written++
			}
			return err
		}
		if !joinedThread {
			return nil
		}
		replies, err := tx.OwnRepliesInThread(ctx, threadID, messageIDHint)
		if err != nil {
			return err
		}
		for _, sent := range replies {
			_, ok, err := tx.RecordRepliesForSent(ctx, sent, "ingest")
			if err != nil {
				return err
			}
			if ok {
				written++
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return written, nil
}

// OwnRepliesInThread returns the user's own, non-draft messages in threadID
// whose threading headers name msgHint — the reverse direction of
// RecordRepliesForSent, for an original that is ingested AFTER the reply to it
// (an initial sync that reaches Sent before the Inbox). Thread-scoped so it rides
// the (user_id, thread_id) index rather than scanning every References array.
func (tx *UserTx) OwnRepliesInThread(ctx context.Context, threadID ulid.ULID, msgHint string) ([]ulid.ULID, error) {
	hint := strings.Trim(strings.TrimSpace(msgHint), "<>")
	if hint == "" {
		return nil, nil
	}
	rows, err := tx.tx.Query(ctx, `
		SELECT m.id, COALESCE(m.from_address::text, '')
		  FROM app.messages m
		 WHERE m.user_id = $1 AND m.thread_id = $2 AND NOT m.is_draft
		   AND (m.in_reply_to IN ($3, '<' || $3 || '>')
		        OR $3 = ANY(m.references_ids) OR ('<' || $3 || '>') = ANY(m.references_ids))
		   AND `+verifiedSentSQL+`
		 ORDER BY m.id`,
		tx.userID.String(), threadID.String(), hint)
	if err != nil {
		return nil, fmt.Errorf("store: own replies in thread: %w", err)
	}
	type cand struct{ id, from string }
	var cands []cand
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.id, &c.from); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: own replies in thread (scan): %w", err)
		}
		cands = append(cands, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: own replies in thread (rows): %w", err)
	}
	var out []ulid.ULID
	for _, c := range cands {
		own, err := tx.IsOwnAddress(ctx, c.from)
		if err != nil {
			return nil, err
		}
		if !own {
			continue
		}
		id, err := ulid.Parse(c.id)
		if err != nil {
			return nil, fmt.Errorf("store: own replies in thread: parse id %q: %w", c.id, err)
		}
		out = append(out, id)
	}
	return out, nil
}

// OwnThreadedMessages lists every non-draft message From one of the user's own
// addresses, VERIFIABLY sent by them (verifiedSentSQL), that carries threading
// headers — the population the reply backfill and `priority-report` resolve.
// Spoofed "own" mail in the Inbox is not in it. Ordered by id, so a report is
// stable. UnverifiedOwnThreadedCount counts what the predicate excluded.
//
// Own-ness is decided in Go through IsOwnAddress, once per DISTINCT sender, so
// a wildcard or subaddress identity is honoured exactly as the send path
// honours it rather than re-implemented in SQL.
func (tx *UserTx) OwnThreadedMessages(ctx context.Context) (ids []ulid.ULID, unverified int, err error) {
	rows, err := tx.tx.Query(ctx, `
		SELECT DISTINCT from_address::text
		  FROM app.messages
		 WHERE user_id = $1 AND from_address IS NOT NULL AND NOT is_draft
		   AND (COALESCE(in_reply_to, '') <> '' OR cardinality(references_ids) > 0)`,
		tx.userID.String())
	if err != nil {
		return nil, 0, fmt.Errorf("store: own threaded messages (senders): %w", err)
	}
	var senders []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			rows.Close()
			return nil, 0, fmt.Errorf("store: own threaded messages (scan sender): %w", err)
		}
		senders = append(senders, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("store: own threaded messages (sender rows): %w", err)
	}
	var own []string
	for _, s := range senders {
		ok, err := tx.IsOwnAddress(ctx, s)
		if err != nil {
			return nil, 0, err
		}
		if ok {
			own = append(own, s)
		}
	}
	if len(own) == 0 {
		return nil, 0, nil
	}
	if unverified, err = tx.UnverifiedOwnThreadedCount(ctx, own); err != nil {
		return nil, 0, err
	}
	idRows, err := tx.tx.Query(ctx, `
		SELECT m.id
		  FROM app.messages m
		 WHERE m.user_id = $1 AND m.from_address::text = ANY($2::text[]) AND NOT m.is_draft
		   AND (COALESCE(m.in_reply_to, '') <> '' OR cardinality(m.references_ids) > 0)
		   AND `+verifiedSentSQL+`
		 ORDER BY m.id`,
		tx.userID.String(), own)
	if err != nil {
		return nil, 0, fmt.Errorf("store: own threaded messages: %w", err)
	}
	defer idRows.Close()
	for idRows.Next() {
		var s string
		if err := idRows.Scan(&s); err != nil {
			return nil, 0, fmt.Errorf("store: own threaded messages (scan): %w", err)
		}
		id, err := ulid.Parse(s)
		if err != nil {
			return nil, 0, fmt.Errorf("store: own threaded messages: parse id %q: %w", s, err)
		}
		ids = append(ids, id)
	}
	return ids, unverified, idRows.Err()
}

// SentSubmissionMessages lists the distinct messages the user sent THROUGH
// MAILROOM (a submission in state 'sent'), whether or not the row is still
// flagged a draft — the population the EmailSubmission/set reply seam saw. The
// report counts them separately so "Mailroom's own sends recorded no replies"
// can be told apart from "the user replies from another client".
func (tx *UserTx) SentSubmissionMessages(ctx context.Context) ([]ulid.ULID, error) {
	rows, err := tx.tx.Query(ctx, `
		SELECT DISTINCT message_id FROM app.submissions
		 WHERE user_id = $1 AND state = 'sent'
		 ORDER BY message_id`, tx.userID.String())
	if err != nil {
		return nil, fmt.Errorf("store: sent submission messages: %w", err)
	}
	defer rows.Close()
	var out []ulid.ULID
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, fmt.Errorf("store: sent submission messages (scan): %w", err)
		}
		id, err := ulid.Parse(s)
		if err != nil {
			return nil, fmt.Errorf("store: sent submission messages: parse id %q: %w", s, err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// UnverifiedOwnThreadedCount counts threaded, non-draft messages whose From is
// one of ownAddresses but which are NOT verifiably sent by the user — what
// OwnThreadedMessages refuses to count as replies. Report-only: a non-zero
// number is mail claiming to be from the user that their server never filed
// as sent.
func (tx *UserTx) UnverifiedOwnThreadedCount(ctx context.Context, ownAddresses []string) (int, error) {
	if len(ownAddresses) == 0 {
		return 0, nil
	}
	var n int
	err := tx.tx.QueryRow(ctx, `
		SELECT count(*)::int FROM app.messages m
		 WHERE m.user_id = $1 AND m.from_address::text = ANY($2::text[]) AND NOT m.is_draft
		   AND (COALESCE(m.in_reply_to, '') <> '' OR cardinality(m.references_ids) > 0)
		   AND NOT `+verifiedSentSQL,
		tx.userID.String(), ownAddresses).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: unverified own threaded count: %w", err)
	}
	return n, nil
}

// MessageReceivedAt returns a message's received_at (RLS-scoped).
func (tx *UserTx) MessageReceivedAt(ctx context.Context, msgID ulid.ULID) (time.Time, bool, error) {
	var at time.Time
	err := tx.tx.QueryRow(ctx,
		`SELECT received_at FROM app.messages WHERE user_id = $1 AND id = $2`,
		tx.userID.String(), msgID.String()).Scan(&at)
	if isNoRows(err) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("store: message received at: %w", err)
	}
	return at, true, nil
}

// RecordOpenFeedback records the explicit `open` signal (#842): the client
// showed the message in its reading pane for at least its dwell time. Once per
// message — a re-open adds nothing a per-message label can use. found is false
// when the caller has no such message (RLS makes another user's message
// indistinguishable from a missing one), in which case nothing is written.
func (tx *UserTx) RecordOpenFeedback(ctx context.Context, msgID ulid.ULID) (recorded, found bool, err error) {
	var exists bool
	if err := tx.tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM app.messages WHERE user_id = $1 AND id = $2)`,
		tx.userID.String(), msgID.String()).Scan(&exists); err != nil {
		return false, false, fmt.Errorf("store: open feedback (lookup): %w", err)
	}
	if !exists {
		return false, false, nil
	}
	recorded, err = tx.insertFeedbackOnce(ctx, msgID, FeedbackActionOpen, []byte(`{"source":"client"}`), time.Time{})
	return recorded, true, err
}

// FeedbackActionCounts counts the caller's feedback events per action since
// `since` (zero = all time). For reports: one number per signal source.
func (tx *UserTx) FeedbackActionCounts(ctx context.Context, since time.Time) (map[string]int, error) {
	rows, err := tx.tx.Query(ctx, `
		SELECT action, count(*)::int
		  FROM app.feedback_events
		 WHERE user_id = $1 AND created_at >= $2
		 GROUP BY action`,
		tx.userID.String(), since.UTC())
	if err != nil {
		return nil, fmt.Errorf("store: feedback action counts: %w", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var a string
		var n int
		if err := rows.Scan(&a, &n); err != nil {
			return nil, fmt.Errorf("store: feedback action counts (scan): %w", err)
		}
		out[a] = n
	}
	return out, rows.Err()
}

// ---- model registry: revert and lookup ---------------------------------------

// DeactivateModelKind clears the active flag on the caller's model of kind, so
// the registry falls back to its built-in default (the cold-start heuristic for
// priority). Rows are never deleted: the reverted model stays in the lineage for
// audit and a deliberate re-activation. It returns how many rows it changed (0
// or 1, by the one-active index).
func (tx *UserTx) DeactivateModelKind(ctx context.Context, kind string) (int, error) {
	tag, err := tx.tx.Exec(ctx, `
		UPDATE app.ml_models SET active = false
		 WHERE user_id = $1 AND model_kind = $2 AND active`,
		tx.userID.String(), kind)
	if err != nil {
		return 0, fmt.Errorf("store: deactivate model kind: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// ModelVersionRow returns one (kind, version)'s artifact hash and metadata, for
// a report that scores a specific version without activating it.
func (tx *UserTx) ModelVersionRow(ctx context.Context, kind string, version int) (hash []byte, active bool, createdAt time.Time, found bool, err error) {
	err = tx.tx.QueryRow(ctx, `
		SELECT artifact_hash, active, created_at FROM app.ml_models
		 WHERE user_id = $1 AND model_kind = $2 AND version = $3`,
		tx.userID.String(), kind, version).Scan(&hash, &active, &createdAt)
	if isNoRows(err) {
		return nil, false, time.Time{}, false, nil
	}
	if err != nil {
		return nil, false, time.Time{}, false, fmt.Errorf("store: model version row: %w", err)
	}
	return hash, active, createdAt, true, nil
}

// ---- the gate's decision log --------------------------------------------------

// PriorityGateAuditType is the audit_log object_type of a promotion-gate
// decision. No migration: audit_log is user-scoped already, the same choice the
// placement backfill journal made.
const PriorityGateAuditType = "ml_model.priority"

// PriorityGateDecision is one logged gate verdict.
type PriorityGateDecision struct {
	Action string // priority.gate.promote | priority.gate.refuse | priority.gate.revert
	At     time.Time
	Detail json.RawMessage
}

// RecordPriorityGateDecision appends one gate verdict, with its numbers, to the
// user's audit log. action is one of the PriorityGateDecision.Action values.
func (tx *UserTx) RecordPriorityGateDecision(ctx context.Context, action string, version int, detail any) error {
	raw, err := json.Marshal(detail)
	if err != nil {
		return fmt.Errorf("store: priority gate audit: %w", err)
	}
	if _, err := tx.tx.Exec(ctx, `
		INSERT INTO app.audit_log (user_id, actor, action, object_type, object_id, detail)
		VALUES ($1, 'system', $2, $3, $4, $5)`,
		tx.userID.String(), action, PriorityGateAuditType, fmt.Sprintf("priority:%d", version), raw); err != nil {
		return fmt.Errorf("store: priority gate audit: %w", err)
	}
	return nil
}

// PriorityGateDecisions returns the user's most recent gate verdicts, newest
// first.
func (tx *UserTx) PriorityGateDecisions(ctx context.Context, limit int) ([]PriorityGateDecision, error) {
	if limit <= 0 {
		limit = 10
	}
	rows, err := tx.tx.Query(ctx, `
		SELECT action, created_at, detail FROM app.audit_log
		 WHERE user_id = $1 AND object_type = $2
		 ORDER BY created_at DESC, id DESC
		 LIMIT $3`,
		tx.userID.String(), PriorityGateAuditType, limit)
	if err != nil {
		return nil, fmt.Errorf("store: priority gate decisions: %w", err)
	}
	defer rows.Close()
	var out []PriorityGateDecision
	for rows.Next() {
		var d PriorityGateDecision
		var raw []byte
		if err := rows.Scan(&d.Action, &d.At, &raw); err != nil {
			return nil, fmt.Errorf("store: priority gate decisions (scan): %w", err)
		}
		d.Detail = json.RawMessage(raw)
		out = append(out, d)
	}
	return out, rows.Err()
}

// ---- rescore ------------------------------------------------------------------

// ScoredMessage is one message with a stored priority score.
type ScoredMessage struct {
	ID           ulid.ULID
	Score        float64 // as stored (real), widened
	ModelVersion string
}

// ScoredMessagesSince pages (by id) through the caller's messages received at
// or after since that already carry a triage score — the population ingest
// scored, which is what `priority-rescore` re-scores. A message that was never
// scored (an archive import) is deliberately not given a score here.
func (tx *UserTx) ScoredMessagesSince(ctx context.Context, since time.Time, after ulid.ULID, limit int) ([]ScoredMessage, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := tx.tx.Query(ctx, `
		SELECT m.id, ts.score::double precision, ts.model_version
		  FROM app.messages m
		  JOIN app.triage_scores ts ON ts.user_id = m.user_id AND ts.message_id = m.id
		 WHERE m.user_id = $1 AND m.received_at >= $2 AND m.id > $3
		 ORDER BY m.id
		 LIMIT $4`,
		tx.userID.String(), since.UTC(), after.String(), limit)
	if err != nil {
		return nil, fmt.Errorf("store: scored messages since: %w", err)
	}
	defer rows.Close()
	var out []ScoredMessage
	for rows.Next() {
		var s ScoredMessage
		var id string
		if err := rows.Scan(&id, &s.Score, &s.ModelVersion); err != nil {
			return nil, fmt.Errorf("store: scored messages since (scan): %w", err)
		}
		if s.ID, err = ulid.Parse(id); err != nil {
			return nil, fmt.Errorf("store: scored messages since: parse id %q: %w", id, err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// TriageScoreWouldChange reports, WITHOUT WRITING, whether RescoreTriage with
// these values would change the stored row. It is the same comparison
// RescoreTriage makes, in SQL, so a dry run and an apply cannot disagree about
// what "changed" means (#155's preview rule).
func (tx *UserTx) TriageScoreWouldChange(ctx context.Context, msgID ulid.ULID, score float64, modelVersion string, topFeatures []byte) (bool, error) {
	if len(topFeatures) == 0 {
		topFeatures = []byte("[]")
	}
	var changed bool
	err := tx.tx.QueryRow(ctx, `
		SELECT NOT EXISTS (
			SELECT 1 FROM app.triage_scores
			 WHERE user_id = $1 AND message_id = $2
			   AND (score, model_version, top_features)
			       IS NOT DISTINCT FROM ($3::real, $4::text, $5::jsonb))`,
		tx.userID.String(), msgID.String(), score, modelVersion, string(topFeatures)).Scan(&changed)
	if err != nil {
		return false, fmt.Errorf("store: triage score would change: %w", err)
	}
	return changed, nil
}

// StoredScore is what a float64 score becomes once written to the `real`
// column — the value every SQL comparison (Email/query's isImportant filter, the
// threshold sort) actually sees. A dry run that predicts bands must round the
// same way, or a score of exactly the threshold lands on different sides of it
// in the report and on the screen.
func StoredScore(score float64) float64 { return float64(float32(score)) }

// TriageScoreSummary is the SERVED score distribution of one model version:
// what the user actually saw, as opposed to a recomputation.
type TriageScoreSummary struct {
	ModelVersion string
	Messages     int
	Important    int // stored score >= 0.6
	AtLeast04    int
	MaxScore     float64
	LastScored   time.Time
}

// TriageScoreSummaryByModel summarises stored scores of messages received at
// or after since, per model_version. Read-only.
func (tx *UserTx) TriageScoreSummaryByModel(ctx context.Context, since time.Time) ([]TriageScoreSummary, error) {
	rows, err := tx.tx.Query(ctx, `
		SELECT ts.model_version, count(*)::int,
		       count(*) FILTER (WHERE ts.score >= 0.6)::int,
		       count(*) FILTER (WHERE ts.score >= 0.4)::int,
		       max(ts.score)::double precision, max(ts.scored_at)
		  FROM app.triage_scores ts
		  JOIN app.messages m ON m.user_id = ts.user_id AND m.id = ts.message_id
		 WHERE ts.user_id = $1 AND m.received_at >= $2
		 GROUP BY ts.model_version
		 ORDER BY ts.model_version`,
		tx.userID.String(), since.UTC())
	if err != nil {
		return nil, fmt.Errorf("store: triage score summary: %w", err)
	}
	defer rows.Close()
	var out []TriageScoreSummary
	for rows.Next() {
		var s TriageScoreSummary
		if err := rows.Scan(&s.ModelVersion, &s.Messages, &s.Important, &s.AtLeast04, &s.MaxScore, &s.LastScored); err != nil {
			return nil, fmt.Errorf("store: triage score summary (scan): %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
