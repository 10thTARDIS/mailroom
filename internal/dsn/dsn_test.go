// SPDX-License-Identifier: AGPL-3.0-only

package dsn

import (
	"strings"
	"testing"

	"mailroom/internal/mime"
)

// crlf renders a fixture with real \r\n line endings, the way a wire message
// arrives — MIME boundary parsing is line-ending sensitive, so testing with the
// on-the-wire form is what makes these fixtures meaningful.
func crlf(s string) []byte {
	return []byte(strings.ReplaceAll(s, "\n", "\r\n"))
}

func parse(t *testing.T, raw []byte) (Report, bool) {
	t.Helper()
	m, _ := mime.ParseBytes(raw)
	if m == nil {
		t.Fatal("mime.ParseBytes returned nil message")
	}
	return Parse(m)
}

// A genuine hard bounce (5.1.1 user unknown): a multipart/report with a full
// message/rfc822 returned copy. The recipient, action, status and diagnostic all
// parse, and the bounce links back via the returned Message-ID.
const hardBounce = `From: MAILER-DAEMON@mail.example.com
To: sender@example.com
Subject: Delivery Status Notification (Failure)
Message-ID: <bounce-hard@mail.example.com>
MIME-Version: 1.0
Content-Type: multipart/report; report-type=delivery-status; boundary="BOUND"

--BOUND
Content-Type: text/plain; charset=us-ascii

Delivery to the following recipient failed permanently: nouser@example.net

--BOUND
Content-Type: message/delivery-status

Reporting-MTA: dns; mail.example.com
Original-Envelope-Id: ENV-12345
Arrival-Date: Mon, 07 Jul 2026 12:00:00 +0000

Original-Recipient: rfc822;nouser@example.net
Final-Recipient: rfc822; nouser@example.net
Action: failed
Status: 5.1.1
Remote-MTA: dns; mx.example.net
Diagnostic-Code: smtp; 550 5.1.1 <nouser@example.net>: Recipient address
 rejected: User unknown

--BOUND
Content-Type: message/rfc822

From: sender@example.com
To: nouser@example.net
Subject: Hi there
Message-ID: <original-hard@example.com>
Date: Mon, 07 Jul 2026 11:59:00 +0000

Hello, this is the body that bounced.
--BOUND--
`

func TestParseHardBounce(t *testing.T) {
	report, ok := parse(t, crlf(hardBounce))
	if !ok {
		t.Fatal("hard bounce not recognized as a DSN")
	}
	if report.OriginalMessageID != "original-hard@example.com" {
		t.Errorf("OriginalMessageID = %q, want original-hard@example.com", report.OriginalMessageID)
	}
	if report.OriginalEnvelopeID != "ENV-12345" {
		t.Errorf("OriginalEnvelopeID = %q, want ENV-12345", report.OriginalEnvelopeID)
	}
	if len(report.Recipients) != 1 {
		t.Fatalf("got %d recipients, want 1", len(report.Recipients))
	}
	r := report.Recipients[0]
	if r.Address != "nouser@example.net" {
		t.Errorf("Address = %q, want nouser@example.net", r.Address)
	}
	if r.Action != "failed" {
		t.Errorf("Action = %q, want failed", r.Action)
	}
	if r.Status != "5.1.1" {
		t.Errorf("Status = %q, want 5.1.1", r.Status)
	}
	// The "smtp;" type prefix is stripped and the folded line is joined.
	if !strings.HasPrefix(r.Diagnostic, "550 5.1.1") || !strings.Contains(r.Diagnostic, "User unknown") {
		t.Errorf("Diagnostic = %q, want it to start 550 5.1.1 … User unknown", r.Diagnostic)
	}
	if strings.Contains(r.Diagnostic, "\n") {
		t.Errorf("Diagnostic should be a single folded line, got %q", r.Diagnostic)
	}
}

// A soft/transient delay (4.4.1), and the returned content is only the headers
// (text/rfc822-headers), which is the other returned-part shape we must handle.
const softDelay = `From: postmaster@mail.example.com
To: sender@example.com
Subject: Delivery Status Notification (Delay)
Message-ID: <bounce-delay@mail.example.com>
MIME-Version: 1.0
Content-Type: multipart/report; report-type=delivery-status; boundary="B2"

--B2
Content-Type: text/plain

Your message is delayed and delivery will be retried.

--B2
Content-Type: message/delivery-status

Reporting-MTA: dns; mail.example.com

Final-Recipient: rfc822; slow@example.org
Action: delayed
Status: 4.4.1
Diagnostic-Code: smtp; 421 4.4.1 connection timed out

--B2
Content-Type: text/rfc822-headers

From: sender@example.com
To: slow@example.org
Subject: Are you there
Message-ID: <original-delay@example.com>

--B2--
`

func TestParseSoftDelay(t *testing.T) {
	report, ok := parse(t, crlf(softDelay))
	if !ok {
		t.Fatal("soft delay not recognized as a DSN")
	}
	if report.OriginalMessageID != "original-delay@example.com" {
		t.Errorf("OriginalMessageID = %q, want original-delay@example.com", report.OriginalMessageID)
	}
	if len(report.Recipients) != 1 {
		t.Fatalf("got %d recipients, want 1", len(report.Recipients))
	}
	r := report.Recipients[0]
	if r.Address != "slow@example.org" {
		t.Errorf("Address = %q, want slow@example.org", r.Address)
	}
	if r.Action != "delayed" {
		t.Errorf("Action = %q, want delayed", r.Action)
	}
	if r.Status != "4.4.1" {
		t.Errorf("Status = %q, want 4.4.1", r.Status)
	}
}

// A multi-recipient DSN (one delivered, one failed) — one report can carry
// several recipient groups, and each becomes its own record.
const multiRecipient = `From: MAILER-DAEMON@mail.example.com
To: sender@example.com
Subject: Delivery report
MIME-Version: 1.0
Content-Type: multipart/report; report-type=delivery-status; boundary="MB"

--MB
Content-Type: message/delivery-status

Reporting-MTA: dns; mail.example.com

Final-Recipient: rfc822; ok@example.net
Action: delivered
Status: 2.0.0

Final-Recipient: rfc822; bad@example.net
Action: failed
Status: 5.2.2
Diagnostic-Code: smtp; 552 5.2.2 mailbox full

--MB
Content-Type: message/rfc822

Message-ID: <original-multi@example.com>
Subject: broadcast

body
--MB--
`

func TestParseMultiRecipient(t *testing.T) {
	report, ok := parse(t, crlf(multiRecipient))
	if !ok {
		t.Fatal("multi-recipient report not recognized")
	}
	if len(report.Recipients) != 2 {
		t.Fatalf("got %d recipients, want 2", len(report.Recipients))
	}
	byAddr := map[string]Recipient{}
	for _, r := range report.Recipients {
		byAddr[r.Address] = r
	}
	if got := byAddr["ok@example.net"]; got.Action != "delivered" || got.Status != "2.0.0" {
		t.Errorf("ok@ = %+v, want delivered/2.0.0", got)
	}
	if got := byAddr["bad@example.net"]; got.Action != "failed" || got.Status != "5.2.2" {
		t.Errorf("bad@ = %+v, want failed/5.2.2", got)
	}
}

// report-type parameter absent, but a message/delivery-status part is present:
// the structural signal alone must still identify the DSN.
const noReportType = `From: MAILER-DAEMON@mail.example.com
Subject: bounce
MIME-Version: 1.0
Content-Type: multipart/report; boundary="NR"

--NR
Content-Type: message/delivery-status

Final-Recipient: rfc822; x@example.net
Action: failed
Status: 5.1.1

--NR--
`

func TestParseDetectsWithoutReportType(t *testing.T) {
	report, ok := parse(t, crlf(noReportType))
	if !ok {
		t.Fatal("DSN with no report-type param not recognized via its delivery-status part")
	}
	if len(report.Recipients) != 1 || report.Recipients[0].Action != "failed" {
		t.Errorf("recipients = %+v, want one failed", report.Recipients)
	}
}

// A missing Action must be inferred from the status class rather than dropped or
// crashing: 5.x.x -> failed.
const missingAction = `From: MAILER-DAEMON@mail.example.com
MIME-Version: 1.0
Content-Type: multipart/report; report-type=delivery-status; boundary="MA"

--MA
Content-Type: message/delivery-status

Final-Recipient: rfc822; x@example.net
Status: 5.7.1

--MA--
`

func TestParseInfersActionFromStatus(t *testing.T) {
	report, ok := parse(t, crlf(missingAction))
	if !ok {
		t.Fatal("not recognized")
	}
	if len(report.Recipients) != 1 {
		t.Fatalf("got %d recipients, want 1", len(report.Recipients))
	}
	if report.Recipients[0].Action != "failed" {
		t.Errorf("inferred Action = %q, want failed (from 5.7.1)", report.Recipients[0].Action)
	}
}

// A badly malformed DSN — truncated report, garbage delivery-status body, no
// returned part — must never crash, must be recognized (a delivery-status part
// exists), and must record whatever it can without an original Message-ID.
const malformed = `From: MAILER-DAEMON@mail.example.com
MIME-Version: 1.0
Content-Type: multipart/report; report-type=delivery-status; boundary="XX"

--XX
Content-Type: message/delivery-status

this line has no colon and is nonsense
Final-Recipient rfc822 broken-no-colon
Action:::: failed
Status: not-a-status
Final-Recipient: rfc822; recovered@example.net
Action: failed
`

func TestParseMalformedNeverCrashes(t *testing.T) {
	// The point of the test is that this returns, at all.
	report, ok := parse(t, crlf(malformed))
	if !ok {
		t.Fatal("malformed DSN with a delivery-status part should still be recognized")
	}
	if report.OriginalMessageID != "" {
		t.Errorf("OriginalMessageID = %q, want empty (no returned part)", report.OriginalMessageID)
	}
	// The one well-formed recipient block should still be recovered; the junk
	// "Status: not-a-status" must be dropped to "" rather than stored as garbage.
	var found bool
	for _, r := range report.Recipients {
		if r.Address == "recovered@example.net" && r.Action == "failed" {
			found = true
		}
		if r.Status == "not-a-status" {
			t.Errorf("junk status leaked through: %q", r.Status)
		}
	}
	if !found {
		t.Errorf("did not recover the well-formed recipient; got %+v", report.Recipients)
	}
}

// An ordinary message must not be mistaken for a DSN.
const ordinary = `From: friend@example.com
To: me@example.com
Subject: lunch?
MIME-Version: 1.0
Content-Type: text/plain

Want to grab lunch tomorrow?
`

func TestParseIgnoresOrdinaryMail(t *testing.T) {
	if _, ok := parse(t, crlf(ordinary)); ok {
		t.Error("ordinary text/plain mail was misdetected as a DSN")
	}
}

// A multipart/mixed message (attachment) must not be mistaken for a DSN either.
const ordinaryMultipart = `From: friend@example.com
To: me@example.com
Subject: photo
MIME-Version: 1.0
Content-Type: multipart/mixed; boundary="MM"

--MM
Content-Type: text/plain

here you go
--MM
Content-Type: application/octet-stream
Content-Disposition: attachment; filename=x.bin

AAAA
--MM--
`

func TestParseIgnoresOrdinaryMultipart(t *testing.T) {
	if _, ok := parse(t, crlf(ordinaryMultipart)); ok {
		t.Error("ordinary multipart/mixed mail was misdetected as a DSN")
	}
}

func TestParseNilMessage(t *testing.T) {
	if _, ok := Parse(nil); ok {
		t.Error("nil message reported as a DSN")
	}
}
