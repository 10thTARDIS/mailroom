// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"errors"
	"io/fs"
	"regexp"
	"strings"
	"testing"
	"time"

	"mailroom/db"
)

func TestCleanContactUserName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"  Bob  ", "Bob"},
		{"Bob\x00\x07\x1b[31m Smith", "Bob[31m Smith"},
		{"line\nbreak", "linebreak"},
		{"\u202eevil\u202c name\u2066", "evil name"},
		{"\u200fRTL mark", "RTL mark"},
		{"\x85next line", "next line"},
		{"   \t ", ""},
		{"Zoë 山田", "Zoë 山田"},
	}
	for _, c := range cases {
		got, err := CleanContactUserName(c.in)
		if err != nil || got != c.want {
			t.Errorf("CleanContactUserName(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
	if _, err := CleanContactUserName(strings.Repeat("山", 200)); err != nil {
		t.Errorf("200 characters (600 bytes) refused: %v", err)
	}
	if _, err := CleanContactUserName(strings.Repeat("山", 201)); !errors.Is(err, ErrContactUserNameTooLong) {
		t.Errorf("201 characters: err=%v", err)
	}
	// Counted after cleaning: 200 letters plus controls is fine.
	if _, err := CleanContactUserName(strings.Repeat("a", 200) + "\x00\x00"); err != nil {
		t.Errorf("controls counted toward the cap: %v", err)
	}
}

func TestCleanContactNotes(t *testing.T) {
	got, err := CleanContactNotes("  one\r\ntwo\rthree\tfour\x00\u202e\n\n ")
	if err != nil || got != "one\ntwo\nthree\tfour" {
		t.Errorf("CleanContactNotes = %q, %v", got, err)
	}
	if _, err := CleanContactNotes(strings.Repeat("n", MaxContactNotes)); err != nil {
		t.Errorf("4,000 characters refused: %v", err)
	}
	if _, err := CleanContactNotes(strings.Repeat("n", MaxContactNotes+1)); !errors.Is(err, ErrContactNotesTooLong) {
		t.Errorf("4,001 characters: err=%v", err)
	}
}

func TestCombineBlock(t *testing.T) {
	early, late := time.Unix(1000, 0), time.Unix(2000, 0)
	c := func(blocked bool, dest string, at *time.Time) contactState {
		return contactState{blocked: blocked, dest: dest, blockedAt: at}
	}
	cases := []struct {
		name    string
		a, b    contactState
		blocked bool
		dest    string
		at      *time.Time
	}{
		{"neither: a's preference", c(false, BlockToTrash, nil), c(false, BlockToJunk, nil), false, BlockToTrash, nil},
		{"a only", c(true, BlockToJunk, &late), c(false, BlockToTrash, nil), true, BlockToJunk, &late},
		{"b only", c(false, BlockToTrash, nil), c(true, BlockToJunk, &early), true, BlockToJunk, &early},
		{"both: trash and earlier win", c(true, BlockToJunk, &early), c(true, BlockToTrash, &late), true, BlockToTrash, &early},
		{"both: earlier from b", c(true, BlockToTrash, &late), c(true, BlockToJunk, &early), true, BlockToTrash, &early},
	}
	for _, tc := range cases {
		blocked, dest, at := combineBlock(tc.a, tc.b)
		if blocked != tc.blocked || dest != tc.dest || (at == nil) != (tc.at == nil) || (at != nil && !at.Equal(*tc.at)) {
			t.Errorf("%s: got %v %s %v, want %v %s %v", tc.name, blocked, dest, at, tc.blocked, tc.dest, tc.at)
		}
	}
}

// TestSavedContactsMigrationHasNoBackfill pins ADR-041 §1: 00072 adds its
// columns as plain nullable ones and writes no row. saved_at is filled only by
// contact-dates-backfill --apply, after its report has been read.
func TestSavedContactsMigrationHasNoBackfill(t *testing.T) {
	raw, err := fs.ReadFile(db.Migrations, "migrations/00072_saved_contacts_and_merge.sql")
	if err != nil {
		t.Fatal(err)
	}
	up := string(raw)
	if i := strings.Index(up, "-- +goose Up"); i >= 0 {
		up = up[i:]
	}
	if i := strings.Index(up, "-- +goose Down"); i >= 0 {
		up = up[:i]
	}
	var code []string
	for _, line := range strings.Split(up, "\n") {
		if l := strings.TrimSpace(line); l != "" && !strings.HasPrefix(l, "--") {
			code = append(code, l)
		}
	}
	body := strings.Join(code, "\n")
	if len(code) < 5 {
		t.Fatalf("read only %d code lines of 00072's Up section", len(code))
	}
	if regexp.MustCompile(`(?im)^(UPDATE\s+\S+\s+SET|INSERT\s+INTO|COPY)\b`).MatchString(body) {
		t.Errorf("00072 writes rows; the backfill is the admin verb's, report first:\n%s", body)
	}
	for _, col := range []string{"saved_at", "user_display_name", "merged_into", "merged_from_contact"} {
		decl := regexp.MustCompile(`(?i)ADD COLUMN ` + col + `\b[^\n]*`).FindString(body)
		if decl == "" {
			t.Errorf("00072 does not add %s", col)
			continue
		}
		if regexp.MustCompile(`(?i)\b(DEFAULT|NOT NULL)\b`).MatchString(decl) {
			t.Errorf("%s is declared %q: a DEFAULT or NOT NULL fills existing rows", col, decl)
		}
	}
}

// A learned name made of characters a renderer draws as nothing is no name
// (#853 review): the Hangul filler and variation selectors are
// Default_Ignorable but not Cf, and used to survive.
func TestCleanLearnedContactNameDropsDefaultIgnorables(t *testing.T) {
	for in, want := range map[string]string{
		"\u3164\u3164":              "",
		"Al\uFE0Fice\u034F":         "Alice",
		"Bob\u115F\uFFA0\U000E0100": "Bob",
		"Zoe\u200B":                 "Zoe",
		"Ren\u00e9e \u2764\uFE0F":   "Ren\u00e9e \u2764",
	} {
		if got := CleanLearnedContactName(in); got != want {
			t.Errorf("CleanLearnedContactName(%q) = %q, want %q", in, got, want)
		}
	}
	if !ClassifyContactName("\u3164").Format || ClassifyContactName("plain").Any() {
		t.Error("ClassifyContactName must report a Hangul filler as a format character, and nothing in plain text")
	}
}
