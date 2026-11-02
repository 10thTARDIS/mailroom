// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// `mailroom-admin area-index [--user-id U] [--apply] [--batch N]` (ADR-049):
// the report and the backfill for the stored resolved area (messages.area_id).
//
// REPORT FIRST. With no flags it writes NOTHING and prints, per user: how many
// messages there are, how many already hold a stored area, whether the user is
// marked ready, and every place the stored column disagrees with the resolver,
// as (stored area -> resolved area, which arm decided) with its own count. The
// total of those is the number of rows --apply would write. On a ready user the
// same report is the standing drift check: anything listed is a gap the triggers
// left, and the resolver is right.
//
// --apply fills each user's column in batches (store.AreaIndexBackfillBatch,
// which writes the resolver's own answer), re-runs the comparison, and only then
// marks the user ready, and only if it found zero disagreements. Idempotent: a
// second run writes nothing. A user whose triggers already populated some rows
// is handled the same way, since only rows that differ are written.
//
// Without --user-id it covers every active user.
func runAreaIndex(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("area-index", flag.ContinueOnError)
	var (
		userIDStr = fs.String("user-id", "", "one user (ULID); default every active user")
		apply     = fs.Bool("apply", false, "backfill the stored area and mark the user ready (default: report only)")
		batch     = fs.Int("batch", 1000, "messages per transaction with --apply")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *batch <= 0 {
		return fmt.Errorf("--batch must be positive")
	}
	d, err := openDeps(ctx)
	if err != nil {
		return err
	}
	defer d.close()

	var users []ulid.ULID
	if *userIDStr != "" {
		u, err := parseUserID(*userIDStr)
		if err != nil {
			return err
		}
		users = []ulid.ULID{u}
	} else {
		if users, err = d.store.ListActiveUserIDs(ctx); err != nil {
			return err
		}
	}
	return areaIndex(ctx, os.Stdout, d.store, users, *apply, *batch)
}

// say writes one formatted report line; the report goes to a terminal or a
// test buffer, where a failed write has nowhere better to be reported.
func say(w io.Writer, format string, args ...any) { _, _ = fmt.Fprintf(w, format, args...) }

func areaIndex(ctx context.Context, w io.Writer, st *store.Store, users []ulid.ULID, apply bool, batch int) error {
	say(w, "area-index: %d user(s)\n", len(users))
	for _, u := range users {
		if err := areaIndexUser(ctx, w, st, u, apply, batch); err != nil {
			return fmt.Errorf("area-index: user %s: %w", u, err)
		}
	}
	if !apply {
		say(w, "\nREPORT ONLY - nothing was written. Re-run with --apply to backfill and mark users ready.\n")
	}
	return nil
}

func areaIndexUser(ctx context.Context, w io.Writer, st *store.Store, u ulid.ULID, apply bool, batch int) error {
	names := map[ulid.ULID]string{}
	var rep store.AreaIndexReport
	read := func() error {
		return st.UserTx(ctx, u, func(tx *store.UserTx) error {
			var e error
			if rep, e = tx.AreaIndexReport(ctx); e != nil {
				return e
			}
			views, e := tx.ListAreas(ctx)
			if e != nil {
				return e
			}
			for _, v := range views {
				names[v.ID] = v.Name
			}
			return nil
		})
	}
	if err := read(); err != nil {
		return err
	}
	say(w, "\nuser %s\n", u)
	printAreaIndexReport(w, rep, names)
	if !apply {
		return nil
	}

	var after ulid.ULID
	scanned, changed := 0, 0
	for {
		var (
			last   ulid.ULID
			s, c   int
			berr   error
			finish bool
		)
		berr = st.UserTx(ctx, u, func(tx *store.UserTx) error {
			var e error
			last, s, c, e = tx.AreaIndexBackfillBatch(ctx, after, batch)
			finish = s == 0
			return e
		})
		if berr != nil {
			return berr
		}
		if finish {
			break
		}
		after, scanned, changed = last, scanned+s, changed+c
	}
	say(w, "  applied: scanned %d message(s), wrote %d\n", scanned, changed)

	if err := read(); err != nil {
		return err
	}
	say(w, "  after the backfill:\n")
	printAreaIndexReport(w, rep, names)
	if n := rep.Disagreeing(); n != 0 {
		say(w, "  NOT marked ready: %d message(s) still disagree with the resolver\n", n)
		return nil
	}
	if err := st.UserTx(ctx, u, func(tx *store.UserTx) error { return tx.MarkAreaIndexReady(ctx) }); err != nil {
		return err
	}
	say(w, "  marked ready: 0 disagreements\n")
	return nil
}

func printAreaIndexReport(w io.Writer, rep store.AreaIndexReport, names map[ulid.ULID]string) {
	label := func(id *ulid.ULID) string {
		if id == nil {
			return "(needs sorting / unset)"
		}
		if n, ok := names[*id]; ok {
			return n
		}
		return id.String()
	}
	ready := "not ready"
	if rep.ReadyAt != nil {
		ready = "ready since " + rep.ReadyAt.UTC().Format("2006-01-02 15:04:05Z")
	}
	say(w, "  messages: %d   stored area set: %d   %s\n", rep.Messages, rep.StoredSet, ready)
	say(w, "  disagreeing with the resolver (rows --apply would write): %d\n", rep.Disagreeing())
	for _, d := range rep.Disagreements {
		say(w, "    %-28s -> %-28s via %-5s %d\n", label(d.Stored), label(d.Resolved), d.Via, d.Count)
	}
}
