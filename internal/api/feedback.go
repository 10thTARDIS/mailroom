// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"log/slog"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/auth"
	"mailroom/internal/store"
)

// Triage feedback capture (spec §8, mailroom#33 PR2). The T1 priority model
// LEARNS from the actions a user takes on their mail, and this is where those
// actions are turned into app.feedback_events rows — as a SIDE EFFECT of the
// Email/set and EmailSubmission/set mutations the user already performs, never a
// separate JMAP method (feedback has no request surface of its own).
//
// Two invariants shape every emit:
//
//   - MAIL-SAFE: the emit runs INSIDE the mutation's UserTx but under a nested
//     SAVEPOINT (store.EmitFeedbackEvent), so a feedback-insert failure can never
//     abort the Email/set or the send. The user's action always wins; capturing
//     the signal is best-effort and log-and-continue.
//   - ONE ROW PER ACTION: a single Email/set patch is classified into at most one
//     representative signal, keyed to the affected message, so a mark-read does
//     not also count as (say) an archive.
//
// The action tokens below MUST match ml/eval.go's LabelForAction map exactly (an
// unmapped token is silently skipped by the eval/training harness, so a typo here
// would quietly drop the signal). feedback_action_test.go cross-checks them.
const (
	// A mark-read emits NOTHING (#842). It used to emit mark_read_no_open, a
	// negative, for every mark-read — including the one the web client makes
	// when a message is OPENED, so reading a message taught the model it did
	// not matter (most events on a real instance). The server cannot
	// tell an open from a bulk mark-read; the explicit signal is now
	// Triage/open (triage_open.go), sent after the reading-pane dwell, and a
	// bulk dismissal of unread mail is already archive_unread.
	//
	// feedbackFlag (positive): the user starred/flagged the message.
	feedbackFlag = "flag"
	// feedbackArchiveUnread (negative): the user archived mail they had not read —
	// a real "did not matter" signal. Archiving already-read mail is weak and is
	// deliberately NOT emitted.
	feedbackArchiveUnread = "archive_unread"
	// feedbackTrash (negative): the user trashed the message.
	feedbackTrash = "trash"
	// feedbackReply (positive): the user replied to (or forwarded) the ORIGINAL
	// message a send answers. Reply and forward are indistinguishable server-side
	// (a forward carries no header link) and map to the same positive label, so
	// this one token stands for both — see store.OriginalMessageForSend.
	feedbackReply = "reply"
)

// emitEmailSetFeedback classifies a committed Email/set update into at most one
// triage signal and records it best-effort, keyed to the affected message. It is
// called from INSIDE the Email/set UserTx, AFTER ApplyEmailUpdate applied the
// user's mutation, and its write is SAVEPOINT-guarded, so a feedback failure can
// never roll back the mark-read / star / archive / trash the user asked for.
func (a *API) emitEmailSetFeedback(ctx context.Context, tx *store.UserTx, msgID ulid.ULID, up store.EmailUpdate) {
	// #867: an AI-tool key's actions still happen but never teach the
	// priority model what the USER finds important. Mirrors Triage/open, which
	// is sessionOnly in agent_scopes.go for the same reason.
	if _, isKey := auth.APIKeyIDFromContext(ctx); isKey {
		return
	}
	token, ok, err := feedbackTokenForEmailUpdate(ctx, tx, msgID, up)
	if err != nil {
		slog.Default().Warn("triage feedback: could not classify Email/set action",
			"message_id", msgID.String(), "err", err)
		return
	}
	if !ok {
		return
	}
	if err := tx.EmitFeedbackEvent(ctx, msgID, token, nil); err != nil {
		slog.Default().Warn("triage feedback: emit failed (user mutation preserved)",
			"action", token, "message_id", msgID.String(), "err", err)
	}
}

// feedbackTokenForEmailUpdate maps one parsed Email/set update to its single
// representative feedback token, in a fixed precedence so a compound patch still
// yields one signal: trash > archive-move > flag. A mark-read yields nothing
// (#842, see the const block), and so does an unset/negated change (Seen false,
// unflag, a restore).
func feedbackTokenForEmailUpdate(ctx context.Context, tx *store.UserTx, msgID ulid.ULID, up store.EmailUpdate) (string, bool, error) {
	switch {
	case up.Trash:
		return feedbackTrash, true, nil
	case up.MoveToRole != "" || up.MoveToMailboxID != nil:
		// A folder move (archive et al.). Only an archive of UNREAD mail is a real
		// down-rank; archiving already-read mail is weak signal and is skipped.
		seen, found, err := tx.MessageSeen(ctx, msgID)
		if err != nil {
			return "", false, err
		}
		if found && !seen {
			return feedbackArchiveUnread, true, nil
		}
		return "", false, nil
	case up.Flagged != nil && *up.Flagged:
		return feedbackFlag, true, nil
	}
	return "", false, nil
}

// emitSendFeedback records the positive engagement of sending a reply/forward,
// keyed to the ORIGINAL message it answers (resolved from the draft's threading
// headers). It is called from INSIDE the EmailSubmission/set UserTx after a send
// is scheduled; the write is SAVEPOINT-guarded so it can never abort the send. A
// send that answers no resolvable stored message (a fresh compose, or a reply to
// mail this instance never ingested) emits nothing rather than guessing a target.
func (a *API) emitSendFeedback(ctx context.Context, tx *store.UserTx, draftID ulid.ULID) {
	// #867: an AI-tool key's actions still happen but never teach the
	// priority model what the USER finds important. Mirrors Triage/open, which
	// is sessionOnly in agent_scopes.go for the same reason.
	if _, isKey := auth.APIKeyIDFromContext(ctx); isKey {
		return
	}
	origID, ok, err := tx.OriginalMessageForSend(ctx, draftID)
	if err != nil {
		slog.Default().Warn("triage feedback: could not resolve the message a send answers",
			"draft_id", draftID.String(), "err", err)
		return
	}
	if !ok {
		return
	}
	// Once per original (#842): the same send can also reach the store as a
	// Sent copy that ingest or the backfill resolves to this original.
	if _, err := tx.EmitReplyFeedback(ctx, origID, draftID, "submission"); err != nil {
		slog.Default().Warn("triage feedback: reply emit failed (send preserved)",
			"message_id", origID.String(), "err", err)
	}
}
