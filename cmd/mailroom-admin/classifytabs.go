// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"flag"
	"fmt"
	"sort"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
	"mailroom/internal/tabs"
)

// `mailroom-admin classify-tabs` — fill in the stored tab verdicts for mail that
// already exists (#228, migration 00029, ADR-022).
//
// Ingest stamps every NEW message with `tabs.Classify`'s answer and the rule-list
// version that produced it. This is the mail that arrived before that, plus any
// row whose stamped version is stale because the rules have since changed.
//
// ⚠ WHAT IT WRITES IS THE CLASSIFIER'S VERDICT, NOT A TAB. A message's tab stays
// derived — COALESCE(mapping_tab, classified_tab) — with the mapping half read
// live, so nothing here decides where labelled mail goes and re-running this can
// never move it.
//
// WHY IT MATTERS THAT THIS RUNS AT ALL, in one sentence: a message with no
// stored verdict and no mapped label is INVISIBLE to a tab filter. Not
// mis-filed — absent. So until this has run over a mailbox, `Email/query?inTab`
// answers only from the label mapping, and a tab strip built on it would show a
// confidently incomplete list.
//
// DRY RUN BY DEFAULT, and the report is PER TAB AND PER RULE rather than one
// total. A single "N classified" cannot distinguish a healthy spread from
// everything landing in one tab, and #219's own history is the argument: the
// kind aggregate returns `personal` on 0.0% of this mailbox, so a Primary built
// on it would have shipped EMPTY while the summary count looked fine.
func runClassifyTabs(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("classify-tabs", flag.ContinueOnError)
	var (
		userIDStr = fs.String("user-id", "", "operator user id (ULID)")
		apply     = fs.Bool("apply", false, "write the verdicts (default: dry-run, report only)")
		batch     = fs.Int("batch", 500, "message id page size for the keyset scan")
	)
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

	// The population that needs work, read BEFORE anything is written so the
	// report can state what it set out to do as well as what it did.
	var staleBefore, missingBefore int
	if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		staleBefore, missingBefore, e = tx.MessagesNeedingClassification(ctx, tabs.ClassifierVersion)
		return e
	}); err != nil {
		return fmt.Errorf("count messages needing classification: %w", err)
	}

	var (
		scanned int
		changed int
		same    int
		byTab   = map[tabs.Tab]int{}
		byRule  = map[string]int{}
	)
	after := ulid.ULID{}
	for {
		var page []store.MessageTabFacts
		if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
			var e error
			page, e = tx.MessageTabFactsAfter(ctx, after, *batch)
			return e
		}); err != nil {
			return fmt.Errorf("scan message tab facts: %w", err)
		}
		if len(page) == 0 {
			break
		}
		for _, m := range page {
			after = m.ID
			scanned++
			// The SAME classifier over the SAME facts the serve path reads — one
			// implementation, one fact projection, so a backfill cannot derive a
			// different answer than live delivery would have.
			v := tabs.Classify(m.Facts)
			byTab[v.Tab]++
			byRule[v.Rule]++
			if !*apply {
				continue
			}
			if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
				wrote, e := tx.StoreClassifiedTab(ctx, m.ID, v.Tab, tabs.ClassifierVersion)
				if e != nil {
					return e
				}
				if wrote {
					changed++
				} else {
					same++
				}
				return nil
			}); err != nil {
				return fmt.Errorf("store verdict for %s: %w", m.ID, err)
			}
		}
		if len(page) < *batch {
			break
		}
	}

	mode := "DRY RUN (nothing written)"
	if *apply {
		mode = "APPLIED"
	}
	fmt.Printf("=== classify-tabs: %s ===\n", mode)
	fmt.Printf("  classifier version                 : %d\n", tabs.ClassifierVersion)
	fmt.Printf("  messages scanned                   : %d\n", scanned)
	fmt.Printf("  needed work before this run        : %d missing, %d stale\n",
		missingBefore, staleBefore)
	if *apply {
		fmt.Printf("  rows written                       : %d\n", changed)
		fmt.Printf("  rows already current               : %d\n", same)
	}
	fmt.Println()

	pct := func(n int) float64 {
		if scanned == 0 {
			return 0
		}
		return 100 * float64(n) / float64(scanned)
	}
	fmt.Println("VERDICTS BY TAB")
	for _, t := range tabs.All {
		fmt.Printf("  %-12s %6d  (%5.1f%%)\n", t, byTab[t], pct(byTab[t]))
	}
	fmt.Println("\n  This is the classifier's opinion of EVERY message, including mail whose tab")
	fmt.Println("  the label mapping actually decides — so it is not the tab distribution a")
	fmt.Println("  user would see. `mailroom-admin tabs` reports that.")
	fmt.Println()

	fmt.Println("VERDICTS BY RULE")
	names := make([]string, 0, len(byRule))
	for n := range byRule {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		if byRule[names[i]] != byRule[names[j]] {
			return byRule[names[i]] > byRule[names[j]]
		}
		return names[i] < names[j]
	})
	for _, n := range names {
		fmt.Printf("  %-24s %6d  (%5.1f%%)\n", n, byRule[n], pct(byRule[n]))
	}
	if n := byRule[tabs.RuleNone]; n > 0 {
		fmt.Printf("\n  ⚠ %q fired on %d messages — the rule list is supposed to be total.\n",
			tabs.RuleNone, n)
	}

	if *apply {
		var staleAfter, missingAfter int
		if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
			var e error
			staleAfter, missingAfter, e = tx.MessagesNeedingClassification(ctx, tabs.ClassifierVersion)
			return e
		}); err != nil {
			return fmt.Errorf("re-count after apply: %w", err)
		}
		fmt.Printf("\nAFTER: %d missing, %d stale\n", missingAfter, staleAfter)
		if missingAfter+staleAfter > 0 {
			// Not an error — a message can arrive mid-run — but it must be SAID,
			// because a tab filter silently omits whatever is left.
			fmt.Println("  Some rows still need work. A message that arrived mid-run is the")
			fmt.Println("  ordinary cause; anything larger means the scan did not reach them, and")
			fmt.Println("  until it does those messages are invisible to a tab filter.")
		}
	} else {
		fmt.Println("\nRe-run with -apply to write the verdicts.")
	}
	return nil
}
