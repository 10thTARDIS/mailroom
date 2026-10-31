// SPDX-License-Identifier: AGPL-3.0-only

package store

import "testing"

// A ranking key is actionable only when its action query searches for
// exactly that row (#53 security review). An address with no local part is
// read by `from:` as a DOMAIN (#712), so it must never be offered as one
// address.
func TestSenderKeyActionable(t *testing.T) {
	cases := []struct {
		g    SenderGroup
		key  string
		want bool
	}{
		{SenderByAddress, "alice@example.com", true},
		{SenderByAddress, "example.com", false},
		{SenderByAddress, "@example.com", false},
		{SenderByAddress, "*.example.com", false},
		{SenderByAddress, ".example.com", false},
		{SenderByAddress, "alice@", false},
		{SenderByAddress, "a@b@example.com", false},
		{SenderByAddress, "*@example.com", false},
		{SenderByAddress, "Alice@Example.com", false}, // keys are lower-cased; anything else did not come from the ranking
		{SenderByAddress, "odd key@example.com", false},
		{SenderByAddress, `"q"@example.com`, false},
		{SenderByAddress, "", false},
		{SenderByAddress, "a@.example.com", false},
		{SenderByAddress, "a@example.com.", false},
		{SenderByAddress, "a@example..com", false},
		{SenderByDomain, "example.com", true},
		{SenderByDomain, "mail.example.com", true},
		{SenderByDomain, "*.example.com", false},
		{SenderByDomain, "a@example.com", false},
		{SenderByDomain, ".example.com", false},
		{SenderByDomain, "localhost", false},
		{SenderByList, "news.lists.example", true},
		{SenderByList, "two words", false},
		{SenderByList, "news@lists.example", false},
		{SenderByList, "*.lists.example", false},
		{SenderGroup("other"), "x@example.com", false},
	}
	for _, c := range cases {
		if got := SenderKeyActionable(c.g, c.key); got != c.want {
			t.Errorf("SenderKeyActionable(%s, %q) = %v, want %v", c.g, c.key, got, c.want)
		}
	}
}
