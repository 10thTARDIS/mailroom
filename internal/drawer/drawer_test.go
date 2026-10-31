// SPDX-License-Identifier: AGPL-3.0-only

package drawer

import (
	"testing"
	"time"
)

// TestAFullBatchComesBackSoonerThanAPartialOne pins the reschedule shape
// (#443): DrawerCandidates is capped at the batch size, so a batch that came
// back FULL may be hiding more work behind it — the worker fires again in
// ONE MINUTE to drain a large first-run backlog
// in a handful of short runs,
// each comfortably under the 15-minute job lease. A batch that came back
// short of the cap means the drawer is caught up, so the next sweep waits
// out the ordinary interval — the same "drain, then settle" shape
// internal/rescan's own nextFireIn test pins for a remaining-count signal.
func TestAFullBatchComesBackSoonerThanAPartialOne(t *testing.T) {
	w := &Worker{interval: time.Hour, batch: 200}

	for _, tc := range []struct {
		name       string
		candidates int
		archived   int
		want       time.Duration
		why        string
	}{
		{
			name: "a full batch that made progress may hide more work", candidates: 200, archived: 200,
			want: fullBatchRetry,
			why:  "the scan is capped at the batch size, so 200 back is not evidence the drawer is empty",
		},
		{
			name: "a batch over the cap cannot happen, but treat it the same as full", candidates: 250, archived: 250,
			want: fullBatchRetry,
			why:  "belt and braces: the same reasoning applies if the cap is ever raised out from under this",
		},
		{
			name: "a full batch that archived only some still comes back soon", candidates: 200, archived: 1,
			want: fullBatchRetry,
			why:  "one message moved is progress; the rest of the backlog is still waiting",
		},
		{
			name: "a full batch that archived nothing waits the interval", candidates: 200, archived: 0,
			want: time.Hour,
			why: "the same 200 would come straight back — every re-check failed or the predicate is wrong — " +
				"and a one-minute retry of that is a busy loop, not a drain",
		},
		{
			name: "a partial batch means the drawer is caught up", candidates: 3, archived: 3,
			want: time.Hour,
			why:  "fewer candidates than the cap is proof the scan saw everything there was to see",
		},
		{
			name: "an empty batch is the ordinary idle state", candidates: 0, archived: 0,
			want: time.Hour,
			why:  "no candidates at all is not a fault; the ordinary cadence is correct",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := SweepResult{Candidates: tc.candidates, Archived: tc.archived}
			if got := w.nextFireIn(res); got != tc.want {
				t.Errorf("nextFireIn(candidates=%d, archived=%d) = %v, want %v — %s",
					tc.candidates, tc.archived, got, tc.want, tc.why)
			}
		})
	}
}

// TestNextFireInIgnoresCountsWhenDisabled pins the master-switch case
// (migration 00048, #443): a Disabled result must always come back on the
// ordinary interval, even one shaped like a full batch that made progress —
// the switch being off means Sweep never ran, so a Candidates/Archived count
// alongside Disabled must never be read as "drain a backlog quickly."
func TestNextFireInIgnoresCountsWhenDisabled(t *testing.T) {
	w := &Worker{interval: time.Hour, batch: 200}
	res := SweepResult{Disabled: true, Candidates: 200, Archived: 0}
	if got := w.nextFireIn(res); got != time.Hour {
		t.Errorf("nextFireIn(%+v) = %v, want the ordinary interval %v", res, got, time.Hour)
	}
}

// TestNewWorkerDefaults pins the fallback interval and default batch size, so
// a caller passing a non-positive interval (or forgetting one) gets the
// documented one-hour cadence rather than a zero-duration timer that fires
// continuously.
func TestNewWorkerDefaults(t *testing.T) {
	w := NewWorker(nil, 0, nil)
	if w.interval != defaultInterval {
		t.Errorf("interval = %v, want the default %v", w.interval, defaultInterval)
	}
	if w.batch != defaultBatch {
		t.Errorf("batch = %d, want the default %d", w.batch, defaultBatch)
	}
	if w.log == nil {
		t.Error("a nil logger was not replaced with slog.Default()")
	}
}
