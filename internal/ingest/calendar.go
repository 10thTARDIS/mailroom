// SPDX-License-Identifier: AGPL-3.0-only

package ingest

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/ical"
	"mailroom/internal/mime"
	"mailroom/internal/ml"
	"mailroom/internal/store"
)

// indexCalendarInvite records an invitation or cancellation in
// app.calendar_invites (#725, migration 00063), in the ingest transaction, so
// the cards of the other messages of the same event can say "updated" or
// "cancelled".
//
// STRICTLY FAIL-SOFT, like the rules and the classifier: the store call is
// savepoint-guarded, and an error is logged and swallowed — indexing a
// calendar can never abort the ingest of the mail it is about. A message whose
// calendar is unreadable, too large, a REPLY, or names no organiser is simply
// not indexed: its card still renders from the bytes.
//
// It never responds to anything. Answering is Email/replyToInvite, which only
// a user's click reaches.
func (in *Ingestor) indexCalendarInvite(ctx context.Context, tx *store.UserTx, msgID ulid.ULID, parsed *mime.Message, received time.Time) {
	if parsed == nil || parsed.Structure == nil {
		return
	}
	inv := ical.FindInMessage(parsed.Structure).Invite
	if inv == nil || inv.UID == "" || inv.Organizer == nil {
		return
	}
	if inv.Method != ical.MethodRequest && inv.Method != ical.MethodCancel {
		return
	}
	err := tx.RecordCalendarInvite(ctx, store.CalendarInviteRow{
		MessageID: msgID,
		Key: store.CalendarKey{
			UID: inv.UID, RecurrenceID: inv.InstanceKey(), Organizer: inv.Organizer.Email,
		},
		Sequence: inv.Sequence,
		Method:   inv.Method,
		// Whether this message may change OTHER messages' cards: authenticated
		// as the organiser by the trusted Authentication-Results (#725 review).
		Verified: ical.Verified(inv, parsed, ml.TrustedAt(in.trustedAuthservIDs, received)),
	})
	if err != nil && !errors.Is(err, store.ErrCalendarKeyInvalid) {
		slog.WarnContext(ctx, "calendar invite not indexed; the message ingested normally",
			"message_id", msgID.String(), "err", err)
	}
}
