// SPDX-License-Identifier: AGPL-3.0-only

package main

// `mailroom-admin jobs-report` — what the background job table holds, and what
// the daily job sweep (migration 00084) would delete from it today. READ-ONLY:
// the sweep itself runs inside the server; this only shows its arithmetic.
//
// It reports per kind and state rather than one total, so "the sweep deletes
// nothing" can be told apart from "the sweep cannot see that state".

import (
	"context"
	"flag"
	"fmt"
	"time"

	"mailroom/internal/store"
)

func runJobsReport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("jobs-report", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	d, err := openDeps(ctx)
	if err != nil {
		return err
	}
	defer d.close()

	// The same windows the server's sweep uses: both come from config.Load(),
	// so MAILROOM_JOB_RETENTION and MAILROOM_DEAD_JOB_RETENTION apply here too.
	doneKeep, deadKeep := d.cfg.JobRetention, d.cfg.DeadJobRetention
	now := time.Now()
	rows, err := d.store.JobCensus(ctx, now, doneKeep, deadKeep)
	if err != nil {
		return fmt.Errorf("jobs-report: %w", err)
	}

	fmt.Printf("=== jobs-report: background job rows ===\n")
	fmt.Printf("retention: done %s · dead %s (MAILROOM_JOB_RETENTION, MAILROOM_DEAD_JOB_RETENTION)\n\n", doneKeep, deadKeep)
	fmt.Printf("%-22s %-8s %8s %10s  %s\n", "KIND", "STATE", "ROWS", "SWEEPABLE", "OLDEST FINISHED")
	var total, sweepable int64
	perState := map[string][2]int64{}
	for _, r := range rows {
		oldest := "-"
		if r.OldestFinished != nil {
			oldest = r.OldestFinished.UTC().Format("2006-01-02")
		}
		fmt.Printf("%-22s %-8s %8d %10d  %s\n", r.Kind, r.State, r.Total, r.Sweepable, oldest)
		total += r.Total
		sweepable += r.Sweepable
		c := perState[r.State]
		perState[r.State] = [2]int64{c[0] + r.Total, c[1] + r.Sweepable}
	}
	fmt.Printf("\nrows: %d · the sweep would delete %d today\n", total, sweepable)
	for _, s := range []string{"pending", "running", "done", "dead"} {
		c := perState[s]
		fmt.Printf("  %-8s %8d rows, %d sweepable\n", s, c[0], c[1])
	}
	if total == 0 {
		fmt.Printf("\n⚠ NO JOB ROWS AT ALL. A running server always has its recurring jobs pending, so " +
			"an empty table means this process cannot see it, not that it is clean.\n")
	}
	fmt.Printf("\nREPORT ONLY: nothing was written. The server's daily sweep deletes the sweepable rows (batch %d).\n",
		store.DefaultJobSweepBatch)
	return nil
}
