// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/areas"
	"mailroom/internal/store"
	"mailroom/internal/tabs"
)

// `mailroom-admin area-report` — how every live message resolves into an AREA
// under a proposed plan, before the areas object mailroom#442 describes is
// ever written.
//
// THE REPORT BEFORE THE FEATURE, the same posture as `tabs` (#219) and every
// other increment-0 report this project ships (CLAUDE.md's "build the report
// before the fix"). An area partitions ALL mail, fed by category tabs and/or
// by labels, and an area plan is exactly the kind of thing that looks
// obviously right and changes only a handful of messages silently (#177) — or, worse
// here, leaves most of a mailbox in "needs sorting" because one tab was never
// given a home. This resolves every live message against the plan and counts
// so that is visible before anything depends on it, not discovered from a
// user staring at an empty area.
//
// PER-AREA COUNTS AND THE THREE UNSORTED REASONS SPLIT APART, never one
// total — the same reasoning `tabs` already states for its own two
// unresolved populations: "unlabelled", "labelled but the label maps
// nowhere", and "tab known but no area claims it" all look identical from a
// single count and have three DIFFERENT fixes (a classifier, a plan edit
// naming the label, a plan edit naming the tab). See internal/areas.Reason.
//
// READ-ONLY, ALWAYS. There is no --apply here: this command exists only to
// let an operator try a plan before mailroom#442 gives it anywhere to write.
func runAreaReport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("area-report", flag.ContinueOnError)
	var (
		userIDStr = fs.String("user-id", "", "operator user id (ULID)")
		planPath  = fs.String("plan", "", "area plan file (default: one area per category tab)")
		threshold = fs.Float64("threshold", 0.5, "triage score at/above which a message is 'surface'")
		batch     = fs.Int("batch", 500, "message id page size for the keyset scan")
		verbose   = fs.Bool("verbose", false, "print every contested tie pair rather than the top 10")
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
		labels      []store.LabelView
		labelCounts = map[string]int{}
	)
	if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		if labels, e = tx.ListLabels(ctx); e != nil {
			return e
		}
		for _, l := range labels {
			n, e := tx.CountLabelUsage(ctx, l.ID)
			if e != nil {
				return e
			}
			labelCounts[l.Name] = n
		}
		return nil
	}); err != nil {
		return fmt.Errorf("area-report: read labels: %w", err)
	}

	names := make([]string, len(labels))
	for i, l := range labels {
		names[i] = l.Name
	}
	cfg, lineErrs, err := buildAreaConfig(*planPath, names)
	if err != nil {
		return fmt.Errorf("area-report: %w", err)
	}
	if len(lineErrs) > 0 {
		for _, le := range lineErrs {
			fmt.Printf("  line %d: %s\n    %s\n", le.Line, le.Reason, le.Raw)
		}
		return fmt.Errorf("area-report: plan refused: %d line(s)", len(lineErrs))
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("area-report: invalid plan: %w", err)
	}

	stats := newAreaStats(cfg)
	after := ulid.ULID{}
	for {
		var page []store.AreaReportRow
		if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
			var e error
			page, e = tx.AreaReportRowsAfter(ctx, after, *batch)
			return e
		}); err != nil {
			return fmt.Errorf("area-report: scan messages: %w", err)
		}
		if len(page) == 0 {
			break
		}
		for _, row := range page {
			after = row.ID
			stats.record(row, cfg, float32(*threshold))
		}
		if len(page) < *batch {
			break
		}
	}

	printAreaReport(cfg, *planPath, *threshold, stats, labels, labelCounts, *verbose)
	return nil
}

// buildAreaConfig is the plan-or-default step, factored out so the choice of
// plan is testable without a database — the same split runMapLabelTabs and
// runUntangleLabels already use for their own plan files.
func buildAreaConfig(planPath string, known []string) (areas.Config, []areas.LineError, error) {
	if planPath == "" {
		return areas.Default(), nil, nil
	}
	f, err := os.Open(planPath)
	if err != nil {
		return areas.Config{}, nil, fmt.Errorf("open plan: %w", err)
	}
	cfg, lineErrs, err := areas.ParsePlan(f, known)
	// Closed explicitly rather than deferred, matching map-label-tabs: the file
	// is finished with here, and errcheck will not accept a bare deferred Close.
	_ = f.Close()
	if err != nil {
		return areas.Config{}, lineErrs, err
	}
	return cfg, lineErrs, nil
}

// areaCount is one area's tally within one population.
//
// byLabel + byTab == total ALWAYS: Resolve returns exactly one Reason
// (ByLabel or ByTab) for any message that lands in an area at all, so these
// two are a partition of total, not an independent measurement of it.
//
// surface + drawer + unscored == total ALWAYS too, for the same reason: the
// three-way switch in areaPopulation.record hits exactly one case per row.
// Both invariants are asserted directly in areareport_test.go rather than
// trusted, because a report whose parts do not sum to its whole is one a
// reader cannot act on (CLAUDE.md).
type areaCount struct {
	total, surface, drawer, unscored int
	byLabel, byTab                   int
}

// areaPopulation is one population's (inbox, or all mail) full tally.
type areaPopulation struct {
	total, scored, unscored int
	perArea                 map[string]*areaCount
	// unsorted counts each of internal/areas' three UnsortedXxx reasons
	// SEPARATELY — collapsing them would hide which of three opposite fixes
	// applies, exactly the reason tabStats never merges "unlabelled" and
	// "labelledUnmapped" (category_tabs.go).
	unsorted map[areas.Reason]int
	// contested counts, per "Winner over Loser" pair, how often the label
	// tie-break in internal/areas.Resolve actually had to decide something —
	// the identical measurement tabStats takes of tabs.FromLabels' own
	// tie-break, and the identical reason: a rule nobody measures is a rule
	// nobody can correct.
	contested      map[string]int
	contestedTotal int
}

func newAreaPopulation(cfg areas.Config) *areaPopulation {
	p := &areaPopulation{
		perArea:   make(map[string]*areaCount, len(cfg.Areas)),
		unsorted:  map[areas.Reason]int{},
		contested: map[string]int{},
	}
	for _, a := range cfg.Areas {
		p.perArea[a.Name] = &areaCount{}
	}
	return p
}

// record folds one message's resolution into this population. threshold is
// passed in rather than read from a field so the same population type never
// needs to know where its threshold came from.
func (p *areaPopulation) record(res areas.Resolution, row store.AreaReportRow, threshold float32) {
	p.total++
	if row.Scored {
		p.scored++
	} else {
		p.unscored++
	}

	if res.Reason.Unsorted() {
		p.unsorted[res.Reason]++
		return
	}

	ac := p.perArea[res.Area]
	if ac == nil {
		// Unreachable through internal/areas.Resolve, which only ever returns an
		// area name drawn from the same Config this population was built from —
		// but failing loudly here beats a message silently dropped into no row
		// at all if that ever stops being true.
		ac = &areaCount{}
		p.perArea[res.Area] = ac
	}
	ac.total++
	switch res.Reason {
	case areas.ByLabel:
		ac.byLabel++
	case areas.ByTab:
		ac.byTab++
	}
	switch {
	case row.Scored && row.Score >= threshold:
		ac.surface++
	case row.Scored && row.Score < threshold:
		ac.drawer++
	default:
		ac.unscored++
	}

	if len(res.Contested) > 0 {
		p.contestedTotal++
		key := res.Area + " over " + strings.Join(res.Contested, "+")
		p.contested[key]++
	}
}

// areaStats holds the report's two populations, kept apart from the I/O that
// feeds them so the counting is testable without a database — the same split
// as tabStats (tabs.go) and functionSignalStats before it.
type areaStats struct {
	inbox *areaPopulation
	all   *areaPopulation
}

func newAreaStats(cfg areas.Config) *areaStats {
	return &areaStats{inbox: newAreaPopulation(cfg), all: newAreaPopulation(cfg)}
}

// record resolves one row under cfg and folds it into both populations — ALL
// MAIL always, and INBOX only when the row actually has a live provenance row
// there. Every message that reaches inbox also reaches all, so the inbox
// population is a strict subset of all — asserted in areareport_test.go.
func (s *areaStats) record(row store.AreaReportRow, cfg areas.Config, threshold float32) {
	tab, tabKnown := row.ResolvedTab()
	res := cfg.Resolve(areas.Facts{Labels: row.Labels, Tab: tab, TabKnown: tabKnown})
	s.all.record(res, row, threshold)
	if row.InInbox {
		s.inbox.record(res, row, threshold)
	}
}

// unsortedOrder is the fixed print order for the three reasons, matching the
// order the spec states them in: the classifier's workload first, then the
// two mapping gaps.
var unsortedOrder = []areas.Reason{
	areas.UnsortedNoLabelNoVerdict,
	areas.UnsortedLabelsFeedNothing,
	areas.UnsortedTabFeedsNothing,
}

// unsortedFix is the one-line remedy printed beside each reason's count —
// deliberately in words, not a command line, because the three fixes are of
// different KINDS (write a classifier vs. edit a plan file) and a single
// imperative would flatten that difference.
var unsortedFix = map[areas.Reason]string{
	areas.UnsortedNoLabelNoVerdict:  "the classifier's workload",
	areas.UnsortedLabelsFeedNothing: "add the label to an area, one plan edit",
	areas.UnsortedTabFeedsNothing:   "give the tab an area in the plan",
}

func printAreaReport(
	cfg areas.Config,
	planPath string,
	threshold float64,
	stats *areaStats,
	labels []store.LabelView,
	labelCounts map[string]int,
	verbose bool,
) {
	fmt.Println("=== areas (#442 increment 0): how every live message resolves into areas under this plan ===")
	fmt.Printf("messages scanned: %d  (all mail)\n", stats.all.total)
	fmt.Printf("in the inbox: %d\n", stats.inbox.total)
	fmt.Printf("threshold: %.2f (surface = score >= threshold)\n", threshold)
	if planPath == "" {
		fmt.Println("plan: default (one area per tab)")
	} else {
		fmt.Printf("plan: %s\n", planPath)
	}
	fmt.Println()

	fmt.Println("PLAN")
	for _, a := range cfg.Areas {
		feeds := make([]string, 0, len(a.Tabs)+len(a.Labels))
		for _, t := range a.Tabs {
			feeds = append(feeds, "tab:"+string(t))
		}
		for _, l := range a.Labels {
			feeds = append(feeds, "label:"+l)
		}
		fmt.Printf("  %-16s %s\n", a.Name, strings.Join(feeds, ", "))
	}
	fmt.Println()

	printAreaPopulation("INBOX (unactioned mail)", cfg, stats.inbox, verbose)
	printAreaPopulation("ALL MAIL", cfg, stats.all, verbose)

	printNotInAnyArea(cfg, labels, labelCounts)

	fmt.Println("read-only: nothing was written.")
	fmt.Println("These counts measure volume at this threshold, not whether the score puts the")
	fmt.Println("right messages on the surface.")
}

func printAreaPopulation(title string, cfg areas.Config, p *areaPopulation, verbose bool) {
	pct := func(n int) float64 {
		if p.total == 0 {
			return 0
		}
		return 100 * float64(n) / float64(p.total)
	}

	fmt.Println(title)
	fmt.Printf("  %-16s %8s %7s   %8s %8s %8s   %8s %8s\n",
		"AREA", "MSGS", "%", "SURFACE", "DRAWER", "UNSCORED", "BY LABEL", "BY TAB")
	for _, a := range cfg.Areas {
		c := p.perArea[a.Name]
		if c == nil {
			c = &areaCount{}
		}
		fmt.Printf("  %-16s %8d %6.1f%%   %8d %8d %8d   %8d %8d\n",
			a.Name, c.total, pct(c.total), c.surface, c.drawer, c.unscored, c.byLabel, c.byTab)
	}
	for _, r := range unsortedOrder {
		n := p.unsorted[r]
		fmt.Printf("  %-16s %8d %6.1f%%   (%s)\n", "Needs sorting:", n, pct(n), r.String()+" -> "+unsortedFix[r])
	}
	fmt.Println()

	if p.contestedTotal == 0 {
		fmt.Println("  ties: the tie rule was never invoked — no message carried labels feeding two or more areas.")
	} else {
		fmt.Printf("  ties: %d message(s) carried labels feeding two or more areas:\n", p.contestedTotal)
		keys := make([]string, 0, len(p.contested))
		for k := range p.contested {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool {
			if p.contested[keys[i]] != p.contested[keys[j]] {
				return p.contested[keys[i]] > p.contested[keys[j]]
			}
			return keys[i] < keys[j]
		})
		limit := len(keys)
		if !verbose && limit > 10 {
			limit = 10
		}
		for _, k := range keys[:limit] {
			fmt.Printf("    %-40s %6d\n", k, p.contested[k])
		}
		if !verbose && len(keys) > limit {
			fmt.Printf("    ... and %d more pair(s); rerun with --verbose to see all\n", len(keys)-limit)
		}
	}
	fmt.Println()
}

// unfedLabelLines splits the labels no area feeds DIRECTLY into the two
// populations a reader has to tell apart, and formats each as a report line.
//
// THE SPLIT IS THE POINT. On the first real run the section listed
// a large label ("Promotions N msgs") under NOT IN ANY AREA, which reads as N messages with
// no home — but every one of them reaches the Promotions area through the
// label's tab. A label that maps to a tab the plan feeds is a plan that is
// merely indirect, not a gap; a label with no tab, or whose tab no area feeds,
// is the real gap, and its mail lands wherever the classifier's verdict sends
// it. Listing both under one heading is the #177 shape: a number that sends
// the operator to fix a non-problem. So: `viaTab` lines say which area the
// label's mail already reaches and how; `none` lines are the gap.
func unfedLabelLines(cfg areas.Config, labels []store.LabelView, labelCounts map[string]int) (viaTab, none []string) {
	fedTabArea := map[tabs.Tab]string{}
	fedLabels := map[string]bool{}
	for _, a := range cfg.Areas {
		for _, t := range a.Tabs {
			fedTabArea[t] = a.Name
		}
		for _, l := range a.Labels {
			fedLabels[strings.ToLower(strings.TrimSpace(l))] = true
		}
	}

	var unfed []store.LabelView
	for _, l := range labels {
		if !fedLabels[strings.ToLower(strings.TrimSpace(l.Name))] {
			unfed = append(unfed, l)
		}
	}
	sort.Slice(unfed, func(i, j int) bool {
		if labelCounts[unfed[i].Name] != labelCounts[unfed[j].Name] {
			return labelCounts[unfed[i].Name] > labelCounts[unfed[j].Name]
		}
		return unfed[i].Name < unfed[j].Name
	})

	for _, l := range unfed {
		if l.Tab != "" {
			if area, ok := fedTabArea[l.Tab]; ok {
				viaTab = append(viaTab, fmt.Sprintf("    %-24s %6d msgs   reaches %s via tab:%s",
					l.Name, labelCounts[l.Name], area, l.Tab))
				continue
			}
			none = append(none, fmt.Sprintf("    %-24s %6d msgs   mapped to tab:%s, which no area feeds",
				l.Name, labelCounts[l.Name], l.Tab))
			continue
		}
		none = append(none, fmt.Sprintf("    %-24s %6d msgs   no tab: lands where the classifier's verdict sends it",
			l.Name, labelCounts[l.Name]))
	}
	return viaTab, none
}

// printNotInAnyArea names every label and tab the plan leaves unclaimed, so a
// gap in the plan is visible even for areas/tabs with zero mail today — and
// separates labels that reach an area indirectly from labels that do not
// (unfedLabelLines).
func printNotInAnyArea(cfg areas.Config, labels []store.LabelView, labelCounts map[string]int) {
	fedTabs := map[tabs.Tab]bool{}
	for _, a := range cfg.Areas {
		for _, t := range a.Tabs {
			fedTabs[t] = true
		}
	}
	viaTab, none := unfedLabelLines(cfg, labels, labelCounts)

	var unfedTabs []tabs.Tab
	for _, t := range tabs.All {
		if !fedTabs[t] {
			unfedTabs = append(unfedTabs, t)
		}
	}

	fmt.Println("NOT IN ANY AREA")
	if len(viaTab) == 0 && len(none) == 0 && len(unfedTabs) == 0 {
		fmt.Println("  every label and every tab feeds an area.")
		fmt.Println()
		return
	}
	if len(none) == 0 {
		fmt.Println("  labels in no area: none.")
	} else {
		fmt.Println("  labels in no area (their mail lands wherever the verdict sends it):")
		for _, line := range none {
			fmt.Println(line)
		}
	}
	if len(viaTab) > 0 {
		fmt.Println("  labels not fed directly, but reaching an area through their tab (not a gap):")
		for _, line := range viaTab {
			fmt.Println(line)
		}
	}
	if len(unfedTabs) == 0 {
		fmt.Println("  tabs: none — every tab feeds an area.")
	} else {
		names := make([]string, len(unfedTabs))
		for i, t := range unfedTabs {
			names[i] = string(t)
		}
		fmt.Printf("  tabs: %s\n", strings.Join(names, ", "))
	}
	fmt.Println()
}
