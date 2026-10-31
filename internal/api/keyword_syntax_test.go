// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"strings"
	"testing"
)

// RFC 8621 §4.1.1 keyword syntax, which Email/set enforces (#49 review, F1).
// RED PROBE: make validJMAPKeyword return true and the refused cases fail.
func TestValidJMAPKeyword(t *testing.T) {
	for kw, want := range map[string]bool{
		"$seen": true, "$draft": true, "MyTag": true, "$Project-7": true,
		"MailroomLabel-invoices-2": true, strings.Repeat("k", 255): true,
		`\Draft`: false, `\Seen`: false, `a\b`: false,
		"": false, strings.Repeat("k", 256): false,
		"has space": false, "tab\there": false, "nul\x00": false, "del\x7f": false,
		"café": false,
		"(":    false, ")": false, "{": false, "]": false, "%": false, "*": false, `"`: false,
	} {
		if got := validJMAPKeyword(kw); got != want {
			t.Errorf("validJMAPKeyword(%q) = %v, want %v", kw, got, want)
		}
	}
}
