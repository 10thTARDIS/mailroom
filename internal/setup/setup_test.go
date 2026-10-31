// SPDX-License-Identifier: AGPL-3.0-only

package setup

import (
	"testing"

	"mailroom/internal/store"
)

// The worker's ONE decision, tested where it can be falsified: which way to
// drain a run, from its status alone.
//
// It matters because Setup/undo enqueues THE SAME JOB KIND as Setup/apply and
// flips the status instead of carrying a direction in the payload (the payload
// carries ids only — store.jobPayloadKeys). So this function is the whole
// difference between reversing a run and re-applying it, and a job re-delivered
// after the run finished must be a no-op rather than a second pass.
func TestDirectionFor(t *testing.T) {
	cases := []struct {
		status string
		want   Direction
		why    string
	}{
		{store.SetupStatusQueued, DirectionApply, "a fresh run drains forwards"},
		{store.SetupStatusRunning, DirectionApply, "a resumed run carries on forwards"},
		{store.SetupStatusFailed, DirectionApply,
			"a failed run is RESUMABLE: MarkSetupRunFailed leaves the journal alone, so the " +
				"dispatcher's own retry has to be able to carry on from the cursor"},
		{store.SetupStatusUndoing, DirectionUndo, "Setup/undo flipped the status; this job reverses it"},
		{store.SetupStatusDone, DirectionNone,
			"a re-delivered job must be a no-op, never a second apply"},
		{store.SetupStatusUndone, DirectionNone, "nothing left to reverse"},
		{"something-new", DirectionNone,
			"an unknown status must NOT default to writing: fail closed, do nothing"},
	}
	for _, tc := range cases {
		t.Run(tc.status, func(t *testing.T) {
			if got := DirectionFor(tc.status); got != tc.want {
				t.Errorf("DirectionFor(%q) = %q, want %q — %s", tc.status, got, tc.want, tc.why)
			}
		})
	}
}

// The kind constant is the wiring between the store's enqueue and the
// dispatcher's registration; a rename on one side only would leave a run
// enqueued that nothing ever handles, with no error anywhere.
func TestJobKindMatchesTheStore(t *testing.T) {
	if JobKind != store.SetupJobKind {
		t.Errorf("JobKind = %q, store.SetupJobKind = %q", JobKind, store.SetupJobKind)
	}
	if JobKind != "setup_import" {
		t.Errorf("JobKind = %q, want %q (the value jobPayloadKeys describes)", JobKind, "setup_import")
	}
}
