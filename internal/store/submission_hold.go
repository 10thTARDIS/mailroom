// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/oklog/ulid/v2"
)

// HELD SUBMISSIONS (#52, ADR-046 §11): outgoing mail that waits, rather than
// failing, while its OAuth account cannot send — because the user must sign in
// again (needs_reauth, or the account was disconnected), or because the
// provider is refusing THIS SERVER's client registration (operator_error, the
// operator's to fix). A held submission stays 'scheduled'; its smtp_response
// carries one of the two fixed reasons below, which is how SubmissionHeldKind
// (and so EmailSubmission.mailroomHeld) recognises it. Only the server writes
// these strings.

// HeldReconnectReason is the stored reason for a submission waiting on its
// user to reconnect the account.
const HeldReconnectReason = "waiting: reconnect this account in Settings and it will be sent"

// heldProviderPrefix starts every HeldProviderReason.
const heldProviderPrefix = "Sign-in to "
const heldProviderSuffix = " is temporarily unavailable on this server; your message will be sent when it is fixed"

// HeldProviderReason is the stored reason for a submission waiting on the
// operator to fix the server's sign-in with the account's provider.
func HeldProviderReason(profile string) string {
	return heldProviderPrefix + ProviderDisplayName(profile) + heldProviderSuffix
}

// ProviderDisplayName names an OAuth account's provider from its profile.
func ProviderDisplayName(profile string) string {
	switch profile {
	case "gmail":
		return "Google"
	case "microsoft":
		return "Microsoft"
	}
	return "your provider"
}

// Held kinds (EmailSubmission.mailroomHeld).
const (
	HeldKindReconnect = "reconnect"
	HeldKindProvider  = "provider"
)

// SubmissionHeldKind reports why a scheduled submission is held, or "".
func SubmissionHeldKind(s Submission) string {
	if s.State != SubmissionScheduled {
		return ""
	}
	switch {
	case s.SMTPResponse == HeldReconnectReason:
		return HeldKindReconnect
	case strings.HasPrefix(s.SMTPResponse, heldProviderPrefix) && strings.HasSuffix(s.SMTPResponse, heldProviderSuffix):
		return HeldKindProvider
	}
	return ""
}

// AwaitingReconnect reports whether an OAuth account needs its user to sign
// in again before it can sync or send: its grant is needs_reauth, or it has no
// grant at all (it was disconnected). A password account never does.
func (a Account) AwaitingReconnect() bool {
	return a.AuthKind == "oauth2" && (a.AuthState == OAuthStateNeedsReauth || a.AuthState == "")
}

// ProviderUnavailable reports whether an OAuth account's provider is refusing
// this server's client registration (operator_error).
func (a Account) ProviderUnavailable() bool {
	return a.AuthKind == "oauth2" && a.AuthState == OAuthStateOperatorError
}

// MarkSubmissionHeld records reason on a submission that is STILL 'scheduled'
// and reports whether it is. The row is read FOR UPDATE under the state guard,
// so a cancel that committed first is seen (held=false: the caller must not
// requeue it), and a cancel arriving later waits for this transaction and then
// cancels the job this transaction linked. The reason is written, with its
// EmailSubmission change row, only when it differs from what is stored: a
// submission looked at every few minutes appends one row, not one per look.
func (tx *UserTx) MarkSubmissionHeld(ctx context.Context, id ulid.ULID, reason string) (held bool, err error) {
	var cur *string
	if err := tx.tx.QueryRow(ctx, `
		SELECT smtp_response FROM app.submissions
		 WHERE user_id = $1 AND id = $2 AND state = $3
		 FOR UPDATE`,
		tx.userID.String(), id.String(), SubmissionScheduled).Scan(&cur); err != nil {
		if isNoRows(err) {
			return false, nil
		}
		return false, fmt.Errorf("store: mark submission held (read): %w", err)
	}
	if cur != nil && *cur == reason {
		return true, nil
	}
	if _, err := tx.tx.Exec(ctx, `
		UPDATE app.submissions SET smtp_response = $3
		 WHERE user_id = $1 AND id = $2 AND state = $4`,
		tx.userID.String(), id.String(), reason, SubmissionScheduled); err != nil {
		return false, fmt.Errorf("store: mark submission held: %w", err)
	}
	if _, err := tx.AppendChange(ctx, ObjectEmailSubmission, id, OpUpdated); err != nil {
		return false, err
	}
	return true, nil
}

// HoldClaimedSubmission returns a submission this worker CLAIMED ('sending')
// to 'scheduled' with reason, guarded on the state it claimed. held=false means
// the row is no longer 'sending' and nothing was written.
func (tx *UserTx) HoldClaimedSubmission(ctx context.Context, id ulid.ULID, reason string) (held bool, err error) {
	tag, err := tx.tx.Exec(ctx, `
		UPDATE app.submissions SET state = $3, smtp_response = $4
		 WHERE user_id = $1 AND id = $2 AND state = $5`,
		tx.userID.String(), id.String(), SubmissionScheduled, reason, SubmissionSending)
	if err != nil {
		return false, fmt.Errorf("store: hold claimed submission: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	if _, err := tx.AppendChange(ctx, ObjectEmailSubmission, id, OpUpdated); err != nil {
		return false, err
	}
	return true, nil
}
