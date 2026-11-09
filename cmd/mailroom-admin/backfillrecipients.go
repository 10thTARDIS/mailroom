// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/crypto"
	"mailroom/internal/mime"
	"mailroom/internal/rcptproj"
	"mailroom/internal/store"
	"mailroom/internal/syncrunner"
)

// `mailroom-admin backfill-recipients [--apply] [--user <id>] [--batch N]` —
// fill the searchable recipient and List-Id columns (migration 00095, ADR-055)
// for mail stored before they existed. Until it has run, that mail matches none
// of the JMAP to/cc/bcc/List-Id filters and sorts as empty on `to`.
//
// ⚠ REPORT BY DEFAULT, --apply OPT-IN. A backfill is the canonical "it ran and
// changed nothing" risk (#177, #191, #206). The default run reads and parses every
// message that still needs filling and reports what --apply would write, per
// signal; it writes nothing. The report and the apply walk the SAME loop (the
// cursor advances over candidates, never restarts), so the path a report
// exercises is the path an apply takes.
//
// What it reports, never as one total:
//
//	messages            the user's messages, and how many were already filled
//	candidates          messages with NULL columns: the set this run visits
//	would fill/filled   candidates whose bytes parsed
//	with To/Cc/Bcc/List-Id   how many of those carry each signal (a column that is
//	                    always empty looks the same as "nothing to do" in a total)
//	truncated           candidates with more recipients than the stored bound
//	unreadable          bytes missing, failed integrity, or would not parse. Counted
//	                    and left NULL; never fatal, never half-written
//	lost a race         the guarded write found the row already filled
//
// It appends NO change_log row: no JMAP-visible Email property changes (to, cc,
// bcc and header:List-Id are read from the message file exactly as before).
// Idempotent and resumable: the candidate predicate is rcpt_to IS NULL and the
// write is guarded by it, so a second --apply, or two overlapping runs, change
// nothing the first did not, and an interrupted run loses at most one batch.

const backfillRecipientsBatch = 500

type recipientBackfillAgg struct {
	users                int
	total, alreadyFilled int
	candidates           int
	parsed               int // would fill (report) / filled (apply), before races
	withTo, withCc       int
	withBcc, withList    int
	truncated            int
	unreadable           int
	written              int
	lostRace             int
}

func (a *recipientBackfillAgg) merge(b recipientBackfillAgg) {
	a.users += b.users
	a.total += b.total
	a.alreadyFilled += b.alreadyFilled
	a.candidates += b.candidates
	a.parsed += b.parsed
	a.withTo += b.withTo
	a.withCc += b.withCc
	a.withBcc += b.withBcc
	a.withList += b.withList
	a.truncated += b.truncated
	a.unreadable += b.unreadable
	a.written += b.written
	a.lostRace += b.lostRace
}

func runBackfillRecipients(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("backfill-recipients", flag.ContinueOnError)
	userFlag := fs.String("user", "", "restrict to one user id (default: every active user)")
	apply := fs.Bool("apply", false, "write the columns (default: report what would be written)")
	batch := fs.Int("batch", backfillRecipientsBatch, "messages per batch (one short transaction each)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *batch < 1 {
		return fmt.Errorf("backfill-recipients: --batch must be at least 1")
	}

	d, err := openDeps(ctx)
	if err != nil {
		return err
	}
	defer d.close()

	users, err := d.store.ListActiveUserIDs(ctx)
	if err != nil {
		return fmt.Errorf("backfill-recipients: list users: %w", err)
	}
	if *userFlag != "" {
		u, perr := ulid.Parse(*userFlag)
		if perr != nil {
			return fmt.Errorf("backfill-recipients: parse --user: %w", perr)
		}
		users = []ulid.ULID{u}
	}

	blobs, err := openBlobStore(ctx, d)
	if err != nil {
		return err
	}
	master, err := d.keys.MasterKey(ctx)
	if err != nil {
		return fmt.Errorf("backfill-recipients: master key: %w", err)
	}
	defer crypto.Zero(master)

	var total recipientBackfillAgg
	for _, uid := range users {
		dek, err := crypto.DeriveKey(master, uid[:], syncrunner.MailBlobInfoV1)
		if err != nil {
			return fmt.Errorf("backfill-recipients: derive mail dek: %w", err)
		}
		read := func(ctx context.Context, hash []byte) ([]byte, error) {
			return readMailBlob(ctx, blobs, uid, dek, hash)
		}
		agg, err := backfillRecipientsForUser(ctx, d.store, uid, read, *apply, *batch)
		crypto.Zero(dek)
		if err != nil {
			return err
		}
		total.merge(agg)
	}
	printRecipientBackfill(os.Stdout, total, *apply)
	return nil
}

// backfillRecipientsForUser walks one user's candidates in id order. The reader
// is injected (the real one decrypts from the blob store; the integration test
// passes fixtures, so the parsing it exercises is the real parser).
func backfillRecipientsForUser(ctx context.Context, st *store.Store, userID ulid.ULID, read rawReader, apply bool, batch int) (recipientBackfillAgg, error) {
	agg := recipientBackfillAgg{users: 1}
	if err := st.UserTx(ctx, userID, func(tx *store.UserTx) error {
		c, e := tx.RecipientCensus(ctx)
		agg.total, agg.alreadyFilled = c.Total, c.Filled
		return e
	}); err != nil {
		return agg, fmt.Errorf("backfill-recipients: census: %w", err)
	}

	var after ulid.ULID
	for {
		var cands []store.RecipientCandidate
		if err := st.UserTx(ctx, userID, func(tx *store.UserTx) error {
			var e error
			cands, e = tx.MessagesMissingRecipients(ctx, after, batch)
			return e
		}); err != nil {
			return agg, fmt.Errorf("backfill-recipients: list candidates: %w", err)
		}
		if len(cands) == 0 {
			return agg, nil
		}

		// Read and parse outside any transaction (a blob read is file I/O and a
		// decrypt), then write the batch in ONE short transaction.
		type fill struct {
			id   ulid.ULID
			cols store.RecipientCols
		}
		var fills []fill
		for _, c := range cands {
			after = c.ID
			agg.candidates++
			raw, err := read(ctx, c.ContentHash)
			if err != nil {
				// Not silently skipped: a message whose bytes cannot be read is one
				// this backfill did not fill, and folding it into "nothing to do"
				// would overstate coverage.
				agg.unreadable++
				continue
			}
			parsed, perr := mime.ParseBytes(raw)
			if perr != nil || parsed == nil {
				agg.unreadable++
				continue
			}
			cols := rcptproj.Project(parsed)
			agg.parsed++
			if cols.To != "" {
				agg.withTo++
			}
			if cols.Cc != "" {
				agg.withCc++
			}
			if cols.Bcc != "" {
				agg.withBcc++
			}
			if cols.ListIDKey != "" {
				agg.withList++
			}
			if cols.Truncated {
				agg.truncated++
			}
			fills = append(fills, fill{c.ID, cols})
		}

		if apply && len(fills) > 0 {
			var wrote, lost int
			if err := st.UserTx(ctx, userID, func(tx *store.UserTx) error {
				wrote, lost = 0, 0
				for _, f := range fills {
					ok, e := tx.FillRecipients(ctx, f.id, f.cols)
					if e != nil {
						return e
					}
					if ok {
						wrote++
					} else {
						lost++
					}
				}
				return nil
			}); err != nil {
				return agg, fmt.Errorf("backfill-recipients: write batch: %w", err)
			}
			agg.written += wrote
			agg.lostRace += lost
		}
		if len(cands) < batch {
			return agg, nil
		}
	}
}

func printRecipientBackfill(w io.Writer, a recipientBackfillAgg, apply bool) {
	fillVerb := "would fill"
	if apply {
		fillVerb = "filled"
	}
	fmt.Fprintf(w, "\n=== backfill-recipients ===\n")
	fmt.Fprintf(w, "  users examined                       %d\n", a.users)
	fmt.Fprintf(w, "  messages                             %d\n", a.total)
	fmt.Fprintf(w, "  already filled (skipped)             %d\n", a.alreadyFilled)
	fmt.Fprintf(w, "  candidates (columns still NULL)      %d\n", a.candidates)
	if apply {
		fmt.Fprintf(w, "  %-36s %d\n", fillVerb, a.written)
		fmt.Fprintf(w, "  lost a race (already filled)         %d\n", a.lostRace)
	} else {
		fmt.Fprintf(w, "  %-36s %d\n", fillVerb, a.parsed)
	}
	fmt.Fprintf(w, "    with a To                          %d\n", a.withTo)
	fmt.Fprintf(w, "    with a Cc                          %d\n", a.withCc)
	fmt.Fprintf(w, "    with a Bcc (sent or drafted mail)  %d\n", a.withBcc)
	fmt.Fprintf(w, "    with a List-Id                     %d\n", a.withList)
	fmt.Fprintf(w, "  recipients past the stored bound     %d\n", a.truncated)
	fmt.Fprintf(w, "  unreadable (bytes or parse)          %d\n", a.unreadable)

	switch {
	case a.candidates == 0:
		fmt.Fprintf(w, "  — nothing to do: every message already has its recipient columns.\n")
	case !apply:
		fmt.Fprintf(w, "  — REPORT ONLY. Nothing was written. Re-run with --apply.\n")
	case a.unreadable > 0:
		fmt.Fprintf(w, "  ⚠ %d message(s) could not be read and stay unfilled; they match no recipient\n", a.unreadable)
		fmt.Fprintf(w, "    filter. A re-run retries them.\n")
	}
}
