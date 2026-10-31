// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"mailroom/internal/store"
	"mailroom/internal/tabs"
)

// `mailroom-admin map-label-tabs` — point a user's labels at category tabs
// (mailroom#219).
//
// WHY A COMMAND EXISTS AT ALL, given the mapping is already editable through
// `Label/set`: applying it by hand-written SQL is the obvious shortcut and it is
// the one thing that must not happen. `app.labels` is JMAP-visible, so an UPDATE
// that set `tab` directly would skip the change_log row and leave every client
// cursor and the search index stale — the load-bearing invariant, and there is
// no other legitimate write path (CLAUDE.md). This routes through
// store.UpdateLabel, which appends that row in the same transaction.
//
// THE PLAN LIVES IN A FILE THE OPERATOR SUPPLIES, never in the binary. The
// mapping recorded in #219 is one person's label names, and compiling it in
// would both hard-code an operator's vocabulary into an open-source repo (the
// portability rule) and be wrong for the second user. Same reasoning, same shape
// as untangle-labels.
//
// DRY RUN BY DEFAULT, and the report is PER LABEL rather than one total: a
// single "N labels updated" cannot distinguish "the mapping applied" from "every
// line matched a label that was already pointed there", and those look identical
// from a count. Each line reports what it would change, from what, and a run
// that changes nothing says so in as many words.
//
// FAIL CLOSED, PER LINE. An unknown label name or an unknown tab is refused and
// named, never guessed at, and one bad line does not abort the rest — a partial
// plan must not pass silently. Anything refused makes the exit non-zero, dry run
// or not, because "would refuse" needs fixing exactly as much as "refused".

// labelTabPlanEntry is one validated line: a label name, and the tab it feeds.
type labelTabPlanEntry struct {
	Line  int
	Label string
	Tab   tabs.Tab
}

type labelTabLineError struct {
	Line   int
	Raw    string
	Reason string
}

// parseLabelTabPlan reads `<label name>\t<tab>` lines. Blank lines and `#`
// comments are skipped so the operator can annotate the file with the reasoning
// — which, for a mapping that is product intent rather than a derivation, is
// worth keeping next to the data.
//
// An EMPTY tab column is meaningful and accepted: it UNMAPS the label, which is
// how a label is taken back out of a tab. Refusing it would leave the only way
// to reverse a mapping outside this tool.
func parseLabelTabPlan(r io.Reader) (entries []labelTabPlanEntry, errs []labelTabLineError, err error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)

	seen := map[string]int{}
	lineNo := 0
	for sc.Scan() {
		lineNo++
		raw := sc.Text()
		line := strings.TrimRight(raw, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		sep := strings.IndexByte(line, '\t')
		if sep < 0 {
			errs = append(errs, labelTabLineError{lineNo, raw, "no tab between the label name and the tab"})
			continue
		}
		name := strings.TrimSpace(line[:sep])
		tabName := strings.TrimSpace(line[sep+1:])
		if name == "" {
			errs = append(errs, labelTabLineError{lineNo, raw, "missing label name"})
			continue
		}
		if prev, dup := seen[name]; dup {
			// Two lines for one label is a plan whose outcome depends on which
			// line runs last — refused rather than silently resolved.
			errs = append(errs, labelTabLineError{lineNo, raw,
				fmt.Sprintf("label %q already mapped on line %d", name, prev)})
			continue
		}
		var t tabs.Tab
		if tabName != "" {
			parsed, ok := tabs.Parse(tabName)
			if !ok {
				errs = append(errs, labelTabLineError{lineNo, raw,
					fmt.Sprintf("%q is not a category tab (want one of %v)", tabName, tabs.All)})
				continue
			}
			t = parsed
		}
		seen[name] = lineNo
		entries = append(entries, labelTabPlanEntry{Line: lineNo, Label: name, Tab: t})
	}
	if e := sc.Err(); e != nil {
		return nil, errs, fmt.Errorf("read plan: %w", e)
	}
	return entries, errs, nil
}

func runMapLabelTabs(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("map-label-tabs", flag.ContinueOnError)
	var (
		userIDStr = fs.String("user-id", "", "operator user id (ULID)")
		planPath  = fs.String("plan", "", "plan file: <label name>TAB<tab>, one per line ('' unmaps)")
		apply     = fs.Bool("apply", false, "write the mapping (default: dry-run, report only)")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	userID, err := parseUserID(*userIDStr)
	if err != nil {
		return err
	}
	if *planPath == "" {
		return fmt.Errorf("map-label-tabs: --plan FILE is required")
	}
	f, err := os.Open(*planPath)
	if err != nil {
		return fmt.Errorf("open plan: %w", err)
	}
	entries, lineErrs, err := parseLabelTabPlan(f)
	// Closed explicitly rather than deferred, matching untangle-labels: the file
	// is finished with here, and errcheck will not accept a bare deferred Close.
	_ = f.Close()
	if err != nil {
		return err
	}

	d, err := openDeps(ctx)
	if err != nil {
		return err
	}
	defer d.close()

	var labels []store.LabelView
	usage := map[string]int{}
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
			usage[l.Name] = n
		}
		return nil
	}); err != nil {
		return fmt.Errorf("read labels: %w", err)
	}
	byName := make(map[string]store.LabelView, len(labels))
	for _, l := range labels {
		byName[l.Name] = l
	}

	type change struct {
		entry labelTabPlanEntry
		from  tabs.Tab
		msgs  int
	}
	var changes, unchanged []change
	for _, en := range entries {
		l, ok := byName[en.Label]
		if !ok {
			lineErrs = append(lineErrs, labelTabLineError{en.Line, en.Label,
				"no label with that name — names are exact and case-sensitive"})
			continue
		}
		c := change{entry: en, from: l.Tab, msgs: usage[en.Label]}
		if l.Tab == en.Tab {
			unchanged = append(unchanged, c)
			continue
		}
		changes = append(changes, c)
	}

	mode := "DRY RUN (nothing written)"
	if *apply {
		mode = "APPLIED"
	}
	fmt.Printf("=== map-label-tabs: %s ===\n", mode)
	fmt.Printf("  plan lines accepted : %d\n", len(entries))
	fmt.Printf("  plan lines refused  : %d\n", len(lineErrs))
	fmt.Printf("  labels in this account: %d\n\n", len(labels))

	if len(changes) == 0 {
		fmt.Println("NOTHING WOULD CHANGE. Every label in the plan already points where the plan says.")
		fmt.Println("That is a no-op, not a success — if you expected a change, the plan and the")
		fmt.Println("account already agree and the thing you meant to alter is somewhere else.")
	} else {
		fmt.Printf("%-20s %8s  %-14s -> %s\n", "LABEL", "MSGS", "FROM", "TO")
		sort.Slice(changes, func(i, j int) bool { return changes[i].msgs > changes[j].msgs })
		for _, c := range changes {
			from, to := string(c.from), string(c.entry.Tab)
			if from == "" {
				from = "— (unmapped)"
			}
			if to == "" {
				to = "— (unmapped)"
			}
			fmt.Printf("%-20s %8d  %-14s -> %s\n", c.entry.Label, c.msgs, from, to)
		}
	}
	if len(unchanged) > 0 {
		fmt.Printf("\n%d label(s) already pointed where the plan says:", len(unchanged))
		for _, c := range unchanged {
			fmt.Printf(" %s", c.entry.Label)
		}
		fmt.Println()
	}
	// Labels the plan says nothing about, so a mapping gap is visible from this
	// command too rather than only from `tabs`.
	var silent []string
	planned := map[string]bool{}
	for _, en := range entries {
		planned[en.Label] = true
	}
	for _, l := range labels {
		if !planned[l.Name] && l.Tab == "" {
			silent = append(silent, l.Name)
		}
	}
	if len(silent) > 0 {
		sort.Strings(silent)
		fmt.Printf("\n%d label(s) are unmapped and NOT in the plan — their mail stays with the\n", len(silent))
		fmt.Printf("classifier rather than the mapping: %s\n", strings.Join(silent, ", "))
	}

	if len(lineErrs) > 0 {
		fmt.Println("\nREFUSED LINES (nothing was applied for these):")
		for _, e := range lineErrs {
			fmt.Printf("  line %d: %s — %q\n", e.Line, e.Reason, e.Raw)
		}
	}

	if !*apply {
		fmt.Println("\nRe-run with -apply to write the mapping.")
		if len(lineErrs) > 0 {
			return fmt.Errorf("map-label-tabs: %d plan line(s) would be refused", len(lineErrs))
		}
		return nil
	}

	var applied int
	for _, c := range changes {
		l := byName[c.entry.Label]
		if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
			found, e := tx.UpdateLabel(ctx, l.ID, store.LabelPatch{SetTab: true, Tab: c.entry.Tab})
			if e != nil {
				return e
			}
			if !found {
				return fmt.Errorf("label %q vanished between read and write", c.entry.Label)
			}
			return nil
		}); err != nil {
			lineErrs = append(lineErrs, labelTabLineError{c.entry.Line, c.entry.Label, err.Error()})
			continue
		}
		applied++
	}
	fmt.Printf("\nlabels re-pointed: %d (each appending one Label change_log row)\n", applied)
	if len(lineErrs) > 0 {
		return fmt.Errorf("map-label-tabs: %d line(s) refused", len(lineErrs))
	}
	return nil
}
