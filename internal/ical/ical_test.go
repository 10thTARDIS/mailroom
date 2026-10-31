// SPDX-License-Identifier: AGPL-3.0-only

package ical

import (
	"errors"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func sample(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func mustInvite(t *testing.T, data []byte) *Invite {
	t.Helper()
	inv, err := ExtractInvite(data)
	if err != nil {
		t.Fatalf("ExtractInvite: %v", err)
	}
	return inv
}

func utc(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func wantUTC(t *testing.T, label string, got *Time, want string) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s: nil, want %s", label, want)
	}
	if got.Kind != TimeUTC || !got.UTC.Equal(utc(want)) {
		t.Fatalf("%s = %+v, want UTC %s", label, *got, want)
	}
}

// ---- real-world shapes ---------------------------------------------------

func TestGoogleRequest(t *testing.T) {
	inv := mustInvite(t, sample(t, "google_request.ics"))
	if inv.Method != MethodRequest || inv.Sequence != 0 || inv.Status != "CONFIRMED" {
		t.Fatalf("method/seq/status = %q/%d/%q", inv.Method, inv.Sequence, inv.Status)
	}
	if inv.UID != "c4n8x2w6j1d9s3f7h5a0g2k8m4@google.com" {
		t.Fatalf("UID = %q", inv.UID)
	}
	if inv.Summary != "Weekly sync" || inv.Location != "Room 2, Building B" {
		t.Fatalf("summary/location = %q / %q", inv.Summary, inv.Location)
	}
	wantUTC(t, "start", inv.Start, "2026-10-20T15:00:00Z")
	wantUTC(t, "end", inv.End, "2026-10-20T15:30:00Z")
	if inv.Organizer == nil || inv.Organizer.Email != "alice@example.com" || inv.Organizer.Name != "Alice Example" {
		t.Fatalf("organizer = %+v", inv.Organizer)
	}
	if len(inv.Attendees) != 3 {
		t.Fatalf("attendees = %+v", inv.Attendees)
	}
	got := map[string]string{}
	for _, a := range inv.Attendees {
		got[a.Email] = a.PartStat
	}
	if got["alice@example.com"] != PartStatAccepted || got["bob@example.net"] != PartStatNeedsAction ||
		got["chen@example.org"] != PartStatTentative {
		t.Fatalf("partstats = %v", got)
	}
	if inv.Recurrence != nil || inv.RecurrenceID != nil {
		t.Fatalf("not recurring: %+v %+v", inv.Recurrence, inv.RecurrenceID)
	}
}

func TestGoogleUpdateCarriesAHigherSequence(t *testing.T) {
	orig := mustInvite(t, sample(t, "google_request.ics"))
	upd := mustInvite(t, sample(t, "google_update.ics"))
	if upd.UID != orig.UID || upd.Sequence != 1 {
		t.Fatalf("update uid/seq = %q/%d", upd.UID, upd.Sequence)
	}
	wantUTC(t, "moved start", upd.Start, "2026-10-20T16:00:00Z")
}

func TestGoogleRecurringIsSummarisedNotExpanded(t *testing.T) {
	inv := mustInvite(t, sample(t, "google_recurring.ics"))
	// 2 Nov 2026 is the day after US DST ends (1 Nov), so 10:00 New York is EST.
	wantUTC(t, "start", inv.Start, "2026-11-02T15:00:00Z")
	if inv.Start.TZID != "America/New_York" {
		t.Fatalf("tzid = %q", inv.Start.TZID)
	}
	r := inv.Recurrence
	if r == nil || r.Freq != "WEEKLY" || r.Interval != 1 || strings.Join(r.ByDay, ",") != "MO,WE" {
		t.Fatalf("recurrence = %+v", r)
	}
	if r.Text != "Repeats weekly on Monday, Wednesday" {
		t.Fatalf("text = %q", r.Text)
	}
}

func TestGoogleReplyShowsWhoAnswered(t *testing.T) {
	inv := mustInvite(t, sample(t, "google_reply.ics"))
	if inv.Method != MethodReply || len(inv.Attendees) != 1 {
		t.Fatalf("reply = %+v", inv)
	}
	a := inv.Attendees[0]
	if a.Email != "chen@example.org" || a.Name != "Chen Wei" || a.PartStat != PartStatDeclined {
		t.Fatalf("attendee = %+v", a)
	}
}

func TestOutlookWindowsZoneResolvesFromItsVTIMEZONE(t *testing.T) {
	inv := mustInvite(t, sample(t, "outlook_request.ics"))
	// "Pacific Standard Time" is not an IANA name. 21 Oct 2026 is still PDT
	// (DST ends 1 Nov), so 09:00 is 16:00Z — derived from the file's own
	// STANDARD/DAYLIGHT rules.
	wantUTC(t, "start", inv.Start, "2026-10-21T16:00:00Z")
	wantUTC(t, "end", inv.End, "2026-10-21T17:00:00Z")
	if inv.Organizer == nil || inv.Organizer.Name != "Carol, Ops" {
		t.Fatalf("organizer = %+v", inv.Organizer)
	}
	if !strings.HasPrefix(inv.UID, "040000008200E00074C5B7101A82E008") || len(inv.UID) != 112 {
		t.Fatalf("UID (unfolded) = %q (%d)", inv.UID, len(inv.UID))
	}
	if inv.Summary != "Quarterly review" || inv.Location != "Room 4" {
		t.Fatalf("summary/location = %q/%q", inv.Summary, inv.Location)
	}
	r := inv.Recurrence
	if r == nil || r.Freq != "WEEKLY" || r.Until == nil || r.Until.UTC.Format("2006-01-02") != "2026-12-16" {
		t.Fatalf("recurrence = %+v", r)
	}
	if r.Text != "Repeats weekly on Wednesday, until 2026-12-16" {
		t.Fatalf("text = %q", r.Text)
	}

	// The same file in January resolves to PST: the observance choice is real,
	// not one fixed offset.
	winter := strings.ReplaceAll(string(sample(t, "outlook_request.ics")), "20261021T", "20270113T")
	w := mustInvite(t, []byte(winter))
	wantUTC(t, "winter start", w.Start, "2027-01-13T17:00:00Z")
}

func TestOutlookCancel(t *testing.T) {
	inv := mustInvite(t, sample(t, "outlook_cancel.ics"))
	req := mustInvite(t, sample(t, "outlook_request.ics"))
	if inv.Method != MethodCancel || inv.Status != "CANCELLED" || inv.Sequence != 1 || inv.UID != req.UID {
		t.Fatalf("cancel = %q %q %d %q", inv.Method, inv.Status, inv.Sequence, inv.UID)
	}
}

func TestAppleRequest(t *testing.T) {
	inv := mustInvite(t, sample(t, "apple_request.ics"))
	// 3 Nov 2026 is CET (+1).
	wantUTC(t, "start", inv.Start, "2026-11-03T13:00:00Z")
	wantUTC(t, "end", inv.End, "2026-11-03T14:00:00Z")
	if inv.Summary != "Coffee ☕" || inv.Location != "Café Central, Hauptstraße 1" {
		t.Fatalf("summary/location = %q/%q", inv.Summary, inv.Location)
	}
	// The urn:uuid resource is not an address and is not shown.
	if len(inv.Attendees) != 2 {
		t.Fatalf("attendees = %+v", inv.Attendees)
	}
	if inv.Attendees[0].Role != "CHAIR" || inv.Attendees[0].PartStat != PartStatAccepted {
		t.Fatalf("chair = %+v", inv.Attendees[0])
	}
}

func TestAllDayEventEndsTheNextDay(t *testing.T) {
	inv := mustInvite(t, sample(t, "allday.ics"))
	if inv.Start.Kind != TimeDate || inv.Start.Date != "2026-12-24" {
		t.Fatalf("start = %+v", *inv.Start)
	}
	if inv.End == nil || inv.End.Kind != TimeDate || inv.End.Date != "2026-12-25" {
		t.Fatalf("end = %+v", inv.End)
	}
}

func TestDurationInsteadOfDTEND(t *testing.T) {
	cal := "BEGIN:VCALENDAR\r\nMETHOD:REQUEST\r\nBEGIN:VEVENT\r\nUID:d@x\r\nDTSTART:20261020T150000Z\r\n" +
		"DURATION:PT1H30M\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	inv := mustInvite(t, []byte(cal))
	wantUTC(t, "end", inv.End, "2026-10-20T16:30:00Z")
}

func TestUnsupportedMethodsAreNotInvitations(t *testing.T) {
	for _, m := range []string{"PUBLISH", "COUNTER", "", "REQUEST2"} {
		cal := "BEGIN:VCALENDAR\r\n"
		if m != "" {
			cal += "METHOD:" + m + "\r\n"
		}
		cal += "BEGIN:VEVENT\r\nUID:x\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
		if _, err := ExtractInvite([]byte(cal)); !errors.Is(err, ErrUnsupportedMethod) {
			t.Errorf("METHOD %q: err = %v", m, err)
		}
	}
	if _, err := ExtractInvite([]byte("BEGIN:VCALENDAR\r\nMETHOD:REQUEST\r\nBEGIN:VTODO\r\nEND:VTODO\r\nEND:VCALENDAR\r\n")); !errors.Is(err, ErrNoEvent) {
		t.Errorf("VTODO: err = %v", err)
	}
	if _, err := ExtractInvite([]byte("hello")); !errors.Is(err, ErrNotACalendar) {
		t.Errorf("not a calendar: err = %v", err)
	}
}

// ---- hostile input -------------------------------------------------------

func TestHostileTooLarge(t *testing.T) {
	big := "BEGIN:VCALENDAR\r\nMETHOD:REQUEST\r\nBEGIN:VEVENT\r\nUID:x\r\nDESCRIPTION:" +
		strings.Repeat("A", MaxBytes) + "\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	if _, err := Parse([]byte(big)); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	// The bound is the bound, not a bit under it: a calendar of exactly
	// MaxBytes is read.
	head := "BEGIN:VCALENDAR\r\nMETHOD:REQUEST\r\nBEGIN:VEVENT\r\nUID:x\r\nDESCRIPTION:"
	tail := "\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	exact := head + strings.Repeat("A", MaxBytes-len(head)-len(tail)) + tail
	if len(exact) != MaxBytes {
		t.Fatalf("fixture is %d bytes", len(exact))
	}
	if _, err := Parse([]byte(exact)); err != nil {
		t.Fatalf("exactly MaxBytes: %v", err)
	}
}

func TestHostileDeeplyNested(t *testing.T) {
	var b strings.Builder
	b.WriteString("BEGIN:VCALENDAR\r\n")
	for i := 0; i < 10000; i++ {
		b.WriteString("BEGIN:X-NEST\r\n")
	}
	if _, err := Parse([]byte(b.String())); !errors.Is(err, ErrTooDeep) {
		t.Fatalf("err = %v, want ErrTooDeep", err)
	}
	// Exactly MaxDepth is fine.
	ok := "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nBEGIN:VALARM\r\nBEGIN:X-ONE\r\nEND:X-ONE\r\nEND:VALARM\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	if _, err := Parse([]byte(ok)); err != nil {
		t.Fatalf("depth %d: %v", MaxDepth, err)
	}
}

func TestHostileTooManyComponentsOrProperties(t *testing.T) {
	var b strings.Builder
	b.WriteString("BEGIN:VCALENDAR\r\nMETHOD:REQUEST\r\n")
	for i := 0; i < MaxComponents+1; i++ {
		b.WriteString("BEGIN:VEVENT\r\nEND:VEVENT\r\n")
	}
	b.WriteString("END:VCALENDAR\r\n")
	if _, err := Parse([]byte(b.String())); !errors.Is(err, ErrTooComplex) {
		t.Fatalf("components: err = %v", err)
	}

	b.Reset()
	b.WriteString("BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\n")
	for i := 0; i < MaxProperties+1; i++ {
		b.WriteString("X-A:1\r\n")
	}
	b.WriteString("END:VEVENT\r\nEND:VCALENDAR\r\n")
	if _, err := Parse([]byte(b.String())); !errors.Is(err, ErrTooComplex) {
		t.Fatalf("properties: err = %v", err)
	}

	// Parameters on one line, and values of one parameter, are bounded too: the
	// line is dropped (not the calendar).
	line := "X-P" + strings.Repeat(";A=1", MaxParams+1) + ":v"
	if _, err := parseContentLine(line); err == nil {
		t.Fatal("too many params accepted")
	}
	line = "X-P;A=" + strings.Repeat("1,", MaxParamValues+1) + "1:v"
	if _, err := parseContentLine(line); err == nil {
		t.Fatal("too many param values accepted")
	}
}

func TestHostileAttendeeListIsBoundedAndSaysSo(t *testing.T) {
	var b strings.Builder
	b.WriteString("BEGIN:VCALENDAR\r\nMETHOD:REQUEST\r\nBEGIN:VEVENT\r\nUID:x\r\n")
	for i := 0; i < MaxAttendees+50; i++ {
		b.WriteString("ATTENDEE:mailto:a" + strconv.Itoa(i) + "@example.com\r\n")
	}
	b.WriteString("END:VEVENT\r\nEND:VCALENDAR\r\n")
	inv := mustInvite(t, []byte(b.String()))
	if len(inv.Attendees) != MaxAttendees || inv.AttendeesOmitted != 50 {
		t.Fatalf("kept %d, omitted %d", len(inv.Attendees), inv.AttendeesOmitted)
	}
}

func TestHostileMalformedTZID(t *testing.T) {
	cases := []struct {
		name, tzid string
		wantTZID   string // what may be echoed to a client
	}{
		{"path traversal", "../../../../etc/passwd", "../../../../etc/passwd"},
		{"absolute path", "/etc/localtime", "/etc/localtime"},
		{"server-local zone", "Local", "Local"},
		{"unknown zone, no VTIMEZONE", "Mars/Olympus_Mons", "Mars/Olympus_Mons"},
		{"too long", strings.Repeat("A", 1000), ""},
		{"empty quoted", `""`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tz := c.tzid
			if !strings.HasPrefix(tz, `"`) {
				tz = `"` + tz + `"`
			}
			cal := "BEGIN:VCALENDAR\r\nMETHOD:REQUEST\r\nBEGIN:VEVENT\r\nUID:x\r\n" +
				"DTSTART;TZID=" + tz + ":20261020T090000\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
			inv := mustInvite(t, []byte(cal))
			if inv.Start == nil {
				t.Fatal("start dropped: the time as written must survive a bad zone")
			}
			if c.wantTZID == "" && c.tzid == `""` {
				// An empty TZID is no TZID: floating.
				if inv.Start.Kind != TimeFloating {
					t.Fatalf("start = %+v", *inv.Start)
				}
				return
			}
			if inv.Start.Kind != TimeFloating || inv.Start.Local != "2026-10-20T09:00:00" {
				t.Fatalf("start = %+v, want floating 09:00", *inv.Start)
			}
			if inv.Start.TZID != c.wantTZID {
				t.Fatalf("echoed TZID = %q, want %q", inv.Start.TZID, c.wantTZID)
			}
		})
	}

	// A VTIMEZONE with garbage observances: never a panic, never a made-up
	// offset — floating.
	cal := "BEGIN:VCALENDAR\r\nMETHOD:REQUEST\r\nBEGIN:VTIMEZONE\r\nTZID:Bad Zone\r\n" +
		"BEGIN:STANDARD\r\nDTSTART:garbage\r\nTZOFFSETTO:+9999\r\nRRULE:FREQ=YEARLY;BYDAY=9XX\r\nEND:STANDARD\r\n" +
		"END:VTIMEZONE\r\nBEGIN:VEVENT\r\nUID:x\r\nDTSTART;TZID=Bad Zone:20261020T090000\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	inv := mustInvite(t, []byte(cal))
	if inv.Start.Kind != TimeFloating || inv.Start.TZID != "Bad Zone" {
		t.Fatalf("garbage VTIMEZONE: start = %+v", *inv.Start)
	}
}

func TestHostileCRLFInjectionInFields(t *testing.T) {
	// Every way a stranger can try to get a line break into a field:
	//   - a TEXT escape (\n) in SUMMARY and LOCATION;
	//   - a lone CR inside a line (not a line ending);
	//   - a folded continuation that tries to start a property;
	//   - an escaped newline inside the UID;
	//   - an ORGANIZER whose mailto carries a header.
	cal := "BEGIN:VCALENDAR\r\nMETHOD:REQUEST\r\nBEGIN:VEVENT\r\n" +
		"UID:good-uid@example.com\r\n" +
		`SUMMARY:Lunch\nATTENDEE;PARTSTAT=ACCEPTED:mailto:evil@example.com\nBcc: x@example.com` + "\r\n" +
		"LOCATION:Room\r1\rMETHOD:CANCEL\r\n" +
		"DESCRIPTION:a\r\n END:VEVENT\r\n" +
		"ORGANIZER;CN=\"Org\":mailto:org@example.com\r\n" +
		"END:VEVENT\r\nEND:VCALENDAR\r\n"
	inv := mustInvite(t, []byte(cal))
	for _, s := range []string{inv.Summary, inv.Location} {
		if strings.ContainsAny(s, "\r\n") {
			t.Fatalf("control character survived: %q", s)
		}
	}
	if inv.Method != MethodRequest {
		t.Fatalf("method = %q: a CR smuggled METHOD:CANCEL in", inv.Method)
	}
	if len(inv.Attendees) != 0 {
		t.Fatalf("an attendee was injected: %+v", inv.Attendees)
	}

	// The reply echoes SUMMARY and UID: re-parse it and require EXACTLY the
	// properties BuildReply writes, one ATTENDEE, and nothing the sender chose
	// as a property of its own.
	raw, err := BuildReply(inv, Address{Name: "Me\r\nX-EVIL:1", Email: "me@example.net"}, PartStatAccepted, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	assertOnlyReplyProperties(t, raw)

	// An escaped newline inside the UID makes the UID unusable — and a reply
	// without a UID is refused rather than sent with a cut one.
	cal2 := strings.Replace(cal, "UID:good-uid@example.com", `UID:a\nEND:VEVENT`, 1)
	inv2 := mustInvite(t, []byte(cal2))
	if inv2.UID != "" {
		t.Fatalf("UID with a newline kept: %q", inv2.UID)
	}
	if _, err := BuildReply(inv2, Address{Email: "me@example.net"}, PartStatAccepted, time.Now()); !errors.Is(err, ErrNoUID) {
		t.Fatalf("reply without UID: err = %v", err)
	}

	// An organiser address with anything but a plain addr-spec is no organiser.
	for _, org := range []string{
		"mailto:org@example.com\\nBcc: x@example.com",
		"mailto:Org <org@example.com>",
		"mailto:a@example.com,b@example.com",
		"http://example.com/org",
		"mailto:",
		// These pass the character prefilter and are refused only by the
		// addr-spec parse — the probe that removes that parse needs them.
		"mailto:noatsign",
		"mailto:a@b@example.com",
		"mailto:@example.com",
		"mailto:a@",
	} {
		c := strings.Replace(cal, "mailto:org@example.com", org, 1)
		c = strings.Replace(c, "ORGANIZER;CN=\"Org\":mailto:org@example.com", "ORGANIZER:"+org, 1)
		i := mustInvite(t, []byte(c))
		if i.Organizer != nil {
			t.Errorf("organiser %q accepted as %+v", org, i.Organizer)
		}
	}
}

// TestParseLeavesNoCarriageReturnInAnyValue: Parse is exported, and a CR that
// survives into a property value is the raw material of an injection for any
// caller that writes the value somewhere line-oriented. Unfolding removes it.
func TestParseLeavesNoCarriageReturnInAnyValue(t *testing.T) {
	cal, err := Parse([]byte("BEGIN:VCALENDAR\r\nX-A;P=\"v\rw\":a\rb\r\r\nBEGIN:VEVENT\r\nX:1\r2\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	var walk func(c *Component)
	n := 0
	walk = func(c *Component) {
		for _, p := range c.Properties {
			n++
			if strings.Contains(p.Value, "\r") {
				t.Fatalf("CR in %s value %q", p.Name, p.Value)
			}
			for _, vs := range p.Params {
				for _, v := range vs {
					if strings.Contains(v, "\r") {
						t.Fatalf("CR in %s param %q", p.Name, v)
					}
				}
			}
		}
		for _, ch := range c.Children {
			walk(ch)
		}
	}
	walk(cal)
	if n == 0 {
		t.Fatal("no properties read: the assertion above checked nothing")
	}
}

// assertOnlyReplyProperties parses a REPLY and requires the exact property set.
func assertOnlyReplyProperties(t *testing.T, raw []byte) {
	t.Helper()
	cal, err := Parse(raw)
	if err != nil {
		t.Fatalf("reply does not parse: %v\n%s", err, raw)
	}
	if len(cal.Children) != 1 || cal.Children[0].Name != "VEVENT" {
		t.Fatalf("reply components = %+v", cal.Children)
	}
	allowed := map[string]bool{"UID": true, "SEQUENCE": true, "DTSTAMP": true, "ORGANIZER": true,
		"ATTENDEE": true, "RECURRENCE-ID": true, "SUMMARY": true}
	attendees := 0
	for _, p := range cal.Children[0].Properties {
		if !allowed[p.Name] {
			t.Fatalf("reply carries property %q:\n%s", p.Name, raw)
		}
		if p.Name == "ATTENDEE" {
			attendees++
		}
	}
	if attendees != 1 {
		t.Fatalf("reply carries %d ATTENDEE lines", attendees)
	}
	for _, p := range cal.Properties {
		if p.Name != "PRODID" && p.Name != "VERSION" && p.Name != "METHOD" {
			t.Fatalf("calendar property %q in reply", p.Name)
		}
	}
}

func TestHostileRRuleIsNeverExpanded(t *testing.T) {
	cal := "BEGIN:VCALENDAR\r\nMETHOD:REQUEST\r\nBEGIN:VEVENT\r\nUID:x\r\nDTSTART:20261020T150000Z\r\n" +
		"RRULE:FREQ=SECONDLY;COUNT=2000000000;BYDAY=MO,TU,WE,TH,FR,SA,SU,MO,TU,WE,TH\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	start := time.Now()
	inv := mustInvite(t, []byte(cal))
	if time.Since(start) > time.Second {
		t.Fatal("an RRULE was expanded")
	}
	r := inv.Recurrence
	if r == nil || r.Count != 2000000000 || len(r.ByDay) != maxByDay {
		t.Fatalf("recurrence = %+v", r)
	}
	// A rule that cannot be read still says "repeats".
	inv = mustInvite(t, []byte(strings.Replace(cal, "RRULE:FREQ=SECONDLY", "RRULE:FREQ;FREQ", 1)))
	if inv.Recurrence == nil || inv.Recurrence.Text != "Repeats" {
		t.Fatalf("unreadable rule = %+v", inv.Recurrence)
	}
}

// TestParseIsTotal feeds every sample truncated at every byte, and randomly
// mutated, and requires no panic. ExtractInvite may fail; it must not crash.
func TestParseIsTotal(t *testing.T) {
	entries, err := os.ReadDir("testdata")
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(725))
	for _, e := range entries {
		data := sample(t, e.Name())
		for i := 0; i <= len(data); i++ {
			_, _ = ExtractInvite(data[:i])
		}
		for n := 0; n < 2000; n++ {
			m := append([]byte(nil), data...)
			for k := 0; k < 1+rng.Intn(8); k++ {
				m[rng.Intn(len(m))] = byte(rng.Intn(256))
			}
			if inv, err := ExtractInvite(m); err == nil {
				_, _ = BuildReply(inv, Address{Email: "me@example.net"}, PartStatAccepted, time.Now())
			}
		}
	}
}
