// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"
)

// THE DRAFTS AN API KEY WROTE, AND WHAT IT MAY STILL DO WITH THEM (#872).
//
// app.api_key_drafts records every draft a key's Email/set created (ADR-042
// §3): a key sends only those. #872 adds two things a key does with its OWN
// unsent drafts, both keyed off that same table and nothing else:
//
//   - a repeat of a draft it saved moments ago is collapsed into that draft
//     (RecentAPIKeyDraft, under LockAPIKeyDrafts), because a model that does
//     not understand why it cannot send saves the same draft again and again;
//   - it may replace one (draft_update) or discard one (draft_discard), which
//     needs to know the draft is the key's, still an unsent draft in Drafts,
//     and not waiting on a person's approval to be sent (APIKeyDraftStatus).
//
// This file reads and locks; the writes those decisions lead to are the
// ordinary Email/set paths (SaveDraft, ApplyEmailUpdate), which append the
// change_log rows. Nothing here mutates a JMAP-visible object.

// KeyDraftStatus is what one message is to one API key's draft tools.
type KeyDraftStatus struct {
	// Exists: the message is the user's (RLS) at all.
	Exists bool
	// Own: this key wrote it (app.api_key_drafts).
	Own bool
	// IsDraft: it carries $draft.
	IsDraft bool
	// Trashed: it is in Trash (trashed_at).
	Trashed bool
	// InDrafts: it is effectively in a Drafts-role mailbox (the RetireDraft
	// predicate). Read only for a draft; false otherwise.
	InDrafts bool
	// Submitted: it has a submission row (sent, scheduled, or in Undo Send).
	Submitted bool
	// SendPending: a send of it is waiting for, or has just been given, a
	// person's approval (app.api_key_pending, kind 'send', still to run).
	SendPending bool
	// The draft's threading, which a replacement inherits.
	InReplyTo  string
	References []string
}

// Live reports whether it is still an unsent draft where drafts live.
func (s KeyDraftStatus) Live() bool {
	return s.Exists && s.IsDraft && !s.Trashed && s.InDrafts && !s.Submitted
}

// Usable reports whether the key may replace or discard the draft.
func (s KeyDraftStatus) Usable() bool { return s.Own && s.Live() && !s.SendPending }

// LockAPIKeyDrafts serialises one key's draft writes (creates, replacements,
// discards, sends) for the rest of the transaction, so two calls that
// race cannot both pass a check the other is about to falsify — two identical
// creates both missing each other's draft, or a discard and a send of the same
// draft (#872). A transaction-scoped advisory lock, as feedbackOnceLock: no
// migration, and released by the commit that makes the first write visible.
//
// LOCK ORDER: take it BEFORE anything that locks the key's app.api_keys row
// (an app.api_key_drafts insert does, through its foreign key), and never in
// a transaction that already holds that row (the admission transaction).
func (tx *UserTx) LockAPIKeyDrafts(ctx context.Context, keyID ulid.ULID) error {
	key := "mailroom.key_drafts:" + tx.userID.String() + ":" + keyID.String()
	if _, err := tx.tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, key); err != nil {
		return fmt.Errorf("store: lock key drafts: %w", err)
	}
	return nil
}

// RecentAPIKeyDraft finds the newest draft THIS key wrote within window whose
// content fingerprint is fp and which is still a live draft: $draft, not in
// Trash, not submitted, and effectively in a Drafts-role mailbox. found is
// false when there is none (or fp is empty). The window is measured on the
// database clock, which stamped created_at.
func (tx *UserTx) RecentAPIKeyDraft(ctx context.Context, keyID ulid.ULID, fp []byte, window time.Duration) (ulid.ULID, bool, error) {
	if len(fp) == 0 {
		return ulid.ULID{}, false, nil
	}
	rows, err := tx.tx.Query(ctx, `
		SELECT d.message_id
		  FROM app.api_key_drafts d
		  JOIN app.messages m ON m.user_id = d.user_id AND m.id = d.message_id
		 WHERE d.user_id = $1 AND d.key_id = $2 AND d.content_fingerprint = $3
		   AND d.created_at > now() - $4::float8 * interval '1 second'
		   AND m.is_draft AND m.trashed_at IS NULL
		   AND NOT EXISTS (SELECT 1 FROM app.submissions s WHERE s.user_id = m.user_id AND s.message_id = m.id)
		 ORDER BY d.created_at DESC, d.message_id DESC
		 LIMIT 5`,
		tx.userID.String(), keyID.String(), fp, window.Seconds())
	if err != nil {
		return ulid.ULID{}, false, fmt.Errorf("store: recent key draft: %w", err)
	}
	var ids []ulid.ULID
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			rows.Close()
			return ulid.ULID{}, false, fmt.Errorf("store: scan recent key draft: %w", err)
		}
		id, err := ulid.Parse(s)
		if err != nil {
			rows.Close()
			return ulid.ULID{}, false, fmt.Errorf("store: parse recent key draft: %w", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return ulid.ULID{}, false, fmt.Errorf("store: recent key draft: %w", err)
	}
	// The mailbox half is inDraftsMailbox, the predicate RetireDraft applies,
	// checked per row because a mailbox's role is read from its name when the
	// server sets no special-use (roleForMailbox). A draft the owner moved out
	// of Drafts is not "the draft in Drafts" the key is told about.
	for _, id := range ids {
		in, err := tx.inDraftsMailbox(ctx, id)
		if err != nil {
			return ulid.ULID{}, false, err
		}
		if in {
			return id, true, nil
		}
	}
	return ulid.ULID{}, false, nil
}

// APIKeyDraftStatus reads what msgID is to keyID's draft tools. A message the
// user does not have (or another user's: RLS) is Exists=false.
func (tx *UserTx) APIKeyDraftStatus(ctx context.Context, keyID, msgID ulid.ULID) (KeyDraftStatus, error) {
	var (
		s        KeyDraftStatus
		inReply  *string
		ownerKey *string
	)
	err := tx.tx.QueryRow(ctx, `
		SELECT m.is_draft, m.trashed_at IS NOT NULL,
		       EXISTS (SELECT 1 FROM app.submissions s WHERE s.user_id = m.user_id AND s.message_id = m.id),
		       EXISTS (SELECT 1 FROM app.api_key_pending p
		                WHERE p.user_id = m.user_id AND p.kind = 'send'
		                  AND p.payload->>'draftId' = m.id::text
		                  AND ((p.state = 'pending' AND p.expires_at > now())
		                       OR (p.state = 'approved' AND p.ran_at IS NULL))),
		       m.in_reply_to, m.references_ids,
		       (SELECT d.key_id::text FROM app.api_key_drafts d WHERE d.user_id = m.user_id AND d.message_id = m.id)
		  FROM app.messages m
		 WHERE m.user_id = $1 AND m.id = $2`,
		tx.userID.String(), msgID.String()).Scan(&s.IsDraft, &s.Trashed, &s.Submitted, &s.SendPending,
		&inReply, &s.References, &ownerKey)
	if isNoRows(err) {
		return KeyDraftStatus{}, nil
	}
	if err != nil {
		return KeyDraftStatus{}, fmt.Errorf("store: key draft status: %w", err)
	}
	s.Exists = true
	s.Own = ownerKey != nil && *ownerKey == keyID.String()
	if inReply != nil {
		s.InReplyTo = *inReply
	}
	if s.IsDraft {
		if s.InDrafts, err = tx.inDraftsMailbox(ctx, msgID); err != nil {
			return KeyDraftStatus{}, err
		}
	}
	return s, nil
}
