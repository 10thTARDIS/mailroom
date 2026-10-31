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
)

// `mailroom-admin writeback-ledger` — the report-first look at the ingest-only
// ledger ADR-028 §4 builds: every writeback intent an account accumulated as
// `suppressed` while it was shadowed (`writeback_enabled=false`). These are
// changes Mailroom deliberately did NOT make to a real mailbox, and the ADR
// treats that queue as the shadow run's most valuable artifact — a countable,
// per-op-class record of everything Mailroom wanted to do, which is exactly
// the evidence the go-live decision should rest on rather than a vibe.
//
// REPORT-FIRST, --apply OPT-IN (CLAUDE.md "Build the report before the fix").
// Replay is a one-way door out of `suppressed` — ReplaySuppressedIntents is
// the ONLY path — so lifting the mode must never drain it as a side effect
// (ADR-028 §4), and neither does this command by default: with no --apply it
// only prints the ledger and changes nothing.
//
// PER-OP, NEVER ONE TOTAL. A single number cannot tell "the shadow run
// changed nothing" from "the one thing I measured is always empty", so the
// report breaks the ledger down by op (+flag, -flag, +kw, -kw, move, append,
// expunge) with a total alongside, and states the account it counted for so a
// "0" cannot be misread as "0 across the instance".
//
// REFUSES TO REPLAY A STILL-SHADOWED ACCOUNT, checking BOTH halves of ADR-028
// §2's switch: the account's own `writeback_enabled` column and the
// instance-level MAILROOM_WRITEBACK_DISABLED kill switch. Either one being off
// means the account is still shadowed.
//
// ReplaySuppressedIntents writes `pending` unconditionally — it does not
// re-check either switch — so replaying a still-shadowed account does NOT
// "just re-suppress every intent straight back" (that claim was false and has
// been corrected: nothing re-suppresses an already-pending row; only
// SuppressAccountIntents does that, and it is called from Account/set, not
// from the enqueue or drain path). What actually happens is worse: the
// intents become `pending` and sit there, invisible to this ledger (which only
// counts `suppressed`), until the operator lifts BOTH switches — at which
// point the whole backlog fires UNREVIEWED on the very next drain, with no
// second chance to look at it first. The operator must lift ingest-only mode
// first (Account/set writebackEnabled=true, and unset MAILROOM_WRITEBACK_
// DISABLED if it is set), THEN run this command, so the report the operator
// reviewed is the report that describes what fires.
func runWritebackLedger(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("writeback-ledger", flag.ContinueOnError)
	var (
		userIDStr    = fs.String("user-id", "", "operator user id (ULID)")
		accountIDStr = fs.String("account-id", "", "account id (ULID; see list-accounts)")
		apply        = fs.Bool("apply", false, "replay the suppressed ledger back to pending (default: report only)")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	userID, err := parseUserID(*userIDStr)
	if err != nil {
		return err
	}
	accountID, err := ulid.Parse(*accountIDStr)
	if err != nil {
		return fmt.Errorf("writeback-ledger: --account-id: %w", err)
	}

	d, err := openDeps(ctx)
	if err != nil {
		return err
	}
	defer d.close()

	var (
		acct    store.Account
		byOp    map[string]int64
		buckets []store.SuppressedIntentRow
		total   int64
	)
	if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		if acct, e = tx.GetAccount(ctx, accountID); e != nil {
			return e
		}
		if byOp, e = tx.SuppressedIntentsByOp(ctx, accountID); e != nil {
			return e
		}
		if buckets, e = tx.SuppressedIntentsByOpAndSource(ctx, accountID); e != nil {
			return e
		}
		total, e = tx.SuppressedIntentCount(ctx, accountID)
		return e
	}); err != nil {
		return fmt.Errorf("writeback-ledger: %w", err)
	}

	// Computed once, here, and threaded to both the report below and the
	// refusal further down — ADR-028 §2's switch has two halves, and evaluating
	// them twice (once for the report, once for the refusal) is exactly the
	// "two predicates for one question" shape three rounds of review already
	// spent fixing in this package. See writebackShadowReason.
	instanceDisabled := d.store.WritebackDisabled()
	printWritebackLedgerReport(acct, byOp, buckets, total, instanceDisabled)

	if !*apply {
		fmt.Println("\nREPORT ONLY — nothing was changed. Re-run with --apply to replay this ledger.")
		return nil
	}

	// The trap ADR-028 §4 names explicitly: without this guard, --apply against
	// a still-shadowed account would revive every intent straight to 'pending'
	// (ReplaySuppressedIntents does not re-check either switch), where it sits
	// until BOTH switches lift and then fires UNREVIEWED on the next drain —
	// nothing re-suppresses it in the meantime. Checked on BOTH halves of the
	// switch (ADR-028 §2): the account's own column, and the instance-level
	// MAILROOM_WRITEBACK_DISABLED kill switch — either one being off means this
	// is still a shadow run, and the operator needs to know WHICH one to flip.
	if which := writebackShadowReason(acct, instanceDisabled); which != "" {
		return fmt.Errorf("writeback-ledger: account %s (%s) is still in ingest-only mode (%s) — "+
			"replaying now would move these %d intent(s) to pending, where they would sit until the "+
			"mode lifts and then fire UNREVIEWED on the next drain, not re-suppress; lift ingest-only "+
			"mode first (Account/set writebackEnabled=true, and unset MAILROOM_WRITEBACK_DISABLED if "+
			"that is the one blocking), then re-run with --apply", acct.ID, acct.Name, which, total)
	}

	var replayed int64
	if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		replayed, e = tx.ReplaySuppressedIntents(ctx, accountID)
		return e
	}); err != nil {
		return fmt.Errorf("writeback-ledger: replay: %w", err)
	}
	fmt.Printf("\nAPPLIED: replayed %d suppressed intent(s) for account %s (%s) back to pending — "+
		"the writeback engine will apply them on its next drain pass.\n", replayed, acct.ID, acct.Name)
	return nil
}

// writebackShadowReason names which half of ADR-028 §2's writeback switch is
// holding an account back, or "" if the account is effectively live. It is
// the ONE evaluation of "is this account really live" in this file — used by
// both the ledger report's mode line and the --apply refusal above — so the
// two can never disagree about an account the instance-level
// MAILROOM_WRITEBACK_DISABLED kill switch has shadowed even though its own
// writebackEnabled column reads true. (This overlaps mailroom#554 item 6,
// which is about Account/get reporting the column instead of the effective
// value; this fixes the CLI report only.)
func writebackShadowReason(acct store.Account, instanceDisabled bool) string {
	switch {
	case !acct.WritebackEnabled && instanceDisabled:
		return "the account's writebackEnabled column is false AND the instance-level MAILROOM_WRITEBACK_DISABLED is set"
	case !acct.WritebackEnabled:
		return "the account's writebackEnabled column is false"
	case instanceDisabled:
		return "the instance-level MAILROOM_WRITEBACK_DISABLED is set"
	default:
		return ""
	}
}

// writebackOpOrder is the canonical op-class order the report prints in
// (app.writeback_intents.op, ADR-028 §4), independent of which ops actually
// have rows this run — a reader comparing two runs wants the same op on the
// same line every time rather than however GROUP BY happened to sort it.
var writebackOpOrder = []string{"+flag", "-flag", "+kw", "-kw", "move", "append", "expunge"}

// printWritebackLedgerReport prints the per-op-class ledger for one account.
// It states the account (id and name) it counted for on every path, including
// the empty one, so a "0" can never be misread as "0 across the instance"
// (CLAUDE.md: "a bounded or empty result must report its bound").
//
// instanceDisabled is the MAILROOM_WRITEBACK_DISABLED kill switch, threaded in
// rather than read here so the mode line reports the EFFECTIVE state through
// writebackShadowReason — the same test the --apply refusal in
// runWritebackLedger applies. Reading only acct.WritebackEnabled used to print
// "LIVE" for an account the instance flag had shadowed, while --apply on that
// very same run correctly refused it: the report contradicted the command.
func printWritebackLedgerReport(acct store.Account, byOp map[string]int64, buckets []store.SuppressedIntentRow, total int64, instanceDisabled bool) {
	fmt.Printf("=== writeback-ledger (#521, ADR-028 §4): the ingest-only ledger for account %s (%s) ===\n",
		acct.ID, acct.Name)
	mode := "LIVE (writeback enabled)"
	if which := writebackShadowReason(acct, instanceDisabled); which != "" {
		mode = fmt.Sprintf("SHADOWED (ingest-only mode — %s)", which)
	}
	fmt.Printf("account mode: %s\n\n", mode)

	if total == 0 {
		fmt.Printf("no suppressed intents — account %s (%s) has made no local mutations while shadowed.\n",
			acct.ID, acct.Name)
		return
	}

	fmt.Printf("%-10s %8s\n", "OP", "COUNT")
	fmt.Println(strings.Repeat("-", 19))
	seen := map[string]bool{}
	for _, op := range writebackOpOrder {
		seen[op] = true
		n, ok := byOp[op]
		if !ok || n == 0 {
			continue
		}
		fmt.Printf("%-10s %8d\n", op, n)
	}
	// An op value this report's fixed order does not know about yet is printed
	// rather than silently folded into the total unlabelled — a new op landing
	// in app.writeback_intents must not go missing from the breakdown that is
	// the entire point of this command.
	var unknown []string
	for op := range byOp {
		if !seen[op] {
			unknown = append(unknown, op)
		}
	}
	sort.Strings(unknown)
	for _, op := range unknown {
		fmt.Printf("%-10s %8d  (op not in the known list — report this)\n", op, byOp[op])
	}
	fmt.Println(strings.Repeat("-", 19))
	fmt.Printf("%-10s %8d  for account %s (%s)\n", "total", total, acct.ID, acct.Name)

	printWritebackLedgerBySource(buckets)
}

// printWritebackLedgerBySource breaks the same ledger down by WHAT ASKED for
// each intent (#554 item 3, migration 00057).
//
// ⚠ THIS IS THE TABLE AN OPERATOR ACTUALLY DECIDES ON. The op breakdown above
// says what would be done to the mailbox; it cannot say whether a person asked
// for it. Ingest-time rule labelling produces '+kw' in bulk — on a real
// corpus most intents are '+kw' — so a replay decision made from the
// op table alone is made from a number dominated by machinery.
//
// Every bucket the query returned is printed, including sources this build does
// not know about, for the same reason the op loop prints unknown ops: a value
// that goes missing from the breakdown defeats the breakdown.
func printWritebackLedgerBySource(buckets []store.SuppressedIntentRow) {
	if len(buckets) == 0 {
		return
	}
	fmt.Printf("\n%-10s %-10s %8s\n", "OP", "SOURCE", "COUNT")
	fmt.Println(strings.Repeat("-", 30))
	var unknownSource int64
	for _, b := range buckets {
		src := b.Source
		if src == "" {
			// ⚠ NOT "user". Empty means the row predates migration 00057 and the
			// actor is unrecoverable; printing a name here would manufacture the
			// provenance the column exists to stop manufacturing.
			src = "unknown"
			unknownSource += b.Count
		}
		fmt.Printf("%-10s %-10s %8d\n", b.Op, src, b.Count)
	}
	fmt.Println(strings.Repeat("-", 30))
	if unknownSource > 0 {
		fmt.Printf("%d intent(s) predate the source column (migration 00057) and cannot be\n"+
			"attributed. They are NOT counted as user actions.\n", unknownSource)
	}
}
