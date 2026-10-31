// SPDX-License-Identifier: AGPL-3.0-only

package outofoffice

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"mailroom/internal/sieve"
)

func baseSettings() ScriptSettings {
	return ScriptSettings{
		Subject:     "Away",
		Body:        "I am away.\nBack on Monday.",
		Days:        7,
		FromAddress: "me@example.com",
		Addresses:   []string{"me@example.com", "other@example.com"},
	}
}

func mustBuild(t *testing.T, s ScriptSettings, max int) Built {
	t.Helper()
	b, err := Build(s, max)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return b
}

// TestScriptIsWellFormedSieve reads the generated script back with
// internal/sieve's own lexer: it parses completely, requires exactly what it
// uses, and holds exactly one vacation action.
func TestScriptIsWellFormedSieve(t *testing.T) {
	from := time.Date(2026, 10, 20, 7, 0, 0, 0, time.UTC)
	to := from.Add(7 * 24 * time.Hour)
	s := baseSettings()
	s.From, s.To = &from, &to
	b := mustBuild(t, s, 0)
	sum := sieve.Summarize(b.Script)
	if !sum.Complete {
		t.Fatalf("the script does not lex: %s at %d\n%s", sum.Problem, sum.StoppedAt, b.Script)
	}
	if !reflect.DeepEqual(sum.Require, []string{"date", "relational", "vacation"}) {
		t.Errorf("require = %v", sum.Require)
	}
	if sum.Commands["vacation"] != 1 || sum.Commands["if"] != 1 || !sum.Vacation {
		t.Errorf("commands = %v", sum.Commands)
	}
	if !reflect.DeepEqual(sum.VacationTags, []string{":addresses", ":days", ":from", ":subject"}) {
		t.Errorf("vacation tags = %v", sum.VacationTags)
	}
	for _, want := range []string{
		`currentdate :zone "+0000" :value "ge" "iso8601" "2026-10-20T07:00:00Z"`,
		`currentdate :zone "+0000" :value "lt" "iso8601" "2026-10-27T07:00:00Z"`,
		`not header :is ["X-Spam", "X-Spam-Flag"] "yes"`,
		`:from "me@example.com" :addresses ["me@example.com", "other@example.com"]`,
		"# Managed by Mailroom",
	} {
		if !strings.Contains(b.Script, want) {
			t.Errorf("script lacks %q\n%s", want, b.Script)
		}
	}
	if !strings.Contains(b.Script, "\r\n") || strings.Contains(strings.ReplaceAll(b.Script, "\r\n", ""), "\n") {
		t.Error("the script must use CRLF line endings throughout")
	}
}

// TestNoWindowNoDateExtension: an open-ended reply needs neither date nor
// relational, so it does not ask the server for them.
func TestNoWindowNoDateExtension(t *testing.T) {
	sum := sieve.Summarize(mustBuild(t, baseSettings(), 0).Script)
	if !reflect.DeepEqual(sum.Require, []string{"vacation"}) || sum.Tests["currentdate"] != 0 {
		t.Fatalf("require %v tests %v", sum.Require, sum.Tests)
	}
}

// TestUserTextCannotEscapeItsString: quotes, backslashes, line breaks in the
// subject and a lone "." line in the body stay data. If any escaped, the
// lexer would see extra commands or stop early.
func TestUserTextCannotEscapeItsString(t *testing.T) {
	s := baseSettings()
	s.Subject = `Out "of" office \ ; } discard; "`
	s.Body = "line one\n.\n..two dots\n; discard;\n.\r\nend"
	b := mustBuild(t, s, 0)
	sum := sieve.Summarize(b.Script)
	if !sum.Complete || sum.Commands["discard"] != 0 || sum.Commands["vacation"] != 1 {
		t.Fatalf("user text changed the script's structure: %+v\n%s", sum, b.Script)
	}
	if !strings.Contains(b.Script, `:subject "Out \"of\" office \\ ; } discard; \""`) {
		t.Errorf("subject not escaped as expected:\n%s", b.Script)
	}
	// Dot-stuffing: a body line "." becomes "..", "..two" becomes "...two".
	if !strings.Contains(b.Script, "\r\n..\r\n...two dots\r\n") {
		t.Errorf("body not dot-stuffed:\n%q", b.Script)
	}
	// Line breaks in the subject are flattened, never carried.
	s.Subject = "a\r\nb\x00c"
	if !strings.Contains(mustBuild(t, s, 0).Script, `:subject "a  bc"`) {
		t.Error("subject control characters were not removed")
	}
}

func TestBuildRejectsWhatCannotBeAScript(t *testing.T) {
	from := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	cases := map[string]func(*ScriptSettings){
		"days 0":            func(s *ScriptSettings) { s.Days = 0 },
		"days 31":           func(s *ScriptSettings) { s.Days = 31 },
		"empty body":        func(s *ScriptSettings) { s.Body = " \n " },
		"end before start":  func(s *ScriptSettings) { s.From, s.To = &from, &from },
		"quote in from":     func(s *ScriptSettings) { s.FromAddress = `me"@example.com` },
		"no at in address":  func(s *ScriptSettings) { s.Addresses = []string{"me"} },
		"no own addresses":  func(s *ScriptSettings) { s.Addresses = nil },
		"newline in a from": func(s *ScriptSettings) { s.FromAddress = "me@example.com\r\n" },
	}
	for name, mut := range cases {
		s := baseSettings()
		mut(&s)
		if _, err := Build(s, 0); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
}

func contacts(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("person%04d@example.net", i)
	}
	return out
}

// TestContactsOnlyFitsTheServerLimit: the list is cut, most recent first,
// until the script fits; the cut is REPORTED (included < total), never hidden.
func TestContactsOnlyFitsTheServerLimit(t *testing.T) {
	s := baseSettings()
	s.ContactsOnly = true
	s.Contacts = contacts(MaxContacts + 500)

	unlimited := mustBuild(t, s, 0)
	if unlimited.ContactsIncluded != MaxContacts || unlimited.ContactsTotal != MaxContacts+500 {
		t.Fatalf("without a server limit: included %d of %d, want %d of %d",
			unlimited.ContactsIncluded, unlimited.ContactsTotal, MaxContacts, MaxContacts+500)
	}

	const migadu = 49152 // the script-size limit measured against the provider
	b := mustBuild(t, s, migadu)
	if len(b.Script) > migadu {
		t.Fatalf("script is %d bytes, over the %d limit", len(b.Script), migadu)
	}
	if b.ContactsIncluded == 0 || b.ContactsIncluded >= MaxContacts {
		t.Fatalf("included %d: the limit should have cut the list", b.ContactsIncluded)
	}
	// Maximal: one more contact would not fit.
	s2 := s
	s2.Contacts = s.Contacts[:b.ContactsIncluded+1]
	if over, _ := Build(s2, 0); len(over.Script) <= migadu {
		t.Fatalf("%d contacts would still have fitted (%d bytes)", b.ContactsIncluded+1, len(over.Script))
	}
	// The MOST RECENT are kept: the list is given newest first.
	if !strings.Contains(b.Script, `"person0000@example.net"`) ||
		strings.Contains(b.Script, fmt.Sprintf(`"person%04d@example.net"`, b.ContactsIncluded)) {
		t.Fatal("the cut kept the wrong end of the list")
	}
	sum := sieve.Summarize(b.Script)
	if !sum.Complete || sum.Tests["address"] != 1 {
		t.Fatalf("contacts-only script: %+v", sum)
	}
}

func TestTooLargeAndNoContacts(t *testing.T) {
	s := baseSettings()
	s.Body = strings.Repeat("x", 2000)
	if _, err := Build(s, 1000); !errors.Is(err, ErrTooLarge) {
		t.Errorf("long body: err = %v, want ErrTooLarge", err)
	}
	s.ContactsOnly, s.Contacts = true, contacts(3)
	if _, err := Build(s, 1000); !errors.Is(err, ErrTooLarge) {
		t.Errorf("long body, contacts: err = %v, want ErrTooLarge", err)
	}
	s = baseSettings()
	s.ContactsOnly = true
	if _, err := Build(s, 0); !errors.Is(err, ErrNoContacts) {
		t.Errorf("no contacts: err = %v, want ErrNoContacts", err)
	}
}
