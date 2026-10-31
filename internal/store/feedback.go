// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"fmt"

	"github.com/oklog/ulid/v2"
)

// This file is the WRITE side of the feedback loop (spec §8, mailroom#33 PR2):
// capturing the user actions the T1 priority model learns from into
// app.feedback_events. The READ side (ListFeedbackEvents, for training/eval)
// lives in ml_models.go.
//
// app.feedback_events is OPERATIONAL data — the SAME category as
// app.triage_scores and app.ml_models, NOT a JMAP-visible object. A write here
// appends NO change_log row and feedback_events is NOT a store ObjectType
// (CLAUDE.md's load-bearing invariant is about JMAP-visible objects only).
// Nothing here is ever synced via /changes, the indexer, or a client cursor.
//
// Every method runs inside a UserTx, so RLS (app.feedback_events carries FORCE
// ROW LEVEL SECURITY) scopes each statement to the caller — no identity, no
// rows (fail closed).

// InsertFeedbackEvent records one observed user action on a message: a plain
// insert of (ULID PK, user_id, message_id, action, context jsonb, created_at).
// It appends NO change_log row (feedback is operational data — see the file
// header).
//
// The schema allows a message-less action (message_id is nullable), but every
// action Mailroom currently emits is tied to a specific message, so a zero msgID
// is rejected rather than written as a NULL that no training path could use.
// contextJSON is the optional explainability payload (reply latency, surface,
// …); an empty/nil argument becomes the column's default-shaped empty object.
//
// No ::app.ulid casts are needed: like UpsertTriageScore, each id param is used
// exactly ONCE, as a VALUES target for its column, so pgx infers the column type
// directly (contrast InsertModelVersion / EnsureRecurringUserJob, where a param
// reused in a WHERE/NOT EXISTS compare forced the cast to avoid SQLSTATE 42P08).
func (tx *UserTx) InsertFeedbackEvent(ctx context.Context, msgID ulid.ULID, action string, contextJSON []byte) error {
	if msgID == (ulid.ULID{}) {
		return fmt.Errorf("store: insert feedback event: message id is required")
	}
	if action == "" {
		return fmt.Errorf("store: insert feedback event: action is required")
	}
	if len(contextJSON) == 0 {
		contextJSON = []byte("{}")
	}
	id := NewULID()
	if _, err := tx.tx.Exec(ctx, `
		INSERT INTO app.feedback_events (id, user_id, message_id, action, context, created_at)
		VALUES ($1, $2, $3, $4, $5::jsonb, now())`,
		id.String(), tx.userID.String(), msgID.String(), action, string(contextJSON)); err != nil {
		return fmt.Errorf("store: insert feedback event: %w", err)
	}
	return nil
}

// EmitFeedbackEvent is the SAVEPOINT-GUARDED, best-effort emitter the API seams
// (Email/set, EmailSubmission/set) call from INSIDE the user's mutation UserTx.
// Capturing feedback is a side effect of the user's action, and the user's
// action always wins: a feedback-insert failure must NEVER abort the surrounding
// Email/set or submission. So the insert runs under its own nested SAVEPOINT
// (the recoverable-failure pattern of labels.go / ApplyRuleActions) — on failure
// it rolls back to the savepoint, leaving the outer tx fully usable, and returns
// the error for the caller to log-and-continue. Without the savepoint a failed
// insert would poison the whole transaction (SQLSTATE 25P02) and lose the user's
// actual mutation.
func (tx *UserTx) EmitFeedbackEvent(ctx context.Context, msgID ulid.ULID, action string, contextJSON []byte) error {
	return tx.inSavepoint(ctx, func() error {
		return tx.InsertFeedbackEvent(ctx, msgID, action, contextJSON)
	})
}

// MessageSeen reports whether a message is currently marked read ($seen). The
// Email/set feedback seam uses it to decide archive_unread vs. a (skipped)
// already-read archive: an archive of unread mail is a real down-rank signal,
// while archiving mail you already read is weak and not emitted. found is false
// when the message does not exist for the caller.
func (tx *UserTx) MessageSeen(ctx context.Context, msgID ulid.ULID) (seen, found bool, err error) {
	err = tx.tx.QueryRow(ctx,
		`SELECT is_seen FROM app.messages WHERE user_id = $1 AND id = $2`,
		tx.userID.String(), msgID.String()).Scan(&seen)
	if isNoRows(err) {
		return false, false, nil
	}
	if err != nil {
		return false, false, fmt.Errorf("store: message seen: %w", err)
	}
	return seen, true, nil
}

// OriginalMessageForSend resolves the ORIGINAL message a just-sent draft answers,
// so a reply/forward's positive feedback is keyed to the mail that prompted it
// rather than to the outgoing draft. The link is the draft's threading headers:
// its In-Reply-To (the direct parent), falling back to the last References entry
// (the immediate ancestor), matched against a stored message's Message-ID
// (message_id_hint). ok is false when the draft carries no usable header or the
// referenced message is not one this user has — the caller then SKIPS emitting,
// rather than guessing a target (spec §8: a mis-keyed signal is worse than none).
//
// A note on reply vs. forward: RFC 5322 gives a reply a header link (In-Reply-To)
// but a forward none — a forwarded message is a fresh composition that re-attaches
// the original, with no standard header tying it back (api/draft.go). So the
// server cannot reliably distinguish the two, and it does not try: both are
// POSITIVE engagement (LabelForAction maps reply and forward to 1.0 alike), so the
// LABEL is identical either way and the emitter uses the reply token as the
// representative "I acted on this mail by sending about it" signal.
func (tx *UserTx) OriginalMessageForSend(ctx context.Context, draftID ulid.ULID) (ulid.ULID, bool, error) {
	var (
		inReplyTo string
		refs      []string
	)
	err := tx.tx.QueryRow(ctx, `
		SELECT COALESCE(in_reply_to, ''), references_ids
		  FROM app.messages
		 WHERE user_id = $1 AND id = $2`,
		tx.userID.String(), draftID.String()).Scan(&inReplyTo, &refs)
	if err != nil {
		if isNoRows(err) {
			return ulid.ULID{}, false, nil
		}
		return ulid.ULID{}, false, fmt.Errorf("store: original message for send (read draft): %w", err)
	}

	// Same resolution as the ingest/backfill path (priority_signals.go): the
	// In-Reply-To parent, else the nearest References ancestor, SKIPPING the
	// user's own mail — a reply in a thread whose last message is your own
	// follow-up engages with the inbound message before it, not with yourself
	// (#842). It used to take the first header that resolved to anything.
	orig, res, err := tx.resolveInboundParent(ctx, draftID, inReplyTo, refs)
	if err != nil {
		return ulid.ULID{}, false, err
	}
	return orig, res == ReplyResolved, nil
}
