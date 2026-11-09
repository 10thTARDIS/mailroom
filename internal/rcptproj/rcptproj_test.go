// SPDX-License-Identifier: AGPL-3.0-only

package rcptproj

import (
	"fmt"
	"strings"
	"testing"

	"mailroom/internal/mime"
)

func parse(t *testing.T, headers string) *mime.Message {
	t.Helper()
	m, err := mime.ParseBytes([]byte("From: S <s@example.net>\r\n" + headers + "Subject: x\r\nContent-Type: text/plain\r\n\r\nbody\r\n"))
	if err != nil || m == nil {
		t.Fatalf("parse: %v", err)
	}
	return m
}

func TestProjectColumns(t *testing.T) {
	c := Project(parse(t, "To: \"Dana  Lee\" <Dana@Example.ORG>, erin@example.org\r\n"+
		"Cc: Ed <ed@example.org>\r\nBcc: Secret <secret@example.org>\r\nList-Id: Plans <Plans.Example.NET>\r\n"))
	if c.To != "dana lee\ndana@example.org\nerin@example.org" {
		t.Errorf("To = %q", c.To)
	}
	if c.Cc != "ed\ned@example.org" || c.Bcc != "secret\nsecret@example.org" {
		t.Errorf("Cc = %q Bcc = %q", c.Cc, c.Bcc)
	}
	if c.ToSort != "Dana Lee" {
		t.Errorf("ToSort = %q: the first To's display name, case kept", c.ToSort)
	}
	if c.ListIDKey != "plans.example.net" {
		t.Errorf("ListIDKey = %q", c.ListIDKey)
	}
}

func TestProjectSortKeyFallsBackToAddress(t *testing.T) {
	if c := Project(parse(t, "To: erin@example.org, Dana <dana@example.org>\r\n")); c.ToSort != "erin@example.org" {
		t.Errorf("ToSort = %q, want the first recipient's address when it has no name", c.ToSort)
	}
}

func TestProjectNothingThereIsEmptyNotMissing(t *testing.T) {
	c := Project(parse(t, ""))
	if c.To != "" || c.Cc != "" || c.Bcc != "" || c.ToSort != "" || c.ListIDKey != "" || c.Truncated {
		t.Errorf("a message with no recipients: %+v", c)
	}
	if c := Project(nil); c.To != "" || c.ListIDKey != "" {
		t.Errorf("nil message: %+v", c)
	}
}

func TestProjectBound(t *testing.T) {
	var as []string
	for i := 0; i < MaxRecipients+5; i++ {
		as = append(as, fmt.Sprintf("r%03d@example.org", i))
	}
	c := Project(parse(t, "To: "+strings.Join(as, ", ")+"\r\n"))
	if got := strings.Count(c.To, "\n") + 1; got != MaxRecipients || !c.Truncated {
		t.Errorf("stored %d recipients (truncated=%v), want %d and truncated", got, c.Truncated, MaxRecipients)
	}
	if strings.Contains(c.To, "r204@") || !strings.Contains(c.To, "r199@") {
		t.Error("the bound must keep the FIRST recipients")
	}
	long := strings.Repeat("n", 500)
	c = Project(parse(t, "To: "+long+" <a@example.org>\r\n"))
	if n := len([]rune(c.ToSort)); n != maxNameRunes {
		t.Errorf("a %d-rune name was stored at %d runes, want the %d bound", 500, n, maxNameRunes)
	}
}

func TestListIDKey(t *testing.T) {
	for in, want := range map[string]string{
		"<plans.example.net>":               "plans.example.net",
		"Plans list <Plans.Example.Net>":    "plans.example.net",
		"  bare.example.net  ":              "bare.example.net",
		"\"Odd <name>\" <real.example.org>": "real.example.org",
		"":                                  "",
		"   ":                               "",
		"<>":                                "<>",
		"nul\x00in<a.example>\x00":          "a.example",
	} {
		if got := ListIDKey(in); got != want {
			t.Errorf("ListIDKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStoredTextHasNoNulOrInvalidUTF8(t *testing.T) {
	c := Project(parse(t, "To: \"bad\xff\xfename\" <a@example.org>\r\n"))
	if strings.ContainsRune(c.To, 0) || strings.ContainsRune(c.ToSort, 0) {
		t.Error("NUL in stored text")
	}
	for _, v := range []string{c.To, c.ToSort} {
		if v != strings.ToValidUTF8(v, "") {
			t.Errorf("invalid UTF-8 stored: %q", v)
		}
	}
}

// A whole column is bounded (security review of beta 6), not only its item count.
func TestColumnIsBoundedInBytes(t *testing.T) {
	var as []mime.Address
	for i := 0; i < MaxRecipients; i++ {
		as = append(as, mime.Address{Name: strings.Repeat("n", 128), Address: strings.Repeat("a", 240) + "@example.com"})
	}
	got, truncated := text(as)
	if len(got) > MaxColumnBytes || !truncated {
		t.Fatalf("column is %d bytes (truncated %v), want <= %d and truncated", len(got), truncated, MaxColumnBytes)
	}
}

// clip cuts at a rune boundary without materialising a huge string as runes.
func TestClipIsRuneSafe(t *testing.T) {
	if got := clip("äöü"+strings.Repeat("x", 1<<20), 2); got != "äö" {
		t.Fatalf("clip = %q", got)
	}
	if got := clip("ab", 5); got != "ab" {
		t.Fatalf("short clip = %q", got)
	}
}
