// SPDX-License-Identifier: AGPL-3.0-only

package api

import "testing"

// ruleScanLimit is the one place Rule/preview and Rule/apply choose their scan
// bound (#720); a unit test needs no database.
func TestRuleScanLimit(t *testing.T) {
	for _, tc := range []struct {
		name      string
		requested int
		bodies    bool
		want      int
	}{
		{"row rule default is the whole-mailbox bound", 0, false, previewMaxScanRows},
		{"row rule negative takes the default", -3, false, previewMaxScanRows},
		{"row rule limit lowers it", 100, false, 100},
		{"row rule clamps above the ceiling", previewMaxScanRows + 1, false, previewMaxScanRows},
		{"body rule default is the blob default", 0, true, previewDefaultScanBlob},
		{"body rule limit lowers it", 50, true, 50},
		{"body rule clamps to the blob maximum", 50000, true, previewMaxScanBlob},
	} {
		if got := ruleScanLimit(tc.requested, tc.bodies); got != tc.want {
			t.Errorf("%s: ruleScanLimit(%d, %v) = %d, want %d", tc.name, tc.requested, tc.bodies, got, tc.want)
		}
	}
}
