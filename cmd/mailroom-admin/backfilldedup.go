// SPDX-License-Identifier: AGPL-3.0-only

package main

// `mailroom-admin backfill-dedup-hash` — populate dedup_hash for mail already held (#206).
//
// WHY THIS IS NEEDED FOR CORRECTNESS, not tidiness. Migration 00023 adds the column
// nullable, and ingest writes it for every NEW message. But the lookup matches on the
// column, so a message ingested BEFORE this feature has a NULL dedup_hash and matches
// nothing — meaning the next re-delivery of it still inserts a second row. Until this
// has run, the fix only protects mail that arrived after it.
//
// It has to be a Go command rather than a SQL UPDATE because the hash is computed over
// the message BODY, and the body lives in an encrypted, content-addressed blob. Every
// row means one blob read and one decrypt.
//
// DRY RUN IS THE DEFAULT, and the report is the interesting part: it says how many rows
// WOULD get a hash and — more usefully — how many DISTINCT messages the existing rows
// collapse to, which is the count of redundant rows sitting in the mailbox right now.
// That number is what part 3 (the actual collapse) has to deal with, and seeing it before
// touching anything is the point.
//
// WRITING THE HASH DOES NOT MERGE ANYTHING. It records a fact about each row and appends
// no change_log entry, because nothing JMAP-visible changes: dedup_hash is not an Email
// property and no client can see it. Collapsing the rows that now share a hash is a
// separate, destructive step with its own dry run — this command deliberately cannot do
// it, so that "populate the column" and "delete rows" can never be one keystroke.

import (
	"context"
	"encoding/hex"
	"flag"
	"fmt"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/crypto"
	"mailroom/internal/ingest"
	"mailroom/internal/store"
	"mailroom/internal/syncrunner"
)

func runBackfillDedupHash(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("backfill-dedup-hash", flag.ContinueOnError)
	var (
		userIDStr = fs.String("user-id", "", "operator user id (ULID)")
		apply     = fs.Bool("apply", false, "write dedup_hash (default: dry-run, report only)")
		dryRun    = fs.Bool("dry-run", true, "report without writing (overridden by -apply)")
		batch     = fs.Int("batch", 500, "message id page size for the keyset scan")
		recompute = fs.Bool("recompute", false,
			"also rehash rows that already have one (use after changing DedupHash)")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	userID, err := parseUserID(*userIDStr)
	if err != nil {
		return err
	}
	write := *apply || !*dryRun

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
		scanned, hashed, errCount int
		// Which messages share a computed hash — the redundancy that is already in
		// the database. Counted here because this scan is the only place that sees
		// every hash at once, and the number is what part 3 has to act on.
		byHash = map[string][]string{}
	)

	var after ulid.ULID
	for {
		var refs []store.DedupBackfillRef
		if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
			var e error
			refs, e = tx.DedupBackfillRefsAfter(ctx, after, *batch, *recompute)
			return e
		}); err != nil {
			return fmt.Errorf("scan messages: %w", err)
		}
		if len(refs) == 0 {
			break
		}

		for _, ref := range refs {
			after = ref.ID
			scanned++

			raw, err := readMailBlob(ctx, blobs, userID, dek, ref.ContentHash)
			if err != nil {
				// A missing or undecryptable blob is reported and skipped, never
				// fatal: one bad row must not stop a backfill over a whole mailbox,
				// and a row left NULL is exactly as safe as it was before.
				errCount++
				d.log.Warn("backfill-dedup-hash: read blob failed", "message", ref.ID, "err", err)
				continue
			}
			h := ingest.DedupHash(raw)
			key := hex.EncodeToString(h[:])
			byHash[key] = append(byHash[key], ref.ID.String())

			if !write {
				hashed++
				continue
			}
			if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
				return tx.SetMessageDedupHash(ctx, ref.ID, h[:])
			}); err != nil {
				errCount++
				d.log.Warn("backfill-dedup-hash: write failed", "message", ref.ID, "err", err)
				continue
			}
			hashed++
		}
		if len(refs) < *batch {
			break
		}
	}

	// The redundancy already present, which is the number worth reading.
	groups, redundant := 0, 0
	for _, ids := range byHash {
		if len(ids) > 1 {
			groups++
			redundant += len(ids) - 1
		}
	}

	// Asked of the DATABASE afterwards rather than inferred from the loop. "scanned N,
	// wrote N" says nothing about rows the scan never reached; this is the number that
	// actually decides whether every message now participates.
	var remaining int64
	if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		remaining, e = tx.CountMessagesWithoutDedupHash(ctx)
		return e
	}); err != nil {
		return fmt.Errorf("count remaining: %w", err)
	}

	mode := "DRY RUN (nothing written)"
	if write {
		mode = "APPLIED"
	}
	fmt.Printf("=== backfill-dedup-hash: %s ===\n", mode)
	fmt.Printf("  messages scanned                 : %d\n", scanned)
	fmt.Printf("  dedup_hash computed              : %d\n", hashed)
	fmt.Printf("  errors (blob unreadable)         : %d\n", errCount)
	fmt.Printf("  still WITHOUT a dedup_hash       : %d   <- 0 means every message participates\n",
		remaining)
	fmt.Printf("\n  messages sharing a hash with another : %d groups, %d redundant rows\n",
		groups, redundant)
	if groups > 0 {
		fmt.Println("\n  Those redundant rows are already in the mailbox. Writing the hash does NOT")
		fmt.Println("  remove them — collapsing them is #206 part 3, a separate step with its own")
		fmt.Println("  dry run, because it moves provenance and deletes message rows.")
	}
	if !write {
		fmt.Println("\n  Re-run with -apply to write the hashes.")
	}
	return nil
}
