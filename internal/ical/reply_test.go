// SPDX-License-Identifier: AGPL-3.0-only

package ical

import (
	"errors"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

var replyNow = time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)

// TestReplyWireFormat pins the exact bytes of an iTIP REPLY (RFC 5546 §3.2.3):
// what an organiser's calendar server actually receives.
func TestReplyWireFormat(t *testing.T) {
	inv := mustInvite(t, sample(t, "google_request.ics"))
	raw, err := BuildReply(inv, Address{Name: "Bob", Email: "bob@example.net"}, PartStatAccepted, replyNow)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		"BEGIN:VCALENDAR",
		"PRODID:-//Mailroom//Mailroom Calendar Reply//EN",
		"VERSION:2.0",
		"METHOD:REPLY",
		"BEGIN:VEVENT",
		"UID:c4n8x2w6j1d9s3f7h5a0g2k8m4@google.com",
		"SEQUENCE:0",
		"DTSTAMP:20261017T120000Z",
		`ORGANIZER;CN="Alice Example":mailto:alice@example.com`,
		`ATTENDEE;CN="Bob";PARTSTAT=ACCEPTED:mailto:bob@example.net`,
		"SUMMARY:Weekly sync",
		"END:VEVENT",
		"END:VCALENDAR",
		"",
	}, "\r\n")
	if string(raw) != want {
		t.Fatalf("reply bytes:\n%q\nwant:\n%q", raw, want)
	}

	// It reads back as what it claims to be.
	back := mustInvite(t, raw)
	if back.Method != MethodReply || back.UID != inv.UID || back.Sequence != inv.Sequence ||
		len(back.Attendees) != 1 || back.Attendees[0].PartStat != PartStatAccepted ||
		back.Organizer == nil || back.Organizer.Email != "alice@example.com" {
		t.Fatalf("reply re-reads as %+v", back)
	}
}

// TestReplyEscapesTextExactly pins RFC 5545 §3.3.11 TEXT escaping of echoed
// values, and that a control character reaching BuildReply by a path that did
// NOT go through ExtractInvite (a caller building an Invite by hand) still
// cannot start a content line of its own.
func TestReplyEscapesTextExactly(t *testing.T) {
	if got := escapeText("Lunch, 1;2\\3\nnext"); got != `Lunch\, 1\;2\\3\nnext` {
		t.Fatalf("escapeText = %q", got)
	}
	inv := mustInvite(t, sample(t, "google_request.ics"))
	inv.Summary = "Hi\r\nX-EVIL:1\rMETHOD:CANCEL\x00"
	inv.Organizer = &Address{Name: "O\r\nX-EVIL:2", Email: "alice@example.com"}
	raw, err := BuildReply(inv, Address{Name: "Me\nX-EVIL:3", Email: "bob@example.net"}, PartStatAccepted, replyNow)
	if err != nil {
		t.Fatal(err)
	}
	assertOnlyReplyProperties(t, raw)
	if strings.Contains(string(raw), "\x00") {
		t.Fatal("NUL in reply")
	}
}

func TestReplyEchoesSequenceAndEveryAnswer(t *testing.T) {
	inv := mustInvite(t, sample(t, "google_update.ics"))
	for _, ps := range []string{PartStatAccepted, PartStatTentative, PartStatDeclined} {
		raw, err := BuildReply(inv, Address{Email: "bob@example.net"}, ps, replyNow)
		if err != nil {
			t.Fatal(err)
		}
		back := mustInvite(t, raw)
		if back.Sequence != 1 || back.Attendees[0].PartStat != ps {
			t.Fatalf("%s: re-read %+v", ps, back)
		}
		assertOnlyReplyProperties(t, raw)
	}
}

func TestReplyFoldsLongLinesWithoutSplittingUTF8(t *testing.T) {
	inv := mustInvite(t, sample(t, "outlook_request.ics"))
	inv.Summary = strings.Repeat("Réunion trimestrielle — ", 12)
	raw, err := BuildReply(inv, Address{Name: "Dave Smith", Email: "dave@example.net"}, PartStatDeclined, replyNow)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range strings.Split(strings.TrimSuffix(string(raw), "\r\n"), "\r\n") {
		if len(l) > foldOctets {
			t.Fatalf("line of %d octets: %q", len(l), l)
		}
		if !utf8.ValidString(l) {
			t.Fatalf("fold split a UTF-8 sequence: %q", l)
		}
	}
	if strings.Contains(strings.ReplaceAll(string(raw), "\r\n", ""), "\n") {
		t.Fatal("bare LF in reply")
	}
	back := mustInvite(t, raw)
	if back.UID != inv.UID {
		t.Fatalf("folded UID re-reads as %q", back.UID)
	}
	if !strings.HasPrefix(back.Summary, "Réunion trimestrielle — Réunion") {
		t.Fatalf("folded summary re-reads as %q", back.Summary)
	}
}

func TestReplyToOneInstanceEchoesRecurrenceID(t *testing.T) {
	cal := "BEGIN:VCALENDAR\r\nMETHOD:REQUEST\r\nBEGIN:VEVENT\r\nUID:series@example.com\r\nSEQUENCE:2\r\n" +
		"RECURRENCE-ID;TZID=Europe/Berlin:20261110T090000\r\nDTSTART;TZID=Europe/Berlin:20261110T100000\r\n" +
		"ORGANIZER:mailto:org@example.com\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	inv := mustInvite(t, []byte(cal))
	if inv.RecurrenceID == nil {
		t.Fatal("recurrence id not read")
	}
	raw, err := BuildReply(inv, Address{Email: "me@example.net"}, PartStatTentative, replyNow)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "\r\nRECURRENCE-ID;TZID=\"Europe/Berlin\":20261110T090000\r\n") {
		t.Fatalf("RECURRENCE-ID not echoed:\n%s", raw)
	}

	// A RECURRENCE-ID whose TZID carries anything a zone name does not is NOT
	// echoed: the reply goes out about the series rather than carry it.
	bad := strings.Replace(cal, "RECURRENCE-ID;TZID=Europe/Berlin", "RECURRENCE-ID;TZID=Europe/Berlin<x>", 1)
	inv = mustInvite(t, []byte(bad))
	if inv.RecurrenceID == nil {
		t.Fatal("the instance must still be read; only its echo is refused")
	}
	raw, err = BuildReply(inv, Address{Email: "me@example.net"}, PartStatTentative, replyNow)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "RECURRENCE-ID") {
		t.Fatalf("unsafe RECURRENCE-ID echoed:\n%s", raw)
	}
}

func TestReplyRefusals(t *testing.T) {
	req := mustInvite(t, sample(t, "google_request.ics"))
	me := Address{Email: "bob@example.net"}

	cancel := mustInvite(t, sample(t, "outlook_cancel.ics"))
	if _, err := BuildReply(cancel, me, PartStatAccepted, replyNow); !errors.Is(err, ErrNotAnInvitation) {
		t.Errorf("CANCEL: %v", err)
	}
	reply := mustInvite(t, sample(t, "google_reply.ics"))
	if _, err := BuildReply(reply, me, PartStatAccepted, replyNow); !errors.Is(err, ErrNotAnInvitation) {
		t.Errorf("REPLY: %v", err)
	}
	noOrg := *req
	noOrg.Organizer = nil
	if _, err := BuildReply(&noOrg, me, PartStatAccepted, replyNow); !errors.Is(err, ErrNoOrganizer) {
		t.Errorf("no organiser: %v", err)
	}
	badOrg := *req
	badOrg.Organizer = &Address{Email: "not an address"}
	if _, err := BuildReply(&badOrg, me, PartStatAccepted, replyNow); !errors.Is(err, ErrNoOrganizer) {
		t.Errorf("bad organiser: %v", err)
	}
	for _, ps := range []string{"NEEDS-ACTION", "DELEGATED", "accepted", ""} {
		if _, err := BuildReply(req, me, ps, replyNow); !errors.Is(err, ErrBadPartStat) {
			t.Errorf("partstat %q: %v", ps, err)
		}
	}
	if _, err := BuildReply(req, Address{Email: "x\r\n@example.net"}, PartStatAccepted, replyNow); !errors.Is(err, ErrBadAttendee) {
		t.Errorf("bad attendee: %v", err)
	}
}

// TestPackageNeverFetches is the structural half of "never fetch a URL from the
// .ics": no file in this package may import a network package. A calendar
// carries URL, ATTACH, TZURL and ALTREP — every one an address a stranger
// chose — and the only way to be sure none is ever followed is for the package
// to be unable to.
func TestPackageNeverFetches(t *testing.T) {
	forbidden := map[string]bool{"net": true, "net/http": true, "net/url": false, "os/exec": true}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		pf, err := parser.ParseFile(token.NewFileSet(), f, src, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range pf.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			if forbidden[path] || strings.HasPrefix(path, "net/http") {
				t.Errorf("%s imports %s", f, path)
			}
		}
		checked++
	}
	if checked < 5 {
		t.Fatalf("checked only %d files: the glob is not seeing the package", checked)
	}
}
