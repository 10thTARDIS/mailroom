// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"testing"
	"time"
)

// lagSeconds is pure: no database, no fixture. It converts "last successful sync"
// into whole seconds of staleness, is nil for a never-synced account, and never
// goes negative under clock skew.
func TestLagSeconds(t *testing.T) {
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)

	t.Run("never synced is nil", func(t *testing.T) {
		if got := lagSeconds(now, nil); got != nil {
			t.Errorf("lagSeconds(nil) = %v, want nil", *got)
		}
	})

	t.Run("whole seconds since last success", func(t *testing.T) {
		last := now.Add(-90 * time.Minute) // 5400s
		got := lagSeconds(now, &last)
		if got == nil {
			t.Fatal("lagSeconds = nil, want 5400")
		}
		if *got != 5400 {
			t.Errorf("lagSeconds = %d, want 5400", *got)
		}
	})

	t.Run("truncates toward the second", func(t *testing.T) {
		last := now.Add(-(3 * time.Second) - 900*time.Millisecond) // 3.9s → 3
		got := lagSeconds(now, &last)
		if got == nil || *got != 3 {
			t.Errorf("lagSeconds = %v, want 3", got)
		}
	})

	t.Run("clock skew clamps to zero", func(t *testing.T) {
		future := now.Add(30 * time.Second) // last success slightly in the future
		got := lagSeconds(now, &future)
		if got == nil || *got != 0 {
			t.Errorf("lagSeconds(future) = %v, want 0", got)
		}
	})
}
