// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"reflect"
	"testing"
)

// applyFlagDelta is the record of what an acknowledged STORE left on a copy
// (#576). Case-insensitive both ways, because the server reported `$Junk` and a
// client asked to remove `$junk` is the same keyword, and an existing spelling
// is kept rather than duplicated.
func TestApplyFlagDelta(t *testing.T) {
	cases := []struct {
		name             string
		cur, add, remove []string
		want             []string
		changed          bool
	}{
		{"remove junk keeps the rest in order", []string{`\Seen`, "$Junk", "work"}, nil, []string{"$Junk"}, []string{`\Seen`, "work"}, true},
		{"remove is case-insensitive", []string{"$Junk", "Junk"}, nil, []string{"$junk", "JUNK"}, []string{}, true},
		{"add appends", []string{`\Seen`}, []string{"$NotJunk"}, nil, []string{`\Seen`, "$NotJunk"}, true},
		{"add of a present flag keeps its spelling", []string{"$notjunk"}, []string{"$NotJunk"}, nil, []string{"$notjunk"}, false},
		{"remove of an absent flag is no change", []string{`\Seen`}, nil, []string{"$Junk"}, []string{`\Seen`}, false},
		{"empty stays empty", nil, nil, nil, []string{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, changed := applyFlagDelta(tc.cur, tc.add, tc.remove)
			if !reflect.DeepEqual(got, tc.want) || changed != tc.changed {
				t.Errorf("applyFlagDelta(%v, +%v, -%v) = %v, %v; want %v, %v",
					tc.cur, tc.add, tc.remove, got, changed, tc.want, tc.changed)
			}
		})
	}
}
