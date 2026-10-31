// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// `mailroom-admin contact-dates-backfill --user-id U [--apply] [--list]` (#816,
// ADR-041 §5): correct contacts' first/last-seen dates to the true mail dates
// contact-dates-report measures, and save the contacts the user has written to.
//
// REPORT FIRST. Without --apply it writes NOTHING: it prints the report's own
// buckets, then exactly what it would change, per signal and per bucket:
//
//	first_seen_at / last_seen_at   set to the true date where the stored one
//	                               is a day or more away (the report's
//	                               "within 1 d" bucket is left alone)
//	saved_at                       set on contacts with outbound_count > 0
//	                               that are not saved
//
// With --apply it makes those changes and prints the same counts as done, plus
// how many fixes were skipped as stale (ingest moved the row between the read
// and the write; never forced). A rerun then plans 0 changes: the dates it
// wrote are the true dates, and a saved contact is not saved again. --list
// prints every planned change, one line each.
//
// THE TRUE DATES are exactly the report's (trueDates in contactdatesreport.go):
// messages.received_at, From side and sent side. Addresses with no mail found
// are untouched, and counted.
//
// CHANGE ROWS: one Contact `updated` row per contact it changed, in the same
// savepoint as the batch's writes (store.ApplyContactBackfillBatch). A
// dates-only change counts: firstMailAt and lastMailAt are Contact properties.
func runContactDatesBackfill(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("contact-dates-backfill", flag.ContinueOnError)
	userFlag := fs.String("user-id", "", "user whose contacts to correct (required)")
	apply := fs.Bool("apply", false, "make the changes (default: report only, write nothing)")
	list := fs.Bool("list", false, "print every planned change")
	sample := fs.Int("sample", 10, "addresses the report section lists")
	batch := fs.Int("batch", 500, "messages per page of the sent-mail walk")
	perTx := fs.Int("contacts-per-batch", 200, "contacts written per transaction with --apply")
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
	read, done, err := openMailReader(ctx, d, userID, "contact-dates-backfill")
	if err != nil {
		return err
	}
	defer done()
	_, err = backfillContactDates(ctx, os.Stdout, d.store, userID, read, backfillOptions{
		apply: *apply, list: *list, sample: *sample, batch: *batch, perTx: *perTx, now: time.Now().UTC(),
	})
	return err
}

type backfillOptions struct {
	apply, list  bool
	sample       int
	batch, perTx int
	now          time.Time
}

// contactDatesPlan is what the backfill would change.
type contactDatesPlan struct {
	fixes []store.ContactDateFix
	save  []ulid.ULID
	// per-signal buckets of the planned fixes (no "within" entries by
	// construction), and how many addresses were left alone and why.
	first, last           directionCounts
	firstFixes, lastFixes int
	withinBoth            int
	noMail                int
	contacts              map[ulid.ULID]bool
}

// planContactDates builds the plan from the same input the report reads.
func planContactDates(in contactDatesInput, save []ulid.ULID) *contactDatesPlan {
	p := &contactDatesPlan{save: save, contacts: map[ulid.ULID]bool{}}
	for _, r := range in.rows {
		trueFirst, trueLast, _, _, ok := trueDates(r, in.sent)
		if !ok {
			p.noMail++
			continue
		}
		fixFirst := absDur(r.FirstSeenAt.Sub(trueFirst)) >= day
		fixLast := absDur(r.LastSeenAt.Sub(trueLast)) >= day
		if !fixFirst && !fixLast {
			p.withinBoth++
			continue
		}
		if fixFirst {
			p.firstFixes++
			p.first.add(r.FirstSeenAt, trueFirst)
		}
		if fixLast {
			p.lastFixes++
			p.last.add(r.LastSeenAt, trueLast)
		}
		p.fixes = append(p.fixes, store.ContactDateFix{
			ContactID: r.ContactID, Address: r.Address,
			OldFirst: r.FirstSeenAt, OldLast: r.LastSeenAt,
			NewFirst: trueFirst, NewLast: trueLast,
			FixFirst: fixFirst, FixLast: fixLast,
		})
		p.contacts[r.ContactID] = true
	}
	for _, id := range save {
		p.contacts[id] = true
	}
	return p
}

func absDur(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// backfillDone is what --apply did.
type backfillDone struct {
	store.ContactBackfillBatch
	batches int
}

// backfillContactDates is the verb's body: measure, print, and (with apply)
// write. It returns the plan, for tests.
func backfillContactDates(ctx context.Context, w io.Writer, st *store.Store, userID ulid.ULID, read rawReader, o backfillOptions) (*contactDatesPlan, error) {
	in, err := gatherContactDates(ctx, st, userID, read, o.batch)
	if err != nil {
		return nil, fmt.Errorf("contact-dates-backfill: %w", err)
	}
	var save []ulid.ULID
	if err := st.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		save, e = tx.UnsavedWrittenToContacts(ctx)
		return e
	}); err != nil {
		return nil, fmt.Errorf("contact-dates-backfill: contacts to save: %w", err)
	}
	rep := buildContactDatesReport(in.rows, in.sent, o.now, o.sample)
	rep.sent = in.stats
	plan := planContactDates(in, save)

	mode := "DRY RUN (no --apply): nothing is written"
	if o.apply {
		mode = "--apply: writing"
	}
	_, _ = fmt.Fprintf(w, "contact-dates-backfill; %s\n\n", mode)
	_, _ = fmt.Fprintln(w, "---- the measurement (contact-dates-report) ----")
	rep.fprint(w)
	_, _ = fmt.Fprintln(w, "\n---- the plan ----")
	plan.fprint(w, "would")
	if o.list {
		plan.fprintList(w)
	}
	if !o.apply {
		_, _ = fmt.Fprintln(w, "\nnothing written; rerun with --apply to make these changes")
		return plan, nil
	}
	res, err := applyContactDatesPlan(ctx, st, userID, plan, o.perTx)
	if err != nil {
		return plan, err
	}
	_, _ = fmt.Fprintln(w, "\n---- done ----")
	_, _ = fmt.Fprintf(w, "first_seen_at set:              %d addresses\n", res.FirstFixed)
	_, _ = fmt.Fprintf(w, "last_seen_at set:               %d addresses\n", res.LastFixed)
	_, _ = fmt.Fprintf(w, "saved:                          %d contacts\n", res.Saved)
	_, _ = fmt.Fprintf(w, "skipped as stale (row moved):   %d addresses\n", res.Stale)
	_, _ = fmt.Fprintf(w, "Contact change rows written:    %d (one per changed contact), in %d transactions\n", res.Contacts, res.batches)
	return plan, nil
}

// applyContactDatesPlan writes the plan in batches of whole contacts, so a
// contact with several addresses gets one change row.
func applyContactDatesPlan(ctx context.Context, st *store.Store, userID ulid.ULID, plan *contactDatesPlan, perTx int) (backfillDone, error) {
	var done backfillDone
	if perTx <= 0 {
		perTx = 200
	}
	byContact := map[ulid.ULID][]store.ContactDateFix{}
	for _, f := range plan.fixes {
		byContact[f.ContactID] = append(byContact[f.ContactID], f)
	}
	saving := map[ulid.ULID]bool{}
	for _, id := range plan.save {
		saving[id] = true
	}
	ids := make([]ulid.ULID, 0, len(plan.contacts))
	for id := range plan.contacts {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].Compare(ids[j]) < 0 })
	for start := 0; start < len(ids); start += perTx {
		end := min(start+perTx, len(ids))
		var fixes []store.ContactDateFix
		var save []ulid.ULID
		for _, id := range ids[start:end] {
			fixes = append(fixes, byContact[id]...)
			if saving[id] {
				save = append(save, id)
			}
		}
		if err := st.UserTx(ctx, userID, func(tx *store.UserTx) error {
			r, e := tx.ApplyContactBackfillBatch(ctx, fixes, save)
			if e != nil {
				return e
			}
			done.FirstFixed += r.FirstFixed
			done.LastFixed += r.LastFixed
			done.Stale += r.Stale
			done.Saved += r.Saved
			done.Contacts += r.Contacts
			return nil
		}); err != nil {
			return done, fmt.Errorf("contact-dates-backfill: apply batch %d: %w", done.batches+1, err)
		}
		done.batches++
	}
	return done, nil
}

func (p *contactDatesPlan) fprint(w io.Writer, verb string) {
	_, _ = fmt.Fprintf(w, "first_seen_at: %s set %d addresses to the true first mail\n", verb, p.firstFixes)
	fprintPlanDirection(w, &p.first)
	_, _ = fmt.Fprintf(w, "last_seen_at:  %s set %d addresses to the true last mail\n", verb, p.lastFixes)
	fprintPlanDirection(w, &p.last)
	_, _ = fmt.Fprintf(w, "saved_at:      %s save %d contacts (written to, outbound_count > 0, not saved)\n", verb, len(p.save))
	_, _ = fmt.Fprintf(w, "contacts changed (= change rows): %d\n", len(p.contacts))
	_, _ = fmt.Fprintf(w, "left alone: %d addresses within 1 d on both dates; %d addresses with no mail found\n", p.withinBoth, p.noMail)
}

func fprintPlanDirection(w io.Writer, c *directionCounts) {
	for i := bucketDay30; i < numBuckets; i++ {
		_, _ = fmt.Fprintf(w, "  stored later,   %-9s %d\n", bucketNames[i]+":", c.later[i])
	}
	for i := bucketDay30; i < numBuckets; i++ {
		_, _ = fmt.Fprintf(w, "  stored earlier, %-9s %d\n", bucketNames[i]+":", c.earlier[i])
	}
}

func (p *contactDatesPlan) fprintList(w io.Writer) {
	_, _ = fmt.Fprintln(w, "\nplanned changes:")
	for _, f := range p.fixes {
		if f.FixFirst {
			_, _ = fmt.Fprintf(w, "  %s first %s -> %s\n", f.Address, fmtDate(f.OldFirst), fmtDate(f.NewFirst))
		}
		if f.FixLast {
			_, _ = fmt.Fprintf(w, "  %s last  %s -> %s\n", f.Address, fmtDate(f.OldLast), fmtDate(f.NewLast))
		}
	}
	for _, id := range p.save {
		_, _ = fmt.Fprintf(w, "  save contact %s\n", id)
	}
}
