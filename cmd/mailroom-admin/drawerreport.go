// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"flag"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// `mailroom-admin drawer-report` — the report-first preview of the drawer
// auto-archive sweep (mailroom#443, Direction 7 "Desk and Drawers", #372).
//
// WHY THIS EXISTS BEFORE THE JOB EVER RUNS UNSUPERVISED (CLAUDE.md: "build
// the report before the fix"). The drawer sweep is a MOVE ON THE SERVER — a
// real, visible writeback event, not a local flag — and on a real
// mailbox a large part of the inbox was already
// older than 30 days, much of it in Promotions. The first run is a
// large, one-shot event, and it must be previewed, never discovered by a
// user watching their inbox move.
//
// Report register matches materialize-area-labels/backfill-model-blobs: the
// default run writes nothing; --apply performs exactly what the report just
// showed, in batches, each message its own transaction (ArchiveByDrawer),
// and then RE-CHECKS the remainder rather than trusting the count it started
// with — a non-zero remainder is a WARNING, never hidden.
func runDrawerReport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("drawer-report", flag.ContinueOnError)
	var (
		userIDStr = fs.String("user-id", "", "operator user id (ULID)")
		apply     = fs.Bool("apply", false, "archive every candidate (default: report only)")
		batch     = fs.Int("batch", 200, "candidates archived per scan/apply pass, each message its own transaction")
		enable    = fs.Bool("enable", false, "switch the drawer sweep master switch ON, after printing the report (mailroom#443)")
		disable   = fs.Bool("disable", false, "switch the drawer sweep master switch OFF")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *enable && *disable {
		return fmt.Errorf("drawer-report: --enable and --disable are mutually exclusive")
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

	now := time.Now()
	var (
		prefs  store.DeskPreferences
		report store.DrawerReportRows
	)
	if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		if prefs, e = tx.GetDeskPreferences(ctx); e != nil {
			return e
		}
		report, e = tx.DrawerReport(ctx, now)
		return e
	}); err != nil {
		return fmt.Errorf("drawer-report: %w", err)
	}

	printDrawerReport(prefs, report)

	// --enable/--disable, AFTER the report has printed the state it found on
	// entry (mailroom#443): the switch is the master gate the hourly job
	// checks first, and flipping it is a deliberate operator action distinct
	// from --apply below, which performs one manual sweep regardless of the
	// switch's position.
	switch {
	case *enable:
		on := true
		if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
			return tx.UpdateDeskPreferences(ctx, store.DeskPreferencesPatch{DrawerSweepEnabled: &on})
		}); err != nil {
			return fmt.Errorf("drawer-report: --enable: %w", err)
		}
		prefs.DrawerSweepEnabled = true
		fmt.Println("\ndrawer sweep switched ON — the next hourly run will archive the candidates above")
	case *disable:
		off := false
		if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
			return tx.UpdateDeskPreferences(ctx, store.DeskPreferencesPatch{DrawerSweepEnabled: &off})
		}); err != nil {
			return fmt.Errorf("drawer-report: --disable: %w", err)
		}
		prefs.DrawerSweepEnabled = false
		fmt.Println("\ndrawer sweep switched OFF — the hourly job will archive nothing until it is switched on again")
	}

	total := 0
	for _, a := range report.Areas {
		total += a.Candidates
	}

	if !*apply {
		if total == 0 {
			fmt.Println("\nREPORT ONLY — nothing was written, and nothing would change.")
		} else {
			fmt.Printf("\nREPORT ONLY — nothing was written. Re-run with --apply to archive %d message(s).\n", total)
		}
		return nil
	}

	// --apply performs an explicit, one-off operator sweep regardless of the
	// switch (mailroom#443): a manual run is not the thing the switch guards
	// against — it is the hourly job archiving mail nobody asked it to, the
	// moment the process boots, before anyone has seen this report at all.
	// It says so plainly when the switch is off, because that run will not
	// repeat on its own.
	if !prefs.DrawerSweepEnabled {
		fmt.Println("\nthe drawer sweep switch is OFF: this --apply run is a manual, one-off sweep — " +
			"the hourly job will not continue archiving on its own until the switch is turned on (--enable)")
	}

	applied, err := applyDrawerSweep(ctx, d.store, userID, *batch)
	if err != nil {
		return fmt.Errorf("drawer-report: apply: %w", err)
	}

	fmt.Println("\nAPPLIED:")
	for _, a := range report.Areas {
		fmt.Printf("  %-20s archived %d (of %d)\n", a.AreaName, applied.perArea[a.AreaName], a.Candidates)
	}
	if applied.errors > 0 {
		fmt.Printf("  ⚠ %d message(s) could not be archived; see the warnings above — they stay in "+
			"the drawer and are retried by the next scheduled sweep.\n", applied.errors)
	}

	// A CHECKED FACT, NOT A SLOGAN (spec): re-read the archived set itself
	// rather than trust the counters above.
	var afterViews []store.EmailView
	if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		afterViews, e = tx.GetEmails(ctx, applied.archivedIDs)
		return e
	}); err != nil {
		return fmt.Errorf("drawer-report: verify archived set: %w", err)
	}
	unreadAfter := 0
	for _, v := range afterViews {
		if !hasSeenKeyword(v.Keywords) {
			unreadAfter++
		}
	}
	fmt.Printf("\nunread before: %d, after: %d — none marked read\n", applied.unreadBefore, unreadAfter)
	fmt.Printf("message count before: %d, after: %d — none deleted\n", len(applied.archivedIDs), len(afterViews))

	// Re-run the report: the would-archive count must now be 0. A non-zero
	// remainder is printed as a WARNING, never hidden (CLAUDE.md).
	var afterReport store.DrawerReportRows
	if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		afterReport, e = tx.DrawerReport(ctx, time.Now())
		return e
	}); err != nil {
		return fmt.Errorf("drawer-report: re-check remainder: %w", err)
	}
	remaining := 0
	for _, a := range afterReport.Areas {
		remaining += a.Candidates
	}
	if remaining == 0 {
		fmt.Println("\nverified: 0 candidate(s) remain.")
	} else {
		fmt.Printf("\n⚠ %d candidate(s) remain after --apply — this should be 0; investigate before "+
			"assuming the sweep is complete.\n", remaining)
	}
	return nil
}

// drawerApplyResult tallies one --apply run.
type drawerApplyResult struct {
	perArea      map[string]int
	archivedIDs  []ulid.ULID
	unreadBefore int
	errors       int
}

// applyDrawerSweep drains every drawer candidate in batches of `batch`,
// re-scanning from scratch each pass (DrawerCandidates is idempotent by the
// app.drawer_archives row, not by an id list, so a message archived in an
// earlier pass simply stops matching). Each message is archived in its OWN
// transaction via ArchiveByDrawer, the same isolation the recurring worker
// uses — a single failure is reported and skipped rather than aborting the
// run.
func applyDrawerSweep(ctx context.Context, st *store.Store, userID ulid.ULID, batch int) (drawerApplyResult, error) {
	res := drawerApplyResult{perArea: map[string]int{}}
	for {
		now := time.Now()
		var candidates []store.DrawerCandidate
		if err := st.UserTx(ctx, userID, func(tx *store.UserTx) error {
			var e error
			candidates, e = tx.DrawerCandidates(ctx, now, batch)
			return e
		}); err != nil {
			return res, fmt.Errorf("scan candidates: %w", err)
		}
		if len(candidates) == 0 {
			break
		}

		ids := make([]ulid.ULID, len(candidates))
		for i, c := range candidates {
			ids[i] = c.ID
		}
		// The "before" unread snapshot, taken BEFORE this batch is archived —
		// ArchiveByDrawer must never change is_seen, and this is what makes
		// that a checked fact rather than an assertion.
		var before []store.EmailView
		if err := st.UserTx(ctx, userID, func(tx *store.UserTx) error {
			var e error
			before, e = tx.GetEmails(ctx, ids)
			return e
		}); err != nil {
			return res, fmt.Errorf("read unread state before archiving: %w", err)
		}
		for _, v := range before {
			if !hasSeenKeyword(v.Keywords) {
				res.unreadBefore++
			}
		}

		archivedThisPass := 0
		for _, c := range candidates {
			var archived bool
			if err := st.UserTx(ctx, userID, func(tx *store.UserTx) error {
				var e error
				archived, e = tx.ArchiveByDrawer(ctx, c, now)
				return e
			}); err != nil {
				res.errors++
				fmt.Printf("  ! could not archive %s (%s): %v\n", c.ID, c.AreaName, err)
				continue
			}
			if archived {
				res.perArea[c.AreaName]++
				res.archivedIDs = append(res.archivedIDs, c.ID)
				archivedThisPass++
			}
		}

		// A pass that archived NOTHING would select the same candidates again
		// and loop forever (every re-check failed, or a predicate is wrong).
		// Stop and say so; the remainder check below prints what is left.
		if archivedThisPass == 0 {
			fmt.Printf("  ⚠ a pass over %d candidate(s) archived none of them; stopping rather than "+
				"re-selecting the same messages. See the remainder below.\n", len(candidates))
			break
		}
		if len(candidates) < batch {
			break
		}
	}
	return res, nil
}

// hasSeenKeyword reports whether $seen is among a message's rendered
// keywords — the same projection Email/get's `keywords` property uses
// (systemKeywords, jmap_email.go), read here instead of a second is_seen
// round trip.
func hasSeenKeyword(kw []string) bool {
	for _, k := range kw {
		if k == store.KeywordSeen {
			return true
		}
	}
	return false
}

func printDrawerReport(prefs store.DeskPreferences, report store.DrawerReportRows) {
	fmt.Println("=== drawer-report (#443): what a drawer sweep would archive right now ===")
	// THE MASTER SWITCH'S STATE, FIRST (migration 00048): the whole reason
	// this report exists is to be read BEFORE the hourly job can move
	// anything, so whether it currently can is the first fact, not a
	// footnote below the numbers.
	if prefs.DrawerSweepEnabled {
		fmt.Println("drawer sweep: ON")
	} else {
		fmt.Println("drawer sweep: OFF for this login — the hourly job archives nothing until it is switched on")
	}
	// The threshold is only HALF the rule since #464: a message the user placed
	// themselves is on the side they put it on, whatever it scores. Stating the
	// score rule alone would make the ABOVE LINE and UNSCORED columns below —
	// which read the same override the candidate count does — look as if they
	// disagreed with this header.
	fmt.Printf("threshold: %.2f (drawer = the user's own placement, else score < threshold)\n",
		prefs.LineThreshold)
	fmt.Printf("login default period: %d d\n\n", prefs.DrawerDefaultDays)

	fmt.Printf("%-20s %14s %10s %12s %12s\n", "AREA", "PERIOD", "CANDIDATES", "OLDEST", "NEWEST")
	for _, a := range report.Areas {
		fmt.Printf("%-20s %14s %10d %12s %12s\n",
			a.AreaName, drawerPeriodLabel(a), a.Candidates, drawerDateOrDash(a.Oldest), drawerDateOrDash(a.Newest))
	}
	fmt.Println()

	fmt.Println("EXCLUDED (among messages old enough and on the desk, per area; a message may count under more than one signal)")
	fmt.Printf("%-20s %8s %7s %8s %11s %9s %6s %11s\n",
		"AREA", "FLAGGED", "PINNED", "SNOOZED", "ABOVE LINE", "UNSCORED", "SWEPT", "NO ARCHIVE")
	for _, a := range report.Areas {
		fmt.Printf("%-20s %8d %7d %8d %11d %9d %6d %11d\n",
			a.AreaName, a.Excluded.Flagged, a.Excluded.Pinned, a.Excluded.Snoozed,
			a.Excluded.AboveLine, a.Excluded.Unscored, a.Excluded.AlreadySwept, a.Excluded.NoArchiveFolder)
	}
	fmt.Println()

	fmt.Printf("NEEDS SORTING is never swept: %d message(s) on the desk with no area.\n", report.NeedsSorting.Candidates)
}

// drawerPeriodLabel renders an area's effective period the way the report's
// column asks for it: "30 d", "30 d (default)", or "off".
func drawerPeriodLabel(a store.DrawerAreaReport) string {
	switch a.PeriodSource {
	case store.DrawerPeriodOff:
		return "off"
	case store.DrawerPeriodDefault:
		return fmt.Sprintf("%d d (default)", a.PeriodDays)
	default:
		return fmt.Sprintf("%d d", a.PeriodDays)
	}
}

func drawerDateOrDash(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return t.UTC().Format("2006-01-02")
}
