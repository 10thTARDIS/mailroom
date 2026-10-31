// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"errors"
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

// `mailroom-admin area-plan` — load an area plan through the ONE place that
// can, until the desk client exists (v1.66.0, #442).
//
// Increments 1-2 gave areas a store and a JMAP surface; nothing yet lets an
// operator SET one except hand-written Area/set calls. This reads the same
// plan file format area-report already accepts (`areas.ParsePlan`) and
// reports the DIFF against the STORED areas, then applies it with --apply —
// the same report-first shape as untangle-labels and map-label-tabs, which
// make the identical argument for why the plan lives in a file the operator
// supplies rather than compiled into the binary (portability, and the
// second user's plan is not the first user's).
//
// THE DIFF, NOT A REPLACE: an area named in both the plan and the store is
// updated in place (feeds diffed, position reset to the plan's line order);
// an area the plan names that is NOT stored is created; and — the
// deliberately conservative choice — an area that IS stored but the plan
// does not mention is KEPT, giving up only a feed the plan assigns to some
// other area (the default plan already holds every tab, so without that the
// first real plan could never be applied). This command never deletes an
// area on the operator's behalf; Area/set (or a future desk client) is how
// you retire one.
func runAreaPlan(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("area-plan", flag.ContinueOnError)
	var (
		userIDStr = fs.String("user-id", "", "operator user id (ULID)")
		planPath  = fs.String("plan", "", "area plan file (areas.ParsePlan format; see area-report --plan)")
		apply     = fs.Bool("apply", false, "write the diff (default: report only)")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	userID, err := parseUserID(*userIDStr)
	if err != nil {
		return err
	}
	if *planPath == "" {
		return fmt.Errorf("area-plan: --plan FILE is required")
	}

	d, err := openDeps(ctx)
	if err != nil {
		return err
	}
	defer d.close()

	var (
		stored        []store.AreaView
		labels        []store.LabelView
		labelNameByID map[ulid.ULID]string
	)
	if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		// ListAreas, not GetAreas: materialises the default plan on first
		// contact (exactly as Area/get does), so a first-ever area-plan run
		// diffs against something rather than an empty inventory.
		if stored, e = tx.ListAreas(ctx); e != nil {
			return e
		}
		labels, e = tx.ListLabels(ctx)
		return e
	}); err != nil {
		return fmt.Errorf("area-plan: read stored areas: %w", err)
	}
	labelNameByID = make(map[ulid.ULID]string, len(labels))
	knownLabelNames := make([]string, len(labels))
	for i, l := range labels {
		labelNameByID[l.ID] = l.Name
		knownLabelNames[i] = l.Name
	}

	f, err := os.Open(*planPath)
	if err != nil {
		return fmt.Errorf("area-plan: open plan: %w", err)
	}
	plan, lineErrs, err := areas.ParsePlan(f, knownLabelNames)
	// Closed explicitly rather than deferred, matching area-report/
	// map-label-tabs: the file is finished with here, and errcheck will not
	// accept a bare deferred Close.
	_ = f.Close()
	if err != nil {
		return fmt.Errorf("area-plan: %w", err)
	}
	if len(lineErrs) > 0 {
		fmt.Println("REFUSED PLAN LINES (nothing was diffed for these):")
		for _, le := range lineErrs {
			fmt.Printf("  line %d: %s\n    %s\n", le.Line, le.Reason, le.Raw)
		}
		return fmt.Errorf("area-plan: plan refused: %d line(s)", len(lineErrs))
	}
	if err := plan.Validate(); err != nil {
		return fmt.Errorf("area-plan: invalid plan: %w", err)
	}

	diff := diffAreaPlan(stored, labelNameByID, plan)
	printAreaPlanDiff(diff)

	nCreate, nUpdate := 0, 0
	for _, l := range diff.Lines {
		switch l.Kind {
		case planLineCreate:
			nCreate++
		case planLineUpdate:
			nUpdate++
		}
	}

	if !*apply {
		if nCreate+nUpdate == 0 {
			fmt.Println("\nREPORT ONLY — nothing was written, and nothing would change.")
		} else {
			fmt.Printf("\nREPORT ONLY — nothing was written. Re-run with --apply to create %d area(s) and update %d.\n",
				nCreate, nUpdate)
		}
		return nil
	}

	if len(diff.Conflicts) > 0 {
		return fmt.Errorf("area-plan: refusing --apply: %d conflict(s) listed above; nothing was written", len(diff.Conflicts))
	}
	if err := applyAreaPlanDiff(ctx, d.store, userID, diff); err != nil {
		return fmt.Errorf("area-plan: apply: %w", err)
	}
	fmt.Printf("\nAPPLIED — %d area(s) created, %d updated, %d stripped of feeds the plan moved, %d left exactly as they were.\n",
		nCreate, nUpdate, len(diff.Strips), len(diff.Lines)-nCreate-nUpdate+len(diff.StoredNotInPlan)-len(diff.Strips))
	return nil
}

// areaPlanLineKind is one plan line's outcome against the stored areas.
type areaPlanLineKind int

const (
	planLineCreate areaPlanLineKind = iota
	planLineUpdate
	planLineKeep
)

// areaPlanLine is one line of the diff, in PLAN order (diffAreaPlan's Lines
// slice is exactly len(plan.Areas) long — every plan line produces exactly
// one, whatever its outcome).
type areaPlanLine struct {
	Kind     areaPlanLineKind
	Name     string
	StoredID ulid.ULID // zero for Create
	Position int       // the plan's own line order — Create and Update both set it
	PlanArea areas.Area

	AddTabs, RemoveTabs     []tabs.Tab
	AddLabels, RemoveLabels []string // label NAMES, as they appear in the plan/store respectively
}

// areaPlanDiff is the whole comparison, computed with NO database access
// (diffAreaPlan is a pure function) so it is testable the same way
// materializearealabels.go's classification is.
type areaPlanDiff struct {
	Lines           []areaPlanLine
	StoredNotInPlan []store.AreaView // #442: never deleted — "leave alone, list them"
	// Strips are feeds a stored-but-not-in-plan area must GIVE UP because the
	// plan assigns them elsewhere. The area itself stays; only the feed moves.
	// Without this the operator's very first plan could never be applied: the
	// default plan materialised on first contact already holds every tab
	// (Primary holds tab:primary, Updates holds tab:updates, ...), so a plan
	// line `Personal = tab:primary` would hit ErrFeedTaken in pass 3 against an
	// area the plan never mentions. A plan says where each feed goes; an area
	// it omits keeps existing, but cannot keep a feed the plan has spoken for.
	Strips []areaPlanStrip
	// Conflicts are claims the plan makes that cannot be honoured without
	// deleting an area: the plan names, as a feed, the OWN label of a stored
	// area it does not mention. The own label is never a removable feed, so
	// --apply refuses up front (before any write) rather than failing mid-way.
	Conflicts []string
}

// areaPlanStrip is one kept area's list of feeds the plan moves away from it,
// with the plan area each one goes to (for the report line).
type areaPlanStrip struct {
	View         store.AreaView
	RemoveTabs   []tabs.Tab
	RemoveLabels []string          // stored label NAMES
	MovedTo      map[string]string // feed token ("tab:x" / "label:y") -> plan area name
}

// normalizeAreaPlanName/normalizeAreaPlanLabel compare names the way
// internal/areas itself does everywhere (case-insensitive, trimmed) — areas
// package keeps its own normalizeLabel unexported, so this is the one place
// outside it that needs the identical rule restated, for the identical
// reason ParsePlan's own doc gives: a plan written by hand will not reliably
// match stored data byte-for-byte.
func normalizeAreaPlanName(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// storedFeedSets splits v's feeds into a tab set and a label set (keyed by
// NORMALIZED name, valued by the STORED name — so a removal can be printed
// with the name as it actually appears in the account, not as the plan
// happened to spell it).
func storedFeedSets(v store.AreaView, labelNameByID map[ulid.ULID]string) (map[tabs.Tab]bool, map[string]string) {
	tabSet := map[tabs.Tab]bool{}
	labelSet := map[string]string{}
	for _, f := range v.Feeds {
		switch {
		case f.Tab != "":
			tabSet[f.Tab] = true
		case f.LabelID != nil:
			name := labelNameByID[*f.LabelID]
			labelSet[normalizeAreaPlanName(name)] = name
		}
	}
	return tabSet, labelSet
}

func sortedTabs(m map[tabs.Tab]bool) []tabs.Tab {
	out := make([]tabs.Tab, 0, len(m))
	for t := range m {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// diffAreaPlan compares plan against stored, matching areas BY NAME
// (case-insensitive — the same rule areas.Config.Validate uses for
// duplicate detection). It touches no database: every fact it needs (stored
// feeds resolved to label NAMES via labelNameByID) is passed in, which is
// what makes this testable without one.
func diffAreaPlan(stored []store.AreaView, labelNameByID map[ulid.ULID]string, plan areas.Config) areaPlanDiff {
	storedByName := make(map[string]store.AreaView, len(stored))
	for _, v := range stored {
		storedByName[normalizeAreaPlanName(v.Name)] = v
	}
	matched := make(map[string]bool, len(stored))

	var diff areaPlanDiff
	for i, a := range plan.Areas {
		key := normalizeAreaPlanName(a.Name)
		v, ok := storedByName[key]
		if !ok {
			diff.Lines = append(diff.Lines, areaPlanLine{Kind: planLineCreate, Name: a.Name, Position: i, PlanArea: a})
			continue
		}
		matched[key] = true

		haveTabs, haveLabels := storedFeedSets(v, labelNameByID)
		wantTabs := make(map[tabs.Tab]bool, len(a.Tabs))
		for _, t := range a.Tabs {
			wantTabs[t] = true
		}
		wantLabels := make(map[string]bool, len(a.Labels))
		for _, l := range a.Labels {
			wantLabels[normalizeAreaPlanName(l)] = true
		}

		addTabsSet := map[tabs.Tab]bool{}
		for t := range wantTabs {
			if !haveTabs[t] {
				addTabsSet[t] = true
			}
		}
		removeTabsSet := map[tabs.Tab]bool{}
		for t := range haveTabs {
			if !wantTabs[t] {
				removeTabsSet[t] = true
			}
		}

		var addLabels []string
		for _, l := range a.Labels {
			if _, have := haveLabels[normalizeAreaPlanName(l)]; !have {
				addLabels = append(addLabels, l)
			}
		}
		sort.Strings(addLabels)

		// THE OWN LABEL IS NEVER A REMOVE (#442): it is the area's identity,
		// not a feed a plan can take away — giving it up means deleting the
		// area, which this command never does on the operator's behalf.
		ownNorm := normalizeAreaPlanName(labelNameByID[v.LabelID])
		var removeLabels []string
		for norm, name := range haveLabels {
			if norm == ownNorm {
				continue
			}
			if !wantLabels[norm] {
				removeLabels = append(removeLabels, name)
			}
		}
		sort.Strings(removeLabels)

		addTabs := sortedTabs(addTabsSet)
		removeTabs := sortedTabs(removeTabsSet)

		if len(addTabs) == 0 && len(removeTabs) == 0 && len(addLabels) == 0 && len(removeLabels) == 0 && v.Position == i {
			diff.Lines = append(diff.Lines, areaPlanLine{Kind: planLineKeep, Name: a.Name, StoredID: v.ID, Position: i})
			continue
		}
		diff.Lines = append(diff.Lines, areaPlanLine{
			Kind: planLineUpdate, Name: a.Name, StoredID: v.ID, Position: i, PlanArea: a,
			AddTabs: addTabs, RemoveTabs: removeTabs, AddLabels: addLabels, RemoveLabels: removeLabels,
		})
	}

	// Every feed the plan speaks for, and which plan area claims it.
	claimedTab := map[tabs.Tab]string{}
	claimedLabel := map[string]string{} // normalized label name -> plan area
	for _, a := range plan.Areas {
		for _, t := range a.Tabs {
			claimedTab[t] = a.Name
		}
		for _, l := range a.Labels {
			claimedLabel[normalizeAreaPlanName(l)] = a.Name
		}
	}

	for _, v := range stored {
		if matched[normalizeAreaPlanName(v.Name)] {
			continue
		}
		diff.StoredNotInPlan = append(diff.StoredNotInPlan, v)

		haveTabs, haveLabels := storedFeedSets(v, labelNameByID)
		ownNorm := normalizeAreaPlanName(labelNameByID[v.LabelID])
		strip := areaPlanStrip{View: v, MovedTo: map[string]string{}}
		for _, t := range sortedTabs(haveTabs) {
			if to, claimed := claimedTab[t]; claimed {
				strip.RemoveTabs = append(strip.RemoveTabs, t)
				strip.MovedTo["tab:"+string(t)] = to
			}
		}
		for norm, name := range haveLabels {
			to, claimed := claimedLabel[norm]
			if !claimed {
				continue
			}
			if norm == ownNorm {
				diff.Conflicts = append(diff.Conflicts, fmt.Sprintf(
					"the plan gives label %q to %s, but it is the own label of stored area %q, which the plan does not mention; "+
						"an own label cannot be taken away — rename the plan line to %q, or retire that area first",
					name, to, v.Name, v.Name))
				continue
			}
			strip.RemoveLabels = append(strip.RemoveLabels, name)
			strip.MovedTo["label:"+name] = to
		}
		sort.Strings(strip.RemoveLabels)
		if len(strip.RemoveTabs) > 0 || len(strip.RemoveLabels) > 0 {
			diff.Strips = append(diff.Strips, strip)
		}
	}
	sort.Slice(diff.StoredNotInPlan, func(i, j int) bool { return diff.StoredNotInPlan[i].Name < diff.StoredNotInPlan[j].Name })
	sort.Slice(diff.Strips, func(i, j int) bool { return diff.Strips[i].View.Name < diff.Strips[j].View.Name })
	sort.Strings(diff.Conflicts)
	return diff
}

// feedTokens renders a plan area's feeds as `tab:x`/`label:y` tokens, sorted
// tabs-then-labels, for the "create ... feeds: ..." line.
func feedTokens(a areas.Area) []string {
	out := make([]string, 0, len(a.Tabs)+len(a.Labels))
	tabsCopy := append([]tabs.Tab(nil), a.Tabs...)
	sort.Slice(tabsCopy, func(i, j int) bool { return tabsCopy[i] < tabsCopy[j] })
	for _, t := range tabsCopy {
		out = append(out, "tab:"+string(t))
	}
	labelsCopy := append([]string(nil), a.Labels...)
	sort.Strings(labelsCopy)
	for _, l := range labelsCopy {
		out = append(out, "label:"+l)
	}
	return out
}

func printAreaPlanDiff(diff areaPlanDiff) {
	fmt.Println("=== area-plan: diff against the stored areas ===")
	for _, l := range diff.Lines {
		switch l.Kind {
		case planLineCreate:
			fmt.Printf("create %-20s feeds: %s\n", l.Name, strings.Join(feedTokens(l.PlanArea), ", "))
		case planLineUpdate:
			var tokens []string
			for _, t := range l.AddTabs {
				tokens = append(tokens, "+tab:"+string(t))
			}
			for _, lb := range l.AddLabels {
				tokens = append(tokens, "+label:"+lb)
			}
			for _, t := range l.RemoveTabs {
				tokens = append(tokens, "-tab:"+string(t))
			}
			for _, lb := range l.RemoveLabels {
				tokens = append(tokens, "-label:"+lb)
			}
			fmt.Printf("update %-20s %s\n", l.Name, strings.Join(tokens, "  "))
		case planLineKeep:
			fmt.Printf("keep   %-20s (no change)\n", l.Name)
		}
	}
	fmt.Println()
	if len(diff.StoredNotInPlan) == 0 {
		fmt.Println("stored areas not in the plan: none")
	} else {
		fmt.Println("stored areas not in the plan — KEPT, never deleted on the operator's behalf")
		fmt.Println("(a feed the plan gives to another area is taken from them; the area itself stays):")
		stripByName := map[string]areaPlanStrip{}
		for _, s := range diff.Strips {
			stripByName[s.View.Name] = s
		}
		for _, v := range diff.StoredNotInPlan {
			s, stripped := stripByName[v.Name]
			if !stripped {
				fmt.Printf("  keep   %-20s (no change)\n", v.Name)
				continue
			}
			var tokens []string
			for _, t := range s.RemoveTabs {
				tok := "tab:" + string(t)
				tokens = append(tokens, fmt.Sprintf("-%s (moved to %s)", tok, s.MovedTo[tok]))
			}
			for _, l := range s.RemoveLabels {
				tok := "label:" + l
				tokens = append(tokens, fmt.Sprintf("-%s (moved to %s)", tok, s.MovedTo[tok]))
			}
			fmt.Printf("  strip  %-20s %s\n", v.Name, strings.Join(tokens, "  "))
		}
	}
	if len(diff.Conflicts) > 0 {
		fmt.Println()
		fmt.Println("CONFLICTS — this plan cannot be applied as written:")
		for _, c := range diff.Conflicts {
			fmt.Printf("  %s\n", c)
		}
	}
}

// applyAreaPlanDiff writes the diff in ONE transaction, in three passes so
// that a feed a MOVE takes from one area and gives to another is always
// FREED before it is CLAIMED, whichever order the plan happens to list the
// two areas in (#442):
//
//  1. CREATE every new area with its own label alone (no extra feeds yet) —
//     the label is an existing one of the same name, reused case-insensitively
//     (the same reuse rule materialiseDefaultAreas already applies for the
//     default plan), or freshly minted when none matches.
//  2. REMOVE every feed an existing area's line is giving up, leaving each
//     area at "have MINUS its removals" — nothing is added yet, so this pass
//     can never conflict with anything: a feed can only be removed from
//     whoever currently holds it.
//  3. ADD every feed a line (create or update) is gaining. By now every feed
//     any line wants has already been freed from its old owner in pass 2 (or
//     was never held by anyone else), so the only way this pass still fails
//     is a plan naming a feed a KEPT area (stored, not in the plan) still
//     holds — printed via the store's own ErrFeedTaken message and the whole
//     transaction rolls back, exactly as CreateArea's own savepoint discipline
//     already guarantees for a single call.
func applyAreaPlanDiff(ctx context.Context, st *store.Store, userID ulid.ULID, diff areaPlanDiff) error {
	return st.UserTx(ctx, userID, func(tx *store.UserTx) error {
		// Pass 1: create with the own label only. Mutates diff.Lines in
		// place (by index) so passes 2/3, which range over the same slice,
		// see each create's real StoredID.
		for i, l := range diff.Lines {
			if l.Kind != planLineCreate {
				continue
			}
			ownLabel, err := findOrNilLabelByName(ctx, tx, l.Name)
			if err != nil {
				return fmt.Errorf("create %q: %w", l.Name, err)
			}
			id, err := tx.CreateArea(ctx, store.AreaRow{Name: l.Name, Position: l.Position, LabelID: ownLabel})
			if err != nil {
				return fmt.Errorf("create %q: %w", l.Name, err)
			}
			diff.Lines[i].StoredID = id
		}

		// Pass 2: remove. An update line's "keep set" is its CURRENT extra
		// feeds minus what it is giving up — computed here from RemoveTabs/
		// RemoveLabels against the live AreaView rather than trusted from the
		// diff snapshot, so a --plan run against a store that changed since
		// the report was printed still removes exactly what is actually
		// there. A line with no removals still gets its Position written
		// here, since that is a scalar change with nothing to sequence
		// around.
		for _, l := range diff.Lines {
			if l.Kind != planLineUpdate {
				continue
			}
			pos := l.Position
			if len(l.RemoveTabs) == 0 && len(l.RemoveLabels) == 0 {
				if _, err := tx.UpdateArea(ctx, l.StoredID, store.AreaPatch{Position: &pos}); err != nil {
					return fmt.Errorf("update %q (position): %w", l.Name, err)
				}
				continue
			}
			keep, err := currentFeedsMinus(ctx, tx, l.StoredID, l.RemoveTabs, l.RemoveLabels)
			if err != nil {
				return fmt.Errorf("update %q (compute removals): %w", l.Name, err)
			}
			if _, err := tx.UpdateArea(ctx, l.StoredID, store.AreaPatch{
				Position: &pos, SetFeeds: true, Feeds: keep,
			}); err != nil {
				return fmt.Errorf("update %q (remove feeds): %w", l.Name, err)
			}
		}

		// Pass 2, continued: the STRIPS — kept areas the plan never mentions
		// giving up feeds it assigns elsewhere. Same "free before claim" reason,
		// same UpdateArea call; only the position is left untouched, since the
		// plan has no opinion about where an area it does not name should sit.
		for _, s := range diff.Strips {
			keep, err := currentFeedsMinus(ctx, tx, s.View.ID, s.RemoveTabs, s.RemoveLabels)
			if err != nil {
				return fmt.Errorf("strip %q (compute removals): %w", s.View.Name, err)
			}
			if _, err := tx.UpdateArea(ctx, s.View.ID, store.AreaPatch{SetFeeds: true, Feeds: keep}); err != nil {
				return fmt.Errorf("strip %q (remove feeds): %w", s.View.Name, err)
			}
		}

		// Pass 3: add. Creates get their full plan-stated feeds; updates get
		// their post-removal set plus whatever they are gaining.
		for _, l := range diff.Lines {
			switch l.Kind {
			case planLineCreate:
				feeds, err := resolvePlanFeeds(ctx, tx, l.PlanArea)
				if err != nil {
					return fmt.Errorf("create %q (feeds): %w", l.Name, err)
				}
				if len(feeds) == 0 {
					continue
				}
				if _, err := tx.UpdateArea(ctx, l.StoredID, store.AreaPatch{SetFeeds: true, Feeds: feeds}); err != nil {
					return fmt.Errorf("create %q (add feeds): %w", l.Name, err)
				}
			case planLineUpdate:
				if len(l.AddTabs) == 0 && len(l.AddLabels) == 0 {
					continue
				}
				feeds, err := resolvePlanFeeds(ctx, tx, l.PlanArea)
				if err != nil {
					return fmt.Errorf("update %q (feeds): %w", l.Name, err)
				}
				if _, err := tx.UpdateArea(ctx, l.StoredID, store.AreaPatch{SetFeeds: true, Feeds: feeds}); err != nil {
					return fmt.Errorf("update %q (add feeds): %w", l.Name, err)
				}
			}
		}
		return nil
	})
}

// findOrNilLabelByName looks for an existing label matching name
// case-insensitively — the reuse rule materialiseDefaultAreas already applies
// for the compiled default plan (areas.go), restated here for an
// operator-supplied one: an operator who labelled mail by hand before giving
// it an area plausibly already has a label of the same name, and minting a
// second one would either collide outright or leave two labels meaning the
// same thing. Returns nil (mint one) when nothing matches.
func findOrNilLabelByName(ctx context.Context, tx *store.UserTx, name string) (*ulid.ULID, error) {
	labels, err := tx.ListLabels(ctx)
	if err != nil {
		return nil, err
	}
	norm := normalizeAreaPlanName(name)
	for _, l := range labels {
		if normalizeAreaPlanName(l.Name) == norm {
			id := l.ID
			return &id, nil
		}
	}
	return nil, nil
}

// resolvePlanFeeds turns a plan area's Tabs/Labels into store.AreaFeeds,
// resolving each label NAME to an id via ListLabels — every name here was
// already validated to exist by areas.ParsePlan's knownLabels check, except
// for a label this SAME apply just created as another area's own label
// (impossible: an own label is minted from the AREA's name, not a `label:`
// line), so a lookup miss here is refused rather than silently dropped.
func resolvePlanFeeds(ctx context.Context, tx *store.UserTx, a areas.Area) ([]store.AreaFeed, error) {
	feeds := make([]store.AreaFeed, 0, len(a.Tabs)+len(a.Labels))
	for _, t := range a.Tabs {
		feeds = append(feeds, store.AreaFeed{Tab: t})
	}
	if len(a.Labels) == 0 {
		return feeds, nil
	}
	labels, err := tx.ListLabels(ctx)
	if err != nil {
		return nil, err
	}
	byName := make(map[string]ulid.ULID, len(labels))
	for _, l := range labels {
		byName[normalizeAreaPlanName(l.Name)] = l.ID
	}
	for _, l := range a.Labels {
		id, ok := byName[normalizeAreaPlanName(l)]
		if !ok {
			return nil, fmt.Errorf("label %q named in the plan no longer exists", l)
		}
		lid := id
		feeds = append(feeds, store.AreaFeed{LabelID: &lid})
	}
	return feeds, nil
}

// currentFeedsMinus reads areaID's CURRENT additional feeds (excluding its
// own label, which UpdateArea always keeps regardless) and returns them minus
// removeTabs/removeLabels — the "keep set" pass 2 writes back before any
// addition happens.
func currentFeedsMinus(ctx context.Context, tx *store.UserTx, areaID ulid.ULID, removeTabs []tabs.Tab, removeLabels []string) ([]store.AreaFeed, error) {
	views, err := tx.GetAreas(ctx, []ulid.ULID{areaID})
	if err != nil {
		return nil, err
	}
	if len(views) == 0 {
		return nil, errors.New("area vanished between diff and apply")
	}
	v := views[0]
	removeTab := make(map[tabs.Tab]bool, len(removeTabs))
	for _, t := range removeTabs {
		removeTab[t] = true
	}
	removeLbl := make(map[string]bool, len(removeLabels))
	for _, l := range removeLabels {
		removeLbl[normalizeAreaPlanName(l)] = true
	}
	var keep []store.AreaFeed
	for _, f := range v.Feeds {
		switch {
		case f.LabelID != nil && *f.LabelID == v.LabelID:
			continue // the own label is implicit; UpdateArea always keeps it
		case f.Tab != "":
			if !removeTab[f.Tab] {
				keep = append(keep, f)
			}
		case f.LabelID != nil:
			// Name unknown here without another round trip; removeLabels was
			// computed from the SAME stored names diffAreaPlan read, so an id
			// match against the diff's own removal set is enough — resolved
			// via a fresh GetLabels call for the one label in question.
			name, err := labelNameFor(ctx, tx, *f.LabelID)
			if err != nil {
				return nil, err
			}
			if !removeLbl[normalizeAreaPlanName(name)] {
				keep = append(keep, f)
			}
		}
	}
	return keep, nil
}

func labelNameFor(ctx context.Context, tx *store.UserTx, id ulid.ULID) (string, error) {
	views, err := tx.GetLabels(ctx, []ulid.ULID{id})
	if err != nil {
		return "", err
	}
	if len(views) == 0 {
		return "", fmt.Errorf("label %s vanished between diff and apply", id)
	}
	return views[0].Name, nil
}
