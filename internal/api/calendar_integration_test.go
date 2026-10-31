// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// Calendar invitations END TO END (#725): real bytes through the real ingest
// (which indexes them), out of the real Email/get, answered through the real
// Email/replyToInvite — which saves a draft and submits it through the same
// createSubmission EmailSubmission/set runs — and taken back by the real Undo
// Send.

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/compose"
	"mailroom/internal/ical"
	"mailroom/internal/ingest"
)

const inviteeAddr = "bob@example.net"

// trustedID is the authserv-id the harness's ingest and API believe
// (MAILROOM_AUTHSERV_IDS), so a message can be AUTHENTICATED as coming from
// its organiser (ical.Verified) the way a real provider's header makes it.
const trustedID = "mx.example.test"

// newCalendarHarness is the compose harness with the trusted authserv-id
// wired into the API, as production wires MAILROOM_AUTHSERV_IDS.
func newCalendarHarness(t *testing.T) *composeHarness {
	t.Helper()
	h := newComposeHarness(t)
	h.api = New(h.store, WithComposer(compose.NewComposer(h.blobs, fakeKeys{}, time.Millisecond)),
		WithTrustedAuthservIDs([]string{trustedID}))
	return h
}

func icsSample(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "ical", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// inviteMail wraps a calendar the way Google sends one: the text/calendar
// alternative plus an invite.ics attachment of the same bytes, From the
// samples' organiser and AUTHENTICATED (a trusted DMARC pass for its domain).
func inviteMail(cal, to, subject, msgID string) []byte {
	return inviteMailFrom(cal, "Alice Example <alice@example.com>", "dmarc=pass header.from=example.com", to, subject, msgID)
}

// inviteMailFrom is inviteMail with the From and the trusted
// Authentication-Results verdict chosen ("" = no such header at all).
func inviteMailFrom(cal, from, verdict, to, subject, msgID string) []byte {
	auth := ""
	if verdict != "" {
		auth = "Authentication-Results: " + trustedID + "; " + verdict + "\n"
	}
	enc := base64.StdEncoding.EncodeToString([]byte(cal))
	method := "REQUEST"
	for _, l := range strings.Split(cal, "\n") {
		if strings.HasPrefix(l, "METHOD:") {
			method = strings.TrimSpace(strings.TrimPrefix(l, "METHOD:"))
		}
	}
	return []byte(strings.ReplaceAll(auth+`From: `+from+`
To: `+to+`
Subject: `+subject+`
Message-ID: <`+msgID+`>
Date: Thu, 15 Oct 2026 09:30:00 +0000
MIME-Version: 1.0
Content-Type: multipart/mixed; boundary="mixed"

--mixed
Content-Type: multipart/alternative; boundary="alt"

--alt
Content-Type: text/plain; charset="UTF-8"

You have been invited.
--alt
Content-Type: text/calendar; charset="UTF-8"; method=`+method+`
Content-Transfer-Encoding: base64

`+enc+`
--alt--
--mixed
Content-Type: application/ics; name="invite.ics"
Content-Disposition: attachment; filename="invite.ics"
Content-Transfer-Encoding: base64

`+enc+`
--mixed--
`, "\n", "\r\n"))
}

var nextIngestUID int64 = 9000

// ingestMail runs raw bytes through the REAL ingest into the fixture's Inbox.
func ingestMail(t *testing.T, h *composeHarness, f apiFixture, raw []byte) ulid.ULID {
	t.Helper()
	ctx := context.Background()
	dek, dekID, _ := fakeKeys{}.MailDEK(ctx, f.UserID)
	nextIngestUID++
	res, err := ingest.New(h.store, h.blobs, ingest.WithTrustedAuthservIDs([]string{trustedID})).Ingest(ctx, f.UserID, dek, dekID, raw, ingest.Provenance{
		AccountID: f.AccountID, MailboxID: f.InboxID, UID: nextIngestUID, UIDValidity: 1,
		InternalDate: time.Now(),
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	return res.MessageID
}

func getInvite(t *testing.T, h *composeHarness, f apiFixture, id ulid.ULID) (any, bool) {
	t.Helper()
	res := call(t, h.api.emailGet, f.UserID, map[string]any{
		"ids": []string{id.String()}, "properties": []string{"id", "calendarInvite"},
	})
	list, _ := res["list"].([]any)
	if len(list) != 1 {
		t.Fatalf("Email/get: %#v", res)
	}
	v, ok := list[0].(map[string]any)["calendarInvite"]
	return v, ok
}

func inviteObj(t *testing.T, h *composeHarness, f apiFixture, id ulid.ULID) map[string]any {
	t.Helper()
	v, _ := getInvite(t, h, f, id)
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("calendarInvite = %#v, want an object", v)
	}
	return m
}

func calRows(t *testing.T, h *composeHarness, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := h.super.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

func TestCalendarInviteOnEmailGet(t *testing.T) {
	h := newCalendarHarness(t)
	a, _ := seedTwo(t, h.super)
	seedSendable(t, h.super, a, inviteeAddr, "")

	id := ingestMail(t, h, a, inviteMail(icsSample(t, "google_request.ics"), inviteeAddr, "Invitation: Weekly sync", "inv-1@example.com"))
	inv := inviteObj(t, h, a, id)
	if inv["problem"] != nil || inv["method"] != "REQUEST" || inv["summary"] != "Weekly sync" {
		t.Fatalf("invite = %#v", inv)
	}
	start, _ := inv["start"].(map[string]any)
	if start["kind"] != "utc" || start["utc"] != "2026-10-20T15:00:00Z" {
		t.Fatalf("start = %#v", start)
	}
	if org, _ := inv["organizer"].(map[string]any); org["email"] != "alice@example.com" {
		t.Fatalf("organizer = %#v", inv["organizer"])
	}
	if at, _ := inv["attendees"].([]any); len(at) != 3 {
		t.Fatalf("attendees = %#v", inv["attendees"])
	}
	if inv["canReply"] != true || inv["cancelled"] != false || inv["response"] != nil {
		t.Fatalf("canReply/cancelled/response = %v/%v/%v (%v)", inv["canReply"], inv["cancelled"], inv["response"], inv["cannotReplyReason"])
	}

	// NEVER AUTO-RESPOND: ingesting and reading the invitation wrote no draft,
	// no submission and no answer.
	if n := calRows(t, h, `SELECT count(*) FROM app.submissions WHERE user_id=$1`, a.UserID.String()); n != 0 {
		t.Fatalf("%d submissions exist after merely receiving and reading an invitation", n)
	}
	if n := calRows(t, h, `SELECT count(*) FROM app.calendar_responses WHERE user_id=$1`, a.UserID.String()); n != 0 {
		t.Fatalf("%d answers exist after merely receiving and reading an invitation", n)
	}
	// It was indexed by ingest.
	if n := calRows(t, h, `SELECT count(*) FROM app.calendar_invites WHERE message_id=$1`, id.String()); n != 1 {
		t.Fatalf("ingest indexed %d rows for the invitation", n)
	}

	// A message with no calendar: present and null.
	v, present := getInvite(t, h, a, a.MessageID)
	if !present || v != nil {
		t.Fatalf("plain message: calendarInvite = %#v (present %v)", v, present)
	}

	// A calendar too large to read says so, rather than looking like none.
	big := strings.Replace(icsSample(t, "google_request.ics"), "SUMMARY:Weekly sync",
		"SUMMARY:Weekly sync\r\nDESCRIPTION:"+strings.Repeat("x", ical.MaxBytes), 1)
	bigID := ingestMail(t, h, a, inviteMail(big, inviteeAddr, "Big", "inv-big@example.com"))
	if got := inviteObj(t, h, a, bigID); got["problem"] != "tooLarge" {
		t.Fatalf("oversized calendar: %#v", got)
	}

	// A REPLY from an attendee shows who answered, and offers no buttons.
	replyID := ingestMail(t, h, a, inviteMail(icsSample(t, "google_reply.ics"), inviteeAddr, "Declined: Design review", "rep-1@example.com"))
	rep := inviteObj(t, h, a, replyID)
	at, _ := rep["attendees"].([]any)
	if rep["method"] != "REPLY" || len(at) != 1 || at[0].(map[string]any)["partstat"] != "DECLINED" ||
		rep["canReply"] != false || rep["cannotReplyReason"] != "notAnInvitation" {
		t.Fatalf("reply card = %#v", rep)
	}
}

func TestCalendarUpdatesAndCancellationsLinkTheCards(t *testing.T) {
	h := newCalendarHarness(t)
	a, _ := seedTwo(t, h.super)
	seedSendable(t, h.super, a, inviteeAddr, "")

	v0 := ingestMail(t, h, a, inviteMail(icsSample(t, "google_request.ics"), inviteeAddr, "Invitation", "v0@example.com"))
	before := calRows(t, h, `SELECT count(*) FROM app.change_log WHERE object_type='Email' AND object_id=$1`, v0.String())
	v1 := ingestMail(t, h, a, inviteMail(icsSample(t, "google_update.ics"), inviteeAddr, "Updated invitation", "v1@example.com"))
	if d := calRows(t, h, `SELECT count(*) FROM app.change_log WHERE object_type='Email' AND object_id=$1`, v0.String()) - before; d != 1 {
		t.Fatalf("the update appended %d change rows to the original, want 1 (its card changed)", d)
	}

	c0, c1 := inviteObj(t, h, a, v0), inviteObj(t, h, a, v1)
	if c0["newerEmailId"] != v1.String() || c0["updatesEmailId"] != nil {
		t.Fatalf("original: newer=%v updates=%v", c0["newerEmailId"], c0["updatesEmailId"])
	}
	if c1["updatesEmailId"] != v0.String() || c1["sequence"] != float64(1) || c1["newerEmailId"] != nil {
		t.Fatalf("update: updates=%v seq=%v newer=%v", c1["updatesEmailId"], c1["sequence"], c1["newerEmailId"])
	}

	// A stranger's CANCEL for the same UID cancels nothing.
	stranger := strings.Replace(icsSample(t, "google_update.ics"), "METHOD:REQUEST", "METHOD:CANCEL", 1)
	stranger = strings.Replace(stranger, "SEQUENCE:1", "SEQUENCE:5", 1)
	stranger = strings.Replace(stranger, "ORGANIZER;CN=Alice Example:mailto:alice@example.com", "ORGANIZER:mailto:mallory@example.org", 1)
	ingestMail(t, h, a, inviteMail(stranger, inviteeAddr, "Cancelled", "evil@example.org"))
	if c := inviteObj(t, h, a, v1); c["cancelled"] != false || c["canReply"] != true {
		t.Fatalf("a stranger's CANCEL cancelled the card: %#v", c)
	}

	// The organiser's CANCEL does.
	cancel := strings.Replace(icsSample(t, "google_update.ics"), "METHOD:REQUEST", "METHOD:CANCEL", 1)
	cancel = strings.Replace(cancel, "SEQUENCE:1", "SEQUENCE:2", 1)
	cancel = strings.Replace(cancel, "STATUS:CONFIRMED", "STATUS:CANCELLED", 1)
	cID := ingestMail(t, h, a, inviteMail(cancel, inviteeAddr, "Cancelled: Weekly sync", "cx@example.com"))
	for _, id := range []ulid.ULID{v0, v1} {
		c := inviteObj(t, h, a, id)
		if c["cancelled"] != true || c["cancelledByEmailId"] != cID.String() ||
			c["canReply"] != false || c["cannotReplyReason"] != "cancelled" {
			t.Fatalf("after the CANCEL, %s: %#v", id, c)
		}
	}
	if c := inviteObj(t, h, a, cID); c["method"] != "CANCEL" || c["cancelled"] != true {
		t.Fatalf("the cancellation itself: %#v", c)
	}
}

func TestReplyToInviteSendsAnITIPReplyThroughTheSubmissionPath(t *testing.T) {
	h := newCalendarHarness(t)
	a, _ := seedTwo(t, h.super)
	seedSendable(t, h.super, a, inviteeAddr, "")
	inv := ingestMail(t, h, a, inviteMail(icsSample(t, "google_request.ics"), inviteeAddr, "Invitation: Weekly sync", "inv-r@example.com"))
	upd := ingestMail(t, h, a, inviteMail(icsSample(t, "google_update.ics"), inviteeAddr, "Updated", "upd-r@example.com"))

	b0 := calRows(t, h, `SELECT count(*) FROM app.change_log WHERE object_type='Email' AND object_id=$1`, inv.String())
	b1 := calRows(t, h, `SELECT count(*) FROM app.change_log WHERE object_type='Email' AND object_id=$1`, upd.String())
	res := call(t, h.api.emailReplyToInvite, a.UserID, map[string]any{
		"emailId": upd.String(), "partstat": "accepted",
	})
	draftID, _ := res["emailId"].(string)
	sub, _ := res["submission"].(map[string]any)
	if draftID == "" || sub == nil || sub["undoStatus"] != "pending" {
		t.Fatalf("result = %#v", res)
	}
	// ONE change row per message of the event, in the answer's transaction.
	if d0, d1 := calRows(t, h, `SELECT count(*) FROM app.change_log WHERE object_type='Email' AND object_id=$1`, inv.String())-b0,
		calRows(t, h, `SELECT count(*) FROM app.change_log WHERE object_type='Email' AND object_id=$1`, upd.String())-b1; d0 != 1 || d1 != 1 {
		t.Fatalf("answer appended %d/%d change rows, want 1/1", d0, d1)
	}

	// THE WIRE: the draft that will be sent is From the invited address, To
	// the organiser, threaded to the invitation, and carries a text/calendar
	// METHOD:REPLY with exactly this attendee's PARTSTAT.
	got := call(t, h.api.emailGet, a.UserID, map[string]any{
		"ids":                []string{draftID},
		"properties":         []string{"from", "to", "subject", "inReplyTo", "bodyStructure", "bodyValues"},
		"bodyProperties":     []string{"partId", "type"},
		"fetchAllBodyValues": true,
	})
	d := got["list"].([]any)[0].(map[string]any)
	if f := d["from"].([]any)[0].(map[string]any); f["email"] != inviteeAddr {
		t.Fatalf("from = %#v", d["from"])
	}
	if to := d["to"].([]any); len(to) != 1 || to[0].(map[string]any)["email"] != "alice@example.com" {
		t.Fatalf("to = %#v", d["to"])
	}
	if d["subject"] != "Accepted: Weekly sync (moved)" {
		t.Fatalf("subject = %#v", d["subject"])
	}
	if irt, _ := d["inReplyTo"].([]any); len(irt) != 1 || irt[0] != "upd-r@example.com" {
		t.Fatalf("inReplyTo = %#v", d["inReplyTo"])
	}
	var calPart string
	for _, p := range d["bodyStructure"].(map[string]any)["subParts"].([]any) {
		pm := p.(map[string]any)
		if strings.HasPrefix(pm["type"].(string), "text/calendar") {
			calPart = pm["partId"].(string)
		}
	}
	if calPart == "" {
		t.Fatalf("no text/calendar part: %#v", d["bodyStructure"])
	}
	val := d["bodyValues"].(map[string]any)[calPart].(map[string]any)["value"].(string)
	reply, err := ical.ExtractInvite([]byte(val))
	if err != nil {
		t.Fatalf("the sent calendar does not parse: %v\n%s", err, val)
	}
	if reply.Method != ical.MethodReply || reply.Sequence != 1 || len(reply.Attendees) != 1 ||
		reply.Attendees[0].Email != inviteeAddr || reply.Attendees[0].PartStat != ical.PartStatAccepted {
		t.Fatalf("REPLY = %+v", reply)
	}
	if n := calRows(t, h, `SELECT count(*) FROM app.submissions WHERE user_id=$1 AND state='scheduled'`, a.UserID.String()); n != 1 {
		t.Fatalf("%d scheduled submissions, want the one reply", n)
	}

	// IT SURVIVES A RELOAD: a fresh Email/get on BOTH messages of the event
	// says "You accepted".
	for _, id := range []ulid.ULID{inv, upd} {
		r, _ := inviteObj(t, h, a, id)["response"].(map[string]any)
		if r == nil || r["partstat"] != "ACCEPTED" || r["submissionId"] != sub["id"] {
			t.Fatalf("%s response = %#v", id, r)
		}
	}

	// UNDO SEND through the ordinary EmailSubmission/set takes it back.
	call(t, h.api.submissionSet, a.UserID, map[string]any{
		"update": map[string]any{sub["id"].(string): map[string]any{"undoStatus": "canceled"}},
	})
	if r := inviteObj(t, h, a, upd)["response"]; r != nil {
		t.Fatalf("after Undo Send the card still says %#v", r)
	}
}

func TestReplyToInviteRefusals(t *testing.T) {
	h := newCalendarHarness(t)
	a, _ := seedTwo(t, h.super)
	seedSendable(t, h.super, a, inviteeAddr, "")

	refused := func(id ulid.ULID, partstat, wantType, wantText string) {
		t.Helper()
		_, mErr := h.api.emailReplyToInvite(context.Background(), a.UserID, mustJSON(t, map[string]any{
			"emailId": id.String(), "partstat": partstat,
		}))
		if mErr == nil || mErr.Type != wantType || !strings.Contains(mErr.Description, wantText) {
			t.Fatalf("got %+v, want %s containing %q", mErr, wantType, wantText)
		}
	}

	noOrg := strings.Replace(icsSample(t, "google_request.ics"), "ORGANIZER;CN=Alice Example:mailto:alice@example.com\r\n", "", 1)
	refused(ingestMail(t, h, a, inviteMail(noOrg, inviteeAddr, "No organiser", "no-org@example.com")),
		"accepted", errCannotReply, "does not name a valid organiser")
	badOrg := strings.Replace(icsSample(t, "google_request.ics"), "mailto:alice@example.com\r\n", "mailto:alice@example.com,mallory@example.org\r\n", 1)
	refused(ingestMail(t, h, a, inviteMail(badOrg, inviteeAddr, "Bad organiser", "bad-org@example.com")),
		"accepted", errCannotReply, "does not name a valid organiser")
	refused(ingestMail(t, h, a, inviteMail(icsSample(t, "outlook_cancel.ics"), inviteeAddr, "Cancelled", "cx2@example.com")),
		"accepted", errCannotReply, "Only an invitation can be answered")
	refused(ingestMail(t, h, a, inviteMail(icsSample(t, "google_request.ics"), "someone-else@example.org", "Not for me", "nfm@example.com")),
		"accepted", errCannotReply, "not delivered to an address you can send from")
	plain := ingestMail(t, h, a, []byte("From: x@example.com\r\nTo: "+inviteeAddr+
		"\r\nSubject: hi\r\nMessage-ID: <plain@example.com>\r\n\r\nno calendar\r\n"))
	refused(plain, "accepted", errCannotReply, "no invitation")
	ok := ingestMail(t, h, a, inviteMail(icsSample(t, "google_request.ics"), inviteeAddr, "Fine", "fine@example.com"))
	refused(ok, "needs-action", errInvalidArguments, "partstat must be")

	// Every refusal wrote NOTHING: no draft, no submission, no answer.
	for _, q := range []string{
		`SELECT count(*) FROM app.submissions WHERE user_id=$1`,
		`SELECT count(*) FROM app.calendar_responses WHERE user_id=$1`,
		`SELECT count(*) FROM app.messages WHERE user_id=$1 AND is_draft`,
	} {
		if n := calRows(t, h, q, a.UserID.String()); n != 0 {
			t.Fatalf("%s = %d after only refusals", q, n)
		}
	}
}

// TestSpoofedCancellationsAndUpdatesChangeNoOtherCard is the #725 security
// review's case: a UID is in every copy of an invitation, so a message that
// merely NAMES the UID and the organiser must not cancel or supersede the
// genuine card. Only one authenticated as the organiser (From = ORGANIZER and
// a trusted, aligned DMARC pass) may.
func TestSpoofedCancellationsAndUpdatesChangeNoOtherCard(t *testing.T) {
	h := newCalendarHarness(t)
	a, _ := seedTwo(t, h.super)
	seedSendable(t, h.super, a, inviteeAddr, "")
	rows := func(id ulid.ULID) int {
		return calRows(t, h, `SELECT count(*) FROM app.change_log WHERE object_type='Email' AND object_id=$1`, id.String())
	}

	v0 := ingestMail(t, h, a, inviteMail(icsSample(t, "google_request.ics"), inviteeAddr, "Invitation", "g-v0@example.com"))
	if c := inviteObj(t, h, a, v0); c["verified"] != true {
		t.Fatalf("CONTROL: the genuine, authenticated invitation reads as unverified: %#v", c)
	}
	cancel := strings.Replace(icsSample(t, "google_update.ics"), "METHOD:REQUEST", "METHOD:CANCEL", 1)
	cancel = strings.Replace(cancel, "SEQUENCE:1", "SEQUENCE:3", 1)
	update := strings.Replace(icsSample(t, "google_update.ics"), "SEQUENCE:1", "SEQUENCE:2", 1)

	before := rows(v0)
	spoofs := []ulid.ULID{
		// From the organiser's address, with no authentication at all.
		ingestMail(t, h, a, inviteMailFrom(cancel, "Alice Example <alice@example.com>", "", inviteeAddr, "Cancelled", "s1@example.org")),
		// From the organiser's address, failing DMARC.
		ingestMail(t, h, a, inviteMailFrom(cancel, "Alice Example <alice@example.com>", "dmarc=fail header.from=example.com", inviteeAddr, "Cancelled", "s2@example.org")),
		// Authenticated perfectly, as SOMEBODY ELSE.
		ingestMail(t, h, a, inviteMailFrom(cancel, "Mallory <mallory@example.org>", "dmarc=pass header.from=example.org", inviteeAddr, "Cancelled", "s3@example.org")),
		// A pass claimed by an authserv-id nobody trusts.
		ingestMail(t, h, a, []byte(strings.Replace(string(inviteMailFrom(cancel, "Alice Example <alice@example.com>", "", inviteeAddr, "Cancelled", "s4@example.org")),
			"From:", "Authentication-Results: evil.example; dmarc=pass header.from=example.com\r\nFrom:", 1))),
		// A colleague at the organiser's domain, authenticated, naming the
		// organiser in Sender: DMARC vouches for the From domain, not Sender.
		ingestMail(t, h, a, []byte(strings.Replace(string(inviteMailFrom(cancel, "Eve <eve@example.com>", "dmarc=pass header.from=example.com", inviteeAddr, "Cancelled", "s6@example.com")),
			"From:", "Sender: alice@example.com\r\nFrom:", 1))),
		// Two From fields, the organiser's first, authenticated.
		ingestMail(t, h, a, []byte(strings.Replace(string(inviteMail(cancel, inviteeAddr, "Cancelled", "s7@example.com")),
			"\r\nTo:", "\r\nFrom: Mallory <mallory@example.org>\r\nTo:", 1))),
		// An unverified UPDATE.
		ingestMail(t, h, a, inviteMailFrom(update, "Alice Example <alice@example.com>", "", inviteeAddr, "Moved", "s5@example.org")),
	}
	c := inviteObj(t, h, a, v0)
	if c["cancelled"] != false || c["newerEmailId"] != nil || c["canReply"] != true {
		t.Fatalf("a spoofed CANCEL or update changed the genuine card: %#v", c)
	}
	if d := rows(v0) - before; d != 0 {
		t.Fatalf("spoofs appended %d change rows to the genuine invitation", d)
	}
	for i, id := range spoofs {
		sc := inviteObj(t, h, a, id)
		if sc["verified"] != false {
			t.Fatalf("spoof %d reads as verified: %#v", i+1, sc)
		}
	}
	// Answering the genuine invitation is still possible: no spoof made it
	// "cancelled" for the reply path either.
	call(t, h.api.emailReplyToInvite, a.UserID, map[string]any{"emailId": v0.String(), "partstat": "tentative"})

	// The organiser's AUTHENTICATED cancellation does cancel it.
	before = rows(v0)
	real := ingestMail(t, h, a, inviteMail(cancel, inviteeAddr, "Cancelled: Weekly sync", "real-cx@example.com"))
	c = inviteObj(t, h, a, v0)
	if c["cancelled"] != true || c["cancelledByEmailId"] != real.String() {
		t.Fatalf("a verified CANCEL did not cancel the card: %#v", c)
	}
	if d := rows(v0) - before; d != 1 {
		t.Fatalf("a verified CANCEL appended %d change rows to the original, want 1", d)
	}
}

// TestReplyCardIsVerifiedOnlyFromTheAttendee: an attendee's REPLY is
// authenticated only when it comes From that attendee, with a trusted pass.
func TestReplyCardIsVerifiedOnlyFromTheAttendee(t *testing.T) {
	h := newCalendarHarness(t)
	a, _ := seedTwo(t, h.super)
	seedSendable(t, h.super, a, inviteeAddr, "")
	reply := icsSample(t, "google_reply.ics") // ATTENDEE chen@example.org
	good := ingestMail(t, h, a, inviteMailFrom(reply, "Chen Wei <chen@example.org>", "dmarc=pass header.from=example.org", inviteeAddr, "Declined", "r-good@example.org"))
	bad := ingestMail(t, h, a, inviteMailFrom(reply, "Alice Example <alice@example.com>", "dmarc=pass header.from=example.com", inviteeAddr, "Declined", "r-bad@example.com"))
	if c := inviteObj(t, h, a, good); c["verified"] != true {
		t.Fatalf("the attendee's own authenticated reply reads as unverified: %#v", c)
	}
	if c := inviteObj(t, h, a, bad); c["verified"] != false {
		t.Fatalf("a reply From someone else reads as verified: %#v", c)
	}
}
