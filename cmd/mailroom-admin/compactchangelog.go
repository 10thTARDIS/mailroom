// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"flag"
	"fmt"
	"time"

	"mailroom/internal/changelog"
	"mailroom/internal/config"
	"mailroom/internal/store"
)

// `mailroom-admin compact-changelog` — the report-first preview (and,
// with --apply, the manual run) of change_log compaction (mailroom#526,
// ADR-003 Consequences, mailroom-data-model.md §3.3).
//
// REPORT-FIRST, --apply OPT-IN (CLAUDE.md "build the report before the
// fix"). The report call passes an UNCAPPED maxDeletePerType so the numbers
// it prints are the TRUE eligible total for every type, not one batch's cap;
// --apply then drains that backlog through the same batched loop the weekly
// job uses (changelog.Worker.Sweep — store.DefaultChangeLogCompactBatch rows
// per type per transaction), so a large first-run backlog on an old account
// never sits behind one unbounded DELETE.
//
// Honours the same MinChangeLogRetention (7 days) floor config.Load enforces
// on MAILROOM_CHANGE_LOG_RETENTION, for the same reason: a shorter window
// would force every client, not just one genuinely offline a while, through
// cannotCalculateChanges on a recurring basis.
func runCompactChangelog(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("compact-changelog", flag.ContinueOnError)
	var (
		userIDStr = fs.String("user-id", "", "operator user id (ULID)")
		retention = fs.Duration("retention", config.DefaultChangeLogRetention,
			"how far back change_log history is kept (minimum 7 days; mirrors MAILROOM_CHANGE_LOG_RETENTION)")
		apply = fs.Bool("apply", false, "delete the eligible prefix and raise change_floor (default: report only)")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	userID, err := parseUserID(*userIDStr)
	if err != nil {
		return err
	}
	if *retention < config.MinChangeLogRetention {
		return fmt.Errorf("compact-changelog: --retention must be at least %s (a shorter window would force every "+
			"client to resync on a recurring basis), got %s", config.MinChangeLogRetention, retention)
	}

	d, err := openDeps(ctx)
	if err != nil {
		return err
	}
	defer d.close()

	cutoff := time.Now().UTC().Add(-*retention)

	var report []store.CompactionResult
	if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		report, e = tx.CompactChangeLog(ctx, cutoff, 0, false) // 0 = uncapped: the true eligible total
		return e
	}); err != nil {
		return fmt.Errorf("compact-changelog: report: %w", err)
	}

	fmt.Printf("=== compact-changelog (#526): user %s, retention %s, cutoff %s ===\n",
		userID, retention, cutoff.Format(time.RFC3339))
	printCompactionReport(report)

	total := int64(0)
	for _, r := range report {
		total += r.Deleted
	}

	if !*apply {
		if total == 0 {
			fmt.Println("\nREPORT ONLY — nothing was written, and nothing would change.")
		} else {
			fmt.Printf("\nREPORT ONLY — nothing was written. Re-run with --apply to delete %d row(s).\n", total)
		}
		return nil
	}
	if total == 0 {
		fmt.Println("\nnothing to apply.")
		return nil
	}

	// The same batched drain the weekly job runs — each batch its own
	// transaction (store.DefaultChangeLogCompactBatch rows per type), looped
	// until nothing more is deleted.
	w := changelog.NewWorker(d.store, *retention, 0, d.log)
	sweepRes, err := w.Sweep(ctx, userID)
	if err != nil {
		return fmt.Errorf("compact-changelog: apply: %w", err)
	}
	applied := accumulateCompactionResults(sweepRes.Types)
	fmt.Println("\nAPPLIED:")
	printCompactionReport(applied)
	if sweepRes.Capped {
		fmt.Println("\n⚠ at least one type hit its per-batch cap; re-run to continue draining the remainder.")
	}

	// A CHECKED FACT, NOT A SLOGAN (CLAUDE.md): re-run the report rather than
	// trust the counters above — it must now show 0 eligible.
	var afterReport []store.CompactionResult
	if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		afterReport, e = tx.CompactChangeLog(ctx, cutoff, 0, false)
		return e
	}); err != nil {
		return fmt.Errorf("compact-changelog: re-check remainder: %w", err)
	}
	remaining := int64(0)
	for _, r := range afterReport {
		remaining += r.Deleted
	}
	if remaining == 0 {
		fmt.Println("\nverified: 0 row(s) remain eligible.")
	} else {
		fmt.Printf("\n⚠ %d row(s) remain eligible after --apply — expected only if a batch was capped above; "+
			"investigate if it was not.\n", remaining)
	}
	return nil
}

// accumulateCompactionResults folds a Worker.Sweep's per-batch results
// (Worker.Sweep calls CompactChangeLog once per batch, so every type that had
// anything to delete appears once per batch it was touched in) into one
// summary row per type: RowsBefore/OldFloor from the FIRST batch that
// touched the type, Deleted summed across every batch, NewFloor/OldestKept
// from the LAST — so "rows before" and "rows after" describe the whole
// --apply run, not just its final batch.
func accumulateCompactionResults(all []store.CompactionResult) []store.CompactionResult {
	var order []store.ObjectType
	byType := make(map[store.ObjectType]store.CompactionResult, len(all))
	for _, r := range all {
		acc, ok := byType[r.ObjectType]
		if !ok {
			order = append(order, r.ObjectType)
			acc = store.CompactionResult{ObjectType: r.ObjectType, RowsBefore: r.RowsBefore, OldFloor: r.OldFloor}
		}
		acc.Deleted += r.Deleted
		acc.NewFloor = r.NewFloor
		acc.OldestKept = r.OldestKept
		byType[r.ObjectType] = acc
	}
	out := make([]store.CompactionResult, 0, len(order))
	for _, t := range order {
		out = append(out, byType[t])
	}
	return out
}

// printCompactionReport prints one row per object type with the arithmetic
// that proves "before - deleted = after" spelled out, rather than left for
// the reader to compute.
func printCompactionReport(rows []store.CompactionResult) {
	fmt.Printf("%-18s %11s %9s %9s %9s %10s %12s\n",
		"TYPE", "ROWS BEFORE", "DELETED", "FLR OLD", "FLR NEW", "ROWS AFTER", "OLDEST KEPT")
	for _, r := range rows {
		after := r.RowsBefore - r.Deleted
		fmt.Printf("%-18s %11d %9d %9d %9d %10d %12s\n",
			r.ObjectType, r.RowsBefore, r.Deleted, r.OldFloor, r.NewFloor, after, oldestKeptOrDash(r.OldestKept))
		fmt.Printf("  %d - %d = %d\n", r.RowsBefore, r.Deleted, after)
	}
}

func oldestKeptOrDash(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return t.UTC().Format("2006-01-02")
}
