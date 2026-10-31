// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// `mailroom-admin contact-names-clean --user-id U [--apply] [--sample N]` (#831):
// clean the learned display names stored before ingest started cleaning them
// (store.CleanLearnedContactName, the same sanitiser ingest uses).
//
// REPORT FIRST. Without --apply it writes NOTHING: it prints how many contacts
// there are with a display name, how many of those contain each kind of
// character (bidi controls, other C0/C1 controls, zero-width and other format
// characters; a name with several kinds counts under each), how many would
// become empty, and a few examples with the invisible characters escaped. With
// --apply it makes those changes and prints the same counts as done. A rerun
// plans 0: a cleaned name has none of those characters.
//
// CHANGE ROWS: one Contact `updated` row per changed contact, in the same
// savepoint as the batch's writes (store.ApplyContactNameFixes).
func runContactNamesClean(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("contact-names-clean", flag.ContinueOnError)
	userFlag := fs.String("user-id", "", "user whose contacts to clean (required)")
	apply := fs.Bool("apply", false, "make the changes (default: report only, write nothing)")
	sample := fs.Int("sample", 5, "examples to print")
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
	_, err = cleanContactNames(ctx, os.Stdout, d.store, userID, *apply, *sample, *perTx)
	return err
}

// contactNamesPlan is what the verb would change.
type contactNamesPlan struct {
	total                 int // contacts with a display name
	bidi, control, format int // names containing each kind
	toEmpty               int
	fixes                 []store.ContactNameFix
	examples              []string
}

func planContactNames(names []store.ContactName, sample int) *contactNamesPlan {
	p := &contactNamesPlan{total: len(names)}
	for _, n := range names {
		k := store.ClassifyContactName(n.Name)
		if !k.Any() {
			continue
		}
		if k.Bidi {
			p.bidi++
		}
		if k.Control {
			p.control++
		}
		if k.Format {
			p.format++
		}
		clean := store.CleanLearnedContactName(n.Name)
		if clean == "" {
			p.toEmpty++
		}
		p.fixes = append(p.fixes, store.ContactNameFix{ID: n.ID, Old: n.Name, New: clean})
		if len(p.examples) < sample {
			p.examples = append(p.examples, fmt.Sprintf("%s  %s -> %s", n.ID, escapeInvisible(n.Name), escapeInvisible(clean)))
		}
	}
	return p
}

// escapeInvisible renders every rune that is not plainly printable as \uXXXX.
func escapeInvisible(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		if store.ClassifyContactName(string(r)).Any() || r == '�' {
			fmt.Fprintf(&b, "\\u%04X", r)
			continue
		}
		b.WriteRune(r)
	}
	b.WriteByte('"')
	return b.String()
}

func cleanContactNames(ctx context.Context, w io.Writer, st *store.Store, userID ulid.ULID, apply bool, sample, perTx int) (*contactNamesPlan, error) {
	var names []store.ContactName
	if err := st.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		names, e = tx.ContactDisplayNames(ctx)
		return e
	}); err != nil {
		return nil, fmt.Errorf("contact-names-clean: %w", err)
	}
	plan := planContactNames(names, sample)
	mode := "DRY RUN (no --apply): nothing is written"
	if apply {
		mode = "--apply: writing"
	}
	verb := "would clean"
	_, _ = fmt.Fprintf(w, "contact-names-clean; %s\n\n", mode)
	_, _ = fmt.Fprintf(w, "contacts with a display name:       %d\n", plan.total)
	_, _ = fmt.Fprintf(w, "  containing bidi controls:         %d\n", plan.bidi)
	_, _ = fmt.Fprintf(w, "  containing other C0/C1 controls:  %d\n", plan.control)
	_, _ = fmt.Fprintf(w, "  containing zero-width/format:     %d\n", plan.format)
	_, _ = fmt.Fprintf(w, "(a name with several kinds counts under each)\n")
	_, _ = fmt.Fprintf(w, "%s %d names (= change rows), %d of them to empty (the UI then shows the address)\n", verb, len(plan.fixes), plan.toEmpty)
	for _, e := range plan.examples {
		_, _ = fmt.Fprintf(w, "  e.g. %s\n", e)
	}
	if !apply {
		_, _ = fmt.Fprintln(w, "\nnothing written; rerun with --apply to make these changes")
		return plan, nil
	}
	if perTx <= 0 {
		perTx = 200
	}
	var changed, stale, batches int
	for start := 0; start < len(plan.fixes); start += perTx {
		batch := plan.fixes[start:min(start+perTx, len(plan.fixes))]
		if err := st.UserTx(ctx, userID, func(tx *store.UserTx) error {
			c, s, e := tx.ApplyContactNameFixes(ctx, batch)
			changed += c
			stale += s
			return e
		}); err != nil {
			return plan, fmt.Errorf("contact-names-clean: apply batch %d: %w", batches+1, err)
		}
		batches++
	}
	_, _ = fmt.Fprintln(w, "\n---- done ----")
	_, _ = fmt.Fprintf(w, "names cleaned:                    %d\n", changed)
	_, _ = fmt.Fprintf(w, "skipped as stale (name moved):    %d\n", stale)
	_, _ = fmt.Fprintf(w, "Contact change rows written:      %d (one per changed contact), in %d transactions\n", changed, batches)
	return plan, nil
}
