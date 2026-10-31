// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"sort"
	"strings"

	"mailroom/internal/crypto"
	"mailroom/internal/kind"
	"mailroom/internal/mime"
	"mailroom/internal/store"
	"mailroom/internal/syncrunner"
)

// runKindSweep answers the question the kind layer's thresholds have never been
// answerable from: what do decideMin and decideMargin actually cost and buy, on
// real mail? (mailroom#175)
//
// WHY THIS EXISTS. The weights and thresholds in internal/kind are hand-set, and
// hand-tuning them by intuition has been tried twice and made things measurably
// worse both times (47% unknown -> 55% -> 57%). The lesson from #157 is that a
// scoring change is only trustworthy when a report over real mail says so — the
// priority weights became answerable the moment `rescore` printed a histogram.
// This is that histogram for kinds.
//
// WHAT IT MEASURES, and why not accuracy. Kinds are NOT labels: they are
// internal machinery that BINDS to the user's own labels (#175 is explicit that
// kinds must never be user-facing categories). So "did the kind match the label"
// is the wrong question. The right one is whether a label's hand-labelled mail
// concentrates on ONE kind strongly enough to offer a binding — which is exactly
// two numbers:
//
//	coverage — how much of the label's mail got any verdict at all;
//	purity   — how much of THAT agreed on one kind.
//
// A label is bindable when both clear their thresholds. Maximising bindable
// labels is the honest objective, because a binding is the only thing the kind
// layer actually delivers to a user.
//
// THE LEADER TABLE IS THE OTHER HALF. Sweeping thresholds cannot surface a kind
// whose signals never fire, and those two failures look identical in a
// distribution (both read as Unknown). So the sweep also reports which kind LEADS
// with the thresholds removed entirely. A kind that never leads at zero is
// missing signals, and no threshold in the grid will ever help it.
//
// Writes nothing.
func runKindSweep(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("kind-sweep", flag.ContinueOnError)
	var (
		userIDStr   = fs.String("user-id", "", "operator user id (ULID)")
		limit       = fs.Int("limit", 5000, "how many messages to scan")
		minCoverage = fs.Float64("min-coverage", 0.6, "fraction of a label's mail that must get a verdict for it to count as bindable")
		minPurity   = fs.Float64("min-purity", 0.8, "fraction of the decided mail that must agree on one kind")
		explain     = fs.String("explain", "", "print every fired signal and score for messages carrying this label")
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

	blobs, err := openBlobStore(ctx, d)
	if err != nil {
		return err
	}
	master, err := d.keys.MasterKey(ctx)
	if err != nil {
		return fmt.Errorf("master key: %w", err)
	}
	dek, err := crypto.DeriveKey(master, userID[:], syncrunner.MailBlobInfoV1)
	crypto.Zero(master)
	if err != nil {
		return fmt.Errorf("derive mail dek: %w", err)
	}
	defer crypto.Zero(dek)

	var (
		candidates []store.RulePreviewCandidate
		total      int
		labelName  = map[string]string{}
	)
	if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		if candidates, e = tx.RulePreviewCandidates(ctx, *limit); e != nil {
			return e
		}
		if total, e = tx.CountPreviewableMessages(ctx); e != nil {
			return e
		}
		views, e := tx.ListLabels(ctx)
		if e != nil {
			return e
		}
		for _, v := range views {
			labelName[v.ID.String()] = v.Name
		}
		return nil
	}); err != nil {
		return err
	}

	// SCAN ONCE. Scores are all a threshold sweep needs, so the blobs are opened
	// and parsed a single time however large the grid is.
	var (
		rows            []scored
		parsed, failed  int
		leaderIgnoring  = map[kind.Kind]int{}
		labelTotals     = map[string]int{}
		orderedLabelSet = map[string]bool{}
	)
	for _, c := range candidates {
		raw, err := readMailBlob(ctx, blobs, userID, dek, c.ContentHash)
		if err != nil {
			failed++
			continue
		}
		msg, err := mime.Parse(bytes.NewReader(raw))
		if err != nil || msg == nil {
			failed++
			continue
		}
		parsed++

		facts := factsFromHeader(msg)
		_ = d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
			if sent, _, e := tx.OutboundCountForAddress(ctx, facts.FromAddress); e == nil {
				facts.HasCorresponded = sent > 0
			}
			return nil
		})

		v := kind.Classify(facts)
		// WHY A PER-MESSAGE EXPLAIN EXISTS. Twice now a weight has been changed on
		// the strength of arithmetic done in someone's head about which rules
		// "must" be firing, and twice the mailbox disagreed. The scores are right
		// there in the Verdict; printing them costs nothing and removes the
		// guessing entirely.
		if *explain != "" && labelledWith(c, labelName, *explain) {
			fmt.Printf("\n  %-30s %s\n", trunc(facts.FromAddress, 30), trunc(facts.Subject, 44))
			fmt.Printf("    verdict %s (leader %s, score %.2f, margin %.2f)\n",
				v.Kind, v.Leader, v.Score, v.Margin)
			fmt.Printf("    scores  %s\n", scoreLine(v.Scores))
			for _, s := range v.Signals {
				fmt.Printf("      + %-24s %s\n", s.Name, scoreLine(s.Deltas))
			}
		}
		row := scored{scores: v.Scores}
		for _, lid := range c.UserLabelIDs {
			if n, ok := labelName[lid.String()]; ok {
				row.labels = append(row.labels, n)
				labelTotals[n]++
				orderedLabelSet[n] = true
			}
		}
		rows = append(rows, row)

		// Thresholds removed entirely: which kind WOULD have won.
		_, leader, score, _ := kind.Decide(v.Scores, 0, 0)
		if score <= 0 {
			leader = kind.Unknown // nothing scored at all — not a near miss
		}
		leaderIgnoring[leader]++
	}

	fmt.Printf("scanned %d of %d messages (%d parsed, %d unreadable)\n",
		len(candidates), total, parsed, failed)
	fmt.Printf("bindable = coverage >= %.0f%% AND purity >= %.0f%%\n\n",
		*minCoverage*100, *minPurity*100)

	// ---- the leader table --------------------------------------------------
	fmt.Println("LEADING KIND WITH THE THRESHOLDS REMOVED  (min=0, margin=0)")
	fmt.Println("  A kind that never leads here has NO SIGNALS that fire, and no threshold")
	fmt.Println("  in the sweep below can ever surface it. That is a signals problem.")
	for _, k := range allKinds() {
		n := leaderIgnoring[k]
		pct := 0.0
		if parsed > 0 {
			pct = 100 * float64(n) / float64(parsed)
		}
		flag := ""
		if n == 0 && k != kind.Unknown {
			flag = "   <-- never leads: signals missing, not thresholds"
		}
		fmt.Printf("  %-14s %5d  %5.1f%%%s\n", k, n, pct, flag)
	}

	// ---- the grid ----------------------------------------------------------
	mins := []float64{0.5, 1.0, 1.5, 2.0, 2.5, 3.0}
	margins := []float64{0.0, 0.25, 0.5, 0.75, 1.0, 1.5}

	fmt.Printf("\nTHRESHOLD SWEEP — unknown%%, and how many of the %d labels become bindable\n",
		len(orderedLabelSet))
	fmt.Printf("  %-6s", "min\\mgn")
	for _, mg := range margins {
		fmt.Printf("  %10.2f", mg)
	}
	fmt.Println()

	type best struct {
		min, margin float64
		bindable    int
		unknownPct  float64
	}
	var bestCell best
	bestCell.bindable = -1
	bestCell.unknownPct = 1e9

	for _, mn := range mins {
		fmt.Printf("  %-6.2f", mn)
		for _, mg := range margins {
			unknownPct, bindable, _ := evaluate(rows, mn, mg, labelTotals, *minCoverage, *minPurity)
			fmt.Printf("  %4.0f%%/%-4d", unknownPct, bindable)
			// Prefer more bindable labels; break ties on the LOWER unknown rate.
			//
			// This used to break ties toward the stricter setting, reasoning that a
			// wrong kind costs more than an Unknown. That is true of the THRESHOLD
			// CHOICE and false of this report: it made the tool announce 2.00/0.00
			// at 38.6% unknown as "best" while 1.50/0.00 sat in the same grid at
			// 19% with the same two bindable labels, and I read that as a
			// regression that had not happened. A summary that hides the better
			// cell is worse than no summary.
			if bindable > bestCell.bindable ||
				(bindable == bestCell.bindable && unknownPct < bestCell.unknownPct) {
				bestCell = best{min: mn, margin: mg, bindable: bindable, unknownPct: unknownPct}
			}
		}
		fmt.Println()
	}
	fmt.Println("  (cells read: unknown% / bindable labels)")

	// ---- per-label detail, current vs best ---------------------------------
	names := make([]string, 0, len(orderedLabelSet))
	for n := range orderedLabelSet {
		names = append(names, n)
	}
	sort.Strings(names)

	for _, setting := range []struct {
		label       string
		min, margin float64
	}{
		{"SHIPPED (decideMin 1.5, decideMargin 0.75)", 1.5, 0.75},
		{fmt.Sprintf("BEST IN SWEEP (min %.2f, margin %.2f)", bestCell.min, bestCell.margin), bestCell.min, bestCell.margin},
	} {
		unknownPct, bindable, detail := evaluate(rows, setting.min, setting.margin, labelTotals, *minCoverage, *minPurity)
		fmt.Printf("\n%s — unknown %.1f%%, %d bindable\n", setting.label, unknownPct, bindable)
		for _, n := range names {
			dd := detail[n]
			mark := "  "
			if dd.bindable {
				mark = " *"
			}
			fmt.Printf("%s %-14s %2d labelled  cov %3.0f%%  pur %3.0f%%  COLLATERAL %4d  prec >=%3.0f%%  -> %s\n",
				mark, n, labelTotals[n], dd.coverage*100, dd.purity*100,
				dd.Collateral, dd.Precision*100, dd.dominant)
		}
	}
	fmt.Println("\n  * = bindable. A binding is the only thing the kind layer delivers to a user,")
	fmt.Println("    so this count — not the unknown rate — is what a weight change must improve.")
	return nil
}

// scored is one message reduced to what a threshold sweep needs: its per-kind
// scores (the thresholds are applied later, many times) and the labels the USER
// applied to it, which are the only ground truth available.
type scored struct {
	scores map[kind.Kind]float64
	labels []string
}

// labelDetail is one label's binding evidence at a given threshold setting.
type labelDetail struct {
	coverage float64
	purity   float64
	dominant kind.Kind
	// Collateral is how many messages the binding would ALSO catch — every
	// message of the dominant kind that the user did not put under this label.
	//
	// WITHOUT THIS COLUMN THE WHOLE REPORT IS UNREADABLE, and it was missing.
	// purity is P(kind | label): "of this label's mail, how much agreed". The
	// rule a binding actually ships is the other direction — kind => apply label
	// — whose error rate is 1 - P(label | kind). Promotions read "coverage 77%,
	// purity 100%, BINDABLE" while `promotional` was the verdict on a third of
	// the mailbox, so the rule it endorsed would have labelled hundreds of
	// messages the user never touched. rules.Induce has scored exactly this since
	// #168 and calls it Collateral; this report simply dropped it.
	Collateral int
	// Precision is covers/(covers+collateral) — and it is a LOWER BOUND, not an
	// estimate. Unlabelled mail is not known-negative: the user labelled only a
	// small share of messages, so most collateral is unjudged rather than wrong.
	Precision float64
	bindable  bool
}

// evaluate re-decides every cached score at one threshold setting and reports
// the mailbox-wide unknown rate plus each label's binding evidence. It calls
// kind.Decide, so a swept cell is the decision the serve path would make.
func evaluate(
	rows []scored,
	min, margin float64,
	labelTotals map[string]int,
	minCoverage, minPurity float64,
) (unknownPct float64, bindable int, detail map[string]labelDetail) {
	detail = map[string]labelDetail{}
	byLabelKind := map[string]map[kind.Kind]int{}
	byKindTotal := map[kind.Kind]int{}
	unknown := 0

	for _, r := range rows {
		decided, _, _, _ := kind.Decide(r.scores, min, margin)
		if decided == kind.Unknown {
			unknown++
		}
		byKindTotal[decided]++
		for _, n := range r.labels {
			if byLabelKind[n] == nil {
				byLabelKind[n] = map[kind.Kind]int{}
			}
			byLabelKind[n][decided]++
		}
	}
	if len(rows) > 0 {
		unknownPct = 100 * float64(unknown) / float64(len(rows))
	}

	for name, counts := range byLabelKind {
		total := labelTotals[name]
		if total == 0 {
			continue
		}
		decided, topN := 0, 0
		top := kind.Unknown
		// Iterated over allKinds() rather than the map, so a tie resolves the same
		// way on every run. Ranging a Go map here made the reported dominant kind
		// flip between runs on a 3-3 split — in a package whose own suite asserts
		// TestClassificationIsDeterministic.
		for _, k := range allKinds() {
			n := counts[k]
			if k == kind.Unknown || n == 0 {
				continue
			}
			decided += n
			if n > topN {
				topN, top = n, k
			}
		}
		d := labelDetail{dominant: top}
		d.coverage = float64(decided) / float64(total)
		if decided > 0 {
			d.purity = float64(topN) / float64(decided)
		}
		d.Collateral = byKindTotal[top] - topN
		if topN+d.Collateral > 0 {
			d.Precision = float64(topN) / float64(topN+d.Collateral)
		}
		d.bindable = d.coverage >= minCoverage && d.purity >= minPurity
		if d.bindable {
			bindable++
		}
		detail[name] = d
	}
	return unknownPct, bindable, detail
}

func allKinds() []kind.Kind {
	return []kind.Kind{kind.Personal, kind.Transactional, kind.Promotional,
		kind.Notification, kind.List, kind.Automated, kind.Unknown}
}

// labelledWith reports whether a candidate carries a label of this name.
func labelledWith(c store.RulePreviewCandidate, names map[string]string, want string) bool {
	for _, lid := range c.UserLabelIDs {
		if strings.EqualFold(names[lid.String()], want) {
			return true
		}
	}
	return false
}

// scoreLine renders a per-kind score map compactly, non-zero entries only, in a
// fixed order so two messages can be compared line by line.
func scoreLine(m map[kind.Kind]float64) string {
	parts := make([]string, 0, len(m))
	for _, k := range allKinds() {
		if v, ok := m[k]; ok && v != 0 {
			parts = append(parts, fmt.Sprintf("%s %+.2f", k, v))
		}
	}
	if len(parts) == 0 {
		return "(none)"
	}
	return strings.Join(parts, "  ")
}
