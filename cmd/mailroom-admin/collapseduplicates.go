// SPDX-License-Identifier: AGPL-3.0-only

package main

// `mailroom-admin collapse-duplicates` — repair mail that duplicated before the fix (#206 part 3).
//
// Parts 1 and 2 measured the problem and stopped NEW re-deliveries becoming a second
// message. This is the repair for rows that were already there — on a real
// mailbox that is why some messages still list three and four times.
//
// THIS IS THE DESTRUCTIVE ONE. It deletes message rows and rewrites provenance on the FK
// hub that ADR-001 calls the most expensive thing in the schema to get wrong. So:
//
//   - DRY RUN IS THE DEFAULT, and the dry run prints every group it would touch along
//     with what moves — provenance, labels the keeper lacks, feedback events — so an
//     operator can see the blast radius before authorising it.
//   - FAIL CLOSED. A group with anything unaccounted for is SKIPPED and reported, never
//     partially collapsed: a half-merged group is worse than an un-merged one, because
//     the duplicate is still visible AND its provenance has moved.
//   - ONE TRANSACTION PER GROUP, so one refusal cannot roll back groups that succeeded.
//   - NOTHING IS DONE TO THE MAIL SERVER. The redundant copies exist there too, and
//     removing them from there is a different and much larger decision. This tidies our
//     own rows only.
//
// The blob FILE is deleted INSIDE the transaction, before it commits — the same ordering
// purge.Worker.hardDelete uses, and for the same reason: the row delete is invisible to a
// concurrent ingest until commit, so the window a byte-identical re-ingest would need in
// order to re-Put those bytes opens only after the file is already gone. A file-delete
// failure aborts the transaction, so the failure mode is "nothing happened, retry", never
// a canonical row that outlives its bytes.

import (
	"context"
	"encoding/hex"
	"flag"
	"fmt"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/blob"
	"mailroom/internal/crypto"
	"mailroom/internal/store"
	"mailroom/internal/syncrunner"
)

func runCollapseDuplicates(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("collapse-duplicates", flag.ContinueOnError)
	var (
		userIDStr = fs.String("user-id", "", "operator user id (ULID)")
		apply     = fs.Bool("apply", false, "actually collapse (default: dry-run, report only)")
		dryRun    = fs.Bool("dry-run", true, "report without changing anything (overridden by -apply)")
		limit     = fs.Int("limit", 200, "how many duplicate groups to consider")
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
	// The DEK is not needed to DELETE a blob (no decryption involved), but the blob
	// store's path derivation is per-user and already handled by openBlobStore. The
	// master key is still zeroed promptly out of habit rather than necessity.
	master, err := d.keys.MasterKey(ctx)
	if err != nil {
		return fmt.Errorf("master key: %w", err)
	}
	dek, err := crypto.DeriveKey(master, userID[:], syncrunner.MailBlobInfoV1)
	crypto.Zero(master)
	if err != nil {
		return fmt.Errorf("derive mail dek: %w", err)
	}
	crypto.Zero(dek)

	var groups []store.CollapseCandidate
	if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		groups, e = tx.DuplicateCollapseCandidates(ctx, *limit)
		return e
	}); err != nil {
		return fmt.Errorf("find collapse candidates: %w", err)
	}

	if len(groups) == 0 {
		fmt.Println("no duplicate groups to collapse — every dedup_hash is unique")
		fmt.Println("(if you expected some, run backfill-dedup-hash first: a NULL hash does not group)")
		return nil
	}

	var (
		wouldCollapse, wouldSkip               int
		wouldDelete, wouldMoveProv, wouldUnion int
		wouldMoveFeedback, wouldUpgrade        int
		wouldPlacement, wouldPlacementConflict int
		wouldCorrectionWins                    int
		wouldJournal, wouldJournalMerge        int
		movedPlacement, replacedPlacement      int
		droppedPlacement, movedJournal         int
		mergedJournal                          int
		collapsed, skipped, failed             int
		deleted, movedProv, unioned, movedFb   int
		upgraded                               int
		threadsDeleted, blobsReclaimed         int
	)

	for _, g := range groups {
		blockers := g.Blockers()
		if len(blockers) > 0 {
			wouldSkip++
			fmt.Printf("SKIP  keeper %s (%d copies) — cannot be collapsed:\n",
				g.KeeperID, len(g.Losers)+1)
			for _, b := range blockers {
				fmt.Printf("        %s\n", b)
			}
			if write {
				skipped++
			}
			continue
		}

		wouldCollapse++
		loserIDs := make([]ulid.ULID, 0, len(g.Losers))
		for _, l := range g.Losers {
			loserIDs = append(loserIDs, l.ID)
			wouldDelete++
			wouldMoveProv += l.ProvenanceRows
			wouldUnion += l.LabelsToUnion
			wouldUpgrade += l.LabelsToUpgrade
			wouldMoveFeedback += l.FeedbackEvents
			wouldPlacement += l.LearnedPlacements
			wouldPlacementConflict += l.LearnedPlacementConflicts
			wouldCorrectionWins += l.LearnedCorrectionsWin
			wouldJournal += l.SetupJournalRows
			wouldJournalMerge += l.SetupJournalConflicts
		}

		if !write {
			fmt.Printf("WOULD keep %s, fold in %d cop%s (hash %s…)\n",
				g.KeeperID, len(g.Losers), plural(len(g.Losers)), hex.EncodeToString(g.DedupHash)[:12])
			for _, l := range g.Losers {
				fmt.Printf("        %s  provenance %d  labels-to-union %d  labels-to-upgrade %d  feedback %d  placements %d (keeper has one: %d, correction wins: %d)  setup-journal %d (same run as another copy: %d)\n",
					l.ID, l.ProvenanceRows, l.LabelsToUnion, l.LabelsToUpgrade, l.FeedbackEvents,
					l.LearnedPlacements, l.LearnedPlacementConflicts, l.LearnedCorrectionsWin,
					l.SetupJournalRows, l.SetupJournalConflicts)
			}
			continue
		}

		// One transaction per group, with the blob files deleted inside it.
		var res store.CollapseResult
		var ok bool
		if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
			var e error
			res, ok, e = tx.CollapseDuplicateGroup(ctx, g.KeeperID, loserIDs)
			if e != nil || !ok {
				return e
			}
			for _, h := range res.ReclaimBlobs {
				var ch blob.ContentHash
				if len(h) != len(ch) {
					return fmt.Errorf("reclaimed hash is %d bytes, want %d", len(h), len(ch))
				}
				copy(ch[:], h)
				if e := blobs.Delete(ctx, userID, ch); e != nil {
					// Aborts the transaction: nothing is destroyed for this group and
					// the next run retries it.
					return fmt.Errorf("reclaim blob file: %w", e)
				}
			}
			return nil
		}); err != nil {
			failed++
			fmt.Printf("FAIL  keeper %s — %v (nothing changed for this group)\n", g.KeeperID, err)
			continue
		}
		if !ok {
			skipped++
			fmt.Printf("SKIP  keeper %s — not safe under the row lock (something changed since the scan)\n",
				g.KeeperID)
			continue
		}
		collapsed++
		deleted += res.MessagesDeleted
		movedProv += res.ProvenanceMoved
		unioned += res.LabelsUnioned
		upgraded += res.LabelsUpgraded
		movedFb += res.FeedbackMoved
		movedPlacement += res.PlacementsMoved
		replacedPlacement += res.PlacementsReplaced
		droppedPlacement += res.PlacementsDropped
		movedJournal += res.JournalMoved
		mergedJournal += res.JournalMerged
		threadsDeleted += res.ThreadsDeleted
		blobsReclaimed += len(res.ReclaimBlobs)
		fmt.Printf("OK    keeper %s — %d deleted, %d provenance moved, %d labels unioned, %d label attributions upgraded, %d feedback moved, %d threads, %d blobs\n",
			g.KeeperID, res.MessagesDeleted, res.ProvenanceMoved, res.LabelsUnioned, res.LabelsUpgraded,
			res.FeedbackMoved, res.ThreadsDeleted, len(res.ReclaimBlobs))
	}

	fmt.Println()
	if !write {
		fmt.Println("=== collapse-duplicates: DRY RUN (nothing changed) ===")
		fmt.Printf("  groups that would collapse       : %d\n", wouldCollapse)
		fmt.Printf("  groups that would be SKIPPED     : %d\n", wouldSkip)
		fmt.Printf("  message rows that would be gone  : %d\n", wouldDelete)
		fmt.Printf("  provenance rows that would MOVE  : %d\n", wouldMoveProv)
		fmt.Printf("  labels that would be preserved   : %d   <- would be LOST by a naive delete\n", wouldUnion)
		fmt.Printf("  label attributions to UPGRADE    : %d   <- the copy's attribution is stronger (e.g. the user's own filing)\n", wouldUpgrade)
		fmt.Printf("  feedback events that would MOVE  : %d   <- user corrections, and training data\n", wouldMoveFeedback)
		fmt.Printf("  learned placements on copies     : %d   <- would MOVE to the kept copy (#882); a correction there needs them\n", wouldPlacement)
		fmt.Printf("    of which the keeper has its own: %d   <- one row per message: a correction wins, else the keeper's\n", wouldPlacementConflict)
		fmt.Printf("    of which a correction replaces : %d   <- the copy's correction outranks the keeper's non-correction\n", wouldCorrectionWins)
		fmt.Printf("  day-one undo rows on copies      : %d   <- would MOVE to the kept copy (#882); undo needs them to take the label back\n", wouldJournal)
		fmt.Printf("    of which share a run with another copy: %d   <- merged into one row\n", wouldJournalMerge)
		fmt.Println("\n  Labels move WITH who applied them (#859): a learned, desk, rule, agent or setup")
		fmt.Println("  label stays that, and never becomes the user's own filing on the kept copy.")
		fmt.Println("\n  Nothing is done to the mail server: the copies there are untouched.")
		fmt.Println("  Re-run with -apply to collapse.")
		return nil
	}

	fmt.Println("=== collapse-duplicates: APPLIED ===")
	fmt.Printf("  groups collapsed                 : %d\n", collapsed)
	fmt.Printf("  groups skipped                   : %d\n", skipped)
	fmt.Printf("  groups failed                    : %d\n", failed)
	fmt.Printf("  message rows deleted             : %d\n", deleted)
	fmt.Printf("  provenance rows moved            : %d\n", movedProv)
	fmt.Printf("  labels preserved on the keeper    : %d\n", unioned)
	fmt.Printf("  label attributions upgraded      : %d\n", upgraded)
	fmt.Printf("  feedback events moved            : %d\n", movedFb)
	fmt.Printf("  learned placements moved         : %d (replacing the keeper's: %d, discarded: %d)\n", movedPlacement, replacedPlacement, droppedPlacement)
	fmt.Printf("  day-one undo rows moved          : %d (folded into the keeper's: %d)\n", movedJournal, mergedJournal)
	fmt.Printf("  empty threads removed            : %d\n", threadsDeleted)
	fmt.Printf("  blobs reclaimed (row + file)     : %d\n", blobsReclaimed)
	if failed > 0 {
		fmt.Println("\n  A failed group changed NOTHING — its transaction aborted. Re-run to retry.")
	}
	return nil
}

func plural(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}
