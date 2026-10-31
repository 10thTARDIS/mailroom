// SPDX-License-Identifier: AGPL-3.0-only

package ical

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Limits on what an Invite carries. A string past its cap is cut, not refused:
// a long meeting title is still a meeting.
const (
	// MaxAttendees bounds the attendee list. The number dropped is reported in
	// Invite.AttendeesOmitted — a bounded list must say it is bounded.
	MaxAttendees = 100
	maxSummary   = 500
	maxLocation  = 500
	maxName      = 200
	// maxUID bounds a UID. Exchange's are ~110 characters; a UID longer than
	// this, or one carrying a control character, is not used at all (UID is
	// then ""), because a cut UID would name a different event.
	maxUID = 512
	// maxByDay bounds the BYDAY entries a recurrence summary reports.
	maxByDay = 7
)

// The iTIP methods a mail client acts on (RFC 5546 §1.4). PUBLISH, REFRESH,
// COUNTER, DECLINECOUNTER and ADD are not invitations to answer.
const (
	MethodRequest = "REQUEST"
	MethodCancel  = "CANCEL"
	MethodReply   = "REPLY"
)

// Participation statuses (RFC 5545 §3.2.12) an invite can carry, and the three
// a REPLY from Mailroom may send.
const (
	PartStatNeedsAction = "NEEDS-ACTION"
	PartStatAccepted    = "ACCEPTED"
	PartStatTentative   = "TENTATIVE"
	PartStatDeclined    = "DECLINED"
	PartStatDelegated   = "DELEGATED"
)

var (
	// ErrUnsupportedMethod is a calendar whose METHOD is not REQUEST, CANCEL or
	// REPLY — a published calendar, a counter-proposal — which is not an
	// invitation to show.
	ErrUnsupportedMethod = errors.New("ical: not an invitation, cancellation or reply")
	// ErrNoEvent is a calendar with no VEVENT (a VTODO invitation, say).
	ErrNoEvent = errors.New("ical: no event in the calendar")
)

// Address is a calendar user: an email address and the CN it was given.
type Address struct {
	Name  string
	Email string
}

// Attendee is one ATTENDEE with the status the invitation (or reply) gives them.
type Attendee struct {
	Address
	PartStat string
	Role     string
}

// Recurrence is a SUMMARY of an RRULE, never its expansion.
type Recurrence struct {
	Freq     string   // DAILY, WEEKLY, MONTHLY, YEARLY (HOURLY etc. are reported as-is)
	Interval int      // 1 = every period
	Count    int      // 0 = not bounded by count
	Until    *Time    // nil = not bounded by date
	ByDay    []string // e.g. ["MO","WE"], ["1MO"], ["-1FR"]; capped at maxByDay
	// Text is an English one-line description ("Repeats weekly on Monday,
	// 10 times") for JMAP clients that render no recurrence of their own. The
	// web client localises from the structured fields instead.
	Text string
}

// Invite is what a mail client shows about an invitation.
type Invite struct {
	Method   string
	UID      string
	Sequence int
	// Status is the VEVENT's STATUS (CANCELLED, CONFIRMED, TENTATIVE), or "".
	Status   string
	Summary  string
	Location string
	Start    *Time
	End      *Time
	// Organizer is nil when the invitation names none, or names something that
	// is not a single plain mailto: address. The reply path refuses on nil.
	Organizer        *Address
	Attendees        []Attendee
	AttendeesOmitted int
	Recurrence       *Recurrence
	// RecurrenceID is set when the invitation is about ONE instance of a
	// recurring event, not the series.
	RecurrenceID *Time

	// recurrenceIDRaw is RECURRENCE-ID exactly as it must be echoed in a REPLY
	// (value and TZID), already validated — see BuildReply.
	recurrenceIDRaw *Property
}

// ExtractInvite parses data and reads the invitation out of it.
func ExtractInvite(data []byte) (*Invite, error) {
	cal, err := Parse(data)
	if err != nil {
		return nil, err
	}
	return InviteFrom(cal)
}

// InviteFrom reads the invitation out of an already-parsed calendar.
func InviteFrom(cal *Component) (*Invite, error) {
	inv := &Invite{}
	if m := cal.Prop("METHOD"); m != nil {
		inv.Method = strings.ToUpper(strings.TrimSpace(m.Value))
	}
	switch inv.Method {
	case MethodRequest, MethodCancel, MethodReply:
	default:
		return nil, ErrUnsupportedMethod
	}

	// The series, not an override: the first VEVENT without a RECURRENCE-ID.
	// A calendar that carries ONLY overrides (Google's "this event only"
	// update) is about that instance, and the first one is the answer.
	events := cal.ChildrenNamed("VEVENT")
	if len(events) == 0 {
		return nil, ErrNoEvent
	}
	ev := events[0]
	for _, e := range events {
		if e.Prop("RECURRENCE-ID") == nil {
			ev = e
			break
		}
	}

	if p := ev.Prop("UID"); p != nil {
		uid := strings.TrimSpace(unescapeText(p.Value))
		if len(uid) <= maxUID && !hasControl(uid) {
			inv.UID = uid
		}
	}
	if p := ev.Prop("SEQUENCE"); p != nil {
		if n, err := strconv.Atoi(strings.TrimSpace(p.Value)); err == nil && n >= 0 && n < 1<<31-1 {
			inv.Sequence = n
		}
	}
	if p := ev.Prop("STATUS"); p != nil {
		switch s := strings.ToUpper(strings.TrimSpace(p.Value)); s {
		case "CANCELLED", "CONFIRMED", "TENTATIVE":
			inv.Status = s
		}
	}
	if p := ev.Prop("SUMMARY"); p != nil {
		inv.Summary = cleanText(unescapeText(p.Value), maxSummary)
	}
	if p := ev.Prop("LOCATION"); p != nil {
		inv.Location = cleanText(unescapeText(p.Value), maxLocation)
	}

	if t, ok := resolveTime(ev.Prop("DTSTART"), cal); ok {
		inv.Start = &t
	}
	inv.End = endOf(ev, inv.Start, cal)

	if p := ev.Prop("ORGANIZER"); p != nil {
		if addr := calAddress(p.Value); addr != "" {
			inv.Organizer = &Address{Name: cleanText(p.Param("CN"), maxName), Email: addr}
		}
	}
	for _, p := range ev.Props("ATTENDEE") {
		addr := calAddress(p.Value)
		if addr == "" {
			// Not an address (a room resource by URN, say): nobody to show.
			continue
		}
		if len(inv.Attendees) >= MaxAttendees {
			inv.AttendeesOmitted++
			continue
		}
		inv.Attendees = append(inv.Attendees, Attendee{
			Address:  Address{Name: cleanText(p.Param("CN"), maxName), Email: addr},
			PartStat: partStat(p.Param("PARTSTAT")),
			Role:     role(p.Param("ROLE")),
		})
	}

	if p := ev.Prop("RRULE"); p != nil {
		inv.Recurrence = summariseRRule(p.Value)
	}
	if p := ev.Prop("RECURRENCE-ID"); p != nil {
		if t, ok := resolveTime(p, cal); ok {
			inv.RecurrenceID = &t
			if raw, ok := echoableRecurrenceID(p); ok {
				inv.recurrenceIDRaw = raw
			}
		}
	}
	return inv, nil
}

// endOf is DTEND, else DTSTART + DURATION, else (RFC 5545 §3.6.1) the next day
// for an all-day event and the start itself for a timed one.
func endOf(ev *Component, start *Time, cal *Component) *Time {
	if t, ok := resolveTime(ev.Prop("DTEND"), cal); ok {
		return &t
	}
	if start == nil {
		return nil
	}
	d := time.Duration(0)
	if start.Kind == TimeDate {
		d = 24 * time.Hour
	}
	if p := ev.Prop("DURATION"); p != nil {
		if dd, err := parseDuration(p.Value); err == nil && dd >= 0 {
			d = dd
		}
	}
	end := *start
	switch start.Kind {
	case TimeUTC:
		end.UTC = start.UTC.Add(d)
	case TimeDate:
		t, err := time.Parse("2006-01-02", start.Date)
		if err != nil {
			return nil
		}
		end.Date = t.Add(d).Format("2006-01-02")
	case TimeFloating:
		t, err := time.Parse("2006-01-02T15:04:05", start.Local)
		if err != nil {
			return nil
		}
		end.Local = t.Add(d).Format("2006-01-02T15:04:05")
	}
	return &end
}

func partStat(v string) string {
	switch s := strings.ToUpper(strings.TrimSpace(v)); s {
	case PartStatAccepted, PartStatTentative, PartStatDeclined, PartStatDelegated, PartStatNeedsAction:
		return s
	default:
		// RFC 5545 §3.2.12: absent means NEEDS-ACTION, and an unrecognised
		// x-name or iana-token "MUST be treated as NEEDS-ACTION".
		return PartStatNeedsAction
	}
}

func role(v string) string {
	switch s := strings.ToUpper(strings.TrimSpace(v)); s {
	case "CHAIR", "REQ-PARTICIPANT", "OPT-PARTICIPANT", "NON-PARTICIPANT":
		return s
	default:
		return "REQ-PARTICIPANT"
	}
}

// summariseRRule reads the parts of an RRULE a person needs to know — how
// often, on which days, until when — and NEVER generates an occurrence. A rule
// with a FREQ this cannot name is still reported (the card says "repeats"),
// because "this repeats" is true even when "how" is not readable.
func summariseRRule(v string) *Recurrence {
	parts := rruleParts(v)
	if parts == nil {
		return &Recurrence{Text: "Repeats"}
	}
	r := &Recurrence{Interval: 1}
	switch f := parts["FREQ"]; f {
	case "SECONDLY", "MINUTELY", "HOURLY", "DAILY", "WEEKLY", "MONTHLY", "YEARLY":
		r.Freq = f
	}
	if n, err := strconv.Atoi(parts["INTERVAL"]); err == nil && n > 0 && n < 1000 {
		r.Interval = n
	}
	if n, err := strconv.Atoi(parts["COUNT"]); err == nil && n > 0 && n < 1<<31-1 {
		r.Count = n
	}
	if u := parts["UNTIL"]; u != "" {
		wall, isDate, isUTC, err := parseDateOrDateTime(u, "")
		if err == nil {
			switch {
			case isDate:
				r.Until = &Time{Kind: TimeDate, Date: wall.Format("2006-01-02")}
			case isUTC:
				r.Until = &Time{Kind: TimeUTC, UTC: wall}
			default:
				r.Until = &Time{Kind: TimeFloating, Local: wall.Format("2006-01-02T15:04:05")}
			}
		}
	}
	if d := parts["BYDAY"]; d != "" {
		for _, e := range strings.Split(d, ",") {
			if len(r.ByDay) >= maxByDay {
				break
			}
			if nth, _, ok := parseByDay(e); ok {
				day := strings.ToUpper(strings.TrimSpace(e))
				if nth == 0 {
					day = day[len(day)-2:]
				}
				r.ByDay = append(r.ByDay, day)
			}
		}
	}
	r.Text = recurrenceText(r)
	return r
}

var freqWords = map[string][2]string{
	"SECONDLY": {"every second", "seconds"},
	"MINUTELY": {"every minute", "minutes"},
	"HOURLY":   {"hourly", "hours"},
	"DAILY":    {"daily", "days"},
	"WEEKLY":   {"weekly", "weeks"},
	"MONTHLY":  {"monthly", "months"},
	"YEARLY":   {"yearly", "years"},
}

var dayNames = map[string]string{
	"MO": "Monday", "TU": "Tuesday", "WE": "Wednesday", "TH": "Thursday",
	"FR": "Friday", "SA": "Saturday", "SU": "Sunday",
}

func recurrenceText(r *Recurrence) string {
	w, ok := freqWords[r.Freq]
	if !ok {
		return "Repeats"
	}
	var b strings.Builder
	if r.Interval > 1 {
		fmt.Fprintf(&b, "Repeats every %d %s", r.Interval, w[1])
	} else {
		b.WriteString("Repeats " + w[0])
	}
	if len(r.ByDay) > 0 {
		names := make([]string, 0, len(r.ByDay))
		for _, d := range r.ByDay {
			name := dayNames[d[len(d)-2:]]
			if len(d) > 2 {
				name = ordinal(d[:len(d)-2]) + " " + name
			}
			names = append(names, name)
		}
		b.WriteString(" on " + strings.Join(names, ", "))
	}
	switch {
	case r.Count > 0:
		fmt.Fprintf(&b, ", %d times", r.Count)
	case r.Until != nil:
		b.WriteString(", until " + timeText(r.Until))
	}
	return b.String()
}

func ordinal(n string) string {
	switch n {
	case "1", "+1":
		return "first"
	case "2", "+2":
		return "second"
	case "3", "+3":
		return "third"
	case "4", "+4":
		return "fourth"
	case "-1":
		return "last"
	default:
		return n
	}
}

func timeText(t *Time) string {
	switch t.Kind {
	case TimeUTC:
		return t.UTC.Format("2006-01-02")
	case TimeDate:
		return t.Date
	default:
		if len(t.Local) >= 10 {
			return t.Local[:10]
		}
		return t.Local
	}
}

// echoableRecurrenceID returns RECURRENCE-ID in the exact form a REPLY may
// carry: its value only if it is a well-formed DATE or DATE-TIME, its TZID only
// if it is made of characters a zone name is made of, VALUE=DATE preserved.
// Anything else and the property is not echoed — a REPLY about "the series"
// is a smaller wrong than a REPLY carrying bytes a stranger chose.
func echoableRecurrenceID(p *Property) (*Property, bool) {
	v := strings.TrimSpace(p.Value)
	if _, _, _, err := parseDateOrDateTime(v, p.Param("VALUE")); err != nil {
		return nil, false
	}
	out := &Property{Name: "RECURRENCE-ID", Value: v}
	if tz := strings.TrimSpace(p.Param("TZID")); tz != "" {
		if !safeParamValue(tz) {
			return nil, false
		}
		out.Params = map[string][]string{"TZID": {tz}}
	}
	if strings.EqualFold(p.Param("VALUE"), "DATE") {
		if out.Params == nil {
			out.Params = map[string][]string{}
		}
		out.Params["VALUE"] = []string{"DATE"}
	}
	return out, true
}

// maxInstanceKey matches app.calendar_*.recurrence_id's CHECK.
const maxInstanceKey = 64

// InstanceKey names the instance an invitation is about in a canonical form —
// "" for the series — so the index matches "the Tuesday of this series"
// whichever zone or format the RECURRENCE-ID was written in.
func (inv *Invite) InstanceKey() string {
	t := inv.RecurrenceID
	if t == nil {
		return ""
	}
	var k string
	switch t.Kind {
	case TimeUTC:
		k = "U" + t.UTC.UTC().Format("20060102T150405Z")
	case TimeDate:
		k = "D" + t.Date
	default:
		k = "L" + t.Local
	}
	if len(k) > maxInstanceKey {
		k = k[:maxInstanceKey]
	}
	return k
}

// safeParamValue: letters, digits and the punctuation real zone names use.
func safeParamValue(s string) bool {
	if s == "" || len(s) > maxTZIDLen {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune("/_-+. ()", r):
		default:
			return false
		}
	}
	return true
}
