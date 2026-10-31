// SPDX-License-Identifier: AGPL-3.0-only

package ical

import (
	"strings"
	"testing"

	"mailroom/internal/mime"
)

var trusted = []string{"mx.example.test"}

func headersOnly(t *testing.T, lines ...string) *mime.Message {
	t.Helper()
	m, _ := mime.ParseBytes([]byte(strings.Join(lines, "\r\n") + "\r\n\r\nbody\r\n"))
	if m == nil {
		t.Fatal("did not parse")
	}
	return m
}

func TestVerifiedRequiresTheOrganiserAndATrustedAlignedPass(t *testing.T) {
	inv := mustInvite(t, sample(t, "outlook_cancel.ics")) // ORGANIZER carol@example.org
	pass := "Authentication-Results: mx.example.test; dmarc=pass header.from=example.org"
	cases := []struct {
		name    string
		msg     []string
		trusted []string
		want    bool
	}{
		{"organiser, trusted aligned pass", []string{pass, "From: Carol <carol@example.org>"}, trusted, true},
		{"organiser address in another case", []string{pass, "From: CAROL@Example.org"}, trusted, true},
		// A Sender is never honoured (RFC 6047 §3 delegation): DMARC vouches
		// for the From domain, not the Sender mailbox.
		{"same-domain Sender claiming to be the organiser", []string{pass, "From: Ops <ops@example.org>", "Sender: carol@example.org"}, trusted, false},
		{"organiser in From with a delegate Sender still verifies", []string{pass, "From: carol@example.org", "Sender: assistant@example.org"}, trusted, true},
		{"two From fields, the organiser's first", []string{pass, "From: carol@example.org", "From: mallory@example.org"}, trusted, false},
		{"two From fields, one empty", []string{pass, "From:", "From: carol@example.org"}, trusted, false},
		{"no Authentication-Results", []string{"From: carol@example.org"}, trusted, false},
		{"DMARC fail", []string{"Authentication-Results: mx.example.test; dmarc=fail header.from=example.org", "From: carol@example.org"}, trusted, false},
		{"pass from an untrusted authserv-id", []string{"Authentication-Results: evil.example; dmarc=pass header.from=example.org", "From: carol@example.org"}, trusted, false},
		{"pass for another domain", []string{"Authentication-Results: mx.example.test; dmarc=pass header.from=example.net", "From: carol@example.org"}, trusted, false},
		{"someone else, authenticated", []string{"Authentication-Results: mx.example.test; dmarc=pass header.from=example.net", "From: mallory@example.net"}, trusted, false},
		{"two From mailboxes", []string{pass, "From: carol@example.org, x@example.org"}, trusted, false},
		{"no trusted authserv-id configured: fail closed", []string{pass, "From: carol@example.org"}, nil, false},
	}
	for _, c := range cases {
		if got := Verified(inv, headersOnly(t, c.msg...), c.trusted); got != c.want {
			t.Errorf("%s: Verified = %v, want %v", c.name, got, c.want)
		}
	}
}

// #846 relaxed DMARC alignment is for learned placement only: "Verified" stays
// strict, so an organiser on a subdomain is not verified by a pass for its
// organisational domain.
//
// NEGATIVE CONTROL: Verified switched to ml.DMARCRelaxed turns the first case
// red; the second is the positive control that the fixture verifies at all.
func TestVerifiedStaysStrictForASubdomainOrganiser(t *testing.T) {
	data := strings.ReplaceAll(string(sample(t, "outlook_cancel.ics")), "carol@example.org", "carol@news.example.org")
	inv := mustInvite(t, []byte(data))
	from := "From: Carol <carol@news.example.org>"
	if Verified(inv, headersOnly(t, "Authentication-Results: mx.example.test; dmarc=pass header.from=example.org", from), trusted) {
		t.Error("an organisational-domain pass verified a subdomain organiser; Verified must stay strict")
	}
	if !Verified(inv, headersOnly(t, "Authentication-Results: mx.example.test; dmarc=pass header.from=news.example.org", from), trusted) {
		t.Error("positive control: an exact pass for the subdomain organiser must verify")
	}
}

func TestVerifiedReplyRequiresTheAttendee(t *testing.T) {
	inv := mustInvite(t, sample(t, "google_reply.ics")) // ATTENDEE chen@example.org
	ok := headersOnly(t, "Authentication-Results: mx.example.test; dmarc=pass header.from=example.org", "From: chen@example.org")
	if !Verified(inv, ok, trusted) {
		t.Fatal("the attendee's own authenticated reply is not verified")
	}
	other := headersOnly(t, "Authentication-Results: mx.example.test; dmarc=pass header.from=example.org", "From: someone@example.org")
	if Verified(inv, other, trusted) {
		t.Fatal("a reply From someone other than its attendee is verified")
	}
}

// #766: a raw date-bounded entry that reaches Verified WITHOUT ml.TrustedAt
// must verify nothing, even for a header bearing its bare id. Control: the
// resolved id verifies the same message.
func TestVerifiedIgnoresAnUnresolvedBoundedEntry(t *testing.T) {
	inv := mustInvite(t, sample(t, "outlook_cancel.ics"))
	msg := headersOnly(t, "Authentication-Results: old.example.test; dmarc=pass header.from=example.org",
		"From: Carol <carol@example.org>")
	if Verified(inv, msg, []string{"old.example.test<2026-07-01"}) {
		t.Error("a raw bounded entry verified a message")
	}
	if !Verified(inv, msg, []string{"old.example.test"}) {
		t.Fatal("CONTROL FAILED: the plain id does not verify the same message")
	}
}
