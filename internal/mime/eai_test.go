// SPDX-License-Identifier: AGPL-3.0-only

package mime

import "testing"

func TestAddressRequiresEAI(t *testing.T) {
	tests := []struct {
		addr string
		want bool
	}{
		{"alice@example.com", false},
		{"", false},
		{"alice+tag@example.com", false},
		{"用户@example.com", true},
		{"alice@例え.jp", true},
	}
	for _, tc := range tests {
		if got := AddressRequiresEAI(tc.addr); got != tc.want {
			t.Errorf("AddressRequiresEAI(%q) = %v, want %v", tc.addr, got, tc.want)
		}
	}
}
