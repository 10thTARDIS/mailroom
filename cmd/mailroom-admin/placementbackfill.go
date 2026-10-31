// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/config"
	"mailroom/internal/crypto"
	"mailroom/internal/ingest"
	"mailroom/internal/placement"
	"mailroom/internal/store"
	"mailroom/internal/syncrunner"
)

// `mailroom-admin placement-backfill` — apply LEARNED PLACEMENT to mail already
// on the desk (#714 increment 2a, ADR-032 addendum).
//
// REPORT FIRST. Without --apply it prints exactly what --apply would do and
// writes nothing: totals, moves per (now in -> learned) area pair, a line PER
// KEY (the sender address or domain, its learned area, how many messages move
// and where they are now, and how many it decided but skipped), and every
// abstention by reason. The report and the apply are one planner
// (store.PlanPlacementBackfill), and --apply hands the very plan it just
// printed to store.ApplyPlacementBackfill.
//
// EXACT, NOT AN UPPER BOUND. Every message the plan would move has its raw
// headers read from the blob store and judged by the arrival-time checks
// (ingest.BackfillSenders -> ingest.LearnedSender, then the store's
// senderRefusal): the trusted DMARC pass bound to the keyed From domain, the
// ambiguous-From refusal and the sender warnings. A message whose bytes cannot
// be read is skipped and counted. placement-report's simulation, which reads
// no headers, is the upper bound of this.
//
// An operator-only tool, so the per-key table prints sender addresses and
// domains. It never prints a subject or a body.
//
// --exclude ADDRESS-or-DOMAIN (repeatable) skips a key's mail; each entry's
// count is printed, 0 included, so a typo shows. --undo BATCH_ID reverses one
// applied batch exactly and skips any message filed since. With learned
// placement switched off, the report still runs and --apply refuses.
func runPlacementBackfill(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("placement-backfill", flag.ContinueOnError)
	var excludes stringList
	var (
		userIDStr = fs.String("user-id", "", "user id (ULID)")
		apply     = fs.Bool("apply", false, "perform the printed plan (default: report only)")
		undo      = fs.String("undo", "", "batch id (ULID): reverse one applied batch")
	)
	fs.Var(&excludes, "exclude", "sender address or domain to leave alone (repeatable)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	userID, err := parseUserID(*userIDStr)
	if err != nil {
		return err
	}
	if err := validateExcludes(excludes); err != nil {
		return err
	}
	if *undo != "" && (*apply || len(excludes) > 0) {
		return errors.New("placement-backfill: --undo takes no --apply or --exclude")
	}

	d, err := openDeps(ctx)
	if err != nil {
		return err
	}
	defer d.close()

	if *undo != "" {
		batch, err := ulid.Parse(*undo)
		if err != nil {
			return fmt.Errorf("--undo %q: not a valid ULID: %w", *undo, err)
		}
		res, err := d.store.UndoPlacementBackfill(ctx, userID, batch)
		if err != nil {
			return fmt.Errorf("placement-backfill: --undo: %w", err)
		}
		printPlacementBackfillUndo(os.Stdout, batch, res)
		return nil
	}

	blobs, err := openBlobStore(ctx, d)
	if err != nil {
		return err
	}
	master, err := d.keys.MasterKey(ctx)
	if err != nil {
		return fmt.Errorf("placement-backfill: master key: %w", err)
	}
	dek, err := crypto.DeriveKey(master, userID[:], syncrunner.MailBlobInfoV1)
	crypto.Zero(master)
	if err != nil {
		return fmt.Errorf("placement-backfill: derive mail dek: %w", err)
	}
	defer crypto.Zero(dek)
	trusted, err := trustedAuthservEntries()
	if err != nil {
		return err
	}

	var plan store.PlacementBackfillPlan
	var batches []store.PlacementBackfillBatch
	if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		plan, e = tx.PlanPlacementBackfill(ctx, store.PlacementBackfillOptions{
			Exclude: excludes,
			Senders: ingest.BackfillSenders(blobs, userID, dek, trusted),
		})
		if e != nil {
			return e
		}
		batches, e = tx.PlacementBackfillBatches(ctx)
		return e
	}); err != nil {
		return fmt.Errorf("placement-backfill: %w", err)
	}
	printPlacementBackfill(os.Stdout, plan, batches, trusted, *apply)

	if !*apply {
		return nil
	}
	if !plan.Enabled {
		return errors.New("placement-backfill: learned placement is switched OFF for this login; refusing --apply (the report above still describes what it would do)")
	}
	if plan.Accounted() != plan.Scanned {
		return fmt.Errorf("placement-backfill: the plan accounts for %d of %d scanned messages; refusing to apply a plan that does not add up", plan.Accounted(), plan.Scanned)
	}
	res, err := d.store.ApplyPlacementBackfill(ctx, userID, plan.Moves)
	printPlacementBackfillApplied(os.Stdout, plan, res)
	if err != nil {
		return fmt.Errorf("placement-backfill: --apply: %w", err)
	}
	return nil
}

// stringList is a repeatable string flag.
type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

// validateExcludes refuses an entry that cannot be an address or a domain.
func validateExcludes(in []string) error {
	for _, e := range in {
		v := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(e)), "@")
		if v == "" || strings.ContainsAny(v, " \t,") {
			return fmt.Errorf("--exclude %q: want one address or domain", e)
		}
		if at := strings.LastIndexByte(v, '@'); at >= 0 {
			if at == 0 || at == len(v)-1 {
				return fmt.Errorf("--exclude %q: not an address", e)
			}
			continue
		}
		if !strings.Contains(v, ".") {
			return fmt.Errorf("--exclude %q: not a domain", e)
		}
	}
	return nil
}

func nowInName(names map[ulid.ULID]string, id ulid.ULID) string {
	if id == (ulid.ULID{}) {
		return "(needs sorting)"
	}
	return areaName(names, id)
}

// backfillReasonOrder is every reason the plan can count, in print order, so
// a reason that never fired still prints as 0 (a bounded result reports its
// bound).
var backfillReasonOrder = []struct {
	r     placement.Reason
	label string
}{
	{placement.NoEvidence, "no evidence"},
	{placement.Thin, "thin"},
	{placement.Mixed, "mixed"},
	{placement.Contradicted, "contradicted"},
	{placement.NoKey, "no key"},
	{placement.DomainBlocked, "domain fallback blocked (freemail/own)"},
	{store.BackfillExcluded, "excluded (--exclude)"},
	{store.BackfillPlacedBefore, "placed by learned placement before"},
	{store.LearnedGated, "area stopped by its correction gate"},
	{store.BackfillUnreadable, "message bytes unreadable"},
	{placement.AmbiguousFrom, "ambiguous From"},
	{placement.Unauthenticated, "sender not authenticated"},
	{placement.SenderWarned, "sender warning"},
	{store.BackfillContested, "contested label"},
	{store.BackfillGone, "gone"},
}

// printPlacementBackfill renders the plan. Separate from the run so a unit
// test can read it without a database.
func printPlacementBackfill(w io.Writer, plan store.PlacementBackfillPlan,
	batches []store.PlacementBackfillBatch, trusted []string, apply bool) {
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }
	names := plan.AreaNames
	p("=== placement-backfill (#714): learned placement for mail already on the desk ===\n")
	if plan.Enabled {
		p("learned placement: ON\n")
	} else {
		p("learned placement: OFF for this login: --apply is refused; the plan below is what it would do\n")
	}
	if len(trusted) > 0 {
		p("authentication: a DMARC pass bound to the From domain, from a trusted authserv-id (%s)\n", strings.Join(trusted, ","))
	} else {
		p("authentication: NO trusted authserv-id configured (%s): mail whose authentication did not clearly fail\n",
			config.EnvAuthservIDs)
	}
	if plan.Exact {
		p("checks: EXACT. Every message below that moves had its own raw headers read and passed the arrival-time\n" +
			"        checks (From is one mailbox in one header, authenticated as above, no sender warning).\n")
	} else {
		p("checks: UPPER BOUND (no headers were read)\n")
	}
	p("evidence: %d filing(s) by the user into an area's own label\n\n", plan.Filings)

	p("scanned %d · held by a user/rule/learned label %d · off the desk %d · already in the learned area %d · move %d\n",
		plan.Scanned, plan.Held, plan.OffDesk, plan.NoOp, len(plan.Moves))
	abst := 0
	for _, n := range plan.Abstain {
		abst += n
	}
	p("not moved, by reason (%d):\n", abst)
	known := map[placement.Reason]bool{}
	for _, r := range backfillReasonOrder {
		known[r.r] = true
		p("  %-42s %6d\n", r.label, plan.Abstain[r.r])
	}
	var other []string
	for r, n := range plan.Abstain {
		if !known[r] {
			other = append(other, fmt.Sprintf("%s %d", r, n))
		}
	}
	sort.Strings(other)
	for _, o := range other {
		p("  %s\n", o)
	}
	p("accounted for %d of %d scanned\n", plan.Accounted(), plan.Scanned)
	if len(plan.Excluded) > 0 {
		var ex []string
		for e := range plan.Excluded {
			ex = append(ex, e)
		}
		sort.Strings(ex)
		p("--exclude:\n")
		for _, e := range ex {
			p("  %-42s %6d message(s)\n", e, plan.Excluded[e])
		}
	}

	p("\nMOVES BY AREA PAIR (%d)\n", len(plan.Moves))
	p("  %-20s %-20s %8s\n", "NOW IN", "LEARNED", "MESSAGES")
	pairs := make([]store.PlacementMove, 0, len(plan.Pairs))
	for m := range plan.Pairs {
		pairs = append(pairs, m)
	}
	sort.Slice(pairs, func(i, j int) bool {
		if plan.Pairs[pairs[i]] != plan.Pairs[pairs[j]] {
			return plan.Pairs[pairs[i]] > plan.Pairs[pairs[j]]
		}
		return pairs[i].From.String()+pairs[i].To.String() < pairs[j].From.String()+pairs[j].To.String()
	})
	for _, m := range pairs {
		p("  %-20s %-20s %8d\n", nowInName(names, m.From), areaName(names, m.To), plan.Pairs[m])
	}

	p("\nPER KEY (each line is exactly what --apply does for that key)\n")
	p("  %-7s %-40s %-14s %6s  %-34s %s\n", "KIND", "KEY", "LEARNED", "MOVES", "NOW IN", "SKIPPED")
	for _, k := range plan.Keys {
		p("  %-7s %-40s %-14s %6d  %-34s %s\n", k.Key.Kind, k.Key.Value, areaName(names, k.AreaID), k.Moves,
			nowInBreakdown(names, k.NowIn), skippedBreakdown(k.Skipped))
	}

	if len(batches) > 0 {
		p("\nAPPLIED BATCHES (reverse one with --undo BATCH_ID)\n")
		for _, b := range batches {
			p("  %s  %s  moved %d · undone %d\n", b.BatchID, b.At.UTC().Format("2006-01-02 15:04Z"), b.Moved, b.Undone)
		}
	}
	if !apply {
		p("\nREPORT ONLY: nothing was written. --apply performs exactly the moves above.\n")
	}
}

func nowInBreakdown(names map[ulid.ULID]string, m map[ulid.ULID]int) string {
	if len(m) == 0 {
		return "-"
	}
	type kv struct {
		name string
		n    int
	}
	var parts []kv
	for id, n := range m {
		parts = append(parts, kv{nowInName(names, id), n})
	}
	sort.Slice(parts, func(i, j int) bool {
		if parts[i].n != parts[j].n {
			return parts[i].n > parts[j].n
		}
		return parts[i].name < parts[j].name
	})
	out := make([]string, len(parts))
	for i, p := range parts {
		out[i] = fmt.Sprintf("%s %d", p.name, p.n)
	}
	return strings.Join(out, ", ")
}

func skippedBreakdown(m map[placement.Reason]int) string {
	if len(m) == 0 {
		return "-"
	}
	var out []string
	for r, n := range m {
		out = append(out, fmt.Sprintf("%s %d", r, n))
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

func printPlacementBackfillApplied(w io.Writer, plan store.PlacementBackfillPlan, res store.PlacementBackfillApplied) {
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }
	p("\nAPPLIED batch %s: moved %d of %d planned\n", res.BatchID, res.Moved, len(plan.Moves))
	var reasons []string
	for r, n := range res.Skipped {
		reasons = append(reasons, fmt.Sprintf("%s %d", r, n))
	}
	sort.Strings(reasons)
	if len(reasons) > 0 {
		p("  not moved after all (the state changed after the plan): %s\n", strings.Join(reasons, ", "))
	}
	mismatch := 0
	for _, k := range plan.Keys {
		if res.PerKey[k.Key] != k.Moves {
			mismatch++
		}
	}
	p("  keys whose written count differs from the plan: %d of %d\n", mismatch, len(plan.Keys))
	p("  undo with: mailroom-admin placement-backfill --user-id ... --undo %s\n", res.BatchID)
}

func printPlacementBackfillUndo(w io.Writer, batch ulid.ULID, res store.PlacementBackfillUndo) {
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }
	p("=== placement-backfill --undo %s ===\n", batch)
	p("journal: %d move(s)\n", res.Journal)
	for _, o := range []string{store.BackfillUndone, store.BackfillAlreadyUndone, store.BackfillRefiled, string(store.BackfillGone)} {
		p("  %-14s %6d\n", o, res.Outcomes[o])
	}
	p("(refiled: the user or a rule filed the message after the batch moved it; their decision stands)\n")
}
