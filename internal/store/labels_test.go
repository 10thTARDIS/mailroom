// SPDX-License-Identifier: AGPL-3.0-only

package store

// Unit tests (no database) for the deterministic label→keyword mapping: slug
// sanitization to the IMAP atom charset, the MailroomLabel- base atom, and the
// pure local-only mirror decision (spec §6). Collision suffixing and rename
// stability against a real UNIQUE constraint are covered in the integration
// suite (labels_integration_test.go).

import "testing"

func TestSlugForKeyword(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"simple", "Invoices", "invoices"},
		{"spaces collapse", "Paid  Invoices", "paid-invoices"},
		{"punctuation to dash", "Invoices/2024!", "invoices-2024"},
		{"leading trailing trimmed", "  @Work@  ", "work"},
		{"unicode dropped", "Café", "caf"},
		{"all invalid empty", "***", ""},
		{"emoji empty", "🚀", ""},
		{"mixed case lowered", "TODO-Later", "todo-later"},
		{"digits kept", "2024", "2024"},
		{"underscores collapse", "a__b", "a-b"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := slugForKeyword(tc.in); got != tc.want {
				t.Errorf("slugForKeyword(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestKeywordAtomBase(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"Invoices", "MailroomLabel-invoices"},
		{"Paid Invoices", "MailroomLabel-paid-invoices"},
		{"***", "MailroomLabel-label"}, // no atom-safe chars → "label" base
		{"🚀", "MailroomLabel-label"},   // emoji-only → "label" base
	}
	for _, tc := range tests {
		if got := keywordAtomBase(tc.in); got != tc.want {
			t.Errorf("keywordAtomBase(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The produced base atoms must satisfy the IMAP atom charset the
// label_keyword_map CHECK constraint enforces (no atom-specials).
func TestKeywordAtomBaseCharset(t *testing.T) {
	for _, name := range []string{"Invoices", "Paid Invoices 2024!", "a/b\\c(d)", "TODO", "***", "Café"} {
		atom := keywordAtomBase(name)
		for _, r := range atom {
			ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
				(r >= '0' && r <= '9') || r == '-'
			if !ok {
				t.Errorf("keywordAtomBase(%q) = %q contains disallowed rune %q", name, atom, r)
			}
		}
	}
}

func TestMirrorKeyword(t *testing.T) {
	tests := []struct {
		name    string
		atom    string
		accepts bool
		want    bool
	}{
		{"mirrors when atom present and mailbox accepts", "MailroomLabel-x", true, true},
		{"local-only when mailbox rejects keywords", "MailroomLabel-x", false, false},
		{"local-only when label has no atom", "", true, false},
		{"local-only both", "", false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := MirrorKeyword(tc.atom, tc.accepts); got != tc.want {
				t.Errorf("MirrorKeyword(%q, %v) = %v, want %v", tc.atom, tc.accepts, got, tc.want)
			}
		})
	}
}
