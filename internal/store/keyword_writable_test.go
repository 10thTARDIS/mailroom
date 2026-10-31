// SPDX-License-Identifier: AGPL-3.0-only

package store

import "testing"

// The store's backstop against a system flag written as a keyword (#49 review,
// F1). RED PROBE: make KeywordWritable return true and the backslash cases fail.
func TestKeywordWritableRefusesSystemFlags(t *testing.T) {
	for kw, want := range map[string]bool{
		`\Draft`:                   false,
		`\draft`:                   false,
		`\Seen`:                    false,
		`\Deleted`:                 false,
		`a\b`:                      false,
		"":                         false,
		"$NotJunk":                 true,
		"MyTag":                    true,
		"MailroomLabel-invoices-2": true,
	} {
		if got := KeywordWritable(kw); got != want {
			t.Errorf("KeywordWritable(%q) = %v, want %v", kw, got, want)
		}
	}
}

// The raw-keyword backstop for keywords with meaning of their own, in any
// casing (#49 review). RED PROBE: return false from KeywordReserved and the
// reserved cases fail.
func TestKeywordReservedFoldsCase(t *testing.T) {
	for kw, want := range map[string]bool{
		"$JUNK": true, "$Junk": true, "junk": true, "JUNK": true,
		"$NotJunk": true, "NonJunk": true,
		"$Draft": true, "$SEEN": true, "$flagged": true, "$Answered": true,
		"$important": false, "MyTag": false, "MailroomLabel-Case-Test": false, "junkmail": false,
	} {
		if got := KeywordReserved(kw); got != want {
			t.Errorf("KeywordReserved(%q) = %v, want %v", kw, got, want)
		}
	}
}
