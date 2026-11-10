// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	"mailroom/internal/store"
)

// `mailroom-admin placement-backfill-summaries --user-id [--apply]` — write the
// Activity summary rows for placement backfills run BEFORE the summaries existed
// (ADR-053 amendment).
//
// The Activity page reads one summary row per batch and direction instead of
// grouping one journal row per message on every request. A batch run by an
// earlier version has journal rows and no summary, so it is missing from
// Activity until this runs once.
//
// REPORT FIRST, --apply OPT-IN. Without --apply it prints, per direction, how
// many batches would get a summary, how many journal rows they cover and how
// many rows already have one, and writes nothing. --apply writes the summaries
// in one transaction, with each batch's newest journal time (so the Activity
// entry's time and count are what the grouping gave), then re-reads and prints
// what is left, which must be zero. Running it again finds nothing to do.
func runPlacementBackfillSummaries(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("placement-backfill-summaries", flag.ContinueOnError)
	userIDStr := fs.String("user-id", "", "user id (ULID)")
	apply := fs.Bool("apply", false, "write the missing summaries (default: report only)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	userID, err := parseUserID(*userIDStr)
	if err != nil {
		return err
	}
	d, err := openDeps(ctx)
	if err != nil {
		return err
	}
	defer d.close()

	before, err := d.store.SummarisePlacementBackfills(ctx, userID, *apply)
	if err != nil {
		return fmt.Errorf("placement-backfill-summaries: %w", err)
	}
	printBackfillSummaries(os.Stdout, before, *apply)
	if *apply {
		after, err := d.store.SummarisePlacementBackfills(ctx, userID, false)
		if err != nil {
			return fmt.Errorf("placement-backfill-summaries: re-read: %w", err)
		}
		_, _ = fmt.Fprintf(os.Stdout, "after --apply, still missing a summary: move %d batch(es), undo %d batch(es) (must be 0)\n",
			after.Move.Batches, after.Undo.Batches)
		if after.Move.Batches != 0 || after.Undo.Batches != 0 {
			return fmt.Errorf("placement-backfill-summaries: --apply left batches without a summary")
		}
	}
	return nil
}

func printBackfillSummaries(w io.Writer, r store.BackfillSummaryReport, apply bool) {
	mode := "REPORT ONLY (nothing written; --apply writes the summaries)"
	if apply {
		mode = "APPLIED"
	}
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }
	p("=== placement-backfill-summaries: %s ===\n", mode)
	for _, row := range []struct {
		name string
		d    store.BackfillSummaryDirection
	}{{"move", r.Move}, {"undo", r.Undo}} {
		p("%-5s batches missing a summary: %d (covering %d journal rows); journal rows already summarised: %d\n",
			row.name, row.d.Batches, row.d.Rows, row.d.Summarised)
	}
}
