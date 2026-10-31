// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strings"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// `mailroom-admin thread-report` — the messages that share an RFC 5322
// Message-ID while sitting in different threads (#210), and the opt-in repair
// that merges them.
//
// WHY IT EXISTS. #210's fix is in store.FindThreadByHints and is FORWARD-ONLY:
// a message keeps the thread_id it was assigned at ingest, so mail already in
// the mailbox stays split. Shipping only the ingest fix would have been a
// change that is correct, deployed, and invisible on a real
// mailbox — the failure shape CLAUDE.md's measurement rules exist for.
//
// READ-ONLY BY DEFAULT (CLAUDE.md, #177/#191/#206). Without --apply this lists
// what WOULD merge and mutates nothing.
func runThreadReport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("thread-report", flag.ContinueOnError)
	var (
		userIDStr = fs.String("user", "", "user id (ULID) — REQUIRED")
		apply     = fs.Bool("apply", false, "merge each group into its keeper thread (default: report only)")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	// --user is required for the same reason quarantine's is: this can MUTATE,
	// and re-threading is a change to one person's conversations.
	if strings.TrimSpace(*userIDStr) == "" {
		return errors.New("thread-report: --user is required (ULID)")
	}
	userID, err := ulid.Parse(*userIDStr)
	if err != nil {
		return fmt.Errorf("thread-report: --user %q: not a valid ULID: %w", *userIDStr, err)
	}

	d, err := openDeps(ctx)
	if err != nil {
		return err
	}
	defer d.close()

	var groups []store.SplitThreadGroup
	if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		groups, e = tx.SplitMessageIDGroups(ctx)
		return e
	}); err != nil {
		return fmt.Errorf("thread-report: %w", err)
	}

	fmt.Printf("thread-report: %d split Message-ID group(s) for user %s\n\n", len(groups), userID)

	if len(groups) == 0 {
		// ⚠ Zero is a real finding, not an empty result, and it has TWO
		// readings that must not be conflated: a mailbox ingested entirely
		// after #210's fix has none to find, and so does one where this has
		// already been applied. Say so rather than printing nothing.
		fmt.Println("  Nothing to merge: no Message-ID in this mailbox spans more than one thread.")
		fmt.Println("  That is the expected state for mail ingested after #210's fix, and also")
		fmt.Println("  the state left behind by a previous --apply run.")
		return nil
	}

	fmt.Printf("  %-46s %-8s %-8s %s\n", "MESSAGE-ID", "MSGS", "THREADS", "KEEPER (earliest received)")
	totalMessages, totalLosers := 0, 0
	for _, g := range groups {
		hint := g.MessageIDHint
		if len(hint) > 46 {
			hint = hint[:43] + "..."
		}
		fmt.Printf("  %-46s %-8d %-8d %s\n", hint, len(g.Messages), len(g.Losers)+1, g.Keeper)
		totalMessages += len(g.Messages)
		totalLosers += len(g.Losers)
	}

	fmt.Printf("\n  %d message(s) across %d thread(s) would collapse into %d.\n",
		totalMessages, totalLosers+len(groups), len(groups))
	fmt.Println("  The keeper is the thread holding the EARLIEST message — the same thread")
	fmt.Println("  FindThreadByHints would have chosen, so a repaired mailbox and a freshly")
	fmt.Println("  ingested one agree.")

	if !*apply {
		fmt.Println("\n  Report only. Re-run with --apply to merge.")
		fmt.Println("  ⚠ A merge DISCARDS the losing threads' own state (muted_at in particular):")
		fmt.Println("    a merged thread cannot carry two values of a per-thread flag.")
		return nil
	}

	var res store.MergeResult
	if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		res, e = tx.MergeSplitThreadGroups(ctx, groups)
		return e
	}); err != nil {
		return fmt.Errorf("thread-report --apply: %w", err)
	}

	fmt.Printf("\n  APPLIED: %d group(s), %d message(s) moved, %d thread(s) deleted.\n",
		res.Groups, res.MessagesMoved, res.ThreadsDeleted)
	fmt.Println("  Every moved message appended an Email change row, every keeper a Thread")
	fmt.Println("  'updated', and every emptied thread a Thread 'destroyed' — so clients")
	fmt.Println("  re-sync the conversations rather than silently disagreeing with the server.")
	return nil
}
