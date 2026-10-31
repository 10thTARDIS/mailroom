// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"log/slog"
	"time"

	"mailroom/internal/store"
)

// jobSweepInterval is how often finished job rows are swept (migration 00084).
const jobSweepInterval = 24 * time.Hour

// runJobSweep deletes finished and dead-lettered job rows past their retention
// windows once now and then every interval until ctx ends. A ticker, like the
// session sweep: the job queue is the thing being swept, a missed run costs
// nothing, and several processes running it at once is harmless.
func runJobSweep(ctx context.Context, st *store.Store, interval, doneKeep, deadKeep time.Duration, log *slog.Logger) {
	sweepJobsOnce(ctx, st, time.Now(), doneKeep, deadKeep, log)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			sweepJobsOnce(ctx, st, now, doneKeep, deadKeep, log)
		}
	}
}

// sweepJobsOnce runs one sweep and logs the count per state, every state listed
// even when zero.
func sweepJobsOnce(ctx context.Context, st *store.Store, now time.Time, doneKeep, deadKeep time.Duration, log *slog.Logger) {
	counts, capped, err := st.SweepTerminalJobs(ctx, now, doneKeep, deadKeep, store.DefaultJobSweepBatch)
	if err != nil {
		if ctx.Err() == nil {
			log.Error("job sweep failed", "err", err)
		}
		return
	}
	attrs := make([]any, 0, 2*len(store.JobSweepStates)+6)
	for _, s := range store.JobSweepStates {
		attrs = append(attrs, s, counts[s])
	}
	attrs = append(attrs, "done_retention", doneKeep.String(), "dead_retention", deadKeep.String(), "capped", capped)
	log.Info("job sweep", attrs...)
}
