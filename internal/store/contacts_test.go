// SPDX-License-Identifier: AGPL-3.0-only

package store

// Unit tests (no database) for the pure helpers of the auto-derived contacts
// layer. The upsert/change-log behaviour and the autocomplete query run against
// a real Postgres in contacts_integration_test.go.

import "testing"

func TestEscapeLike(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"plain", "plain"},
		{"a%b", `a\%b`},
		{"a_b", `a\_b`},
		{`a\b`, `a\\b`},
		{"100%_x", `100\%\_x`},
		{"", ""},
	}
	for _, tc := range tests {
		if got := escapeLike(tc.in); got != tc.want {
			t.Errorf("escapeLike(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
