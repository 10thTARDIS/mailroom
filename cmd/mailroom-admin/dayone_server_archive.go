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

// `mailroom-admin day-one-server-archive --user-id U [--apply] [--limit N]
// [--batch N]` (#934): make day-one's Mailroom-only archives real on the
// server, for mail the server still shows in INBOX.
//
// Day-one setup archives older mail IN MAILROOM ONLY (#446), so the provider's
// webmail and every other client keep showing it in the inbox. This moves each
// such message to its account's Archive folder the way a user archive does —
// one canonical placement, one 'setup'-sourced move intent per live account,
// one Email change row — and the writeback engine then performs the IMAP moves
// at its own pace (up to about a thousand per sync pass, DefaultDrainLimits).
// The predicate and its reasons are in internal/store/dayone_server_archive.go.
//
// REPORT FIRST. Without --apply it writes nothing: it prints how many
// day-one archives are still in a server INBOX, why each excluded one is
// excluded (a count per reason), and per account how many would move and
// where. --limit caps an apply, so a first run can move a handful and be
// checked in the provider's webmail before the rest. A rerun after a complete
// apply reports 0 candidates.
//
// ⚠ Once moved, undoing the setup run no longer returns these messages to
// other clients' inboxes: they are archived on the server.
func runDayOneServerArchive(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("day-one-server-archive", flag.ContinueOnError)
	userFlag := fs.String("user-id", "", "user whose day-one archives to examine (required)")
	apply := fs.Bool("apply", false, "move the candidates to Archive on the server (default: report only, write nothing)")
	limit := fs.Int("limit", 0, "with --apply: move at most N messages (0 = all)")
	batch := fs.Int("batch", 200, "messages per transaction during --apply")
	if err := fs.Parse(args); err != nil {
		return err
	}
	userID, err := parseUserID(*userFlag)
	if err != nil {
		return err
	}
	if *limit < 0 {
		return fmt.Errorf("day-one-server-archive: --limit must not be negative")
	}
	if *batch <= 0 {
		return fmt.Errorf("day-one-server-archive: --batch must be positive")
	}
	d, err := openDeps(ctx)
	if err != nil {
		return err
	}
	defer d.close()
	return dayOneServerArchive(ctx, os.Stdout, d.store, userID, *apply, *limit, *batch)
}

// dayOneServerArchive runs the report and, with apply, the moves.
func dayOneServerArchive(ctx context.Context, w io.Writer, s *store.Store, userID ulid.ULID, apply bool, limit, batch int) error {
	rep, err := readDayOneServerArchiveReport(ctx, s, userID)
	if err != nil {
		return err
	}
	writeDayOneServerArchiveReport(w, rep)

	if !apply {
		_, _ = fmt.Fprintln(w, "\nDRY RUN: nothing changed. Re-run with --apply to move the movable candidates "+
			"(--limit N to move only the first N, oldest first).")
		return nil
	}

	moved, skipped, err := s.ArchiveDayOneOnServerRun(ctx, userID, limit, batch)
	if err != nil {
		return fmt.Errorf("day-one-server-archive: apply (after %d moved): %w", moved, err)
	}
	_, _ = fmt.Fprintf(w, "\nAPPLIED: %d message(s) archived, each with a move to Archive queued for the server "+
		"(source 'setup'; one Email change row per message).\n", moved)
	if skipped > 0 {
		_, _ = fmt.Fprintf(w, "  %d candidate(s) no longer qualified when re-checked and were left alone.\n", skipped)
	}
	_, _ = fmt.Fprintln(w, "  The writeback engine performs the IMAP moves over the next sync passes; "+
		"`mailroom-admin writeback-ledger` shows their progress.")

	after, err := readDayOneServerArchiveReport(ctx, s, userID)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(w, "  Candidates now: %d (movable %d).\n", after.Candidates, after.Movable)
	return nil
}

func readDayOneServerArchiveReport(ctx context.Context, s *store.Store, userID ulid.ULID) (store.DayOneServerArchiveReport, error) {
	var rep store.DayOneServerArchiveReport
	if err := s.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		rep, e = tx.DayOneServerArchiveReport(ctx)
		return e
	}); err != nil {
		return rep, fmt.Errorf("day-one-server-archive: report: %w", err)
	}
	return rep, nil
}

func writeDayOneServerArchiveReport(w io.Writer, r store.DayOneServerArchiveReport) {
	_, _ = fmt.Fprintln(w, "=== day-one-server-archive (#934): day-one archives the server still shows in INBOX ===")
	_, _ = fmt.Fprintf(w, "\nArchived by day-one setup, in Mailroom only : %d\n", r.DeskArchived)
	_, _ = fmt.Fprintf(w, "  with a live copy in a server INBOX         : %d\n", r.InInbox)
	_, _ = fmt.Fprintf(w, "  with no INBOX copy (other folders, or no live copy): %d, never touched\n",
		r.DeskArchived-r.InInbox)

	_, _ = fmt.Fprintln(w, "\nLeft alone, of those in INBOX (reasons overlap; each counted on its own):")
	_, _ = fmt.Fprintf(w, "  placed since (archived, filed or brought back)         : %d\n", r.PlacedSince)
	_, _ = fmt.Fprintf(w, "  also has a copy in another folder (a move would take it): %d\n", r.OutsideInbox)
	_, _ = fmt.Fprintf(w, "  flagged (starred)                                      : %d\n", r.Flagged)
	_, _ = fmt.Fprintf(w, "  pinned                                                 : %d\n", r.Pinned)
	_, _ = fmt.Fprintf(w, "  snoozed                                                : %d\n", r.Snoozed)
	_, _ = fmt.Fprintf(w, "  in Trash                                               : %d\n", r.Trashed)
	_, _ = fmt.Fprintf(w, "  draft or junk                                          : %d\n", r.DraftOrJunk)
	_, _ = fmt.Fprintf(w, "  gone from the server (vanished)                        : %d\n", r.Vanished)

	_, _ = fmt.Fprintf(w, "\nCandidates : %d message(s), every live copy in INBOX\n", r.Candidates)
	_, _ = fmt.Fprintf(w, "  movable  : %d (every account they are in has an Archive folder and live writeback)\n", r.Movable)
	_, _ = fmt.Fprintf(w, "  blocked  : %d (see the accounts below)\n", r.Candidates-r.Movable)

	_, _ = fmt.Fprintln(w, "\nPer account (a message in two accounts counts in each):")
	if len(r.Accounts) == 0 {
		_, _ = fmt.Fprintln(w, "  (this user has no accounts)")
	}
	for _, a := range r.Accounts {
		var note string
		switch {
		case a.Gmail:
			note = "Gmail: the inbox is a label, not a folder; NOT ASSESSED by this command"
		case a.ArchiveMailbox == "":
			note = "BLOCKED: no Archive folder"
		case !a.WritebackLive:
			note = fmt.Sprintf("BLOCKED: %s (a move now would be suppressed for good, not delayed)", a.WritebackOff)
		default:
			note = fmt.Sprintf("-> %q", a.ArchiveMailbox)
		}
		_, _ = fmt.Fprintf(w, "  %-30s %6d  %s\n", a.Name, a.Candidates, note)
	}

	_, _ = fmt.Fprintln(w, "\nMoves this command has queued before (source 'setup'), by state:")
	if len(r.SetupMoves) == 0 {
		_, _ = fmt.Fprintln(w, "  none")
	}
	for _, st := range sortedKeys(r.SetupMoves) {
		note := ""
		switch st {
		case "dead", "suppressed":
			note = "  <- archived in Mailroom but NOT moved on the server; see writeback-ledger"
		case "pending", "inflight":
			note = "  <- still being moved"
		}
		_, _ = fmt.Fprintf(w, "  %-10s : %d%s\n", st, r.SetupMoves[st], note)
	}
	_, _ = fmt.Fprintln(w, "\nOnce moved, undoing the setup run no longer returns these messages to other clients' inboxes.")
}
