// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/compose"
	"mailroom/internal/ical"
	"mailroom/internal/mime"
	"mailroom/internal/ml"
	"mailroom/internal/store"
)

// CALENDAR INVITATIONS (#725, urn:mailroom:calendar, ADR-033).
//
// Two surfaces:
//
//   - Email.calendarInvite — the invitation, cancellation or reply a message
//     carries, parsed HERE on the server (the client never sees a raw .ics),
//     plus what the rest of the mailbox says about it: a newer version, a
//     cancellation, and the user's own answer. A body property (rawProperties),
//     so it costs a list view nothing.
//   - Email/replyToInvite — Accept / Maybe / Decline. It builds an iTIP REPLY
//     (RFC 5546) as an iMIP message (RFC 6047) FROM the address the invitation
//     was delivered to, TO the organiser, saves it as a draft and submits it
//     through createSubmission — the same function EmailSubmission/set create
//     runs — so the From check, the ingest-only refusal and Undo Send all apply
//     exactly as they do to a message the user typed.
//
// ⚠ NOTHING HERE RESPONDS ON ITS OWN. Ingest indexes an invitation and never
// answers it; Email/get reads one and never answers it. The only way a REPLY
// leaves is a user calling Email/replyToInvite. And nothing here fetches any
// URL the calendar names (internal/ical cannot: it imports no network code).

// calendarCapability publishes the parser's bounds, so a client can say "this
// invitation is larger than the server reads" rather than guess (#725).
func calendarCapability() map[string]any {
	return map[string]any{
		"maxCalendarSize": ical.MaxBytes,
		"maxAttendees":    ical.MaxAttendees,
	}
}

// errCannotReply is the method-level error for an invitation that cannot be
// answered: not an invitation, no organiser, cancelled, or not addressed to an
// address the user can send from. An extension error type (RFC 8620 §3.6.2
// allows them); the description is written for the person who clicked.
const errCannotReply = "cannotReply"

// calendarKeyOf is the index key for an invitation, or false when it cannot be
// keyed (no UID or no organiser) — such an invitation still shows its card;
// it just cannot be related to other messages.
func calendarKeyOf(inv *ical.Invite) (store.CalendarKey, bool) {
	if inv == nil || inv.UID == "" || inv.Organizer == nil {
		return store.CalendarKey{}, false
	}
	k := store.CalendarKey{UID: inv.UID, RecurrenceID: inv.InstanceKey(),
		Organizer: strings.ToLower(inv.Organizer.Email)}
	return k, k.Valid()
}

// calendarContext is what the rest of the mailbox says about the invitations
// in one Email/get batch.
type calendarContext struct {
	versions  map[store.CalendarKey][]store.CalendarVersion
	responses map[store.CalendarKey]store.CalendarResponse
}

// resolveCalendarContext reads, in ONE transaction and two queries, the other
// versions of and the user's answers to every invitation in the batch. Nil
// when the client did not ask for calendarInvite.
func (a *API) resolveCalendarContext(ctx context.Context, userID ulid.ULID, props map[string]bool,
	bodies map[ulid.ULID]*emailBody) (*calendarContext, *MethodError) {
	if !props["calendarInvite"] {
		return nil, nil
	}
	var keys []store.CalendarKey
	for _, b := range bodies {
		if b == nil || b.calendar.Invite == nil {
			continue
		}
		if k, ok := calendarKeyOf(b.calendar.Invite); ok {
			keys = append(keys, k)
		}
	}
	cc := &calendarContext{}
	if len(keys) == 0 {
		return cc, nil
	}
	mErr := a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
		var err error
		if cc.versions, err = tx.CalendarVersions(ctx, keys); err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		if cc.responses, err = tx.CalendarResponses(ctx, keys); err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		return nil
	})
	if mErr != nil {
		return nil, mErr
	}
	return cc, nil
}

// calendarInviteObject renders Email.calendarInvite. Null when the message
// carries no calendar; {"problem": …} when it carries one that could not be
// read — a calendar that was there and too large must not look like no
// calendar (a bounded result reports its bound).
func calendarInviteObject(ev store.EmailView, body *emailBody, cc *calendarContext) any {
	if body == nil || !body.calendar.Present {
		return nil
	}
	inv := body.calendar.Invite
	if inv == nil {
		problem := "unreadable"
		switch {
		case errors.Is(body.calendar.Err, ical.ErrTooLarge),
			errors.Is(body.calendar.Err, ical.ErrTooComplex),
			errors.Is(body.calendar.Err, ical.ErrTooDeep):
			problem = "tooLarge"
		case errors.Is(body.calendar.Err, ical.ErrUnsupportedMethod),
			errors.Is(body.calendar.Err, ical.ErrNoEvent):
			// A published calendar or a task is not an invitation, and saying
			// "this invitation could not be read" about it would be false.
			return nil
		}
		return map[string]any{"problem": problem}
	}

	attendees := make([]map[string]any, 0, len(inv.Attendees))
	for _, at := range inv.Attendees {
		attendees = append(attendees, map[string]any{
			"name": nilIfEmpty(at.Name), "email": at.Email,
			"partstat": at.PartStat, "role": at.Role,
		})
	}
	out := map[string]any{
		"problem":          nil,
		"method":           inv.Method,
		"uid":              nilIfEmpty(inv.UID),
		"sequence":         inv.Sequence,
		"status":           nilIfEmpty(inv.Status),
		"summary":          inv.Summary,
		"location":         nilIfEmpty(inv.Location),
		"start":            calendarTime(inv.Start),
		"end":              calendarTime(inv.End),
		"organizer":        calendarAddress(inv.Organizer),
		"attendees":        attendees,
		"attendeesOmitted": inv.AttendeesOmitted,
		"recurrence":       calendarRecurrence(inv.Recurrence),
		"recurrenceId":     calendarTime(inv.RecurrenceID),
		"cancelled":        inv.Method == ical.MethodCancel || inv.Status == "CANCELLED",
		// Whether THIS message is authenticated as coming from the person it
		// speaks for (the organiser; for a REPLY, the attendee). An
		// unverified cancellation, update or reply still shows its own card,
		// and a client says it could not be verified; it changes no other.
		"verified":           body.calendarVerified,
		"cancelledByEmailId": nil,
		"newerEmailId":       nil,
		"updatesEmailId":     nil,
		"response":           nil,
	}

	key, keyed := calendarKeyOf(inv)
	if keyed && cc != nil {
		var newer, earlier, cancelledBy *store.CalendarVersion
		for i := range cc.versions[key] {
			v := &cc.versions[key][i]
			if v.MessageID == ev.ID {
				continue
			}
			// ⚠ ONLY A VERIFIED VERSION SPEAKS FOR THE EVENT (#725 review).
			// A UID is in every copy of an invitation, so an unauthenticated
			// CANCEL or update naming it would otherwise cancel or supersede
			// the genuine card. It may still be the EARLIER version this
			// message updates: that claim is about this message, whose own
			// `verified` the card reports.
			earlierRequest := v.Sequence < inv.Sequence && v.Method == store.CalendarRequest
			if !v.Verified && !earlierRequest {
				continue
			}
			switch {
			case v.Method == store.CalendarCancel && v.Sequence >= inv.Sequence && inv.Method == ical.MethodRequest:
				if cancelledBy == nil {
					cancelledBy = v
				}
			case v.Sequence > inv.Sequence:
				if newer == nil || v.Sequence > newer.Sequence {
					newer = v
				}
			case v.Sequence < inv.Sequence && v.Method == store.CalendarRequest:
				if earlier == nil || v.Sequence > earlier.Sequence {
					earlier = v
				}
			}
		}
		if cancelledBy != nil {
			out["cancelled"] = true
			out["cancelledByEmailId"] = cancelledBy.MessageID.String()
		}
		if newer != nil {
			out["newerEmailId"] = newer.MessageID.String()
		}
		if earlier != nil {
			out["updatesEmailId"] = earlier.MessageID.String()
		}
		if r, ok := cc.responses[key]; ok && inv.Method != ical.MethodReply {
			out["response"] = map[string]any{
				"partstat":     r.PartStat,
				"sequence":     r.Sequence,
				"respondedAt":  jmapDate(r.RespondedAt),
				"submissionId": optULID(r.SubmissionID),
				"emailId":      optULID(r.MessageID),
			}
		}
	}
	// What a client needs to offer the buttons, decided HERE so every client
	// draws the same line: an invitation (not a cancellation or a reply), with
	// an organiser to answer and a UID to answer about, not cancelled, and
	// delivered to an address the user can send from. The reason is given when
	// it cannot, so a client can say WHY rather than hide the buttons.
	out["canReply"], out["cannotReplyReason"] = replyability(ev, inv, out["cancelled"] == true)
	return out
}

// replyability is whether the invitation can be answered, and if not, why —
// one of a closed set of reasons a client can translate.
func replyability(ev store.EmailView, inv *ical.Invite, cancelled bool) (bool, any) {
	switch {
	case inv.Method != ical.MethodRequest:
		return false, "notAnInvitation"
	case cancelled:
		return false, "cancelled"
	case inv.Organizer == nil:
		return false, "noOrganizer"
	case inv.UID == "":
		return false, "noUid"
	case ev.AddressedIdentityID == (ulid.ULID{}) || ev.DeliveredTo == "":
		return false, "notAddressedToYou"
	}
	return true, nil
}

func optULID(id *ulid.ULID) any {
	if id == nil {
		return nil
	}
	return id.String()
}

func calendarTime(t *ical.Time) any {
	if t == nil {
		return nil
	}
	out := map[string]any{"kind": string(t.Kind), "utc": nil, "date": nil, "local": nil,
		"timeZone": nilIfEmpty(t.TZID)}
	switch t.Kind {
	case ical.TimeUTC:
		out["utc"] = jmapDate(t.UTC)
	case ical.TimeDate:
		out["date"] = t.Date
	default:
		out["local"] = t.Local
	}
	return out
}

func calendarAddress(a *ical.Address) any {
	if a == nil {
		return nil
	}
	return map[string]any{"name": nilIfEmpty(a.Name), "email": a.Email}
}

func calendarRecurrence(r *ical.Recurrence) any {
	if r == nil {
		return nil
	}
	byDay := r.ByDay
	if byDay == nil {
		byDay = []string{}
	}
	var count any
	if r.Count > 0 {
		count = r.Count
	}
	return map[string]any{
		"freq":     nilIfEmpty(r.Freq),
		"interval": r.Interval,
		"count":    count,
		"until":    calendarTime(r.Until),
		"byDay":    byDay,
		"text":     r.Text,
	}
}

// ---- Email/replyToInvite ----------------------------------------------------

type replyToInviteArgs struct {
	AccountID string `json:"accountId"`
	EmailID   string `json:"emailId"`
	// PartStat is "accepted", "tentative" or "declined" (any case).
	PartStat string `json:"partstat"`
	// SendAt is EmailSubmission/set's sendAt, passed through unchanged. Absent
	// is the ordinary send: now + the server's Undo Send window.
	SendAt *string `json:"sendAt"`
}

// replySubjectPrefix is Outlook's and Google's convention, which organisers'
// clients sort replies by.
var replySubjectPrefix = map[string]string{
	ical.PartStatAccepted:  "Accepted",
	ical.PartStatTentative: "Tentative",
	ical.PartStatDeclined:  "Declined",
}

var replyBodyVerb = map[string]string{
	ical.PartStatAccepted:  "has accepted",
	ical.PartStatTentative: "has tentatively accepted",
	ical.PartStatDeclined:  "has declined",
}

// emailReplyToInvite answers one invitation.
func (a *API) emailReplyToInvite(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	if a.composer == nil {
		return nil, &MethodError{Type: errServerFail,
			Description: "this server is not configured to send mail (no blob store or SMTP)"}
	}
	var args replyToInviteArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	if mErr := resolveAccount(userID, args.AccountID); mErr != nil {
		return nil, mErr
	}
	emailID, err := ulid.Parse(args.EmailID)
	if err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: "emailId is not an Email id"}
	}
	partstat := strings.ToUpper(strings.TrimSpace(args.PartStat))
	if !ical.ReplyPartStat(partstat) {
		return nil, &MethodError{Type: errInvalidArguments,
			Description: `partstat must be "accepted", "tentative" or "declined"`}
	}

	// 1. The invitation as stored. RLS makes another user's id indistinguishable
	//    from no id at all.
	var (
		ev    store.EmailView
		dated bool
	)
	if mErr := a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
		views, err := tx.GetEmails(ctx, []ulid.ULID{emailID})
		if err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		if len(views) == 0 {
			return &MethodError{Type: errInvalidArguments, Description: "no such email"}
		}
		ev = views[0]
		pd, err := tx.ProviderDated(ctx, []ulid.ULID{emailID})
		if err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		dated = pd[emailID]
		return nil
	}); mErr != nil {
		return nil, mErr
	}

	// 2. Its bytes, read and parsed OUTSIDE any transaction (loadBodies' rule).
	raw, err := a.raw.ReadRaw(ctx, userID, ev.ContentHash)
	if err != nil {
		slog.ErrorContext(ctx, "cannot read an invitation's bytes", "message_id", emailID.String(), "err", err)
		return nil, &MethodError{Type: errServerFail, Description: "this message's contents could not be read"}
	}
	parsed, _ := mime.ParseBytes(raw)
	var found ical.Found
	if parsed != nil {
		found = ical.FindInMessage(parsed.Structure)
	}
	inv := found.Invite
	if inv == nil {
		return nil, &MethodError{Type: errCannotReply,
			Description: "This message carries no invitation that can be answered."}
	}

	// 3. Who answers: the address the invitation was DELIVERED to, which ingest
	//    resolved to one of the user's verified identities (spec §5). The
	//    server re-validates the From of the bytes it builds in SaveDraft and
	//    again in PlanSubmit; this check only turns an unsendable answer into a
	//    clear sentence before anything is written.
	cancelled := inv.Method == ical.MethodCancel || inv.Status == "CANCELLED"
	if key, ok := calendarKeyOf(inv); ok && inv.Method == ical.MethodRequest {
		if mErr := a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
			vs, err := tx.CalendarVersions(ctx, []store.CalendarKey{key})
			if err != nil {
				return &MethodError{Type: errServerFail, Description: err.Error()}
			}
			for _, v := range vs[key] {
				if v.Verified && v.Method == store.CalendarCancel && v.Sequence >= inv.Sequence {
					cancelled = true
				}
			}
			return nil
		}); mErr != nil {
			return nil, mErr
		}
	}
	if ok, reason := replyability(ev, inv, cancelled); !ok {
		return nil, &MethodError{Type: errCannotReply, Description: cannotReplyText(reason)}
	}
	me := ical.Address{Email: ev.DeliveredTo}
	for _, at := range inv.Attendees {
		if strings.EqualFold(at.Email, ev.DeliveredTo) {
			me.Name = at.Name
			break
		}
	}
	ics, err := ical.BuildReply(inv, me, partstat, time.Now())
	if err != nil {
		return nil, &MethodError{Type: errCannotReply, Description: cannotReplyText(reasonOf(err))}
	}

	summary := inv.Summary
	if summary == "" {
		summary = "(no title)"
	}
	who := me.Email
	if me.Name != "" {
		who = me.Name
	}
	draft := compose.DraftRequest{
		From:      compose.Address{Email: ev.DeliveredTo},
		To:        []compose.Address{{Name: inv.Organizer.Name, Email: inv.Organizer.Email}},
		Subject:   replySubjectPrefix[partstat] + ": " + summary,
		Text:      who + " " + replyBodyVerb[partstat] + " this invitation.\n\n" + summary + "\n",
		InReplyTo: ev.MessageIDHint,
		// The parent's chain; compose.Build appends InReplyTo itself.
		References: trimMsgIDs(ev.References),
		Calendar:   &compose.CalendarPart{Method: ical.MethodReply, Data: ics},
	}

	// 4. Draft, submit, record — ONE transaction, so an answer is never
	//    recorded for a send that was refused, and a send is never scheduled
	//    without the answer it carries.
	key, _ := calendarKeyOf(inv)
	var (
		result   map[string]any
		draftID  ulid.ULID
		newState int64
	)
	mErr := a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
		res, err := a.composer.SaveDraft(ctx, tx, userID, draft)
		if err != nil {
			se := draftSetError(err)
			return &MethodError{Type: errCannotReply, Description: se.Description}
		}
		draftID = res.EmailID
		sub := map[string]any{"emailId": res.EmailID.String()}
		if args.SendAt != nil {
			sub["sendAt"] = *args.SendAt
		}
		rawSub, _ := json.Marshal(sub)
		obj, subID, _, serr := a.createSubmission(ctx, tx, userID, rawSub)
		if serr != nil {
			return &MethodError{Type: errCannotReply, Description: serr.Description}
		}
		if err := tx.RecordCalendarResponse(ctx, store.CalendarResponseRow{
			MessageID: emailID, Key: key, Sequence: inv.Sequence,
			PartStat: partstat, SubmissionID: subID,
			Verified: ical.Verified(inv, parsed, ml.TrustedAt(a.trustedAuthservIDs, store.TrustTime(ev.ReceivedAt, dated))),
		}); err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		result = obj
		if newState, err = tx.ChangeState(ctx, store.ObjectEmail); err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		return nil
	})
	if mErr != nil {
		return nil, mErr
	}
	return mustMarshal(map[string]any{
		"accountId":  userID.String(),
		"emailId":    draftID.String(),
		"partstat":   partstat,
		"submission": result,
		"newState":   encodeState(newState),
	}), nil
}

func reasonOf(err error) string {
	switch {
	case errors.Is(err, ical.ErrNoOrganizer):
		return "noOrganizer"
	case errors.Is(err, ical.ErrNoUID):
		return "noUid"
	case errors.Is(err, ical.ErrNotAnInvitation):
		return "notAnInvitation"
	default:
		return "notAddressedToYou"
	}
}

func cannotReplyText(reason any) string {
	switch reason {
	case "notAnInvitation":
		return "Only an invitation can be answered; this message is a cancellation or a reply."
	case "cancelled":
		return "This event has been cancelled, so there is nothing to answer."
	case "noOrganizer":
		return "This invitation does not name a valid organiser address, so there is nobody to send your answer to."
	case "noUid":
		return "This invitation has no event identifier, so an answer could not be matched to it."
	default:
		return "This invitation was not delivered to an address you can send from, so Mailroom cannot answer it for you."
	}
}
