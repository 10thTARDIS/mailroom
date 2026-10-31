// SPDX-License-Identifier: AGPL-3.0-only

package ical

import (
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// ProdID names the software that wrote a REPLY (RFC 5545 §3.7.3). It names the
// product, never the instance.
const ProdID = "-//Mailroom//Mailroom Calendar Reply//EN"

// Errors BuildReply returns.
var (
	ErrNotAnInvitation = errors.New("ical: only an invitation (METHOD:REQUEST) can be answered")
	ErrNoOrganizer     = errors.New("ical: the invitation has no valid organiser address")
	ErrNoUID           = errors.New("ical: the invitation has no usable UID")
	ErrBadPartStat     = errors.New("ical: a reply is ACCEPTED, TENTATIVE or DECLINED")
	ErrBadAttendee     = errors.New("ical: the replying attendee has no valid address")
)

// ReplyPartStat reports whether s is one of the three answers a reply may give.
func ReplyPartStat(s string) bool {
	return s == PartStatAccepted || s == PartStatTentative || s == PartStatDeclined
}

// BuildReply writes the iTIP REPLY (RFC 5546 §3.2.3) carrying one attendee's
// answer to inv.
//
// The REPLY carries exactly what RFC 5546 requires and nothing that was not
// asked for: METHOD:REPLY; one VEVENT with UID, SEQUENCE, DTSTAMP, ORGANIZER,
// RECURRENCE-ID when the invitation was about one instance, and ONE ATTENDEE —
// this attendee, with their PARTSTAT. Other attendees' statuses are not echoed
// (they are not ours to assert), and the SUMMARY is included only because
// Outlook shows it in the organiser's tracking list.
//
// ⚠ EVERY ECHOED VALUE IS RE-ENCODED HERE. UID and SUMMARY are TEXT and are
// escaped (a newline in either becomes the two characters `\n`, never a line
// break); addresses were validated as single plain addr-specs; the CN is quoted
// with DQUOTE and control characters removed; RECURRENCE-ID was validated when
// it was read. So nothing the sender wrote can start a new content line — the
// CRLF-injection tests feed this exactly that.
func BuildReply(inv *Invite, attendee Address, partstat string, now time.Time) ([]byte, error) {
	if inv == nil || inv.Method != MethodRequest {
		return nil, ErrNotAnInvitation
	}
	if inv.Organizer == nil || ValidAddress(inv.Organizer.Email) == "" {
		return nil, ErrNoOrganizer
	}
	if inv.UID == "" {
		return nil, ErrNoUID
	}
	if !ReplyPartStat(partstat) {
		return nil, ErrBadPartStat
	}
	if ValidAddress(attendee.Email) == "" {
		return nil, ErrBadAttendee
	}

	var b strings.Builder
	line := func(name string, params map[string]string, value string) {
		var l strings.Builder
		l.WriteString(name)
		keys := make([]string, 0, len(params))
		for k := range params {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			l.WriteString(";" + k + "=" + params[k])
		}
		l.WriteString(":" + value)
		b.WriteString(fold(l.String()))
	}

	line("BEGIN", nil, "VCALENDAR")
	line("PRODID", nil, ProdID)
	line("VERSION", nil, "2.0")
	line("METHOD", nil, MethodReply)
	line("BEGIN", nil, "VEVENT")
	line("UID", nil, escapeText(inv.UID))
	line("SEQUENCE", nil, strconv.Itoa(inv.Sequence))
	line("DTSTAMP", nil, now.UTC().Format("20060102T150405Z"))
	line("ORGANIZER", cnParam(inv.Organizer.Name), "mailto:"+inv.Organizer.Email)
	att := cnParam(attendee.Name)
	if att == nil {
		att = map[string]string{}
	}
	att["PARTSTAT"] = partstat
	line("ATTENDEE", att, "mailto:"+attendee.Email)
	if r := inv.recurrenceIDRaw; r != nil {
		params := map[string]string{}
		if tz := r.Param("TZID"); tz != "" {
			params["TZID"] = quoteParam(tz)
		}
		if r.Param("VALUE") == "DATE" {
			params["VALUE"] = "DATE"
		}
		line("RECURRENCE-ID", params, r.Value)
	}
	if inv.Summary != "" {
		line("SUMMARY", nil, escapeText(inv.Summary))
	}
	line("END", nil, "VEVENT")
	line("END", nil, "VCALENDAR")
	return []byte(b.String()), nil
}

// cnParam is the CN parameter for a name, or nil for none.
func cnParam(name string) map[string]string {
	name = cleanText(name, maxName)
	if name == "" {
		return nil
	}
	return map[string]string{"CN": quoteParam(name)}
}

// quoteParam DQUOTEs a parameter value (RFC 5545 §3.2: a quoted value may
// contain anything but a control character or DQUOTE). DQUOTE is removed
// rather than escaped because the grammar has no escape for it.
func quoteParam(s string) string {
	s = strings.ReplaceAll(cleanText(s, maxName), `"`, "")
	return `"` + s + `"`
}

// escapeText encodes an RFC 5545 TEXT value. Every control character other than
// a newline is dropped first; newlines become `\n`.
func escapeText(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case ';':
			b.WriteString(`\;`)
		case ',':
			b.WriteString(`\,`)
		case '\n':
			b.WriteString(`\n`)
		default:
			if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
				continue
			}
			b.WriteRune(r)
		}
	}
	return b.String()
}

// foldOctets is RFC 5545 §3.1's line length: "SHOULD NOT be longer than 75
// octets, excluding the line break".
const foldOctets = 75

// fold writes one content line with CRLF, folded at foldOctets without ever
// splitting a UTF-8 sequence.
func fold(s string) string {
	var b strings.Builder
	n := 0
	limit := foldOctets
	for _, r := range s {
		w := utf8.RuneLen(r)
		if n+w > limit {
			b.WriteString("\r\n ")
			n = 0
			limit = foldOctets - 1 // the leading space counts
		}
		b.WriteRune(r)
		n += w
	}
	b.WriteString("\r\n")
	return b.String()
}
