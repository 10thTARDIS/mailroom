// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
)

// This file holds the EmailSubmission layer (spec §5, RFC 8621 §7): the record
// of "this message was submitted for delivery as this From, to these envelope
// recipients, at this time", plus the per-account outbound ledger that keeps a
// day's sends under the provider's cap.
//
// An EmailSubmission IS a JMAP-visible object (changelog.go ObjectEmailSubmission),
// so every create/state-change appends exactly one EmailSubmission change_log
// row in the SAME UserTx as the mutation (the load-bearing invariant —
// CLAUDE.md). The durable job that actually sends it (spec §5/§16: ONE delayed-
// submission engine behind both Undo Send and Send Later) is internal machinery
// and appends nothing.

// Submission states (app.submissions.state).
const (
	// SubmissionScheduled is queued: the send job has not fired yet. This is
	// the Undo-Send window (send_at in the future) — the ONE state from which
	// a submission can still be canceled.
	SubmissionScheduled = "scheduled"
	// SubmissionSending means the job claimed it and SMTP is in flight.
	SubmissionSending = "sending"
	// SubmissionSent means the provider accepted it (final).
	SubmissionSent = "sent"
	// SubmissionFailed means submission failed permanently (final, surfaced).
	SubmissionFailed = "failed"
	// SubmissionCanceled means the user undid the send inside the window.
	SubmissionCanceled = "canceled"
)

// ErrSubmissionNotFound is returned when a submission id does not resolve for
// the caller.
var ErrSubmissionNotFound = errors.New("store: submission not found")

// ErrSubmissionNotCancelable is returned when a cancel loses the race with the
// send job: the submission has already left the scheduled state, so the mail is
// on its way (or gone) and "undo" is a lie we refuse to tell.
var ErrSubmissionNotCancelable = errors.New("store: submission is no longer cancelable")

// ErrOverQuota is returned by ReserveOutbound when the account has spent its
// provider daily send cap (spec §5's outbound ledger). The caller must queue,
// not silently drop.
var ErrOverQuota = errors.New("store: account outbound daily quota exhausted")

// SubmissionRow is the input to CreateSubmission. FromAddress has ALREADY been
// validated against IdentityID by the caller (ResolveSendIdentity) — this layer
// records the decision, it does not make it.
type SubmissionRow struct {
	AccountID   ulid.ULID
	IdentityID  ulid.ULID
	MessageID   ulid.ULID
	FromAddress string
	EnvelopeTo  []string
	SendAt      time.Time
	// IdempotencyKey makes this send SAFE TO RETRY (#62, migration 00025). Supplied by
	// the client, one key per queued action; a repeat with the same key returns the
	// submission that already exists instead of creating a second one.
	//
	// Empty is legal and means "not retry-safe" — the behaviour every caller had before
	// 00025, and still correct for a CLI send that is never replayed.
	IdempotencyKey string
	// ArchiveOnSuccess is Send & archive (#648, migration 00059): the messages to
	// archive when, and only when, this submission is actually SENT. The send
	// worker performs it (ArchiveAfterSend) in the transaction that records the
	// send; nothing else ever reads it. Empty means archive nothing, stored NULL.
	ArchiveOnSuccess []ulid.ULID
	// APIKeyID is the API key that made this send, nil for a person (#49,
	// migration 00076). ArchiveAfterSend reads it so the archive is the key's
	// and records none of the user's learned-placement evidence.
	APIKeyID *ulid.ULID
}

// Submission is a stored submission projection (the JMAP EmailSubmission).
type Submission struct {
	ID           ulid.ULID
	AccountID    ulid.ULID
	IdentityID   ulid.ULID
	MessageID    ulid.ULID
	ThreadID     ulid.ULID
	FromAddress  string
	EnvelopeTo   []string
	SendAt       time.Time
	State        string
	SMTPResponse string
	JobID        *ulid.ULID
	CreatedAt    time.Time
	SentAt       *time.Time
	// ArchiveOnSuccess is what SubmissionRow.ArchiveOnSuccess recorded (#648);
	// nil when the submission asked for no archive.
	ArchiveOnSuccess []ulid.ULID
	// DismissedAt is when the user took this FAILED submission off the Outbox
	// (DismissSubmission, migration 00059); nil when it has not been dismissed.
	DismissedAt *time.Time
}

// UndoStatus maps the stored state onto RFC 8621 §7's undoStatus vocabulary:
// "pending" (still cancelable), "canceled", or "final".
func (s Submission) UndoStatus() string {
	switch s.State {
	case SubmissionScheduled:
		return "pending"
	case SubmissionCanceled:
		return "canceled"
	default:
		return "final"
	}
}

// MessageSubmissionStatus reports whether a message has been handed to the send
// path at all, and whether any of those submissions is still live (scheduled or
// sending — i.e. the mail may yet go out).
//
// It is the guard that keeps a submitted message out of the draft-retire path
// (mailroom#148): such a message has a Sent copy and an outbound record, so
// hard-deleting it is both wrong and — thanks to app.submissions' foreign key —
// impossible.
func (tx *UserTx) MessageSubmissionStatus(ctx context.Context, messageID ulid.ULID) (submitted, pending bool, err error) {
	err = tx.tx.QueryRow(ctx, `
		SELECT count(*) > 0,
		       count(*) FILTER (WHERE state IN ('scheduled', 'sending')) > 0
		  FROM app.submissions
		 WHERE user_id = $1 AND message_id = $2`,
		tx.userID.String(), messageID.String()).Scan(&submitted, &pending)
	if err != nil {
		return false, false, fmt.Errorf("store: message submission status: %w", err)
	}
	return submitted, pending, nil
}

// StrandedSentDraftIDs lists messages this user sent that are STILL sitting in a
// Drafts folder — the wreckage left by mailroom#146, where sending filed the
// Sent copy but never expunged the draft one.
//
// It names them by the shape the bug produced, not by a marker: a message with a
// completed submission that still has a live provenance row in a Drafts-role
// mailbox. Sending is fixed, so this set only ever shrinks; the repair walks it
// once (cmd/mailroom-admin repair-sent-drafts).
func (tx *UserTx) StrandedSentDraftIDs(ctx context.Context, limit int) ([]StrandedSentDraft, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := tx.tx.Query(ctx, `
		SELECT DISTINCT m.id, s.account_id
		  FROM app.messages m
		  JOIN app.submissions s
		    ON s.user_id = m.user_id AND s.message_id = m.id AND s.state = 'sent'
		  JOIN app.message_provenance p
		    ON p.user_id = m.user_id AND p.message_id = m.id AND p.state = 'live'
		  JOIN app.mailboxes mb
		    ON mb.user_id = p.user_id AND mb.id = p.mailbox_id AND mb.special_use = '\Drafts'
		 WHERE m.user_id = $1
		 ORDER BY m.id
		 LIMIT $2`,
		tx.userID.String(), limit)
	if err != nil {
		return nil, fmt.Errorf("store: stranded sent drafts: %w", err)
	}
	defer rows.Close()
	var out []StrandedSentDraft
	for rows.Next() {
		var id, acct string
		if err := rows.Scan(&id, &acct); err != nil {
			return nil, fmt.Errorf("store: scan stranded sent draft: %w", err)
		}
		var s StrandedSentDraft
		var perr error
		if s.MessageID, perr = ulid.Parse(id); perr != nil {
			return nil, fmt.Errorf("store: parse stranded id %q: %w", id, perr)
		}
		if s.AccountID, perr = ulid.Parse(acct); perr != nil {
			return nil, fmt.Errorf("store: parse stranded account id %q: %w", acct, perr)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// StrandedSentDraft is one message left in Drafts by mailroom#146, with the
// account that sent it — which is how the repair resolves the Drafts mailbox by
// role rather than trusting provenance.
type StrandedSentDraft struct {
	MessageID ulid.ULID
	AccountID ulid.ULID
}

// ClearDraftFlag takes the $draft keyword off a message whose draft state was
// only ever a leftover (mailroom#146/#148) and appends the Email change_log row.
// It is the local half of the repair; the IMAP half is RetireSentDraftCopy.
func (tx *UserTx) ClearDraftFlag(ctx context.Context, id ulid.ULID) (bool, error) {
	var touched string
	err := tx.tx.QueryRow(ctx, `
		UPDATE app.messages SET is_draft = false
		 WHERE user_id = $1 AND id = $2 AND is_draft
		RETURNING id`,
		tx.userID.String(), id.String()).Scan(&touched)
	if err != nil {
		if isNoRows(err) {
			return false, nil
		}
		return false, fmt.Errorf("store: clear draft flag: %w", err)
	}
	if _, err := tx.AppendChange(ctx, ObjectEmail, id, OpUpdated); err != nil {
		return false, err
	}
	return true, nil
}

// CreateSubmission records a scheduled submission and appends its
// EmailSubmission change_log row (the invariant). The caller enqueues the send
// job in the SAME transaction and calls SetSubmissionJob with its id, so a
// crash between the two can never leave a submission nobody will ever send.
func (tx *UserTx) CreateSubmission(ctx context.Context, row SubmissionRow) (id ulid.ULID, created bool, err error) {
	if len(row.EnvelopeTo) == 0 {
		return ulid.ULID{}, false, errors.New("store: create submission: envelope has no recipients")
	}
	// RETRY SAFETY (#62). With a key, a repeat of the SAME send returns the submission
	// that already exists rather than creating a second one — because the failure this
	// guards is a lost RESPONSE, where the send already happened and the client cannot
	// know it. Creating a second submission there mails the recipient twice.
	//
	// Checked BEFORE the insert as well as caught after it: the pre-check is what makes
	// the common retry cheap and, more importantly, what lets this return the existing
	// id instead of an error. The post-insert catch is what makes it correct under a
	// genuine race between two drains of the same queue.
	if row.IdempotencyKey != "" {
		if existing, ok, err := tx.submissionByIdempotencyKey(ctx, row.IdempotencyKey); err != nil {
			return ulid.ULID{}, false, err
		} else if ok {
			return existing, false, nil
		}
	}

	id = NewULID()
	// Under a SAVEPOINT: the unique index on (user_id, idempotency_key) makes a
	// concurrent duplicate a RECOVERABLE outcome, and a bare failed statement would
	// abort the surrounding transaction (SQLSTATE 25P02) — the trap that shipped once in
	// ComposeSnippet/set. See store.CreateAccount for the same reasoning.
	sp, err := tx.tx.Begin(ctx) // SAVEPOINT
	if err != nil {
		return ulid.ULID{}, false, fmt.Errorf("store: create submission (savepoint): %w", err)
	}
	// archive_on_success goes in as text[] and is cast to the domain array: pgx
	// has no codec registered for app.ulid[]'s OID, while text[] it encodes
	// natively, and the cast still runs every element through app.ulid's CHECK.
	var archive []string
	if len(row.ArchiveOnSuccess) > 0 {
		archive = ulidStrings(row.ArchiveOnSuccess)
	}
	if _, err := sp.Exec(ctx, `
		INSERT INTO app.submissions
			(id, user_id, account_id, identity_id, message_id, from_address,
			 envelope_to, send_at, state, idempotency_key, archive_on_success, api_key_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11::text[]::app.ulid[], $12)`,
		id.String(), tx.userID.String(), row.AccountID.String(), row.IdentityID.String(),
		row.MessageID.String(), NormalizeAddress(row.FromAddress), row.EnvelopeTo,
		row.SendAt.UTC(), SubmissionScheduled, nullStr(row.IdempotencyKey), archive,
		ulidPtrString(row.APIKeyID)); err != nil {
		_ = sp.Rollback(ctx) // ROLLBACK TO SAVEPOINT — the outer tx stays usable
		// Lost the race: another drain of the same queued action inserted first. Its
		// submission is the one that counts, so return that rather than failing —
		// the caller asked for "this send to have happened", and it has.
		if isUniqueViolation(err) && row.IdempotencyKey != "" {
			if existing, ok, e := tx.submissionByIdempotencyKey(ctx, row.IdempotencyKey); e != nil {
				return ulid.ULID{}, false, e
			} else if ok {
				return existing, false, nil
			}
		}
		return ulid.ULID{}, false, fmt.Errorf("store: create submission: %w", err)
	}
	if err := sp.Commit(ctx); err != nil { // RELEASE SAVEPOINT
		return ulid.ULID{}, false, fmt.Errorf("store: create submission (release): %w", err)
	}
	if _, err := tx.AppendChange(ctx, ObjectEmailSubmission, id, OpCreated); err != nil {
		return ulid.ULID{}, false, err
	}
	return id, true, nil
}

// submissionByIdempotencyKey finds a previous submission made under this key.
func (tx *UserTx) submissionByIdempotencyKey(ctx context.Context, key string) (ulid.ULID, bool, error) {
	var id string
	err := tx.tx.QueryRow(ctx,
		`SELECT id FROM app.submissions WHERE user_id = $1 AND idempotency_key = $2`,
		tx.userID.String(), key).Scan(&id)
	if isNoRows(err) {
		return ulid.ULID{}, false, nil
	}
	if err != nil {
		return ulid.ULID{}, false, fmt.Errorf("store: submission by idempotency key: %w", err)
	}
	parsed, err := ulid.Parse(id)
	if err != nil {
		return ulid.ULID{}, false, fmt.Errorf("store: submission id %q: %w", id, err)
	}
	return parsed, true, nil
}

// SetSubmissionJob links a submission to the durable job that will send it.
// The link is bookkeeping, not a JMAP-visible property, so it appends no
// change_log row.
func (tx *UserTx) SetSubmissionJob(ctx context.Context, id, jobID ulid.ULID) error {
	if _, err := tx.tx.Exec(ctx, `
		UPDATE app.submissions SET job_id = $3 WHERE user_id = $1 AND id = $2`,
		tx.userID.String(), id.String(), jobID.String()); err != nil {
		return fmt.Errorf("store: set submission job: %w", err)
	}
	return nil
}

// submissionColumns is the SELECT list shared by the submission reads.
const submissionColumns = `s.id, s.account_id, s.identity_id, s.message_id,
	COALESCE(m.thread_id, ''), s.from_address, s.envelope_to, s.send_at, s.state,
	COALESCE(s.smtp_response, ''), s.job_id, s.created_at, s.sent_at,
	s.archive_on_success::text[], s.dismissed_at`

// GetSubmissions returns the submissions for the requested ids (RLS-scoped).
func (tx *UserTx) GetSubmissions(ctx context.Context, ids []ulid.ULID) ([]Submission, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := tx.tx.Query(ctx, `
		SELECT `+submissionColumns+`
		  FROM app.submissions s
		  LEFT JOIN app.messages m ON m.id = s.message_id AND m.user_id = s.user_id
		 WHERE s.user_id = $1 AND s.id = ANY($2)`,
		tx.userID.String(), ulidStrings(ids))
	if err != nil {
		return nil, fmt.Errorf("store: get submissions: %w", err)
	}
	defer rows.Close()
	return scanSubmissions(rows)
}

// ListSubmissions returns every submission of the caller, newest first — the
// "outbox" view (scheduled sends the user can still undo, plus history).
func (tx *UserTx) ListSubmissions(ctx context.Context, limit int) ([]Submission, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := tx.tx.Query(ctx, `
		SELECT `+submissionColumns+`
		  FROM app.submissions s
		  LEFT JOIN app.messages m ON m.id = s.message_id AND m.user_id = s.user_id
		 WHERE s.user_id = $1
		 ORDER BY s.created_at DESC, s.id DESC
		 LIMIT $2`,
		tx.userID.String(), limit)
	if err != nil {
		return nil, fmt.Errorf("store: list submissions: %w", err)
	}
	defer rows.Close()
	return scanSubmissions(rows)
}

// SubmissionQuery is EmailSubmission/query's filter and window (RFC 8621 §7.3,
// #653). Every set condition must hold (they AND together); a zero value asks
// for every submission the caller has.
type SubmissionQuery struct {
	// UndoStatus, when non-empty, keeps submissions whose Submission.UndoStatus()
	// is this value: "pending", "final" or "canceled". The SQL below is that
	// method's switch written as a predicate; keep the two in step.
	UndoStatus string
	// EmailIDs, when EmailIDsSet, keeps submissions of these messages only. Set
	// and empty matches nothing, as RFC 8621's "must be in this list" says.
	EmailIDs    []ulid.ULID
	EmailIDsSet bool
	// InOutbox (Mailroom extension) keeps exactly what the web client's Outbox
	// lists: see outboxPredicate.
	InOutbox bool
	// Position is RFC 8620 §5.5's: a negative value counts back from the end of
	// the result and is clamped to 0.
	Position int
	// Limit is the page size. The caller clamps it to its own maximum and says
	// so in the response; this layer applies what it is given.
	Limit int
}

// outboxPredicate is the `inOutbox` condition (#653): the submissions the
// Outbox shows. It is the SERVER-SIDE COPY of web/src/app/ui/outbox.ts
// outboxSubmissions, and the two must say the same thing:
//
//   - "the latest submission per emailId" — outbox.ts keeps the one with the
//     greatest id for each emailId. Here: no other submission of the same
//     message has a greater id. Ids are ULIDs (ADR-007), so id order IS
//     creation order; `created_at` is deliberately not the key, because it is
//     the database's clock and the id is the application's, and the client can
//     only compare ids. COLLATE "C" makes the comparison bytewise, which is
//     what a JavaScript string `<` does on these ASCII ids.
//   - `mailroomState === "failed" && !dismissedAt` — state 'failed' with
//     dismissed_at NULL.
//   - `undoStatus === "pending"` — Submission.UndoStatus maps only 'scheduled'
//     to "pending" ('sending' is "final"), so state 'scheduled'.
//
// The latest-per-message test runs over EVERY submission of the message,
// whatever its state, exactly as outbox.ts collapses before it filters: a
// failure answered by a re-send is superseded even when the re-send is
// already sent (and so not in the Outbox itself).
const outboxPredicate = `(
	    (s.state = 'scheduled' OR (s.state = 'failed' AND s.dismissed_at IS NULL))
	    AND NOT EXISTS (
	      SELECT 1 FROM app.submissions later
	       WHERE later.user_id = s.user_id
	         AND later.message_id = s.message_id
	         AND later.id COLLATE "C" > s.id COLLATE "C"))`

// QuerySubmissions is EmailSubmission/query (#653): the ids of the caller's
// submissions matching q, newest first (created_at, then id, both descending),
// windowed by q.Position/q.Limit, with the total number that match and the
// position the window actually starts at. RLS-scoped like every UserTx read.
func (tx *UserTx) QuerySubmissions(ctx context.Context, q SubmissionQuery) (ids []ulid.ULID, total, position int, err error) {
	where := []string{"s.user_id = $1"}
	args := []any{tx.userID.String()}
	arg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	switch q.UndoStatus {
	case "":
	case "pending":
		where = append(where, "s.state = "+arg(SubmissionScheduled))
	case "canceled":
		where = append(where, "s.state = "+arg(SubmissionCanceled))
	case "final":
		where = append(where, fmt.Sprintf("s.state NOT IN (%s, %s)",
			arg(SubmissionScheduled), arg(SubmissionCanceled)))
	default:
		return nil, 0, 0, fmt.Errorf("store: query submissions: unknown undoStatus %q", q.UndoStatus)
	}
	if q.EmailIDsSet {
		where = append(where, "s.message_id = ANY("+arg(ulidStrings(q.EmailIDs))+"::text[])")
	}
	if q.InOutbox {
		where = append(where, outboxPredicate)
	}
	cond := strings.Join(where, " AND ")

	if err := tx.tx.QueryRow(ctx,
		`SELECT count(*) FROM app.submissions s WHERE `+cond, args...).Scan(&total); err != nil {
		return nil, 0, 0, fmt.Errorf("store: count submissions for query: %w", err)
	}

	position = q.Position
	if position < 0 {
		position += total
		if position < 0 {
			position = 0
		}
	}
	if q.Limit <= 0 || position >= total {
		return []ulid.ULID{}, total, position, nil
	}

	pageArgs := append(append([]any{}, args...), position, q.Limit)
	rows, err := tx.tx.Query(ctx, fmt.Sprintf(`
		SELECT s.id FROM app.submissions s
		 WHERE %s
		 ORDER BY s.created_at DESC, s.id COLLATE "C" DESC
		 OFFSET $%d LIMIT $%d`, cond, len(args)+1, len(args)+2), pageArgs...)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("store: query submissions: %w", err)
	}
	defer rows.Close()
	ids = []ulid.ULID{}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, 0, 0, fmt.Errorf("store: scan submission id: %w", err)
		}
		id, err := ulid.Parse(s)
		if err != nil {
			return nil, 0, 0, fmt.Errorf("store: parse submission id %q: %w", s, err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, 0, fmt.Errorf("store: query submissions: %w", err)
	}
	return ids, total, position, nil
}

// ClaimSubmissionForSend transitions a submission 'scheduled' -> 'sending' and
// returns it. The state guard IS the undo/send race resolution: exactly one of
// CancelSubmission and ClaimSubmissionForSend can win, because both are guarded
// UPDATEs on the same row in the same 'scheduled' state. ok is false when the
// submission was already canceled (or already sent) — the job then does nothing,
// which is precisely what "undo" must mean.
func (tx *UserTx) ClaimSubmissionForSend(ctx context.Context, id ulid.ULID) (Submission, bool, error) {
	var claimed string
	err := tx.tx.QueryRow(ctx, `
		UPDATE app.submissions
		   SET state = $3
		 WHERE user_id = $1 AND id = $2 AND state = $4
		RETURNING id`,
		tx.userID.String(), id.String(), SubmissionSending, SubmissionScheduled).Scan(&claimed)
	if err != nil {
		if isNoRows(err) {
			return Submission{}, false, nil
		}
		return Submission{}, false, fmt.Errorf("store: claim submission: %w", err)
	}
	if _, err := tx.AppendChange(ctx, ObjectEmailSubmission, id, OpUpdated); err != nil {
		return Submission{}, false, err
	}
	subs, err := tx.GetSubmissions(ctx, []ulid.ULID{id})
	if err != nil {
		return Submission{}, false, err
	}
	if len(subs) == 0 {
		return Submission{}, false, ErrSubmissionNotFound
	}
	return subs[0], true, nil
}

// FinishSubmission records the terminal outcome of a send ('sent' or 'failed')
// with the provider's response, and appends the EmailSubmission change_log row.
func (tx *UserTx) FinishSubmission(ctx context.Context, id ulid.ULID, state, response string) error {
	switch state {
	case SubmissionSent, SubmissionFailed, SubmissionScheduled:
	default:
		return fmt.Errorf("store: finish submission: invalid terminal state %q", state)
	}
	var sentAt any
	if state == SubmissionSent {
		sentAt = time.Now().UTC()
	}
	tag, err := tx.tx.Exec(ctx, `
		UPDATE app.submissions
		   SET state = $3, smtp_response = $4, sent_at = $5
		 WHERE user_id = $1 AND id = $2`,
		tx.userID.String(), id.String(), state, nullStr(truncateResponse(response)), sentAt)
	if err != nil {
		return fmt.Errorf("store: finish submission: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrSubmissionNotFound
	}
	if _, err := tx.AppendChange(ctx, ObjectEmailSubmission, id, OpUpdated); err != nil {
		return err
	}
	return nil
}

// maxSMTPResponse bounds what we persist of a provider response. It is a status
// line, never message content — but a hostile server could return a lot of it.
const maxSMTPResponse = 512

func truncateResponse(s string) string {
	if len(s) <= maxSMTPResponse {
		return s
	}
	return s[:maxSMTPResponse] + "…"
}

// CancelSubmission is Undo Send: it moves a submission 'scheduled' -> 'canceled'
// and cancels its pending job in the SAME transaction, then appends the
// EmailSubmission change_log row. The state guard makes it lose cleanly to a job
// that has already claimed the row (ErrSubmissionNotCancelable) — there is no
// window in which both "canceled" and an actual SMTP handoff can happen.
func (tx *UserTx) CancelSubmission(ctx context.Context, id ulid.ULID) error {
	var jobID *string
	err := tx.tx.QueryRow(ctx, `
		UPDATE app.submissions
		   SET state = $3
		 WHERE user_id = $1 AND id = $2 AND state = $4
		RETURNING job_id`,
		tx.userID.String(), id.String(), SubmissionCanceled, SubmissionScheduled).Scan(&jobID)
	if err != nil {
		if isNoRows(err) {
			// Either it does not exist, or it is no longer scheduled. Distinguish
			// so the caller can report notFound vs. "too late to undo".
			var state string
			serr := tx.tx.QueryRow(ctx,
				`SELECT state FROM app.submissions WHERE user_id = $1 AND id = $2`,
				tx.userID.String(), id.String()).Scan(&state)
			if serr != nil {
				if isNoRows(serr) {
					return ErrSubmissionNotFound
				}
				return fmt.Errorf("store: cancel submission (read): %w", serr)
			}
			return ErrSubmissionNotCancelable
		}
		return fmt.Errorf("store: cancel submission: %w", err)
	}

	// Kill the timer too. The job row is user-owned (app.jobs RLS policy), so
	// this is a plain guarded UPDATE — a job already claimed by the dispatcher
	// ('running') is NOT touched, but it will find the submission canceled and
	// do nothing (ClaimSubmissionForSend).
	if jobID != nil {
		if _, err := tx.tx.Exec(ctx, `
			UPDATE app.jobs SET state = 'done', completed_at = now()
			 WHERE user_id = $1 AND id = $2 AND state = 'pending'`,
			tx.userID.String(), *jobID); err != nil {
			return fmt.Errorf("store: cancel submission (job): %w", err)
		}
	}
	if _, err := tx.AppendChange(ctx, ObjectEmailSubmission, id, OpUpdated); err != nil {
		return err
	}
	// AN UNDONE REPLY TAKES ITS ANSWER BACK (#725). If this send carried an
	// iTIP REPLY, the invitation's card stops saying "You accepted" — on every
	// message of that event, each of which gets its Email change row here, in
	// this transaction. Nothing is appended for an ordinary send.
	return tx.calendarResponseWithdrawn(ctx, id)
}

// ErrSubmissionNotDismissable is returned by DismissSubmission for a submission
// that is not 'failed'. Only a permanent failure is something the user is
// asked to act on; a pending send is cancelled (Undo Send), and a sent or
// cancelled one is not in the Outbox at all.
var ErrSubmissionNotDismissable = errors.New("store: only a failed submission can be dismissed")

// DismissSubmission takes a permanently FAILED submission off the Outbox
// (migration 00059): it stamps dismissed_at and appends the EmailSubmission
// change_log row in this transaction (the invariant — dismissedAt is on the
// JMAP object, and every other device's Outbox must drop the row too).
//
// Nothing is deleted. The submission stays as the record of the attempt, and
// its draft is untouched, so the user can still edit and re-send it.
//
// The state guard is the UPDATE's own WHERE, and the table's CHECK
// (submissions_dismissed_only_failed) backs it: a submission that is anything
// but 'failed' cannot carry dismissed_at at all. Dismissing one that is
// already dismissed is a no-op — no second change row.
func (tx *UserTx) DismissSubmission(ctx context.Context, id ulid.ULID) error {
	var touched string
	err := tx.tx.QueryRow(ctx, `
		UPDATE app.submissions
		   SET dismissed_at = now()
		 WHERE user_id = $1 AND id = $2 AND state = $3 AND dismissed_at IS NULL
		RETURNING id`,
		tx.userID.String(), id.String(), SubmissionFailed).Scan(&touched)
	if err == nil {
		_, err = tx.AppendChange(ctx, ObjectEmailSubmission, id, OpUpdated)
		return err
	}
	if !isNoRows(err) {
		return fmt.Errorf("store: dismiss submission: %w", err)
	}
	// Nothing updated: missing, already dismissed, or not failed. Distinguish,
	// as CancelSubmission does, so the caller can say which.
	var (
		state     string
		dismissed bool
	)
	serr := tx.tx.QueryRow(ctx,
		`SELECT state, dismissed_at IS NOT NULL FROM app.submissions WHERE user_id = $1 AND id = $2`,
		tx.userID.String(), id.String()).Scan(&state, &dismissed)
	switch {
	case isNoRows(serr):
		return ErrSubmissionNotFound
	case serr != nil:
		return fmt.Errorf("store: dismiss submission (read): %w", serr)
	case dismissed:
		return nil
	default:
		return fmt.Errorf("%w (it is %s)", ErrSubmissionNotDismissable, state)
	}
}

// OutboundSentToday returns how many sends the account has drawn from today's
// budget (UTC), or 0 when it has not sent today. It backs
// mailroom_outbound_ledger_level (#524).
//
// ⚠ SAMPLED, NOT SET AT SEND TIME, and that is the whole point. The obvious
// wiring — set the gauge inside ReserveOutbound — leaves it reporting
// YESTERDAY'S total all through a day on which nothing has been sent yet,
// because app.outbound_ledger is keyed on (account_id, CURRENT_DATE) and a new
// day simply has no row until the first send. Reading it on each sync pass
// makes the reset at UTC midnight visible without a send to trigger it.
func (tx *UserTx) OutboundSentToday(ctx context.Context, accountID ulid.ULID) (int64, error) {
	var sent int64
	err := tx.tx.QueryRow(ctx, `
		SELECT COALESCE(sent_count, 0) FROM app.outbound_ledger
		 WHERE user_id = $1 AND account_id = $2 AND day = CURRENT_DATE`,
		tx.userID.String(), accountID.String()).Scan(&sent)
	if isNoRows(err) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("store: outbound sent today: %w", err)
	}
	return sent, nil
}

// ReserveOutbound draws one send from the account's daily budget (spec §5's
// outbound_ledger: provider caps are counted per ACCOUNT, per day). It returns
// the remaining allowance after the reservation, or ErrOverQuota when the cap
// is already spent — in which case nothing is reserved and the caller must
// queue the send rather than fail it silently.
//
// The INSERT ... ON CONFLICT DO UPDATE with the quota predicate is atomic: two
// concurrent sends cannot both take the last unit.
func (tx *UserTx) ReserveOutbound(ctx context.Context, accountID ulid.ULID) (remaining int, err error) {
	var quota int
	if err := tx.tx.QueryRow(ctx,
		`SELECT outbound_daily_quota FROM app.accounts WHERE user_id = $1 AND id = $2`,
		tx.userID.String(), accountID.String()).Scan(&quota); err != nil {
		if isNoRows(err) {
			return 0, ErrAccountNotFound
		}
		return 0, fmt.Errorf("store: reserve outbound (quota): %w", err)
	}
	if quota <= 0 {
		// A zero cap means "cannot send at all". Check it BEFORE the upsert: the
		// INSERT branch has no quota predicate to fail on (there is no row yet),
		// so it would otherwise write sent_count = 1 past a cap of 0.
		return 0, ErrOverQuota
	}

	var sent int
	err = tx.tx.QueryRow(ctx, `
		INSERT INTO app.outbound_ledger (account_id, user_id, day, sent_count)
		VALUES ($2, $1, CURRENT_DATE, 1)
		ON CONFLICT (account_id, day) DO UPDATE
			SET sent_count = app.outbound_ledger.sent_count + 1
			WHERE app.outbound_ledger.sent_count < $3
		RETURNING sent_count`,
		tx.userID.String(), accountID.String(), quota).Scan(&sent)
	if err != nil {
		if isNoRows(err) {
			// The DO UPDATE's WHERE filtered the row out: the cap is spent.
			return 0, ErrOverQuota
		}
		return 0, fmt.Errorf("store: reserve outbound: %w", err)
	}
	if sent > quota {
		return 0, ErrOverQuota
	}
	return quota - sent, nil
}

// ReleaseOutbound gives a reserved send back after a submission failed before
// the provider accepted it, so a failed send does not permanently consume the
// user's daily budget. It never goes below zero.
func (tx *UserTx) ReleaseOutbound(ctx context.Context, accountID ulid.ULID) error {
	if _, err := tx.tx.Exec(ctx, `
		UPDATE app.outbound_ledger
		   SET sent_count = GREATEST(sent_count - 1, 0)
		 WHERE user_id = $1 AND account_id = $2 AND day = CURRENT_DATE`,
		tx.userID.String(), accountID.String()); err != nil {
		return fmt.Errorf("store: release outbound: %w", err)
	}
	return nil
}

// OutboundUsage reports today's spend and cap for an account — the user-visible
// quota meter (spec §5).
func (tx *UserTx) OutboundUsage(ctx context.Context, accountID ulid.ULID) (sent, quota int, err error) {
	e := tx.tx.QueryRow(ctx, `
		SELECT COALESCE(l.sent_count, 0), a.outbound_daily_quota
		  FROM app.accounts a
		  LEFT JOIN app.outbound_ledger l
		    ON l.account_id = a.id AND l.user_id = a.user_id AND l.day = CURRENT_DATE
		 WHERE a.user_id = $1 AND a.id = $2`,
		tx.userID.String(), accountID.String()).Scan(&sent, &quota)
	if e != nil {
		if isNoRows(e) {
			return 0, 0, ErrAccountNotFound
		}
		return 0, 0, fmt.Errorf("store: outbound usage: %w", e)
	}
	return sent, quota, nil
}

// scanSubmissions drains a submission result set in submissionColumns order.
func scanSubmissions(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
}) ([]Submission, error) {
	var out []Submission
	for rows.Next() {
		var (
			s                    Submission
			id, acct, ident, msg string
			thread               string
			jobID                *string
			archive              []string
		)
		if err := rows.Scan(&id, &acct, &ident, &msg, &thread, &s.FromAddress,
			&s.EnvelopeTo, &s.SendAt, &s.State, &s.SMTPResponse, &jobID,
			&s.CreatedAt, &s.SentAt, &archive, &s.DismissedAt); err != nil {
			return nil, fmt.Errorf("store: scan submission: %w", err)
		}
		var err error
		for _, a := range archive {
			u, perr := ulid.Parse(a)
			if perr != nil {
				return nil, fmt.Errorf("store: parse submission archive id %q: %w", a, perr)
			}
			s.ArchiveOnSuccess = append(s.ArchiveOnSuccess, u)
		}
		if s.ID, err = ulid.Parse(id); err != nil {
			return nil, fmt.Errorf("store: parse submission id %q: %w", id, err)
		}
		if s.AccountID, err = ulid.Parse(acct); err != nil {
			return nil, fmt.Errorf("store: parse submission account id %q: %w", acct, err)
		}
		if s.IdentityID, err = ulid.Parse(ident); err != nil {
			return nil, fmt.Errorf("store: parse submission identity id %q: %w", ident, err)
		}
		if s.MessageID, err = ulid.Parse(msg); err != nil {
			return nil, fmt.Errorf("store: parse submission message id %q: %w", msg, err)
		}
		if thread != "" {
			if s.ThreadID, err = ulid.Parse(thread); err != nil {
				return nil, fmt.Errorf("store: parse submission thread id %q: %w", thread, err)
			}
		}
		if jobID != nil {
			j, err := ulid.Parse(*jobID)
			if err != nil {
				return nil, fmt.Errorf("store: parse submission job id %q: %w", *jobID, err)
			}
			s.JobID = &j
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: scan submissions: %w", err)
	}
	return out, nil
}
