// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strings"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/config"
	"mailroom/internal/placement"
	"mailroom/internal/store"
)

// `mailroom-admin placement-report` — what LEARNED PLACEMENT knows and has done
// (#714, ADR-032). READ-ONLY: it prints counts and writes nothing.
//
// Learned placement acts at ingest with no shadow period (the operator's
// choice), so this report is how anyone sees whether it is working: the keys
// it is confident about, every placement's outcome, each area's precision with
// the bottom of its Wilson interval and whether the correction gate has
// stopped it, why keys abstain, and what applying it to EXISTING mail would
// move. Counts only: no addresses, names or subjects, so the output can be
// pasted into an issue (CLAUDE.md portability).
//
// --reset-gate AREA_ID is the one write, after the report has printed: the
// operator's re-enable for an area the gate stopped. It starts the gate's
// count afresh from now; no outcome is rewritten.
//
// There is no --apply here: applying learned placement to existing mail is
// `placement-backfill` (#714 increment 2a), which runs the SAME planner with
// every mover's raw headers checked. The simulation below is its upper bound.
func runPlacementReport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("placement-report", flag.ContinueOnError)
	var (
		userIDStr = fs.String("user-id", "", "operator user id (ULID)")
		resetGate = fs.String("reset-gate", "", "area id (ULID): re-enable an area its correction gate stopped, after printing the report")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	userID, err := parseUserID(*userIDStr)
	if err != nil {
		return err
	}
	var resetArea *ulid.ULID
	if *resetGate != "" {
		id, err := ulid.Parse(*resetGate)
		if err != nil {
			return fmt.Errorf("--reset-gate %q: not a valid ULID: %w", *resetGate, err)
		}
		resetArea = &id
	}

	d, err := openDeps(ctx)
	if err != nil {
		return err
	}
	defer d.close()

	var rep store.PlacementReport
	if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		rep, e = tx.PlacementReport(ctx)
		return e
	}); err != nil {
		return fmt.Errorf("placement-report: %w", err)
	}
	trusted, err := trustedAuthservEntries()
	if err != nil {
		return err
	}
	printPlacementReport(os.Stdout, rep, trusted)

	if resetArea != nil {
		var found bool
		if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
			var e error
			found, e = tx.ResetPlacementGate(ctx, *resetArea)
			return e
		}); err != nil {
			return fmt.Errorf("placement-report: --reset-gate: %w", err)
		}
		if !found {
			return fmt.Errorf("placement-report: --reset-gate: no area %s for this user", *resetArea)
		}
		fmt.Printf("\ngate reset for area %s: it counts only placements decided from now on\n", *resetArea)
	}
	return nil
}

// printPlacementReport renders the report. Separate from the run so a test can
// read it without a database.
//
// trusted is MAILROOM_AUTHSERV_IDS as this process reads it; it decides which
// authentication rule applies on arrival, and the report says which.
func printPlacementReport(w io.Writer, rep store.PlacementReport, trusted []string) {
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }
	p("=== placement-report (#714): learned placement ===\n")
	if rep.Enabled {
		p("learned placement: ON\n")
	} else {
		p("learned placement: OFF for this login (nothing is placed at ingest)\n")
	}
	if len(trusted) > 0 {
		p("authentication: on arrival, acts only on a DMARC pass from a trusted authserv-id (%s)\n", strings.Join(trusted, ","))
	} else {
		p("authentication: NO trusted authserv-id configured (%s); on arrival, acts on mail whose authentication did not clearly fail\n",
			config.EnvAuthservIDs)
	}
	p("evidence: %d filing(s) by the user into an area's own label\n", rep.Filings)
	p("rules: act on >= %d agreeing filings (a domain also needs >= %d addresses); kept after %s or on archive;\n",
		placement.MinFilings, placement.MinDomainAddresses, placement.KeptAfter)
	p("       gate stops an area with >= %d resolved and a 95%% Wilson lower bound < %.2f\n\n",
		placement.GateMinResolved, placement.GateMinLCB)

	name := map[ulid.ULID]string{}
	for _, a := range rep.Areas {
		name[a.AreaID] = a.Name
	}

	p("%-20s %9s %9s %6s %6s %9s %10s %7s  %s\n",
		"AREA", "KEYS:ADDR", "KEYS:DOM", "OPEN", "KEPT", "CORRECTED", "PRECISION", "LCB95", "GATE")
	for _, a := range rep.Areas {
		p("%-20s %9d %9d %6d %6d %9d %10s %7s  %s\n",
			a.Name, a.ConfidentAddresses, a.ConfidentDomains,
			a.Counts.Open, a.Counts.Kept, a.Counts.Corrected,
			placementRatio(a.Gate.Precision), placementRatio(a.Gate.LCB), gateWord(a))
		if c := a.Counts; c.BackfillOpen+c.BackfillKept+c.BackfillCorrected > 0 {
			p("%-20s %9s %9s %6d %6d %9d %10s %7s  %s\n",
				"  (backfill)", "", "", c.BackfillOpen, c.BackfillKept, c.BackfillCorrected, "-", "-", "not gated")
		}
	}
	withdrawn := 0
	for _, a := range rep.Areas {
		withdrawn += a.Counts.Withdrawn
	}
	p("withdrawn by an API key (neither kept nor corrected, never gated): %d\n", withdrawn)
	p("(OPEN/KEPT/CORRECTED, PRECISION, LCB95 and the gate are ARRIVAL placements, since the area's gate reset\n" +
		" for the last three. A \"(backfill)\" row is placement-backfill's outcomes: shown, never gated; a\n" +
		" correction of one still blocks its sender's key.)\n\n")

	p("KEYS BY VERDICT\n")
	for _, kind := range []placement.KeyKind{placement.KeyAddress, placement.KeyDomain} {
		r := rep.KeyReasons[kind]
		p("  %-8s confident %d · thin %d · mixed %d · contradicted %d · domain fallback blocked (freemail/own) %d\n",
			kind, r[placement.Confident], r[placement.Thin], r[placement.Mixed], r[placement.Contradicted],
			r[placement.DomainBlocked])
	}
	p("\n")

	p("PLACEMENTS BY ORIGIN (outcome as of now)\n")
	for _, o := range []struct {
		name string
		m    map[string]int
	}{{"arrival", rep.Origins.Arrival}, {"backfill", rep.Origins.Backfill}} {
		p("  %-9s open %d · kept %d · corrected %d · withdrawn by a key %d\n", o.name,
			o.m[store.PlacementOpen], o.m[store.PlacementKept], o.m[store.PlacementCorrected],
			o.m[store.PlacementWithdrawn])
	}
	if len(rep.Batches) > 0 {
		p("  backfill batches:\n")
		for _, b := range rep.Batches {
			p("    %s  %s  moved %d · undone %d\n", b.BatchID, b.At.UTC().Format("2006-01-02 15:04Z"), b.Moved, b.Undone)
		}
	}
	p("\n")

	p("BACKFILL SIMULATION (existing mail; REPORT ONLY; placement-backfill is the exact report and its --apply)\n")
	p("  scanned %d · held by a user/rule/learned label %d · already in the learned area %d · off the desk %d\n",
		rep.Scanned, rep.Held, rep.NoOp, rep.OffDesk)
	p("  skipped: placed before %d · gated %d · contested %d\n",
		rep.Abstain[store.BackfillPlacedBefore], rep.Abstain[store.LearnedGated], rep.Abstain[store.BackfillContested])
	p("  abstain: none %d · thin %d · mixed %d · contradicted %d · no key %d · domain fallback blocked (freemail/own) %d\n",
		rep.Abstain[placement.NoEvidence], rep.Abstain[placement.Thin], rep.Abstain[placement.Mixed],
		rep.Abstain[placement.Contradicted], rep.Abstain[placement.NoKey], rep.Abstain[placement.DomainBlocked])
	p("  abstain: sender not authenticated %d · sender warning %d\n",
		rep.Abstain[placement.Unauthenticated], rep.Abstain[placement.SenderWarned])
	p("  (an UPPER BOUND on authentication: existing mail is judged by its stored auth_fail only. On arrival a\n"+
		"   message is also refused for an ambiguous From (reason \"%s\": two From headers, two mailboxes or a\n"+
		"   group), and needs a trusted DMARC pass bound to its From domain when authserv-ids are configured,\n"+
		"   and a self-spoof check. Those read the raw headers, which this report does not;\n"+
		"   `placement-backfill` does, and its count is exact.)\n", placement.AmbiguousFrom)
	moves := make([]store.PlacementMove, 0, len(rep.Moves))
	total := 0
	for m, n := range rep.Moves {
		moves = append(moves, m)
		total += n
	}
	sort.Slice(moves, func(i, j int) bool {
		if rep.Moves[moves[i]] != rep.Moves[moves[j]] {
			return rep.Moves[moves[i]] > rep.Moves[moves[j]]
		}
		return moves[i].From.String()+moves[i].To.String() < moves[j].From.String()+moves[j].To.String()
	})
	p("  would move %d desk message(s):\n", total)
	p("  %-20s %-20s %8s\n", "NOW IN", "LEARNED", "MESSAGES")
	for _, m := range moves {
		from := "(needs sorting)"
		if m.From != (ulid.ULID{}) {
			from = areaName(name, m.From)
		}
		p("  %-20s %-20s %8d\n", from, areaName(name, m.To), rep.Moves[m])
	}
	p("\nREPORT ONLY: nothing was written.\n")
}

func areaName(names map[ulid.ULID]string, id ulid.ULID) string {
	if n, ok := names[id]; ok {
		return n
	}
	return id.String()
}

func placementRatio(f float64) string {
	if math.IsNaN(f) {
		return "-"
	}
	return fmt.Sprintf("%.3f", f)
}

func gateWord(a store.PlacementReportArea) string {
	switch {
	case a.Gate.Stopped:
		return fmt.Sprintf("STOPPED (%d resolved; re-enable with --reset-gate %s)", a.Gate.Resolved, a.AreaID)
	case !a.Gate.Judged:
		return fmt.Sprintf("acting (%d/%d resolved, not yet judged)", a.Gate.Resolved, placement.GateMinResolved)
	default:
		return "acting"
	}
}
