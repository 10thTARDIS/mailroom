// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"flag"
	"fmt"
	"sort"
	"strings"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/areas"
	"mailroom/internal/store"
)

// `mailroom-admin materialize-area-labels` — the report-first backfill for
// #442 increment 3's "areas are labels": write the area's OWN label onto
// existing mail that only landed there because of a TAB VERDICT, so it
// carries something a user can see, search (`label:Updates`), or keep when
// the classifier changes its mind. area-report (increment 0) already answers
// "how would this plan resolve every message"; this answers the narrower,
// ACTIONABLE question "how many of those would gain a label I have not yet
// written", against the STORED plan rather than a --plan file — the desk's
// truth now lives in the database (store.UserTx.AreaConfig), not in a file an
// operator hands the binary.
//
// REPORT FIRST, --apply SECOND (CLAUDE.md). The default run writes nothing,
// in the exact register backfillmodelblobs.go uses, because "areas are
// labels" looked obviously right in increment 1's own design review and a
// backfill that silently relabels a mailbox by the wrong plan is exactly the
// shape of mistake that rule exists to catch. EVERY POPULATION IS A SEPARATE
// LINE WITH ITS OWN DENOMINATOR — never one total — because "0 labelled"
// cannot tell "nothing needed it" from "the plan matches nothing in this
// mailbox" apart, and those need opposite reactions from an operator.
func runMaterializeAreaLabels(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("materialize-area-labels", flag.ContinueOnError)
	var (
		userIDStr = fs.String("user-id", "", "operator user id (ULID)")
		apply     = fs.Bool("apply", false, "write the area labels (default: report only)")
		batch     = fs.Int("batch", 500, "message id page size for the keyset scan, and the apply transaction size")
		verbose   = fs.Bool("verbose", false, "list would-label message ids per area, not just the counts")
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

	plan, err := readStoredAreaPlan(ctx, d.store, userID)
	if err != nil {
		return fmt.Errorf("materialize-area-labels: %w", err)
	}
	if len(plan.cfg.Areas) == 0 {
		fmt.Println("stored plan: 0 areas — nothing to do")
		return nil
	}
	fmt.Printf("stored plan: %d area(s)\n\n", len(plan.cfg.Areas))

	stats, err := scanMaterializeStats(ctx, d.store, userID, plan, *batch)
	if err != nil {
		return fmt.Errorf("materialize-area-labels: scan messages: %w", err)
	}
	printMaterializeReport(plan, stats, *verbose)

	totalWouldLabel := stats.all.totalWouldLabel()
	if !*apply {
		if totalWouldLabel == 0 {
			fmt.Println("\nREPORT ONLY — nothing was written, and nothing would change.")
		} else {
			fmt.Printf("\nREPORT ONLY — nothing was written. Re-run with --apply to label %d message(s).\n", totalWouldLabel)
		}
		return nil
	}

	candidates := map[string]int{}
	for name, c := range stats.all.perArea {
		candidates[name] = c.wouldLabel
	}

	applied, err := applyMaterializeAreaLabels(ctx, d.store, userID, plan, *batch)
	if err != nil {
		return fmt.Errorf("materialize-area-labels: apply: %w", err)
	}

	fmt.Println("\nAPPLIED:")
	for _, a := range plan.cfg.Areas {
		fmt.Printf("  %-20s labelled %d (of %d candidate(s))\n", a.Name, applied[a.Name], candidates[a.Name])
	}

	// Re-walk at the end: the would-label count must now be 0. A non-zero
	// remainder is printed as a WARNING, never hidden — "what would I observe
	// if this were broken?" (CLAUDE.md).
	after, err := scanMaterializeStats(ctx, d.store, userID, plan, *batch)
	if err != nil {
		return fmt.Errorf("materialize-area-labels: re-walk after apply: %w", err)
	}
	remaining := after.all.totalWouldLabel()
	if remaining == 0 {
		fmt.Println("\nverified: 0 would-label message(s) remain.")
	} else {
		fmt.Printf("\n⚠ %d would-label message(s) remain after --apply — this should be 0; "+
			"investigate before assuming the backfill is complete.\n", remaining)
	}
	return nil
}

// areaPlan is the stored plan plus the two projections a report over it
// needs: the resolver Config itself, and — per area NAME — its own label's
// NAME (to compare against AreaReportRow.Labels, which carries names) and
// whether that label mirrors to IMAP (for the IMAP MIRRORING section).
type areaPlan struct {
	cfg          areas.Config
	ownLabelName map[string]string // area name -> own label name
	mirrors      map[string]bool   // area name -> own label MirrorToIMAP
	idByName     map[string]ulid.ULID
	nameByID     map[ulid.ULID]string
}

// readStoredAreaPlan reads the CALLER'S STORED areas (materialising the
// default plan on first contact, exactly as Area/get does) and the extra
// per-area facts this report needs beyond AreaConfig's own Go resolver shape.
func readStoredAreaPlan(ctx context.Context, st *store.Store, userID ulid.ULID) (areaPlan, error) {
	var (
		cfg      areas.Config
		idByName map[string]ulid.ULID
		views    []store.AreaView
	)
	if err := st.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		cfg, idByName, e = tx.AreaConfig(ctx)
		if e != nil {
			return e
		}
		views, e = tx.ListAreas(ctx)
		return e
	}); err != nil {
		return areaPlan{}, fmt.Errorf("read stored plan: %w", err)
	}

	labelIDs := make([]ulid.ULID, 0, len(views))
	for _, v := range views {
		labelIDs = append(labelIDs, v.LabelID)
	}
	var labels []store.LabelView
	if err := st.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		labels, e = tx.GetLabels(ctx, labelIDs)
		return e
	}); err != nil {
		return areaPlan{}, fmt.Errorf("read own labels: %w", err)
	}
	labelByID := make(map[ulid.ULID]store.LabelView, len(labels))
	for _, l := range labels {
		labelByID[l.ID] = l
	}

	plan := areaPlan{
		cfg:          cfg,
		ownLabelName: make(map[string]string, len(views)),
		mirrors:      make(map[string]bool, len(views)),
		idByName:     idByName,
		nameByID:     make(map[ulid.ULID]string, len(idByName)),
	}
	for _, v := range views {
		lv, ok := labelByID[v.LabelID]
		if !ok {
			continue // unreachable: GetLabels was asked for exactly these ids
		}
		plan.ownLabelName[v.Name] = lv.Name
		plan.mirrors[v.Name] = lv.MirrorToIMAP
	}
	for name, id := range idByName {
		plan.nameByID[id] = name
	}
	return plan, nil
}

// materializeCount is one area's tally within one population (INBOX or ALL
// MAIL): the three OUTCOMES a ByTab/ByLabel resolution can land in. Unlike
// area-report's areaCount, there is no "unsorted" here — that lives on
// materializePopulation itself, exactly as areaPopulation does, because it is
// not a per-area fact.
type materializeCount struct {
	wouldLabel   int // ByTab — the own label is necessarily absent; the actionable number
	already      int // ByLabel, decided by the AREA'S OWN label (a prior run, or the ingest hook)
	labelDecided int // ByLabel, decided by a DIFFERENT feed — untouched, nothing to backfill
}

// materializePopulation is one population's full tally, the same split
// areareport.go's areaPopulation draws between "the inbox" and "all mail".
type materializePopulation struct {
	total    int
	perArea  map[string]*materializeCount
	unsorted map[areas.Reason]int
	// wouldLabelIDs is populated only when the caller wants --verbose detail
	// (nil otherwise, which is the common, cheap case on a real mailbox).
	wouldLabelIDs map[string][]ulid.ULID
}

func newMaterializePopulation(cfg areas.Config, keepIDs bool) *materializePopulation {
	p := &materializePopulation{
		perArea:  make(map[string]*materializeCount, len(cfg.Areas)),
		unsorted: map[areas.Reason]int{},
	}
	for _, a := range cfg.Areas {
		p.perArea[a.Name] = &materializeCount{}
	}
	if keepIDs {
		p.wouldLabelIDs = map[string][]ulid.ULID{}
	}
	return p
}

func (p *materializePopulation) totalWouldLabel() int {
	n := 0
	for _, c := range p.perArea {
		n += c.wouldLabel
	}
	return n
}

// sameLabelName compares a message's label name against an area's own label
// name the way internal/areas.Resolve compares labels everywhere else:
// case-insensitively and trimmed, because a label written by hand will not
// reliably match byte-for-byte.
func sameLabelName(a, b string) bool {
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}

func hasLabelNamed(labels []string, name string) bool {
	for _, l := range labels {
		if sameLabelName(l, name) {
			return true
		}
	}
	return false
}

// record classifies one row into exactly one of the four populations the
// doc comment above names, resolving it with cfg.Resolve exactly as
// area-report does — the SAME resolver, so this report cannot disagree with
// the one that already shipped about which area a message falls into.
//
// ⚠ WHY "ALREADY" READS ByLabel, NOT ByTab, DESPITE THE SPEC PROSE SAYING
// "resolved ByTab and the own label already present". An area's OWN label is
// ITSELF one of its feeds (CreateArea always adds it, areas_resolve.go's
// areasArea folds every feed — own included — into Config's Labels), so the
// moment a message carries it, cfg.Resolve's labels-first rule matches it
// immediately and reports ByLabel, never ByTab. "ByTab with the own label
// present" is therefore unreachable through the SAME resolver every other
// consumer of Config trusts — measured directly: probing the literal spec
// wording here (checking hasLabelNamed on the ByTab branch instead) leaves
// "already" permanently at zero even for a row built to hit it, which is
// exactly CLAUDE.md's "probe your own tests" catching a bucket that can never
// fire. The reachable, mutually-exclusive split a ByLabel resolution can
// actually land in is: the deciding label IS the area's own (nothing to
// backfill — already there, however it got there) or it is a DIFFERENT label
// (a distinct feed already routes this mail; the own label is not part of
// the story at all). ByTab can now only mean "own label absent", so
// would-label needs no extra membership check either.
//
// THE TIE CASE: cfg.Resolve already picks the winner (earliest position) for
// a message whose labels feed two or more areas and reports Contested for
// the rest; this function only ever sees the winner, so a tie resolves like
// any other ByLabel row — already or label-decided, by the same rule above.
func (p *materializePopulation) record(cfg areas.Config, plan areaPlan, row store.AreaReportRow) {
	p.total++
	tab, tabKnown := row.ResolvedTab()
	res := cfg.Resolve(areas.Facts{Labels: row.Labels, Tab: tab, TabKnown: tabKnown})
	if res.Reason.Unsorted() {
		p.unsorted[res.Reason]++
		return
	}
	c := p.perArea[res.Area]
	if c == nil {
		// Unreachable through cfg.Resolve, which only ever names an area from
		// cfg itself — refusing loudly beats a silently dropped count if that
		// ever stops being true.
		c = &materializeCount{}
		p.perArea[res.Area] = c
	}
	switch res.Reason {
	case areas.ByLabel:
		if hasLabelNamed(row.Labels, plan.ownLabelName[res.Area]) {
			c.already++
		} else {
			c.labelDecided++
		}
	case areas.ByTab:
		c.wouldLabel++
		if p.wouldLabelIDs != nil {
			p.wouldLabelIDs[res.Area] = append(p.wouldLabelIDs[res.Area], row.ID)
		}
	}
}

// materializeStats holds the report's two populations, kept apart from the
// I/O that feeds them so the classification is testable without a database —
// the same split as areaStats (areareport.go).
type materializeStats struct {
	inbox *materializePopulation
	all   *materializePopulation
}

func newMaterializeStats(cfg areas.Config, keepIDs bool) *materializeStats {
	return &materializeStats{
		inbox: newMaterializePopulation(cfg, keepIDs),
		all:   newMaterializePopulation(cfg, keepIDs),
	}
}

func (s *materializeStats) record(cfg areas.Config, plan areaPlan, row store.AreaReportRow) {
	s.all.record(cfg, plan, row)
	if row.InInbox {
		s.inbox.record(cfg, plan, row)
	}
}

// scanMaterializeStats walks every live message (AreaReportRowsAfter,
// keyset-style, exactly as area-report does) and folds it into a fresh
// materializeStats. Called twice by a --apply run: once to print the report
// and size --apply's candidates, and once more afterward to verify the
// would-label count actually reached zero.
func scanMaterializeStats(ctx context.Context, st *store.Store, userID ulid.ULID, plan areaPlan, batch int) (*materializeStats, error) {
	stats := newMaterializeStats(plan.cfg, true)
	after := ulid.ULID{}
	for {
		var page []store.AreaReportRow
		if err := st.UserTx(ctx, userID, func(tx *store.UserTx) error {
			var e error
			page, e = tx.AreaReportRowsAfter(ctx, after, batch)
			return e
		}); err != nil {
			return nil, err
		}
		if len(page) == 0 {
			break
		}
		for _, row := range page {
			after = row.ID
			stats.record(plan.cfg, plan, row)
		}
		if len(page) < batch {
			break
		}
	}
	return stats, nil
}

// applyMaterializeAreaLabels walks the caller's live messages AGAIN and calls
// MaterializeAreaLabel per message, in transactions of `batch` messages —
// large enough that a mailbox of thousands does not open one transaction per
// row, small enough that a failure partway through loses at most one batch's
// progress, the same trade-off collapse-duplicates and backfill-alias make
// for their own per-group/per-batch transactions. Returns the per-area count
// of messages actually labelled, keyed by area NAME (plan.nameByID) so the
// caller can print it straight alongside `candidates`.
func applyMaterializeAreaLabels(ctx context.Context, st *store.Store, userID ulid.ULID, plan areaPlan, batch int) (map[string]int, error) {
	applied := map[string]int{}
	after := ulid.ULID{}
	batchNum := 0
	for {
		var page []store.AreaReportRow
		if err := st.UserTx(ctx, userID, func(tx *store.UserTx) error {
			var e error
			page, e = tx.AreaReportRowsAfter(ctx, after, batch)
			return e
		}); err != nil {
			return nil, fmt.Errorf("scan messages: %w", err)
		}
		if len(page) == 0 {
			break
		}
		batchNum++
		batchApplied := 0
		if err := st.UserTx(ctx, userID, func(tx *store.UserTx) error {
			for _, row := range page {
				areaID, ok, e := tx.MaterializeAreaLabel(ctx, row.ID)
				if e != nil {
					return fmt.Errorf("message %s: %w", row.ID, e)
				}
				if ok && areaID != nil {
					if name, known := plan.nameByID[*areaID]; known {
						applied[name]++
					}
					batchApplied++
				}
			}
			return nil
		}); err != nil {
			return nil, fmt.Errorf("batch %d: %w", batchNum, err)
		}
		fmt.Printf("  batch %d: %d message(s) scanned, %d labelled\n", batchNum, len(page), batchApplied)
		for _, row := range page {
			after = row.ID
		}
		if len(page) < batch {
			break
		}
	}
	return applied, nil
}

// unsortedMaterializeOrder mirrors areareport.go's unsortedOrder — the fixed
// print order for the three reasons, matching the order the spec states them
// in.
var unsortedMaterializeOrder = unsortedOrder

func printMaterializeReport(plan areaPlan, stats *materializeStats, verbose bool) {
	fmt.Println("=== materialize-area-labels (#442 increment 3): would this backfill label your existing mail? ===")
	fmt.Printf("messages scanned: %d  (all mail)\n", stats.all.total)
	fmt.Printf("in the inbox: %d\n\n", stats.inbox.total)

	fmt.Printf("%-20s %10s %10s %10s %14s\n", "AREA", "WOULD/IN", "WOULD/ALL", "ALREADY", "LABEL-DECIDED")
	for _, a := range plan.cfg.Areas {
		ic := stats.inbox.perArea[a.Name]
		ac := stats.all.perArea[a.Name]
		if ic == nil {
			ic = &materializeCount{}
		}
		if ac == nil {
			ac = &materializeCount{}
		}
		fmt.Printf("%-20s %10d %10d %10d %14d\n", a.Name, ic.wouldLabel, ac.wouldLabel, ac.already, ac.labelDecided)
		if verbose && len(stats.all.wouldLabelIDs[a.Name]) > 0 {
			ids := stats.all.wouldLabelIDs[a.Name]
			limit := len(ids)
			if limit > 20 {
				limit = 20
			}
			for _, id := range ids[:limit] {
				fmt.Printf("    would label  %s\n", id)
			}
			if len(ids) > limit {
				fmt.Printf("    ... and %d more\n", len(ids)-limit)
			}
		}
	}
	fmt.Println()

	fmt.Println("NEEDS SORTING (untouched by this command either way)")
	for _, r := range unsortedMaterializeOrder {
		fmt.Printf("  %-40s inbox=%-8d all=%-8d\n", r.String(), stats.inbox.unsorted[r], stats.all.unsorted[r])
	}
	fmt.Println()

	fmt.Println("IMAP MIRRORING")
	names := make([]string, 0, len(plan.cfg.Areas))
	for _, a := range plan.cfg.Areas {
		names = append(names, a.Name)
	}
	sort.Strings(names)
	for _, name := range names {
		ac := stats.all.perArea[name]
		if ac == nil {
			ac = &materializeCount{}
		}
		if plan.mirrors[name] {
			fmt.Printf("  %-20s own label %-24q mirrors to IMAP: yes  (%d would-label message(s))\n",
				name, plan.ownLabelName[name], ac.wouldLabel)
		} else {
			fmt.Printf("  %-20s own label %-24q mirrors to IMAP: no\n", name, plan.ownLabelName[name])
		}
	}
	fmt.Println("no IMAP writeback is enqueued by this command, mirroring or not — the desk's")
	fmt.Println("placement is Mailroom's own bookkeeping, and mirrors out later only if the")
	fmt.Println("user acts on the message (see area_labels.go's own doc comment).")
}
