// SPDX-License-Identifier: AGPL-3.0-only

package ical

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"mailroom/internal/mime"
)

func parseMIME(t *testing.T, raw string) *mime.Structure {
	t.Helper()
	m, _ := mime.ParseBytes([]byte(strings.ReplaceAll(raw, "\n", "\r\n")))
	if m == nil || m.Structure == nil {
		t.Fatal("message did not parse")
	}
	return m.Structure
}

func b64(b []byte) string {
	s := base64.StdEncoding.EncodeToString(b)
	var out strings.Builder
	for len(s) > 76 {
		out.WriteString(s[:76] + "\n")
		s = s[76:]
	}
	out.WriteString(s)
	return out.String()
}

// googleShape is how Google Calendar mails an invitation: the calendar twice,
// once as the text/calendar alternative and once as an invite.ics attachment.
func googleShape(cal []byte, method string) string {
	return `From: Alice <alice@example.com>
To: bob@example.net
Subject: Invitation: Weekly sync
MIME-Version: 1.0
Content-Type: multipart/mixed; boundary="mixed"

--mixed
Content-Type: multipart/alternative; boundary="alt"

--alt
Content-Type: text/plain; charset="UTF-8"

You have been invited.
--alt
Content-Type: text/html; charset="UTF-8"

<p>You have been invited.</p>
--alt
Content-Type: text/calendar; charset="UTF-8"; method=` + method + `
Content-Transfer-Encoding: base64

` + b64(cal) + `
--alt--
--mixed
Content-Type: application/ics; name="invite.ics"
Content-Disposition: attachment; filename="invite.ics"
Content-Transfer-Encoding: base64

` + b64(cal) + `
--mixed--
`
}

func TestFindInGoogleShapedMessage(t *testing.T) {
	cal := sample(t, "google_request.ics")
	f := FindInMessage(parseMIME(t, googleShape(cal, "REQUEST")))
	if !f.Present || f.Err != nil || f.Invite == nil || f.Invite.Summary != "Weekly sync" {
		t.Fatalf("found = %+v", f)
	}
}

func TestFindInOutlookShapedMessage(t *testing.T) {
	// Outlook sends multipart/alternative with the calendar as the last
	// alternative, quoted-printable or 8bit, no attachment.
	cal := string(sample(t, "outlook_request.ics"))
	raw := `From: Carol <carol@example.org>
To: dave@example.net
Subject: Quarterly review
MIME-Version: 1.0
Content-Type: multipart/alternative; boundary="b1"

--b1
Content-Type: text/plain; charset="us-ascii"

When: Wednesday 09:00
--b1
Content-Type: text/calendar; charset="utf-8"; method=REQUEST
Content-Transfer-Encoding: 8bit

` + strings.ReplaceAll(cal, "\r\n", "\n") + `
--b1--
`
	f := FindInMessage(parseMIME(t, raw))
	if f.Invite == nil || f.Invite.Summary != "Quarterly review" {
		t.Fatalf("found = %+v", f)
	}
}

func TestFindIgnoresAForwardedInvitation(t *testing.T) {
	inner := googleShape(sample(t, "google_request.ics"), "REQUEST")
	raw := `From: Me <me@example.net>
To: you@example.net
Subject: Fwd: Invitation
MIME-Version: 1.0
Content-Type: multipart/mixed; boundary="outer"

--outer
Content-Type: text/plain

See below.
--outer
Content-Type: message/rfc822

` + inner + `
--outer--
`
	f := FindInMessage(parseMIME(t, raw))
	if f.Present || f.Invite != nil {
		t.Fatalf("a forwarded invitation was treated as this message's: %+v", f)
	}
}

func TestFindReportsTooLargeRatherThanNothing(t *testing.T) {
	head := "BEGIN:VCALENDAR\nMETHOD:REQUEST\nBEGIN:VEVENT\nUID:x\nDESCRIPTION:"
	big := head + strings.Repeat("A", MaxBytes) + "\nEND:VEVENT\nEND:VCALENDAR\n"
	f := FindInMessage(parseMIME(t, googleShape([]byte(big), "REQUEST")))
	if !f.Present || f.Invite != nil || !errors.Is(f.Err, ErrTooLarge) {
		t.Fatalf("found = %+v, want present + ErrTooLarge", f)
	}
}

func TestFindPrefersTheInvitationOverAPublishedCalendar(t *testing.T) {
	pub := strings.Replace(string(sample(t, "google_request.ics")), "METHOD:REQUEST", "METHOD:PUBLISH", 1)
	f := FindInMessage(parseMIME(t, googleShape([]byte(pub), "PUBLISH")))
	if !f.Present || f.Invite != nil || !errors.Is(f.Err, ErrUnsupportedMethod) {
		t.Fatalf("PUBLISH only: %+v", f)
	}
	f = FindInMessage(parseMIME(t, `From: a@example.com
MIME-Version: 1.0
Content-Type: text/plain

no calendar here
`))
	if f.Present || f.Invite != nil || f.Err != nil {
		t.Fatalf("no calendar: %+v", f)
	}
}
