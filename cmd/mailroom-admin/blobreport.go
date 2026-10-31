// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"encoding/hex"
	"flag"
	"fmt"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/blob"
	"mailroom/internal/store"
)

// `mailroom-admin blob-report` — the standing orphan reconciliation (#300).
//
// WHY IT EXISTS. Four orphaned blobs accumulated on the operator's live
// instance and nobody could have known: three delete paths removed a message
// row and left its bytes behind, and there was no query that could see the
// residue. The reclaim fix landed in v1.34.5; this is the other half of that
// issue — the report that would have found it, and that will find the next one.
//
// IT IS READ-ONLY AND HAS NO --apply. That is deliberate and is the project's
// rule (#177): a reconciliation that can delete is a reconciliation people run
// without reading. Acting on what it finds is a separate, explicit decision.
//
// ⚠ IT REPORTS BOTH DIRECTIONS, AND THEY FAIL DIFFERENTLY:
//
//   - a ROW WITH NO FILE is a broken read waiting to happen — the message is
//     listed, opening it fails;
//   - a FILE WITH NO ROW is invisible consumption — nothing breaks, the disk
//     just never comes back, which is why it went unnoticed for months.
//
// A report that printed one "orphans" number could not tell those apart, so
// every count below is per signal. Same reason there is a `--verbose`: a count
// tells you whether to look, hashes tell you where.
func runBlobReport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("blob-report", flag.ContinueOnError)
	verbose := fs.Bool("verbose", false, "list the hash of every discrepancy, not just its count")
	userFlag := fs.String("user", "", "restrict the report to one user id (default: every active user)")
	if err := fs.Parse(args); err != nil {
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
	// THE BACKEND MUST BE ABLE TO ENUMERATE, and if it cannot this command says
	// so and STOPS. The alternative — skipping the on-disk half — would print a
	// clean row-side reconciliation and read as "no orphans", which is precisely
	// the "ran without erroring while doing nothing" outcome this report exists
	// to prevent.
	enum, ok := blobs.(*blob.FSBlobStore)
	if !ok {
		return fmt.Errorf("blob-report: this blob backend (%T) cannot enumerate stored objects, "+
			"so the files-with-no-row half of the reconciliation cannot be computed. Refusing to "+
			"print a half-report that would read as a clean one", blobs)
	}

	users, err := d.store.ListActiveUserIDs(ctx)
	if err != nil {
		return fmt.Errorf("blob-report: list users: %w", err)
	}
	if *userFlag != "" {
		u, perr := ulid.Parse(*userFlag)
		if perr != nil {
			return fmt.Errorf("blob-report: parse -user: %w", perr)
		}
		users = []ulid.ULID{u}
	}

	fmt.Printf("%-28s %6s %6s %8s %9s %9s %9s\n",
		"USER", "ROWS", "FILES", "NO-FILE", "NO-ROW", "UNREF", "UNREADBL")

	var (
		totRows, totFiles, totNoFile, totNoRow int
		totUnref, totSuperseded, totHeld       int
		totUnparsed                            int
	)
	live := map[string]bool{} // user prefix -> seen, for the stray-directory pass

	for _, uid := range users {
		var census store.BlobRowCensus
		if err := d.store.UserTx(ctx, uid, func(tx *store.UserTx) error {
			var e error
			census, e = tx.BlobRowCensus(ctx)
			return e
		}); err != nil {
			return fmt.Errorf("blob-report: census for %s: %w", uid, err)
		}

		onDisk, unparsed, err := enum.UserHashes(ctx, uid)
		if err != nil {
			return fmt.Errorf("blob-report: walk blobs for %s: %w", uid, err)
		}
		live[enum.UserPrefix(uid)] = true

		diskSet := make(map[string]bool, len(onDisk))
		for _, h := range onDisk {
			diskSet[h.String()] = true
		}
		rowSet := make(map[string]bool, len(census.Hashes))
		for _, h := range census.Hashes {
			rowSet[hex.EncodeToString(h)] = true
		}

		var noFile, noRow []string
		for h := range rowSet {
			if !diskSet[h] {
				noFile = append(noFile, h)
			}
		}
		for h := range diskSet {
			if !rowSet[h] {
				noRow = append(noRow, h)
			}
		}

		fmt.Printf("%-28s %6d %6d %8d %9d %9d %9d\n",
			uid, census.Rows, len(onDisk), len(noFile), len(noRow),
			census.Unreferenced, len(unparsed))

		if *verbose {
			printHashes("  row with no file (a read that will fail)", noFile)
			printHashes("  file with no row (disk that never comes back)", noRow)
			printHashes("  unreferenced row (leaks unless reclaimed)", hexAll(census.UnreferencedHashes))
			for _, name := range unparsed {
				fmt.Printf("  unreadable file name: %s\n", name)
			}
		}

		totRows += census.Rows
		totFiles += len(onDisk)
		totNoFile += len(noFile)
		totNoRow += len(noRow)
		totUnref += census.Unreferenced
		totSuperseded += census.UnreferencedSuperseded
		totHeld += census.UnreferencedHeldByIntent
		totUnparsed += len(unparsed)
	}

	// STRAY TOP-LEVEL DIRECTORIES: a user's blobs outliving the user. Today
	// nothing hard-deletes a user, so this is expected to be 0 — and that is
	// exactly why it is worth printing, because it becomes the verifier for #54
	// the moment delete-user exists.
	prefixes, err := enum.RootPrefixes(ctx)
	if err != nil {
		return fmt.Errorf("blob-report: read blob root: %w", err)
	}
	var stray []string
	for _, p := range prefixes {
		if !live[p] {
			stray = append(stray, p)
		}
	}

	fmt.Printf("\nusers examined              : %d\n", len(users))
	fmt.Printf("blob rows                   : %d\n", totRows)
	fmt.Printf("blob files                  : %d\n", totFiles)
	fmt.Printf("rows with no file           : %d\n", totNoFile)
	fmt.Printf("files with no row           : %d\n", totNoRow)
	fmt.Printf("unreferenced rows           : %d\n", totUnref)
	fmt.Printf("  of which superseded bytes : %d   (producer: compose-time PGP signing)\n", totSuperseded)
	fmt.Printf("  of which held by an intent: %d   (NOT leaks — writeback still needs these)\n", totHeld)
	fmt.Printf("unreadable file names       : %d\n", totUnparsed)
	fmt.Printf("stray user directories      : %d\n", len(stray))
	for _, p := range stray {
		fmt.Printf("  %s — no active user derives this prefix\n", p)
	}

	// ⚠ THE BOUND, STATED. A zero above means "zero among the users this run
	// looked at", and -user makes that a proper subset. A lower bound that reads
	// as a total is its own defect (#155).
	if *userFlag != "" {
		fmt.Printf("\n⚠ BOUNDED RUN: -user was given, so every number above covers ONE user. " +
			"The stray-directory count is meaningless in this mode — every other user's " +
			"directory looks stray. Re-run without -user for the standing report.\n")
	} else if len(users) == 0 {
		fmt.Printf("\n⚠ NO ACTIVE USERS. Every count above is zero because there was nothing to " +
			"examine, which is not the same as a clean reconciliation.\n")
	}
	return nil
}

func hexAll(hs [][]byte) []string {
	out := make([]string, 0, len(hs))
	for _, h := range hs {
		out = append(out, hex.EncodeToString(h))
	}
	return out
}

func printHashes(label string, hs []string) {
	for _, h := range hs {
		fmt.Printf("%s: %s\n", label, h)
	}
}
