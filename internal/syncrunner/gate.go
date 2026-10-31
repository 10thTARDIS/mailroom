// SPDX-License-Identifier: AGPL-3.0-only

package syncrunner

import (
	stdsync "sync"
	"time"

	"github.com/oklog/ulid/v2"
)

// syncGate serializes and coalesces sync passes for ONE account (mailroom#103
// Stage 2, Deliverable A). It is the safety foundation that lets the scheduled
// poll loop and the IMAP IDLE trigger both drive SyncAccount for the same
// account without ever running two passes at once — SyncAccount is idempotent
// and cursor-driven, but the engine documents that one account must be synced
// by one call at a time, and nothing else enforces it.
//
// It guarantees two properties, both required for correctness (not just
// efficiency):
//
//   - SERIALIZE: at most one pass runs for the account at any instant.
//   - COALESCE-EXACTLY-ONE: a trigger that arrives while a pass is in flight is
//     never lost and never piles up — it results in EXACTLY ONE more pass after
//     the current one, however many triggers arrived. The one follow-up is
//     mandatory: an EXISTS during a running pass may refer to mail that pass
//     started too early to see, so a guaranteed re-run is the only thing that
//     closes that window without waiting for the next poll tick.
//
// lastIdleTrigger backs the per-account IDLE debounce (Deliverable B) and lives
// here because it shares the account's lifecycle and lock.
type syncGate struct {
	mu              stdsync.Mutex
	running         bool
	pending         bool
	lastIdleTrigger time.Time
}

// gateFor returns the account's gate, creating it (and the user's gate map) on
// first use. Guarded by scheduleMu, the same lock that reaps the gate when the
// account vanishes (dueAccounts) or the user's loop exits (forgetUser).
func (r *Runner) gateFor(userID, accountID ulid.ULID) *syncGate {
	r.scheduleMu.Lock()
	defer r.scheduleMu.Unlock()
	byAcct := r.gates[userID]
	if byAcct == nil {
		byAcct = make(map[ulid.ULID]*syncGate)
		r.gates[userID] = byAcct
	}
	g := byAcct[accountID]
	if g == nil {
		g = &syncGate{}
		byAcct[accountID] = g
	}
	return g
}

// runGated runs run for (userID, accountID) under the account's gate:
//
//   - if a pass is already in flight, it marks the account pending and returns
//     IMMEDIATELY — the in-flight holder will re-run once for it;
//   - otherwise it becomes the holder, runs, and then — as long as a trigger
//     arrived while it ran — clears the pending flag and runs again, looping
//     until no trigger is outstanding, before releasing the account.
//
// run must observe context itself; on cancellation it should return promptly.
// It then re-runs at most once more (also returning promptly on the cancelled
// context), so teardown cannot deadlock or spin.
//
// gate.mu is NOT held while run executes — run does real IMAP and DB work and
// may itself take scheduleMu (advanceSchedule) — so a concurrent trigger for a
// different account never waits on this one.
func (r *Runner) runGated(userID, accountID ulid.ULID, run func()) {
	g := r.gateFor(userID, accountID)

	g.mu.Lock()
	if g.running {
		// A pass is in flight; register the follow-up and leave. The holder
		// will observe pending after its current run and re-run exactly once
		// (further triggers before then collapse into this same one bool).
		g.pending = true
		g.mu.Unlock()
		return
	}
	g.running = true
	g.mu.Unlock()

	for {
		run()

		g.mu.Lock()
		if g.pending {
			g.pending = false
			g.mu.Unlock()
			continue // one more pass for the trigger(s) that arrived while we ran
		}
		g.running = false
		g.mu.Unlock()
		return
	}
}
