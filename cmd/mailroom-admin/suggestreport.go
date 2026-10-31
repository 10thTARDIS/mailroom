// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"flag"
	"fmt"
	"sort"

	"mailroom/internal/rules"
	"mailroom/internal/store"
)

// `suggest-report` — how many label suggestions the RULE INDUCER could actually
// produce for unlabelled mail (#242).
//
// WHY THIS IS A REPORT AND NOT THE FEATURE. The suggestion surface
// (`app.label_suggestions`, `Email.suggestedLabels`, the needsCategory filter)
// is shipped, wired, tested — and produces ZERO rows, because `classifyMessage`
// returns immediately when no label model is promoted and #169's measured
// verdict is that none should be. #242 decided to source suggestions from the
// inducer instead, which is the mechanism that demonstrably works: 66 induced
// rules at zero collateral against a model measured at 0.20–0.60 precision.
//
// But "it ran and changed nothing" is a live possibility here BY CONSTRUCTION,
// and that is the failure this project has paid for most often. Accepted rules
// already ran at ingest, so every unlabelled message is one that no existing
// rule matched. Whether newly induced candidates reach any of them is an open
// empirical question, and the answer could be zero. Build the report before the
// fix (CLAUDE.md); report per-label counts, never one total.
//
// It writes nothing. There is no --apply, because there is nothing to apply
// yet — that is the point.

func runSuggestReport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("suggest-report", flag.ContinueOnError)
	var (
		userIDStr = fs.String("user-id", "", "operator user id (ULID)")
		limit     = fs.Int("limit", 5000, "how many messages to scan as evidence")
		minCovers = fs.Int("min-covers", 2, "only count candidates explaining at least this many of your own labellings")
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

	var (
		candidates []store.RulePreviewCandidate
		total      int
		labels     []store.LabelView
	)
	if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		if candidates, e = tx.RulePreviewCandidates(ctx, *limit); e != nil {
			return e
		}
		if total, e = tx.CountPreviewableMessages(ctx); e != nil {
			return e
		}
		labels, e = tx.ListLabels(ctx)
		return e
	}); err != nil {
		return err
	}

	// A BOUNDED SCAN REPORTS ITS BOUND (#155). A lower bound reads as a total.
	complete := len(candidates) >= total
	fmt.Printf("evidence : %d of %d messages scanned%s\n", len(candidates), total,
		map[bool]string{true: "", false: "  (TRUNCATED — raise --limit)"}[complete])

	// The unlabelled remainder IS the workload. Every message here is one no
	// accepted rule matched at ingest, which is what makes "can induction reach
	// them?" a real question rather than a rhetorical one.
	type unlabelledMsg struct {
		facts   rules.MessageFacts
		from    string
		subject string
	}
	var unlabelled []unlabelledMsg
	for _, c := range candidates {
		if len(c.UserLabelIDs) == 0 {
			unlabelled = append(unlabelled, unlabelledMsg{
				facts:   factsFromCandidate(c),
				from:    c.From,
				subject: c.Subject,
			})
		}
	}
	fmt.Printf("workload : %d unlabelled (%d labelled)\n", len(unlabelled), len(candidates)-len(unlabelled))
	fmt.Printf("counting : candidates with conflicts 0, covers >= %d — collateral is NOT a filter here\n\n",
		*minCovers)
	if len(unlabelled) == 0 {
		fmt.Printf("nothing unlabelled in the scanned window — no suggestion has anywhere to go.\n")
		return nil
	}

	names := map[string]string{}
	for _, l := range labels {
		names[l.ID.String()] = l.Name
	}
	sort.Slice(labels, func(i, j int) bool { return labels[i].Name < labels[j].Name })

	type labelRow struct {
		name       string
		positives  int
		candidates int
		usable     int
		reached    int // unlabelled messages at least one usable candidate matches
	}
	var rowsOut []labelRow
	// hitsPerMessage counts how many DISTINCT labels would suggest for each
	// unlabelled message — the ambiguity a suggestion surface has to render.
	hitsPerMessage := make(map[int]map[string]bool, len(unlabelled))

	for _, l := range labels {
		target := l.ID.String()
		examples := make([]rules.Example, 0, len(candidates))
		positives := 0
		for _, c := range candidates {
			ex := rules.Example{Facts: factsFromCandidate(c)}
			for _, lid := range c.UserLabelIDs {
				if lid.String() == target {
					ex.Positive = true
					continue
				}
				if n, ok := names[lid.String()]; ok {
					ex.OtherUserLabels = append(ex.OtherUserLabels, n)
				}
			}
			if ex.Positive {
				positives++
			}
			examples = append(examples, ex)
		}
		row := labelRow{name: l.Name, positives: positives}
		if positives == 0 {
			rowsOut = append(rowsOut, row)
			continue
		}

		induced := rules.Induce(examples)
		row.candidates = len(induced)

		// MATCH THROUGH THE REAL EVALUATOR, never a second matcher written here.
		// #155 settled that for Rule/preview: a preview that can disagree with
		// the engine is worse than none, and the same applies to a suggestion
		// that claims a rule would have caught a message.
		reached := map[int]bool{}
		for _, c := range induced {
			// ⚠ DO NOT FILTER ON COLLATERAL HERE. It is tempting — `accept-rules`
			// does exactly that, with `--max-collateral 0` — and here it makes the
			// whole report VACUOUS BY CONSTRUCTION.
			//
			// Collateral counts the NON-POSITIVE messages a candidate matches, and
			// every unlabelled message is non-positive. So "collateral <= 0" means
			// "matches no unlabelled message", and REACHED is then forced to 0 for
			// every label, for every corpus, forever. The first run of this report
			// on a real mailbox printed exactly that — zero reached — and it
			// read as a finding about induction rather than as a tautology about
			// the filter.
			//
			// The two commands mean opposite things by the same number. For
			// `accept-rules`, collateral is the blast radius of a rule about to be
			// CREATED, and zero is the safe choice. Here, collateral IS THE
			// SUGGESTION POPULATION: a candidate that reaches unlabelled mail has
			// collateral > 0 by definition, and that is the entire point.
			//
			// Conflicts stay disqualifying — a conflict is mail the user filed
			// under a DIFFERENT label by hand, so suggesting this one contradicts
			// them rather than merely surprising them.
			//
			// THE BAR IS suggestFilter's, NOT A SECOND COPY OF IT (#298 item 3).
			// This was an inline `c.Conflicts > 0 || c.Covers < *minCovers` — the
			// same predicate suggestFilter already applies, written out a second
			// time. The test that claimed to guard it could not fail (it compared
			// two compile-time constants), and runSuggestReport has no test
			// callers, so the copy was unpinned in both directions at once: the
			// bar could drift from its sibling, or regrow the `|| c.Collateral > 0`
			// term that made this report's first real run vacuous, and nothing
			// would go red. Calling the shared function means the sibling's tests
			// — which ARE sound — now cover this site too.
			if suggestFilter(c, *minCovers) != suggestKept {
				continue
			}
			row.usable++
			for i, m := range unlabelled {
				if !candidateMatches(c.Conditions, l.Name, m.facts) {
					continue
				}
				reached[i] = true
				if hitsPerMessage[i] == nil {
					hitsPerMessage[i] = map[string]bool{}
				}
				hitsPerMessage[i][l.Name] = true
			}
		}
		row.reached = len(reached)
		rowsOut = append(rowsOut, row)
	}

	fmt.Printf("%-24s %10s %11s %8s %9s\n", "LABEL", "POSITIVES", "CANDIDATES", "USABLE", "REACHED")
	for _, r := range rowsOut {
		fmt.Printf("%-24s %10d %11d %8d %9d\n",
			truncate(r.name, 24), r.positives, r.candidates, r.usable, r.reached)
	}

	// THE ANSWER, and the numbers that decide whether #242 is worth building.
	var covered, ambiguous int
	for i := range unlabelled {
		switch len(hitsPerMessage[i]) {
		case 0:
		case 1:
			covered++
		default:
			covered++
			ambiguous++
		}
	}
	fmt.Printf("\n%d of %d unlabelled messages would receive at least one rule-derived suggestion",
		covered, len(unlabelled))
	if len(unlabelled) > 0 {
		fmt.Printf(" (%d%%)", 100*covered/len(unlabelled))
	}
	fmt.Printf(".\n")
	fmt.Printf("%d of those would receive suggestions for MORE THAN ONE label.\n", ambiguous)

	// THE REMAINDER IS NOT A ROUNDING ERROR — it is the population the inducer
	// structurally cannot serve, and naming it is the difference between "the
	// feature works" and "the feature works for the mail it can reach".
	fmt.Printf("\n%d unlabelled messages would receive NOTHING.\n", len(unlabelled)-covered)
	fmt.Printf("  That is the honest limit of this mechanism, not a gap to tune away:\n")
	fmt.Printf("  induction needs at least %d of YOUR OWN labellings sharing a condition,\n", *minCovers)
	fmt.Printf("  so a sender you have never filed can produce no candidate at all. Those\n")
	fmt.Printf("  messages abstain into the needs-a-category surface, which is the design.\n")

	// A SAMPLE OF WHAT GETS NOTHING, because a count alone cannot tell "senders
	// you have genuinely never filed" from "a defect in how these facts are
	// built". Naming a few makes the remainder inspectable instead of a number
	// to be believed. Capped, and the cap is stated.
	shown := 0
	for i, m := range unlabelled {
		if len(hitsPerMessage[i]) > 0 {
			continue
		}
		if shown == 0 {
			fmt.Printf("\n  a sample of them (first %d of %d, in scan order):\n", 5, len(unlabelled)-covered)
		}
		fmt.Printf("    %-38s  %s\n", truncate(m.from, 38), truncate(m.subject, 56))
		if shown++; shown >= 5 {
			break
		}
	}

	if covered == 0 {
		fmt.Printf("\n  ⚠ NOTHING WOULD BE SUGGESTED — and before believing it, check the FILTER\n")
		fmt.Printf("    rather than the mechanism. A collateral bound of 0 forces this number to\n")
		fmt.Printf("    0 on every corpus by construction, because collateral counts exactly the\n")
		fmt.Printf("    non-positive messages a candidate reaches. No such bound is applied here;\n")
		fmt.Printf("    if an edit reintroduces one, this is the result it produces.\n")
		fmt.Printf("\n    If the filter is genuinely clear: accepted rules already ran at ingest, so\n")
		fmt.Printf("    every message above is one no existing rule matched; if induction over\n")
		fmt.Printf("    the same labels reaches none of them either, sourcing suggestions from\n")
		fmt.Printf("    the inducer would ship a surface that stays empty. That is a finding\n")
		fmt.Printf("    about #242's premise, and it should change the plan rather than the\n")
		fmt.Printf("    thresholds.\n")
	}
	return nil
}

// candidateMatches reports whether an induced candidate would fire on a
// message, THROUGH THE REAL EVALUATOR rather than a second matcher written
// here. #155 settled that for `Rule/preview` — a preview that can disagree with
// the engine is worse than none — and a suggestion claiming "this rule would
// have caught your message" makes exactly that claim.
//
// ⚠ THE SILENT-ZERO TRAP THIS FUNCTION EXISTS TO MAKE TESTABLE. Everything the
// report prints flows through here. Build the probe rule slightly wrong — omit
// `Enabled`, name an action type the evaluator does not know — and `Evaluate`
// returns nothing for every message, so the report prints zeros across the
// board and those zeros read as a FINDING ("induction reaches nothing") rather
// than as a broken measurement. That is the exact failure this project has paid
// for repeatedly, so the mechanism is a named function with a test and a
// negative control instead of four lines inlined in a loop.
func candidateMatches(c rules.Condition, label string, f rules.MessageFacts) bool {
	probe := []rules.Rule{{
		Name:       "suggest-probe",
		Conditions: c,
		Actions:    []rules.Action{{Type: "addLabel", Label: label}},
		Enabled:    true,
	}}
	return len(rules.Evaluate(probe, f)) > 0
}
