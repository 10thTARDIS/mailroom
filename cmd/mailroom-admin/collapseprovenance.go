// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// `mailroom-admin collapse-provenance --user-id U [--apply] [--show N]` (#859):
// what past runs of collapse-duplicates did to label attribution, and the one
// repair the surviving evidence can justify.
//
// Before #859, collapse-duplicates copied a duplicate's labels onto the copy it
// kept WITHOUT saying who had applied them, so every one arrived as the user's
// own filing — the thing learned placement, the label model and rule induction
// learn from. The evidence kinds, their reach, and what is deliberately NOT
// repaired are documented in internal/store/collapse_provenance.go.
//
// REPORT FIRST. Without --apply it writes nothing: it prints the change_log
// horizon, the collapse transactions still visible in it, the labels those
// wrote per applied_by, the rule-id contradictions per applied_by, the rows
// --apply would re-attribute, and the bound — how many 'user' rows no evidence
// can examine. With --apply it re-attributes exactly the 'user' rows that carry
// a rule id back to 'rule', one Email change row per message, in one
// transaction. A rerun plans 0.
func runCollapseProvenance(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("collapse-provenance", flag.ContinueOnError)
	userFlag := fs.String("user-id", "", "user whose labels to examine (required)")
	apply := fs.Bool("apply", false, "re-attribute the rule-id rows (default: report only, write nothing)")
	show := fs.Int("show", 50, "repairable rows to list")
	if err := fs.Parse(args); err != nil {
		return err
	}
	userID, err := parseUserID(*userFlag)
	if err != nil {
		return err
	}
	d, err := openDeps(ctx)
	if err != nil {
		return err
	}
	defer d.close()
	return collapseProvenance(ctx, os.Stdout, d.store, userID, *apply, *show)
}

// collapseProvenance runs the report and, with apply, the repair.
func collapseProvenance(ctx context.Context, w io.Writer, s *store.Store, userID ulid.ULID, apply bool, show int) error {
	var rep store.CollapseProvenanceReport
	if err := s.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		rep, e = tx.CollapseProvenanceReport(ctx)
		return e
	}); err != nil {
		return fmt.Errorf("collapse provenance report: %w", err)
	}
	writeCollapseProvenanceReport(w, rep, show)

	if !apply {
		_, _ = fmt.Fprintln(w, "\nDRY RUN: nothing changed. Re-run with -apply to re-attribute the rows listed above.")
		return nil
	}
	var labels, messages int
	if err := s.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		labels, messages, e = tx.RepairCollapseRuleProvenance(ctx)
		return e
	}); err != nil {
		return fmt.Errorf("collapse provenance repair: %w", err)
	}
	_, _ = fmt.Fprintf(w, "\nAPPLIED: %d label row(s) on %d message(s) re-attributed from 'user' to 'rule' "+
		"(one Email change row per message).\n", labels, messages)
	return nil
}

func writeCollapseProvenanceReport(w io.Writer, r store.CollapseProvenanceReport, show int) {
	_, _ = fmt.Fprintln(w, "=== collapse-provenance (#859): label attribution after past collapses ===")

	_, _ = fmt.Fprintln(w, "\nEvidence 1 — 'rule id but not applied_by rule' (exact; reaches every past collapse")
	_, _ = fmt.Fprintln(w, "  unless the rule has since been deleted, which erases the id):")
	if len(r.RuleIDByAppliedBy) == 0 {
		_, _ = fmt.Fprintln(w, "  (no label row carries a rule id under any applied_by other than 'rule')")
	}
	for _, by := range sortedKeys(r.RuleIDByAppliedBy) {
		note := "  <- not produced by a collapse; reported, never changed"
		if by == store.LabelByUser {
			note = "  <- a rule's label a collapse turned into a filing: REPAIRABLE"
		}
		_, _ = fmt.Fprintf(w, "  applied_by %-8s : %d%s\n", by, r.RuleIDByAppliedBy[by], note)
	}

	_, _ = fmt.Fprintln(w, "\nEvidence 2 — collapse transactions still in change_log (shows WHICH rows a collapse")
	_, _ = fmt.Fprintln(w, "  wrote, not who applied them on the deleted copy; an upper bound; never repaired):")
	if r.OldestEmailChange == nil {
		_, _ = fmt.Fprintln(w, "  change_log holds NO Email history for this user, so this evidence sees nothing at all.")
	} else {
		_, _ = fmt.Fprintf(w, "  horizon: oldest retained Email change %s (compaction floor seq %d)\n",
			r.OldestEmailChange.UTC().Format("2006-01-02 15:04:05Z"), r.EmailFloorSeq)
		_, _ = fmt.Fprintln(w, "  a collapse before the horizon is INVISIBLE here.")
	}
	_, _ = fmt.Fprintf(w, "  collapse transactions visible : %d (on %d keeper message(s))\n", r.CollapseTxs, r.CollapseKeepers)
	if r.FirstCollapseTx != nil && r.LastCollapseTx != nil {
		_, _ = fmt.Fprintf(w, "  first / last                  : %s / %s\n",
			r.FirstCollapseTx.UTC().Format("2006-01-02 15:04:05Z"), r.LastCollapseTx.UTC().Format("2006-01-02 15:04:05Z"))
	}
	if len(r.CollapseLabelsByBy) == 0 {
		_, _ = fmt.Fprintln(w, "  labels written by those transactions: 0")
	}
	for _, by := range sortedKeys(r.CollapseLabelsByBy) {
		_, _ = fmt.Fprintf(w, "  labels written, now applied_by %-8s : %d\n", by, r.CollapseLabelsByBy[by])
	}

	_, _ = fmt.Fprintln(w, "\nBound — what no surviving evidence can settle:")
	_, _ = fmt.Fprintf(w, "  'user' rows without a rule id             : %d\n", r.UserRowsNoRule)
	_, _ = fmt.Fprintf(w, "    of which older than the change_log horizon : %d\n", r.UserRowsNoRuleBeforeHorizon)
	_, _ = fmt.Fprintln(w, "  A past collapse that copied a learned, desk, agent, setup or deleted-rule label")
	_, _ = fmt.Fprintln(w, "  left a 'user' row indistinguishable from a real filing: the copy that said otherwise")
	_, _ = fmt.Fprintln(w, "  was deleted, and setup_journal / learned_placements rows cascaded with it. Any such")
	_, _ = fmt.Fprintln(w, "  row is among the counts above; this command cannot say which, and does not guess.")

	_, _ = fmt.Fprintf(w, "\nRepairable ('user' -> 'rule'): %d row(s)\n", len(r.Repairable))
	for i, row := range r.Repairable {
		if i >= show {
			_, _ = fmt.Fprintf(w, "  … %d more not listed (raise -show)\n", len(r.Repairable)-show)
			break
		}
		where := "no visible collapse transaction"
		if row.InCollapseTx {
			where = "matches a visible collapse transaction"
		}
		_, _ = fmt.Fprintf(w, "  message %s  label %s  rule %s  (%s)\n", row.MessageID, row.LabelID, row.RuleID, where)
	}
}

func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
