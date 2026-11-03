// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"fmt"
	"time"
)

// The job sweep (migration 00084). Nothing used to delete from app.jobs, so
// every finished job stayed forever. The sweep deletes 'done' rows finished
// longer ago than one retention window and 'dead' rows longer ago than another
// (dead rows are kept longer, so a failure stays inspectable), by the daily
// timer in cmd/mailroom.
//
// Both go through SECURITY DEFINER functions for the same reason
// DequeueDueJobs does: in a SystemTx the app.jobs policy matches system rows
// only, so a plain DELETE or count would silently skip every per-user job.

// JobSweepStates is every key SweepTerminalJobs reports, in order.
var JobSweepStates = []string{"done", "dead"}

// DefaultJobSweepBatch bounds one call's deletes per state. The sweep loops
// until a call comes back short, so this sizes a transaction, not a run.
const DefaultJobSweepBatch = 5000

// maxJobSweepBatches stops a run that keeps finding full batches (a much
// larger backlog than expected, or a clock gone wrong); the next run continues.
const maxJobSweepBatches = 100

// SweepTerminalJobs deletes finished job rows past their retention windows and
// returns the count per state, every state present even when zero. 'pending'
// and 'running' rows are never touched, so a live job's idempotency key is
// never freed. capped reports a run that stopped at maxJobSweepBatches.
func (s *Store) SweepTerminalJobs(ctx context.Context, now time.Time, doneKeep, deadKeep time.Duration, batch int) (counts map[string]int64, capped bool, err error) {
	if doneKeep <= 0 || deadKeep <= 0 {
		return nil, false, fmt.Errorf("store: sweep jobs: retention windows must be positive (done %s, dead %s)", doneKeep, deadKeep)
	}
	if batch <= 0 {
		batch = DefaultJobSweepBatch
	}
	counts = make(map[string]int64, len(JobSweepStates))
	for _, st := range JobSweepStates {
		counts[st] = 0
	}
	for i := 0; i < maxJobSweepBatches; i++ {
		var full bool
		err = s.SystemTx(ctx, func(tx *SystemTx) error {
			rows, err := tx.tx.Query(ctx,
				`SELECT state, deleted FROM app.sweep_terminal_jobs($1, $2, $3, $4)`,
				now.UTC(), doneKeep, deadKeep, batch)
			if err != nil {
				return fmt.Errorf("store: sweep jobs: %w", err)
			}
			defer rows.Close()
			for rows.Next() {
				var (
					state string
					n     int64
				)
				if err := rows.Scan(&state, &n); err != nil {
					return fmt.Errorf("store: scan job sweep: %w", err)
				}
				counts[state] += n
				if n >= int64(batch) {
					full = true
				}
			}
			return rows.Err()
		})
		if err != nil {
			return counts, false, err
		}
		if !full {
			return counts, false, nil
		}
	}
	return counts, true, nil
}

// JobCensusRow is one (kind, state) group of app.jobs, with how many of its
// rows the sweep would delete at the given windows.
type JobCensusRow struct {
	Kind      string
	State     string
	Total     int64
	Sweepable int64
	// OldestFinished is the oldest finish time in the group (for a dead row
	// that died before migration 00084, its last claim or creation time); nil
	// for pending and running rows.
	OldestFinished *time.Time
}

// JobCensus returns app.jobs grouped by kind and state, read-only.
func (s *Store) JobCensus(ctx context.Context, now time.Time, doneKeep, deadKeep time.Duration) ([]JobCensusRow, error) {
	var out []JobCensusRow
	err := s.SystemTx(ctx, func(tx *SystemTx) error {
		rows, err := tx.tx.Query(ctx,
			`SELECT kind, state, total, sweepable, oldest_finished FROM app.job_census($1, $2, $3)`,
			now.UTC(), doneKeep, deadKeep)
		if err != nil {
			return fmt.Errorf("store: job census: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var r JobCensusRow
			if err := rows.Scan(&r.Kind, &r.State, &r.Total, &r.Sweepable, &r.OldestFinished); err != nil {
				return fmt.Errorf("store: scan job census: %w", err)
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, err
}
