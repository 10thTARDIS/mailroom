// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"testing"
	"time"
)

// The old band's boundary comes from RescanTiers, not from a second constant.
func TestOldBandAgeIsTheLastRescannedTiersBound(t *testing.T) {
	if got, want := OldBandAge(), 180*24*time.Hour; got != want {
		t.Errorf("OldBandAge = %v, want %v (the `1-6 months` tier's upper bound)", got, want)
	}
}
