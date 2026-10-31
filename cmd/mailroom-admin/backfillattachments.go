// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"flag"
	"fmt"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/attachproj"
	"mailroom/internal/crypto"
	"mailroom/internal/mime"
	"mailroom/internal/store"
	"mailroom/internal/syncrunner"
)

// `mailroom-admin backfill-attachments` — populate app.attachments for mail
// that predates the projection (#393, ADR-027 §3 increment 2).
//
// ⚠ REPORT BY DEFAULT, --apply OPT-IN. A backfill is the canonical
// "it ran and changed nothing" risk this project keeps paying for (#177, #191,
// #206), and this one has a second failure mode on top: it can also run and
// change something WRONG, because it re-derives a projection from bytes rather
// than copying a stored value.
//
// ⚠ THE COVERAGE MEASUREMENT IS NOT HERE. `attachment-report` already prints
// the projection census, and it shipped in v1.57.0 deliberately BEFORE this
// command existed, so the effect of a backfill is judged by a tool that
// predates it rather than by this command grading its own work.
//
// What this reports per run, never as one total:
//
//	candidates   messages with has_attachments and no projected rows
//	projected    messages this run wrote rows for
//	parts        rows written
//	unreadable   messages whose bytes could not be read or parsed
//	empty        messages the parser found NO attachment parts in, despite
//	             has_attachments being true — a real disagreement, not a no-op
//
// That last bucket is the one worth watching. It cannot be folded into
// "projected 0" without losing the distinction between "nothing to do" and
// "the flag and the bytes disagree", which is the #377 defect's signature.

const backfillAttachmentsChunk = 500

type attachBackfillAgg struct {
	candidates int
	projected  int
	parts      int
	unreadable int
	empty      int
}

func (a *attachBackfillAgg) merge(b attachBackfillAgg) {
	a.candidates += b.candidates
	a.projected += b.projected
	a.parts += b.parts
	a.unreadable += b.unreadable
	a.empty += b.empty
}

func runBackfillAttachments(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("backfill-attachments", flag.ContinueOnError)
	userFlag := fs.String("user", "", "restrict to one user id (default: every active user)")
	apply := fs.Bool("apply", false, "write the rows (default: report what would be written)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	d, err := openDeps(ctx)
	if err != nil {
		return err
	}
	defer d.close()

	users, err := d.store.ListActiveUserIDs(ctx)
	if err != nil {
		return fmt.Errorf("backfill-attachments: list users: %w", err)
	}
	if *userFlag != "" {
		u, perr := ulid.Parse(*userFlag)
		if perr != nil {
			return fmt.Errorf("backfill-attachments: parse -user: %w", perr)
		}
		users = []ulid.ULID{u}
	}

	var total attachBackfillAgg
	for _, uid := range users {
		agg, err := backfillAttachmentsForUser(ctx, d, uid, *apply)
		if err != nil {
			return err
		}
		total.merge(agg)
	}

	fmt.Printf("\n=== backfill-attachments ===\n")
	fmt.Printf("  users examined                 %d\n", len(users))
	fmt.Printf("  candidates (flag set, no rows) %d\n", total.candidates)
	fmt.Printf("  messages projected             %d\n", total.projected)
	fmt.Printf("  attachment rows written        %d\n", total.parts)
	fmt.Printf("  unreadable (blob or parse)     %d\n", total.unreadable)
	fmt.Printf("  ⚠ flag set but NO parts found  %d\n", total.empty)

	switch {
	case total.candidates == 0:
		fmt.Printf("  — nothing to do: every message carrying attachments already has rows.\n")
	case !*apply:
		fmt.Printf("  — REPORT ONLY. Nothing was written. Re-run with --apply.\n")
	case total.empty > 0:
		fmt.Printf("  ⚠ %d message(s) claim attachments the parser cannot find. Those are NOT\n", total.empty)
		fmt.Printf("    projected — there is nothing to project — and they are the same\n")
		fmt.Printf("    flag-vs-bytes disagreement #377 was. Run `attachment-report` to see them\n")
		fmt.Printf("    per message, with ids and direction.\n")
	}
	fmt.Printf("\n  Verify with `mailroom-admin attachment-report`, which measures coverage\n")
	fmt.Printf("  independently and existed before this command did.\n")
	return nil
}

func backfillAttachmentsForUser(ctx context.Context, d *deps, userID ulid.ULID, apply bool) (attachBackfillAgg, error) {
	var agg attachBackfillAgg

	blobs, err := openBlobStore(ctx, d)
	if err != nil {
		return agg, err
	}
	master, err := d.keys.MasterKey(ctx)
	if err != nil {
		return agg, fmt.Errorf("backfill-attachments: master key: %w", err)
	}
	dek, err := crypto.DeriveKey(master, userID[:], syncrunner.MailBlobInfoV1)
	crypto.Zero(master)
	if err != nil {
		return agg, fmt.Errorf("backfill-attachments: derive mail dek: %w", err)
	}
	defer crypto.Zero(dek)

	// Walk in ULID order in bounded chunks. ⚠ The cursor advances over
	// CANDIDATES, and --apply removes rows from that set as it goes, so the
	// query is re-run from the last id rather than restarted — otherwise a
	// report run (which changes nothing) and an apply run would need different
	// loops, and only one of them would ever be exercised.
	var after ulid.ULID
	for {
		var batch []ulid.ULID
		if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
			var e error
			batch, e = tx.MessagesMissingAttachmentRows(ctx, after, backfillAttachmentsChunk)
			return e
		}); err != nil {
			return agg, fmt.Errorf("backfill-attachments: list candidates: %w", err)
		}
		if len(batch) == 0 {
			return agg, nil
		}

		for _, msgID := range batch {
			after = msgID
			agg.candidates++

			var hash []byte
			if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
				ref, e := tx.MessageAuthRefByID(ctx, msgID)
				if e != nil {
					return e
				}
				hash = ref.ContentHash
				return nil
			}); err != nil {
				return agg, fmt.Errorf("backfill-attachments: read message %s: %w", msgID, err)
			}

			raw, err := readMailBlob(ctx, blobs, userID, dek, hash)
			if err != nil {
				// NOT silently skipped: a message whose bytes cannot be read is
				// one this backfill did not project, and letting it fall into
				// "nothing to do" would overstate coverage.
				agg.unreadable++
				d.log.Warn("backfill-attachments: read blob failed", "message", msgID, "err", err)
				continue
			}
			parsed, perr := mime.ParseBytes(raw)
			if perr != nil || parsed == nil {
				agg.unreadable++
				d.log.Warn("backfill-attachments: parse failed", "message", msgID, "err", perr)
				continue
			}

			rows := attachproj.Rows(parsed)
			if len(rows) == 0 {
				// has_attachments is true and the parser disagrees. Counted
				// separately, never projected as "zero rows written".
				agg.empty++
				continue
			}
			agg.projected++
			agg.parts += len(rows)

			if !apply {
				continue
			}
			if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
				return tx.ReplaceAttachments(ctx, msgID, rows)
			}); err != nil {
				return agg, fmt.Errorf("backfill-attachments: write message %s: %w", msgID, err)
			}
		}

		if len(batch) < backfillAttachmentsChunk {
			return agg, nil
		}
	}
}
