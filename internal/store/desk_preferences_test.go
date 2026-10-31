// SPDX-License-Identifier: AGPL-3.0-only

package store

import "testing"

// TestClientFamilyValid pins the closed vocabulary before it ever reaches a
// database round trip.
func TestClientFamilyValid(t *testing.T) {
	cases := []struct {
		f    ClientFamily
		want bool
	}{
		{ClientWeb, true},
		{ClientPhone, true},
		{ClientDesktop, true},
		{ClientFamily("tablet"), false},
		{ClientFamily(""), false},
	}
	for _, c := range cases {
		if got := c.f.Valid(); got != c.want {
			t.Errorf("ClientFamily(%q).Valid() = %v, want %v", c.f, got, c.want)
		}
	}
}
