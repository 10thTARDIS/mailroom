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

// CALENDAR INVITATIONS (#725, migration 00063, ADR-033) — the two pieces of
// state the message bytes cannot hold:
//
//   - app.calendar_invites, an index of invitation and cancellation messages by
//     event, so a card can say "there is a newer version" or "this was
//     cancelled";
//   - app.calendar_responses, the user's own answers, each bound to the
//     submission that carries its iTIP REPLY.
//
// Both surface on the JMAP Email object as `calendarInvite` (internal/api), on
// EVERY message carrying the event — so each write here appends an Email
// `updated` change row for each of those messages, in the same transaction
// (the load-bearing invariant). A write that changes nothing appends nothing.

// CalendarKey names one event as the index matches it: the UID, the instance
// (RecurrenceID, "" for the series) and the ORGANISER. The organiser is part
// of the key on purpose: a CANCEL from somebody else is somebody else's
// event, and must not cancel this one on the card.
type CalendarKey struct {
	UID          string
	RecurrenceID string
	Organizer    string // lower-cased; the column is citext
}

func (k CalendarKey) norm() CalendarKey {
	k.Organizer = strings.ToLower(k.Organizer)
	return k
}

// Valid reports whether the key can be indexed: the same bounds the table's
// CHECKs enforce, checked here so a hostile value is a clean refusal rather
// than a constraint error that poisons the caller's transaction.
func (k CalendarKey) Valid() bool {
	return k.UID != "" && len(k.UID) <= 512 && len(k.RecurrenceID) <= 64 &&
		len(k.Organizer) >= 3 && len(k.Organizer) <= 320 &&
		!strings.ContainsRune(k.UID, 0) && !strings.ContainsRune(k.RecurrenceID, 0) &&
		!strings.ContainsRune(k.Organizer, 0)
}

// Calendar methods the index records. REPLY is not indexed: nothing asks
// about replies across messages.
const (
	CalendarRequest = "REQUEST"
	CalendarCancel  = "CANCEL"
)

// CalendarInviteRow is one indexed invitation or cancellation message.
type CalendarInviteRow struct {
	MessageID ulid.ULID
	Key       CalendarKey
	Sequence  int
	Method    string
	// Verified is whether the message is authenticated as coming from the
	// organiser (internal/ical Verified). Only a verified row changes ANOTHER
	// message's card, so only a verified row appends their change rows.
	Verified bool
}

// ErrCalendarKeyInvalid is a key RecordCalendarInvite or RecordCalendarResponse
// refuses to write.
var ErrCalendarKeyInvalid = errors.New("store: calendar event key is not indexable")

// RecordCalendarInvite indexes one invitation or cancellation message. When
// the row is VERIFIED it appends an Email `updated` change_log row for every
// OTHER message of the same event: their cards now have a newer version (or a
// cancellation) to point to, and a client holding them must hear about it. An
// unverified row changes no other card (calendarInviteObject ignores it as a
// sibling), so it appends nothing for them.
//
// The message itself gets no row from here — it is new, and ingest appends its
// `created`. Re-recording the same message is a no-op that appends nothing.
//
// SAVEPOINT-GUARDED: ingest calls this fail-soft, and a failed statement here
// must cost the index row, never the message.
func (tx *UserTx) RecordCalendarInvite(ctx context.Context, row CalendarInviteRow) error {
	k := row.Key.norm()
	if !k.Valid() || row.Sequence < 0 || (row.Method != CalendarRequest && row.Method != CalendarCancel) {
		return ErrCalendarKeyInvalid
	}
	return tx.inSavepoint(ctx, func() error {
		tag, err := tx.tx.Exec(ctx, `
			INSERT INTO app.calendar_invites
			       (user_id, message_id, uid, recurrence_id, sequence, method, organizer, verified)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			ON CONFLICT (user_id, message_id) DO NOTHING`,
			tx.userID.String(), row.MessageID.String(), k.UID, k.RecurrenceID,
			row.Sequence, row.Method, k.Organizer, row.Verified)
		if err != nil {
			return fmt.Errorf("store: record calendar invite: %w", err)
		}
		if tag.RowsAffected() == 0 || !row.Verified {
			return nil
		}
		others, err := tx.calendarMessages(ctx, k)
		if err != nil {
			return err
		}
		ids := make([]ulid.ULID, 0, len(others))
		for _, id := range others {
			if id != row.MessageID {
				ids = append(ids, id)
			}
		}
		return tx.AppendChanges(ctx, ObjectEmail, ids, OpUpdated)
	})
}

// calendarMessages is every indexed message of one event, newest first.
func (tx *UserTx) calendarMessages(ctx context.Context, k CalendarKey) ([]ulid.ULID, error) {
	rows, err := tx.tx.Query(ctx, `
		SELECT message_id FROM app.calendar_invites
		 WHERE user_id = $1 AND uid = $2 AND recurrence_id = $3 AND organizer = $4::citext
		 ORDER BY recorded_at DESC, message_id DESC`,
		tx.userID.String(), k.UID, k.RecurrenceID, k.Organizer)
	if err != nil {
		return nil, fmt.Errorf("store: calendar messages: %w", err)
	}
	defer rows.Close()
	var out []ulid.ULID
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		id, err := ulid.Parse(s)
		if err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// CalendarVersion is one indexed message of an event.
type CalendarVersion struct {
	MessageID  ulid.ULID
	Sequence   int
	Method     string
	Verified   bool
	ReceivedAt time.Time
}

// maxCalendarVersions bounds the versions read PER EVENT KEY. One event with
// more indexed messages than this is a mailbox being flooded with one UID; the
// card is then judged on the newest maxCalendarVersions of THAT key — and,
// whatever the cap, on the key's highest-SEQUENCE verified CANCEL and
// highest-SEQUENCE verified REQUEST, so a flood of newer rows can never push
// the decisive version out of the window.
//
// ⚠ THE CAP IS PER KEY, IN SQL. It used to be one LIMIT over every row sharing
// a UID, applied before the (recurrence_id, organizer) filter ran in Go — so
// rows for a sibling instance, or a stranger's rows reusing the UID, could
// crowd the real CANCEL out. A var, not a const, so the test can make the
// window small enough to overflow.
var maxCalendarVersions = 500

// CalendarVersions returns, for each key, the indexed messages of that event,
// newest first by arrival. One query for the whole batch; the full key is
// matched in SQL.
func (tx *UserTx) CalendarVersions(ctx context.Context, keys []CalendarKey) (map[CalendarKey][]CalendarVersion, error) {
	out := map[CalendarKey][]CalendarVersion{}
	var uids, rids, orgs []string
	seen := map[CalendarKey]bool{}
	for _, k := range keys {
		k = k.norm()
		if k.UID == "" || seen[k] {
			continue
		}
		seen[k] = true
		uids, rids, orgs = append(uids, k.UID), append(rids, k.RecurrenceID), append(orgs, k.Organizer)
	}
	if len(uids) == 0 {
		return out, nil
	}
	rows, err := tx.tx.Query(ctx, `
		WITH k AS (
		  SELECT * FROM unnest($2::text[], $3::text[], $4::text[]) AS k(uid, rid, org)
		), v AS (
		  SELECT ci.uid, ci.recurrence_id, lower(ci.organizer::text) AS org, ci.message_id,
		         ci.sequence, ci.method, ci.verified, m.received_at,
		         row_number() OVER (PARTITION BY ci.uid, ci.recurrence_id, ci.organizer
		                            ORDER BY m.received_at DESC, ci.message_id DESC) AS recent,
		         row_number() OVER (PARTITION BY ci.uid, ci.recurrence_id, ci.organizer,
		                                         ci.method, ci.verified
		                            ORDER BY ci.sequence DESC, m.received_at DESC, ci.message_id DESC) AS decisive
		    FROM app.calendar_invites ci
		    JOIN k ON ci.uid = k.uid AND ci.recurrence_id = k.rid AND ci.organizer = k.org::citext
		    JOIN app.messages m ON m.id = ci.message_id
		   WHERE ci.user_id = $1
		)
		SELECT uid, recurrence_id, org, message_id, sequence, method, verified, received_at
		  FROM v
		 WHERE recent <= $5 OR (verified AND decisive = 1)
		 ORDER BY received_at DESC, message_id DESC`,
		tx.userID.String(), uids, rids, orgs, maxCalendarVersions)
	if err != nil {
		return nil, fmt.Errorf("store: calendar versions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			k   CalendarKey
			v   CalendarVersion
			mid string
		)
		if err := rows.Scan(&k.UID, &k.RecurrenceID, &k.Organizer, &mid, &v.Sequence, &v.Method, &v.Verified, &v.ReceivedAt); err != nil {
			return nil, err
		}
		if v.MessageID, err = ulid.Parse(mid); err != nil {
			return nil, err
		}
		k = k.norm()
		out[k] = append(out[k], v)
	}
	return out, rows.Err()
}

// CalendarResponse is the user's current answer to one event.
type CalendarResponse struct {
	PartStat    string
	Sequence    int
	RespondedAt time.Time
	// MessageID is the invitation that was answered (nil once it is deleted).
	MessageID *ulid.ULID
	// SubmissionID is the send carrying the REPLY (nil once its message is
	// deleted). A client can watch it to say "your reply could not be sent".
	SubmissionID *ulid.ULID
}

// CalendarResponses returns the current answer for each key: the newest
// response whose submission was not cancelled. One query for the batch.
func (tx *UserTx) CalendarResponses(ctx context.Context, keys []CalendarKey) (map[CalendarKey]CalendarResponse, error) {
	out := map[CalendarKey]CalendarResponse{}
	uids := uniqueUIDs(keys)
	if len(uids) == 0 {
		return out, nil
	}
	want := map[CalendarKey]bool{}
	for _, k := range keys {
		want[k.norm()] = true
	}
	rows, err := tx.tx.Query(ctx, `
		SELECT DISTINCT ON (r.uid, r.recurrence_id, r.organizer)
		       r.uid, r.recurrence_id, r.organizer::text, r.partstat, r.sequence,
		       r.responded_at, r.message_id, r.submission_id
		  FROM app.calendar_responses r
		  LEFT JOIN app.submissions s ON s.id = r.submission_id
		 WHERE r.user_id = $1 AND r.uid = ANY($2)
		   AND (s.state IS NULL OR s.state <> $3)
		 ORDER BY r.uid, r.recurrence_id, r.organizer, r.responded_at DESC, r.id DESC`,
		tx.userID.String(), uids, SubmissionCanceled)
	if err != nil {
		return nil, fmt.Errorf("store: calendar responses: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			k        CalendarKey
			r        CalendarResponse
			msg, sub *string
		)
		if err := rows.Scan(&k.UID, &k.RecurrenceID, &k.Organizer, &r.PartStat, &r.Sequence,
			&r.RespondedAt, &msg, &sub); err != nil {
			return nil, err
		}
		k = k.norm()
		if !want[k] {
			continue
		}
		if r.MessageID, err = parseOptULID(msg); err != nil {
			return nil, err
		}
		if r.SubmissionID, err = parseOptULID(sub); err != nil {
			return nil, err
		}
		out[k] = r
	}
	return out, rows.Err()
}

// CalendarResponseRow is one answer to record.
type CalendarResponseRow struct {
	// MessageID is the invitation answered.
	MessageID    ulid.ULID
	Key          CalendarKey
	Sequence     int
	PartStat     string
	SubmissionID ulid.ULID
	// Verified is the answered invitation's own verification, recorded if
	// this answer is what first indexes it (mail from before migration 00063).
	Verified bool
}

// RecordCalendarResponse stores one answer, makes sure the answered invitation
// is in the index (mail that arrived before migration 00063 is not), and
// appends ONE Email `updated` change row for each message of the event: every
// card for it now reads "You accepted".
func (tx *UserTx) RecordCalendarResponse(ctx context.Context, r CalendarResponseRow) error {
	k := r.Key.norm()
	if !k.Valid() || r.Sequence < 0 {
		return ErrCalendarKeyInvalid
	}
	switch r.PartStat {
	case "ACCEPTED", "TENTATIVE", "DECLINED":
	default:
		return fmt.Errorf("store: record calendar response: partstat %q", r.PartStat)
	}
	if _, err := tx.tx.Exec(ctx, `
		INSERT INTO app.calendar_responses
		       (id, user_id, message_id, uid, recurrence_id, organizer, sequence, partstat, submission_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		NewULID().String(), tx.userID.String(), r.MessageID.String(), k.UID, k.RecurrenceID,
		k.Organizer, r.Sequence, r.PartStat, r.SubmissionID.String()); err != nil {
		return fmt.Errorf("store: record calendar response: %w", err)
	}
	if _, err := tx.tx.Exec(ctx, `
		INSERT INTO app.calendar_invites
		       (user_id, message_id, uid, recurrence_id, sequence, method, organizer, verified)
		VALUES ($1, $2, $3, $4, $5, 'REQUEST', $6, $7)
		ON CONFLICT (user_id, message_id) DO NOTHING`,
		tx.userID.String(), r.MessageID.String(), k.UID, k.RecurrenceID, r.Sequence, k.Organizer, r.Verified); err != nil {
		return fmt.Errorf("store: index answered invitation: %w", err)
	}
	ids, err := tx.calendarMessages(ctx, k)
	if err != nil {
		return err
	}
	return tx.AppendChanges(ctx, ObjectEmail, ids, OpUpdated)
}

// calendarResponseWithdrawn is called when a submission is cancelled (Undo
// Send): any answer it carried stops being the user's answer, so every message
// of that event changes. Appends nothing when the submission carried none.
func (tx *UserTx) calendarResponseWithdrawn(ctx context.Context, submissionID ulid.ULID) error {
	rows, err := tx.tx.Query(ctx, `
		SELECT DISTINCT uid, recurrence_id, organizer::text FROM app.calendar_responses
		 WHERE user_id = $1 AND submission_id = $2`,
		tx.userID.String(), submissionID.String())
	if err != nil {
		return fmt.Errorf("store: calendar response for submission: %w", err)
	}
	var keys []CalendarKey
	for rows.Next() {
		var k CalendarKey
		if err := rows.Scan(&k.UID, &k.RecurrenceID, &k.Organizer); err != nil {
			rows.Close()
			return err
		}
		keys = append(keys, k.norm())
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	seen := map[ulid.ULID]bool{}
	var ids []ulid.ULID
	for _, k := range keys {
		msgs, err := tx.calendarMessages(ctx, k)
		if err != nil {
			return err
		}
		for _, id := range msgs {
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	return tx.AppendChanges(ctx, ObjectEmail, ids, OpUpdated)
}

func uniqueUIDs(keys []CalendarKey) []string {
	seen := map[string]bool{}
	var out []string
	for _, k := range keys {
		if k.UID != "" && !seen[k.UID] {
			seen[k.UID] = true
			out = append(out, k.UID)
		}
	}
	return out
}

func parseOptULID(s *string) (*ulid.ULID, error) {
	if s == nil {
		return nil, nil
	}
	id, err := ulid.Parse(*s)
	if err != nil {
		return nil, err
	}
	return &id, nil
}
