// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"flag"
	"fmt"
	"sort"
	"strings"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
	"mailroom/internal/tabs"
)

// `mailroom-admin tabs` — what the category tabs (#219) hold RIGHT NOW, from the
// label→tab mapping alone.
//
// THE REPORT BEFORE THE FIX. Increment 1 ships the state (migration 00028: the
// enabled-tab set and the mapping) and the resolver's labelled half. The
// classifier that decides UNLABELLED mail from the function signals is increment
// 2, and this report exists to size it before it is written: the number it
// prints under "no label is mapped" IS that classifier's workload, measured
// rather than assumed. If that number were near zero the classifier would have
// nothing to do; if it is most of the mailbox, the acceptance test has a
// population to score against.
//
// PER-TAB COUNTS, NEVER ONE TOTAL, and the unresolved population SPLIT IN TWO.
// #219 already requires per-tab reporting so a Forums recall of 0 cannot drag an
// aggregate; the split matters for the same reason. "Unlabelled" and "labelled
// but the label is not mapped to a tab" both look like a message with no tab,
// and they have OPPOSITE fixes — the first needs a classifier, the second needs
// the operator to map one more label, which is one edit. A single "unresolved"
// count would hide which.
//
// READ-ONLY WITH ONE EXCEPTION, stated in the output rather than buried here:
// the first read of the tab strip MATERIALISES its five rows (store.ListCategoryTabs
// — see migration 00028 for why there is nowhere earlier to seed them). That is
// the same write the first JMAP CategoryTab/get performs, it happens once, and
// the report says when it did.
func runTabs(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("tabs", flag.ContinueOnError)
	var (
		userIDStr = fs.String("user-id", "", "operator user id (ULID)")
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

	// The strip, plus whether reading it is what created it.
	var (
		strip          []store.CategoryTabView
		labels         []store.LabelView
		labelCounts    = map[ulid.ULID]int{}
		materialisedBy int64
	)
	if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		before, e := tx.ChangeState(ctx, store.ObjectCategoryTab)
		if e != nil {
			return e
		}
		if strip, e = tx.ListCategoryTabs(ctx); e != nil {
			return e
		}
		after, e := tx.ChangeState(ctx, store.ObjectCategoryTab)
		if e != nil {
			return e
		}
		materialisedBy = after - before
		if labels, e = tx.ListLabels(ctx); e != nil {
			return e
		}
		for _, l := range labels {
			n, e := tx.CountLabelUsage(ctx, l.ID)
			if e != nil {
				return e
			}
			labelCounts[l.ID] = n
		}
		return nil
	}); err != nil {
		return fmt.Errorf("read tab state: %w", err)
	}

	enabled := make(map[tabs.Tab]bool, len(strip))
	for _, t := range strip {
		enabled[t.Tab] = t.Enabled
	}

	// Walk every message and resolve what the mapping alone can decide.
	stats := newTabStats()
	after := ulid.ULID{}
	for {
		var page []store.MessageTabMapping
		if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
			var e error
			page, e = tx.MessageTabMappingsAfter(ctx, after, *batch)
			return e
		}); err != nil {
			return fmt.Errorf("scan message tab mappings: %w", err)
		}
		if len(page) == 0 {
			break
		}
		for _, m := range page {
			after = m.ID
			stats.record(m, enabled)
		}
		if len(page) < *batch {
			break
		}
	}

	total := stats.total
	pct := func(n int) float64 {
		if total == 0 {
			return 0
		}
		return 100 * float64(n) / float64(total)
	}

	fmt.Println("=== category tabs (#219): what the label→tab mapping alone decides ===")
	fmt.Printf("  messages scanned : %d\n\n", total)

	fmt.Println("TAB STRIP (app.category_tabs)")
	fmt.Printf("  %-4s %-12s %s\n", "POS", "TAB", "ENABLED")
	for _, t := range strip {
		state := "no"
		if t.Enabled {
			state = "yes"
		}
		fmt.Printf("  %-4d %-12s %s\n", t.Position, t.Tab, state)
	}
	if materialisedBy > 0 {
		fmt.Printf("  (materialised on this run: %d rows created, with their change_log rows)\n",
			materialisedBy)
	}
	fmt.Println()

	fmt.Println("LABEL → TAB MAPPING (app.labels.tab)")
	fmt.Printf("  %-20s %8s  %s\n", "LABEL", "MSGS", "TAB")
	sort.Slice(labels, func(i, j int) bool {
		if labelCounts[labels[i].ID] != labelCounts[labels[j].ID] {
			return labelCounts[labels[i].ID] > labelCounts[labels[j].ID]
		}
		return labels[i].Name < labels[j].Name
	})
	mapped := 0
	for _, l := range labels {
		tab := "— (unmapped)"
		if l.Tab != "" {
			tab = string(l.Tab)
			mapped++
		}
		fmt.Printf("  %-20s %8d  %s\n", l.Name, labelCounts[l.ID], tab)
	}
	fmt.Printf("  %d of %d labels are mapped\n\n", mapped, len(labels))

	fmt.Println("MESSAGES BY TAB, from the mapping alone")
	fmt.Printf("  %-12s %8s %7s   %s\n", "TAB", "MSGS", "%", "AFTER THE ENABLED FOLD")
	for _, t := range tabs.All {
		note := ""
		if !enabled[t] {
			note = "  (disabled — folded into primary)"
		}
		fmt.Printf("  %-12s %8d %6.1f%%   %8d%s\n",
			t, stats.byTab[t], pct(stats.byTab[t]), stats.byTabFolded[t], note)
	}
	fmt.Println()

	fmt.Println("NOT DECIDED BY THE MAPPING — the two halves have OPPOSITE fixes")
	fmt.Printf("  unlabelled (the tab classifier's workload, #219 increment 2) : %6d  (%5.1f%%)\n",
		stats.unlabelled, pct(stats.unlabelled))
	fmt.Printf("  labelled, but none of its labels is mapped to a tab          : %6d  (%5.1f%%)\n",
		stats.labelledUnmapped, pct(stats.labelledUnmapped))
	fmt.Println("  (the second needs one more mapping edit, not a classifier)")
	fmt.Println()

	fmt.Println("PRECEDENCE TIE-BREAK (tabs.FromLabels: forums > social > promotions > updates > primary)")
	if stats.contestedTotal == 0 {
		fmt.Println("  invoked on 0 messages — no message's labels span more than one tab.")
		fmt.Println("  EXPECTED: #223 left every labelled message with exactly one label. The rule")
		fmt.Println("  is therefore untested against real mail, and the arbitrary pair in it")
		fmt.Println("  (promotions before updates) has not yet had to decide anything.")
	} else {
		fmt.Printf("  invoked on %d messages (%.1f%%):\n", stats.contestedTotal, pct(stats.contestedTotal))
		keys := make([]string, 0, len(stats.contested))
		for k := range stats.contested {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool {
			if stats.contested[keys[i]] != stats.contested[keys[j]] {
				return stats.contested[keys[i]] > stats.contested[keys[j]]
			}
			return keys[i] < keys[j]
		})
		for _, k := range keys {
			fmt.Printf("    %-40s %6d\n", k, stats.contested[k])
		}
	}
	return nil
}

// tabStats accumulates the report's arithmetic, separately from the I/O that
// feeds it, so the counting is testable without a database — the same split as
// functionSignalStats.
//
// THE TWO UNRESOLVED COUNTERS ARE THE POINT. `unlabelled` is what the tab
// classifier (#219 increment 2) has to decide; `labelledUnmapped` is mail whose
// label simply has no tab yet, which one edit fixes. They look identical from
// the outside — a message with no tab — and a single "unresolved" number would
// let a mapping gap masquerade as a classifier workload.
type tabStats struct {
	total            int
	unlabelled       int
	labelledUnmapped int
	byTab            map[tabs.Tab]int
	byTabFolded      map[tabs.Tab]int
	// contested counts, per "winner over losers" pair, how often the precedence
	// tie-break actually had to decide something. A rule nobody measures is a rule
	// nobody can correct.
	contested      map[string]int
	contestedTotal int
}

func newTabStats() *tabStats {
	return &tabStats{
		byTab:       map[tabs.Tab]int{},
		byTabFolded: map[tabs.Tab]int{},
		contested:   map[string]int{},
	}
}

func (s *tabStats) record(m store.MessageTabMapping, enabled map[tabs.Tab]bool) {
	s.total++
	winner, lost, ok := tabs.FromLabels(m.Mapped)
	if !ok {
		if m.Labels == 0 {
			s.unlabelled++
		} else {
			s.labelledUnmapped++
		}
		return
	}
	s.byTab[winner]++
	s.byTabFolded[tabs.Fold(winner, enabled)]++
	if len(lost) == 0 {
		return
	}
	s.contestedTotal++
	names := make([]string, len(lost))
	for i, t := range lost {
		names[i] = string(t)
	}
	sort.Strings(names)
	s.contested[fmt.Sprintf("%s over %s", winner, strings.Join(names, "+"))]++
}
