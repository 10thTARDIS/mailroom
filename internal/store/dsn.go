// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/oklog/ulid/v2"
)

// This file records parsed RFC 3464 DSNs (bounces) into app.dsn_events and
// surfaces them as delivery health (spec §4, Forgejo #30).
//
// WHAT change_log ROW A DSN OWES. A DSN event is NOT itself one of the
// load-bearing JMAP-visible object types (CLAUDE.md enumerates them: messages,
// mailboxes, labels, threads, identities, contacts, submissions). It is
// delivery-health metadata that ATTACHES to a submission we made. So the
// change_log row a recorded DSN owes is an EmailSubmission `updated` on the
// submission whose delivery status just changed — which is exactly RFC 8621's
// EmailSubmission.deliveryStatus surface — appended in the SAME UserTx as the
// dsn_events insert (the invariant). A bounce we cannot link to a submission
// mutates no JMAP-visible object, so it is still recorded (queryable delivery
// health) but owes no change_log row. This is why no new app.object_type value
// (and thus no migration) is required.
//
// CRASH ISOLATION. RecordDSN is called from inside the INGEST transaction, and a
// malformed DSN must never abort the ingest of the message that carried it. So
// the whole recording runs under a SAVEPOINT, and each recipient row under a
// nested one: any SQL error — a bad enum, a constraint, a poisoned statement —
// rolls back only the DSN work (or the one bad recipient) and leaves the
// surrounding transaction usable (SQLSTATE 25P02; the pattern labels.go
// documents). A wholly unrecordable DSN returns (0, nil); the carrying message
// still ingests.

// dsnActions is the closed app.dsn_action set, guarded so a parser bug can never
// present an out-of-enum value to the column.
var dsnActions = map[string]bool{
	"failed": true, "delayed": true, "delivered": true,
	"relayed": true, "expanded": true,
}

// DSNRecipient is one per-recipient delivery-status record to persist.
type DSNRecipient struct {
	Recipient  string
	Action     string
	Status     string
	Diagnostic string
}

// DSNEvent is a stored delivery-status record (the read projection).
type DSNEvent struct {
	ID                 ulid.ULID
	OriginMessageID    *ulid.ULID
	OriginSubmissionID *ulid.ULID
	DSNMessageID       *ulid.ULID
	Recipient          string
	Action             string
	Status             string
	Diagnostic         string
	CreatedAt          time.Time
}

// RecordDSN persists a parsed DSN best-effort and returns how many recipient
// rows it recorded. dsnMessageID is the ingested DSN message itself;
// originalMessageID is the Message-ID recovered from the returned message, used
// to link the bounce to what we sent. It never fails the caller over DSN
// bookkeeping — see the file header on crash isolation.
func (tx *UserTx) RecordDSN(ctx context.Context, dsnMessageID ulid.ULID, originalMessageID string, recips []DSNRecipient) (int, error) {
	if len(recips) == 0 {
		return 0, nil
	}
	outer, err := tx.tx.Begin(ctx) // SAVEPOINT around ALL DSN work
	if err != nil {
		return 0, nil
	}
	recorded, err := recordDSNUnder(ctx, outer, tx.userID, dsnMessageID, originalMessageID, recips)
	if err != nil || recorded == 0 {
		_ = outer.Rollback(ctx) // ROLLBACK TO SAVEPOINT — ingest tx stays usable
		return 0, nil
	}
	if err := outer.Commit(ctx); err != nil { // RELEASE SAVEPOINT
		return 0, nil
	}
	return recorded, nil
}

// recordDSNUnder resolves the links, inserts the recipient rows, and appends the
// EmailSubmission change_log row, all on the executor q (the outer savepoint) so
// that a failure anywhere rolls the whole lot back without touching the parent
// transaction.
func recordDSNUnder(ctx context.Context, q pgx.Tx, userID, dsnMessageID ulid.ULID, originalMessageID string, recips []DSNRecipient) (int, error) {
	originMsgID, err := resolveOriginMessage(ctx, q, userID, originalMessageID)
	if err != nil {
		return 0, err
	}
	var subID *ulid.ULID
	if originMsgID != nil {
		if subID, err = resolveSubmission(ctx, q, userID, *originMsgID); err != nil {
			return 0, err
		}
	}

	recorded := 0
	for _, r := range recips {
		ok, err := insertDSNEvent(ctx, q, userID, dsnMessageID, originMsgID, subID, r)
		if err != nil {
			return recorded, err // savepoint machinery failure — abandon DSN work
		}
		if ok {
			recorded++
		}
	}

	// The submission's deliveryStatus changed → owe exactly one EmailSubmission
	// `updated`, regardless of recipient count (the invariant).
	if recorded > 0 && subID != nil {
		if err := appendChangeVia(ctx, q, userID, ObjectEmailSubmission, *subID, OpUpdated); err != nil {
			return recorded, err
		}
	}
	return recorded, nil
}

// insertDSNEvent inserts one dsn_events row under a nested SAVEPOINT so a single
// bad recipient (e.g. hostile data that trips a constraint) is skipped without
// losing the good ones or poisoning the surrounding transaction. A skipped row
// returns (false, nil); only a failure to manage the savepoint itself is an
// error.
func insertDSNEvent(ctx context.Context, q pgx.Tx, userID, dsnMessageID ulid.ULID, originMsgID, subID *ulid.ULID, r DSNRecipient) (bool, error) {
	action := strings.ToLower(strings.TrimSpace(r.Action))
	if !dsnActions[action] {
		action = "failed"
	}
	sp, err := q.Begin(ctx) // nested SAVEPOINT (per recipient)
	if err != nil {
		return false, err
	}
	_, err = sp.Exec(ctx, `
		INSERT INTO app.dsn_events
			(id, user_id, origin_message_id, origin_submission_id, dsn_message_id,
			 recipient, action, status_code, diagnostic)
		VALUES ($1, $2, $3::app.ulid, $4::app.ulid, $5::app.ulid,
		        $6, $7::app.dsn_action, $8, $9)`,
		NewULID().String(), userID.String(),
		ulidPtrArg(originMsgID), ulidPtrArg(subID), dsnMessageID.String(),
		NormalizeAddress(r.Recipient), action, nullStr(r.Status), nullStr(r.Diagnostic))
	if err != nil {
		_ = sp.Rollback(ctx)
		return false, nil // contained: skip this recipient
	}
	if err := sp.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// resolveOriginMessage finds the sent message a bounce reports on, by its
// Message-ID header. The header is matched both bracket-stripped (how ingest
// stores message_id_hint) and angle-bracketed, so it links regardless of which
// form a given send path persisted. nil when unrecoverable/unmatched.
func resolveOriginMessage(ctx context.Context, q pgx.Tx, userID ulid.ULID, messageID string) (*ulid.ULID, error) {
	m := strings.Trim(strings.TrimSpace(messageID), "<>")
	if m == "" {
		return nil, nil
	}
	var id string
	err := q.QueryRow(ctx, `
		SELECT id FROM app.messages
		 WHERE user_id = $1 AND message_id_hint IN ($2, '<' || $2 || '>')
		 ORDER BY received_at DESC
		 LIMIT 1`,
		userID.String(), m).Scan(&id)
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: dsn resolve origin message: %w", err)
	}
	u, err := ulid.Parse(id)
	if err != nil {
		return nil, fmt.Errorf("store: parse origin message id %q: %w", id, err)
	}
	return &u, nil
}

// resolveSubmission finds the submission that sent a message (the newest, should
// more than one exist). nil when the message was not submitted through Mailroom.
func resolveSubmission(ctx context.Context, q pgx.Tx, userID, messageID ulid.ULID) (*ulid.ULID, error) {
	var id string
	err := q.QueryRow(ctx, `
		SELECT id FROM app.submissions
		 WHERE user_id = $1 AND message_id = $2
		 ORDER BY created_at DESC
		 LIMIT 1`,
		userID.String(), messageID.String()).Scan(&id)
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: dsn resolve submission: %w", err)
	}
	u, err := ulid.Parse(id)
	if err != nil {
		return nil, fmt.Errorf("store: parse submission id %q: %w", id, err)
	}
	return &u, nil
}

// appendChangeVia allocates a sequence and appends a change_log row using the
// given executor q. It is AppendChange with a different executor, not a different
// implementation: it runs on a SAVEPOINT so the DSN pipeline's change_log write
// is isolated from the ingest transaction along with the rows it accompanies, and
// *UserTx's method is bound to the transaction itself.
//
// ⚠ IT SHARES ITS SQL WITH AppendChange RATHER THAN COPYING IT (#299). Until this
// change these were two hand-maintained texts with identical semantics and
// nothing tying them together — the same duplicate-definition shape the store's
// ml payload struct had, minus the pinning test that one was given. The
// consts live in changelog.go next to the invariant they implement.
func appendChangeVia(ctx context.Context, q pgx.Tx, userID ulid.ULID, t ObjectType, objectID ulid.ULID, op ChangeOp) error {
	var seq int64
	if err := q.QueryRow(ctx, nextChangeSeqSQL, userID.String(), t).Scan(&seq); err != nil {
		return fmt.Errorf("store: dsn append change seq (%s): %w", t, err)
	}
	if _, err := q.Exec(ctx, insertChangeLogSQL,
		userID.String(), t, seq, objectID.String(), op); err != nil {
		return fmt.Errorf("store: dsn append change log (%s): %w", t, err)
	}
	return nil
}

// DSNEventsForSubmission returns the delivery-status records recorded against a
// submission (the per-recipient bounce detail behind RFC 8621's
// EmailSubmission.deliveryStatus), newest first.
func (tx *UserTx) DSNEventsForSubmission(ctx context.Context, submissionID ulid.ULID) ([]DSNEvent, error) {
	rows, err := tx.tx.Query(ctx, dsnSelect+`
		 WHERE user_id = $1 AND origin_submission_id = $2
		 ORDER BY created_at DESC, id DESC`,
		tx.userID.String(), submissionID.String())
	if err != nil {
		return nil, fmt.Errorf("store: dsn events for submission: %w", err)
	}
	defer rows.Close()
	return scanDSNEvents(rows)
}

// DSNEventsForMessage returns the delivery-status records for a sent message —
// the thread-level "delivery to X failed" surface (spec §4) — newest first.
func (tx *UserTx) DSNEventsForMessage(ctx context.Context, messageID ulid.ULID) ([]DSNEvent, error) {
	rows, err := tx.tx.Query(ctx, dsnSelect+`
		 WHERE user_id = $1 AND origin_message_id = $2
		 ORDER BY created_at DESC, id DESC`,
		tx.userID.String(), messageID.String())
	if err != nil {
		return nil, fmt.Errorf("store: dsn events for message: %w", err)
	}
	defer rows.Close()
	return scanDSNEvents(rows)
}

// AccountDeliveryFailures returns the recent failed/bounced delivery events for
// an account — the delivery-failure signal the account-health panel shows
// (spec §12) — resolved through the submissions the bounces link back to,
// newest first. limit defaults to 100.
func (tx *UserTx) AccountDeliveryFailures(ctx context.Context, accountID ulid.ULID, limit int) ([]DSNEvent, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := tx.tx.Query(ctx, `
		SELECT d.id, d.origin_message_id, d.origin_submission_id, d.dsn_message_id,
		       d.recipient, d.action, COALESCE(d.status_code, ''),
		       COALESCE(d.diagnostic, ''), d.created_at
		  FROM app.dsn_events d
		  JOIN app.submissions s
		    ON s.id = d.origin_submission_id AND s.user_id = d.user_id
		 WHERE d.user_id = $1 AND s.account_id = $2 AND d.action = 'failed'
		 ORDER BY d.created_at DESC, d.id DESC
		 LIMIT $3`,
		tx.userID.String(), accountID.String(), limit)
	if err != nil {
		return nil, fmt.Errorf("store: account delivery failures: %w", err)
	}
	defer rows.Close()
	return scanDSNEvents(rows)
}

const dsnSelect = `
	SELECT id, origin_message_id, origin_submission_id, dsn_message_id,
	       recipient, action, COALESCE(status_code, ''), COALESCE(diagnostic, ''),
	       created_at
	  FROM app.dsn_events`

func scanDSNEvents(rows pgx.Rows) ([]DSNEvent, error) {
	var out []DSNEvent
	for rows.Next() {
		var (
			e                    DSNEvent
			id                   string
			originMsg, originSub *string
			dsnMsg               *string
		)
		if err := rows.Scan(&id, &originMsg, &originSub, &dsnMsg,
			&e.Recipient, &e.Action, &e.Status, &e.Diagnostic, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: scan dsn event: %w", err)
		}
		var err error
		if e.ID, err = ulid.Parse(id); err != nil {
			return nil, fmt.Errorf("store: parse dsn event id %q: %w", id, err)
		}
		if e.OriginMessageID, err = parseULIDPtr(originMsg); err != nil {
			return nil, err
		}
		if e.OriginSubmissionID, err = parseULIDPtr(originSub); err != nil {
			return nil, err
		}
		if e.DSNMessageID, err = parseULIDPtr(dsnMsg); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: scan dsn events: %w", err)
	}
	return out, nil
}

// ulidPtrArg renders a *ULID as a nullable SQL argument.
func ulidPtrArg(id *ulid.ULID) any {
	if id == nil {
		return nil
	}
	return id.String()
}

// parseULIDPtr parses an optional ULID column into a *ULID (nil for SQL NULL).
func parseULIDPtr(s *string) (*ulid.ULID, error) {
	if s == nil || *s == "" {
		return nil, nil
	}
	u, err := ulid.Parse(*s)
	if err != nil {
		return nil, fmt.Errorf("store: parse dsn ulid %q: %w", *s, err)
	}
	return &u, nil
}
