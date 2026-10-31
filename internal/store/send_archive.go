// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/oklog/ulid/v2"
)

// SEND & ARCHIVE, SERVER-SIDE (#648, migration 00059).
//
// v1.80.0 archived the conversation from the client the moment
// EmailSubmission/set ACCEPTED the send. Acceptance is not sending: the
// submission is queued behind the Undo-Send window and can still fail. On the
// in practice the archive landed seconds after the click and the
// provider then refused the reply for good (550 5.7.1) — the conversation was
// filed away while the reply never left.
//
// So the archive now runs here, called by the send worker (internal/compose
// send.go) INSIDE the transaction that records the submission as 'sent', and
// nowhere else. Failed, cancelled and exhausted submissions never reach that
// transaction, and this function additionally refuses to act on a submission
// the database does not say is sent (ErrSubmissionNotSent) — so an archive for
// an unsent message needs two independent mistakes, not one.

// ErrSubmissionNotSent is returned by ArchiveAfterSend for a submission whose
// stored state is not 'sent'. Only the send worker calls it, immediately after
// FinishSubmission(sent) in the same transaction, so seeing this means a caller
// is trying to archive for a send that has not happened.
var ErrSubmissionNotSent = errors.New("store: submission is not sent; refusing to archive for it")

// SendArchiveResult is what became of ONE message a sent submission asked to
// archive. Reported per message, never as one total (CLAUDE.md): "nothing
// archived" must be distinguishable as "it had already left the Inbox" versus
// "it is gone" versus "the account has no Archive folder".
type SendArchiveResult string

const (
	// SendArchived: moved to Archive through ApplyEmailUpdate (moved_mailbox_id,
	// the per-account writeback move intent, one Email change_log row).
	SendArchived SendArchiveResult = "archived"
	// SendArchiveGone: the message does not resolve for this user — deleted
	// since the send was queued, or never this user's (RLS makes the two
	// indistinguishable, deliberately).
	SendArchiveGone SendArchiveResult = "gone"
	// SendArchiveNotInInbox: the message is no longer effectively in an Inbox
	// (the user filed, trashed or archived it while the send was queued). Not an
	// error: the user has already dealt with it, and moving it again would undo
	// their choice. The client's own "is the parent in the Inbox" is advisory;
	// this is the check that decides.
	SendArchiveNotInInbox SendArchiveResult = "notInInbox"
	// SendArchiveNoArchiveFolder: none of the message's live accounts has a
	// mailbox with the Archive role — the same "nothing to do" the client's
	// archive action and the drawer sweep report.
	SendArchiveNoArchiveFolder SendArchiveResult = "noArchiveFolder"
	// SendArchiveFailed: the attempt errored. Rolled back to its own SAVEPOINT,
	// so the send's record and the other messages' archives still commit.
	SendArchiveFailed SendArchiveResult = "failed"
)

// SendArchiveOutcome is one message's result, with the error for Failed.
type SendArchiveOutcome struct {
	MessageID ulid.ULID
	Result    SendArchiveResult
	Err       error
}

// inInboxSQL is "message m is effectively in an Inbox" — any mailbox with the
// inbox role (roleForMailbox: the reserved INBOX name, or an `\Inbox`
// SPECIAL-USE) — through mailboxMembershipSQL, the SAME membership derivation
// every other view uses: the canonical placement (moved_mailbox_id) first,
// then live provenance, then the local placement. Three-valued-logic-free for
// the same reason inMailboxSQL is.
const inInboxSQL = `IN (SELECT ib.id FROM app.mailboxes ib
	                 WHERE ib.user_id = m.user_id
	                   AND (upper(ib.imap_name) = 'INBOX' OR lower(ltrim(ib.special_use, '\')) = 'inbox'))`

// ArchiveAfterSend archives what a SENT submission asked to archive
// (archive_on_success), in the caller's transaction, and reports per message.
//
// ⚠ WHY THE CALLER'S TRANSACTION, AND WHY SAVEPOINTS. The send worker calls
// this right after FinishSubmission(sent), in the same UserTx:
//
//   - Same transaction: the archive commits if and only if the 'sent' record
//     does. A crash between two separate transactions would leave a sent reply
//     whose conversation never archives, with nothing left to retry it.
//   - Its own SAVEPOINT for the whole call and for EACH message: the send has
//     already happened on the wire, and nothing about archiving may roll back
//     the record of it. A failed archive rolls back to its savepoint, is
//     reported as SendArchiveFailed, and the transaction stays usable.
//
// The load-bearing invariant holds by construction: the archive IS
// ApplyEmailUpdate{MoveToRole: "archive"} — the drawer sweep's server-side
// archive, and what Email/set does for a move into the archive role — which
// writes moved_mailbox_id, enqueues the per-account writeback move intent and
// appends the Email change_log row together. A savepoint rollback takes all
// three with it.
//
// The returned error is only for a failure to READ the request (the whole call
// then rolled back to its savepoint, having written nothing) or for
// ErrSubmissionNotSent; per-message trouble is in the outcomes.
func (tx *UserTx) ArchiveAfterSend(ctx context.Context, submissionID ulid.ULID) ([]SendArchiveOutcome, error) {
	var out []SendArchiveOutcome
	err := tx.inSavepoint(ctx, func() error {
		var (
			state   string
			archive []string
			byKey   bool
		)
		err := tx.tx.QueryRow(ctx, `
			SELECT state, archive_on_success::text[], api_key_id IS NOT NULL
			  FROM app.submissions
			 WHERE user_id = $1 AND id = $2`,
			tx.userID.String(), submissionID.String()).Scan(&state, &archive, &byKey)
		if err != nil {
			if isNoRows(err) {
				return ErrSubmissionNotFound
			}
			return fmt.Errorf("store: archive after send (read): %w", err)
		}
		// THE SECOND, INDEPENDENT GUARD. The worker only calls this after
		// recording the send; this refuses on the database's word, not the
		// caller's, so a future caller on a failure path archives nothing.
		if state != SubmissionSent {
			return fmt.Errorf("%w (state %q)", ErrSubmissionNotSent, state)
		}
		for _, raw := range archive {
			id, perr := ulid.Parse(raw)
			if perr != nil {
				// Cannot happen: the column's domain CHECK admits only ULIDs.
				return fmt.Errorf("store: archive after send: bad id %q: %w", raw, perr)
			}
			var res SendArchiveResult
			aerr := tx.inSavepoint(ctx, func() error {
				var e error
				res, e = tx.archiveOneAfterSend(ctx, id, byKey)
				return e
			})
			o := SendArchiveOutcome{MessageID: id, Result: res}
			if aerr != nil {
				o.Result, o.Err = SendArchiveFailed, aerr
			}
			out = append(out, o)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// archiveOneAfterSend decides and performs one message's archive.
//
// byKey: an API key made the send (#49), so the archive is the key's
// (AgentLabelSource) — the same update, attributed so that it keeps no learned
// placement on the user's behalf and its writeback intent says 'agent'.
func (tx *UserTx) archiveOneAfterSend(ctx context.Context, id ulid.ULID, byKey bool) (SendArchiveResult, error) {
	args := []any{tx.userID.String(), id.String()}
	var exists, inInbox bool
	if err := tx.tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM app.messages m WHERE m.user_id = $1 AND m.id = $2),
		       EXISTS (SELECT 1 FROM app.messages m
		                WHERE m.user_id = $1 AND m.id = $2
		                  AND m.trashed_at IS NULL AND m.vanished_at IS NULL
		                  AND `+mailboxMembershipSQL("$1", inInboxSQL)+`)`,
		args...).Scan(&exists, &inInbox); err != nil {
		return "", fmt.Errorf("store: archive after send (inbox check): %w", err)
	}
	if !exists {
		return SendArchiveGone, nil
	}
	if !inInbox {
		return SendArchiveNotInInbox, nil
	}

	// The same reachability check ArchiveByDrawer makes: without an Archive
	// mailbox on any live account, ApplyEmailUpdate would still enqueue an
	// intent that can only fail, and "archived" would be a claim about a move
	// that has nowhere to go.
	accounts, err := tx.liveAccountIDs(ctx, id)
	if err != nil {
		return "", err
	}
	hasArchive := false
	for _, acct := range accounts {
		_, found, ferr := tx.MailboxForRole(ctx, acct, "archive")
		if ferr != nil {
			return "", ferr
		}
		if found {
			hasArchive = true
			break
		}
	}
	if !hasArchive {
		return SendArchiveNoArchiveFolder, nil
	}

	up := EmailUpdate{MoveToRole: "archive"}
	if byKey {
		up.LabelSource = AgentLabelSource()
	}
	found, _, err := tx.ApplyEmailUpdate(ctx, id, up)
	if err != nil {
		return "", err
	}
	if !found {
		// Deleted between the check above and the locked read inside
		// ApplyEmailUpdate. Same answer as the check would have given.
		return SendArchiveGone, nil
	}
	return SendArchived, nil
}
