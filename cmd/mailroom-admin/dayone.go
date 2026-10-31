// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// `mailroom-admin day-one` — the operator's route into DAY-ONE SETUP (#446),
// and the report that has to exist before the bulk import ever runs.
//
// WHY THIS EXISTS ALONGSIDE THE JMAP SURFACE. The wizard is a separate client
// increment; until it lands this is the only way to see the census at all, and
// even after it lands it is the only way to see the census for a mailbox the
// operator has not logged into. More to the point, the first apply on a real
// mailbox is a several-thousand-row write (a real reference mailbox measured thousands
// of labels and archives), and CLAUDE.md's rule about that is not negotiable:
// ship the report, make --apply opt-in.
//
// The default run prints TWO tables and writes nothing: the census (what is
// there) and the preview of the DEFAULT PLAN (what the obvious choice would
// do), per folder and in total. Neither can be confused with an apply, because
// an apply says APPLIED at the top of its output and prints a run id.
//
// ⚠ NOTHING MOVES ON THE SERVER unless a folder is explicitly `bringToDesk`, and
// the summary line says so in words every time, including when the number is 0 —
// a promise that is only visible when it is being kept is not a promise anybody
// can check.
func runDayOne(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("day-one", flag.ContinueOnError)
	var (
		userIDStr = fs.String("user-id", "", "operator user id (ULID)")
		acctIDStr = fs.String("account-id", "", "narrow the census to one mail account (ULID; default every account)")
		window    = fs.Int("window", 30, "days of recent mail that stay on the desk, untouched")
		planPath  = fs.String("plan", "", "plan file (one line per folder: NAME | labelArchive|bringToDesk|leave [| AREA]); default: labelArchive everything")
		apply     = fs.Bool("apply", false, "create the run and drive it to completion (default: report only)")
		undoRun   = fs.String("undo", "", "reverse a finished run, by run id (ULID)")
		listRuns  = fs.Bool("runs", false, "list this login's setup runs and exit")
		batch     = fs.Int("batch", 200, "messages per chunk; each chunk is one transaction")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	userID, err := parseUserID(*userIDStr)
	if err != nil {
		return err
	}
	if *window <= 0 {
		return fmt.Errorf("day-one: --window must be positive")
	}
	var accountID *ulid.ULID
	if strings.TrimSpace(*acctIDStr) != "" {
		id, perr := ulid.Parse(strings.TrimSpace(*acctIDStr))
		if perr != nil {
			return fmt.Errorf("day-one: --account-id is not a valid ULID: %w", perr)
		}
		accountID = &id
	}
	if *apply && *undoRun != "" {
		return fmt.Errorf("day-one: --apply and --undo are mutually exclusive")
	}

	d, err := openDeps(ctx)
	if err != nil {
		return err
	}
	defer d.close()

	if *listRuns {
		return printDayOneRuns(ctx, d.store, userID)
	}
	if *undoRun != "" {
		runID, perr := ulid.Parse(strings.TrimSpace(*undoRun))
		if perr != nil {
			return fmt.Errorf("day-one: --undo is not a valid run id: %w", perr)
		}
		return undoDayOneRun(ctx, d.store, userID, runID, *batch)
	}

	var census []store.FolderCensus
	if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		census, e = tx.MailboxCensus(ctx, accountID, *window)
		return e
	}); err != nil {
		return fmt.Errorf("day-one: census: %w", err)
	}
	printDayOneCensus(census, *window)
	if len(census) == 0 {
		fmt.Println("\nno folders to set up: this login has no plain folders (and no INBOX) " +
			"that day-one setup can act on. Nothing was written.")
		return nil
	}

	plan := defaultDayOnePlan(census, *window)
	if *planPath != "" {
		f, oerr := os.Open(*planPath)
		if oerr != nil {
			return fmt.Errorf("day-one: open plan: %w", oerr)
		}
		lines, lineErrs, perr := parseDayOnePlan(f)
		// Closed explicitly rather than deferred, matching area-plan/area-report:
		// the file is finished with here, and errcheck will not accept a bare
		// deferred Close.
		_ = f.Close()
		if perr != nil {
			return fmt.Errorf("day-one: %w", perr)
		}
		if len(lineErrs) > 0 {
			fmt.Println("\nREFUSED PLAN LINES (nothing was planned for these):")
			for _, le := range lineErrs {
				fmt.Printf("  line %d: %s\n    %s\n", le.Line, le.Reason, le.Raw)
			}
			return fmt.Errorf("day-one: plan refused: %d line(s)", len(lineErrs))
		}
		var unmatched []string
		plan, unmatched = dayOnePlanFromCensus(census, lines, *window)
		if len(unmatched) > 0 {
			// REFUSED, not ignored: a plan line naming a folder that is not in the
			// census is either a typo or a folder that is special-use, and either
			// way silently dropping it would apply a plan the operator did not
			// write (CLAUDE.md: a bounded result must report its bound).
			fmt.Println("\nPLAN LINES NAMING NO CENSUS FOLDER (nothing was planned for these):")
			for _, name := range unmatched {
				fmt.Printf("  %s\n", name)
			}
			return fmt.Errorf("day-one: plan names %d folder(s) that are not in the census", len(unmatched))
		}
	}

	var counts store.SetupCounts
	if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		counts, e = tx.SetupPreview(ctx, plan)
		return e
	}); err != nil {
		return fmt.Errorf("day-one: preview: %w", err)
	}
	printDayOnePreview(plan, counts, *planPath == "")

	if !*apply {
		if counts.Labels+counts.Archived+counts.Moves == 0 {
			fmt.Println("\nREPORT ONLY — nothing was written, and nothing would change.")
		} else {
			fmt.Printf("\nREPORT ONLY — nothing was written. Re-run with --apply to %s.\n",
				dayOneSummaryLine(counts))
		}
		return nil
	}

	var runID ulid.ULID
	if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		runID, e = tx.CreateSetupRun(ctx, plan, counts)
		return e
	}); err != nil {
		return fmt.Errorf("day-one: create run: %w", err)
	}
	fmt.Printf("\nAPPLIED — run %s created (labels and areas minted; the import job is enqueued).\n", runID)
	// Driven INLINE rather than left to the dispatcher, exactly as drawer-report's
	// --apply does: an operator running this wants to watch it finish and see the
	// counters, not discover later whether a background job got to it. The job row
	// the store enqueued is harmless — the worker finds the run already done and
	// says so (setup.DirectionNone).
	if err := driveDayOneRun(ctx, d.store, userID, runID, *batch, false); err != nil {
		return err
	}
	return printDayOneRun(ctx, d.store, userID, runID)
}

// undoDayOneRun reverses a run inline and prints the outcome.
func undoDayOneRun(ctx context.Context, st *store.Store, userID, runID ulid.ULID, batch int) error {
	if err := st.UserTx(ctx, userID, func(tx *store.UserTx) error {
		return tx.BeginSetupUndo(ctx, runID)
	}); err != nil {
		return fmt.Errorf("day-one: --undo: %w", err)
	}
	fmt.Printf("UNDOING run %s — replaying its journal in reverse.\n", runID)
	fmt.Println("A label the user has re-applied by hand since the run is THEIRS and is left alone; " +
		"a message that was already archived in Mailroom before the run stays archived.")
	if err := driveDayOneRun(ctx, st, userID, runID, batch, true); err != nil {
		return err
	}
	return printDayOneRun(ctx, st, userID, runID)
}

// driveDayOneRun loops chunks, each in its own transaction, printing one line
// per chunk. Same shape as applyDrawerSweep: bounded work per transaction, and a
// chunk that reports no progress and no completion STOPS rather than spinning.
func driveDayOneRun(ctx context.Context, st *store.Store, userID, runID ulid.ULID, batch int, undo bool) error {
	verb := "apply"
	if undo {
		verb = "undo"
	}
	for chunk := 1; ; chunk++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		var p store.SetupProgress
		if err := st.UserTx(ctx, userID, func(tx *store.UserTx) error {
			var e error
			if undo {
				p, e = tx.SetupUndoChunk(ctx, runID, batch)
			} else {
				p, e = tx.SetupApplyChunk(ctx, runID, batch)
			}
			return e
		}); err != nil {
			return fmt.Errorf("day-one: %s chunk %d: %w", verb, chunk, err)
		}
		if p.Done {
			fmt.Printf("  chunk %d: done\n", chunk)
			return nil
		}
		fmt.Printf("  chunk %d: %d message(s) — %d label(s), %d archived in Mailroom, %d moved on the server\n",
			chunk, p.Processed, p.Labels, p.Archived, p.Moves)
		if p.Processed == 0 {
			return fmt.Errorf("day-one: %s chunk %d made no progress and did not report done; "+
				"stopping rather than looping", verb, chunk)
		}
	}
}

// ---- the plan file ---------------------------------------------------------

// dayOnePlanNoArea is the token that means "label this folder's mail but put it
// in no area at all" — a legitimate choice for a folder that is an archive
// rather than a topic. An OMITTED third field means the opposite (default: an
// area of the folder's own name), so the two cases need distinct spellings.
const dayOnePlanNoArea = "-"

// dayOnePlanLine is one parsed line of a plan file.
type dayOnePlanLine struct {
	Line   int
	Raw    string
	Folder string
	Action string
	// Area is the area name; empty with HasArea false means "default to the
	// folder's name". HasArea true with Area empty means "no area".
	Area    string
	HasArea bool
}

// dayOnePlanLineError is one refused line, with the reason.
type dayOnePlanLineError struct {
	Line   int
	Raw    string
	Reason string
}

// parseDayOnePlan reads the plan file format:
//
//	# comments and blank lines are ignored
//	Receipts    | labelArchive
//	Newsletters | labelArchive | Reading
//	Invoices    | labelArchive | -          # label only, no area
//	To do       | bringToDesk
//	Old stuff   | leave
//
// A refused line is REPORTED, never skipped — see areas.ParsePlan's own
// reasoning: a plan file where a typo silently means "leave this folder alone"
// applies a plan the operator did not write. Pure (io.Reader in, values out) so
// the format is unit-testable without a database or a file.
func parseDayOnePlan(r io.Reader) ([]dayOnePlanLine, []dayOnePlanLineError, error) {
	var (
		out      []dayOnePlanLine
		lineErrs []dayOnePlanLineError
	)
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		raw := sc.Text()
		text := raw
		if i := strings.IndexByte(text, '#'); i >= 0 {
			text = text[:i]
		}
		text = strings.TrimSpace(text)
		if text == "" {
			continue
		}
		parts := strings.Split(text, "|")
		if len(parts) < 2 || len(parts) > 3 {
			lineErrs = append(lineErrs, dayOnePlanLineError{n, raw,
				"want `FOLDER | labelArchive|bringToDesk|leave [| AREA]`"})
			continue
		}
		folder := strings.TrimSpace(parts[0])
		action := strings.TrimSpace(parts[1])
		if folder == "" {
			lineErrs = append(lineErrs, dayOnePlanLineError{n, raw, "no folder name"})
			continue
		}
		switch action {
		case store.SetupActionLabelArchive, store.SetupActionBringToDesk, store.SetupActionLeave:
		default:
			lineErrs = append(lineErrs, dayOnePlanLineError{n, raw,
				"action must be labelArchive, bringToDesk or leave (got " + action + ")"})
			continue
		}
		line := dayOnePlanLine{Line: n, Raw: raw, Folder: folder, Action: action}
		if len(parts) == 3 {
			line.HasArea = true
			area := strings.TrimSpace(parts[2])
			if area != dayOnePlanNoArea {
				line.Area = area
			}
			if area == "" {
				lineErrs = append(lineErrs, dayOnePlanLineError{n, raw,
					"an empty third field is ambiguous: omit it for an area named after the folder, " +
						"or write `-` for no area"})
				continue
			}
		}
		out = append(out, line)
	}
	if err := sc.Err(); err != nil {
		return nil, nil, fmt.Errorf("read plan: %w", err)
	}
	return out, lineErrs, nil
}

// dayOnePlanFromCensus matches plan lines to census folders BY NAME
// (case-insensitive, trimmed — the same rule area-plan uses, and for the same
// reason: a file written by hand will not match stored data byte for byte) and
// returns the store plan plus the names that matched nothing.
//
// Folders the file does not name are LEFT OUT of the plan entirely, which the
// store treats as `leave`. That is the conservative reading: a plan file is a
// statement about the folders it mentions.
//
// PURE, so both the matching and the defaults are unit-testable.
func dayOnePlanFromCensus(census []store.FolderCensus, lines []dayOnePlanLine, windowDays int) (store.SetupPlan, []string) {
	byName := make(map[string]store.FolderCensus, len(census))
	for _, c := range census {
		byName[strings.ToLower(strings.TrimSpace(c.Name))] = c
	}
	plan := store.SetupPlan{WindowDays: windowDays}
	var unmatched []string
	for _, l := range lines {
		c, ok := byName[strings.ToLower(l.Folder)]
		if !ok {
			unmatched = append(unmatched, l.Folder)
			continue
		}
		choice := store.SetupFolderChoice{MailboxID: c.MailboxID, Action: l.Action}
		if l.Action != store.SetupActionLeave && c.Role != "inbox" {
			choice.LabelName = c.Name
			switch {
			case l.HasArea:
				choice.AreaName = l.Area // "" here means "no area", per dayOnePlanNoArea
			case c.ExistingAreaID != nil:
				id := *c.ExistingAreaID
				choice.AreaID = &id
			default:
				choice.AreaName = c.Name
			}
		}
		plan.Folders = append(plan.Folders, choice)
	}
	return plan, unmatched
}

// defaultDayOnePlan is the obvious choice, written down: EVERY folder is
// labelArchive, its label is its own name, and its area is an existing area of
// that name if there is one or a new area of that name if there is not. INBOX is
// labelArchive too — step 2 applies to the inbox, whose older mail is labelled
// with its own resolved area's label rather than a folder name.
//
// It is the plan the report previews when no --plan file is given, so the
// operator sees the consequence of the default before choosing to depart from it.
// PURE, for dayOnePlanFromCensus's reason.
func defaultDayOnePlan(census []store.FolderCensus, windowDays int) store.SetupPlan {
	plan := store.SetupPlan{WindowDays: windowDays}
	for _, c := range census {
		choice := store.SetupFolderChoice{MailboxID: c.MailboxID, Action: store.SetupActionLabelArchive}
		if c.Role != "inbox" {
			choice.LabelName = c.Name
			if c.ExistingAreaID != nil {
				id := *c.ExistingAreaID
				choice.AreaID = &id
			} else {
				choice.AreaName = c.Name
			}
		}
		plan.Folders = append(plan.Folders, choice)
	}
	return plan
}

// ---- output ----------------------------------------------------------------

func printDayOneCensus(census []store.FolderCensus, windowDays int) {
	fmt.Println("=== day-one (#446): the folders, as they are ===")
	fmt.Printf("window: %d days of recent mail stay on the desk, untouched\n", windowDays)
	fmt.Println("special-use folders (Drafts, Sent, Trash, Archive, Junk) are mapped without asking " +
		"and are not listed; INBOX IS listed, because step 2 applies to it too.")
	fmt.Println()
	fmt.Printf("%-24s %7s %7s %7s %7s %8s %8s %8s %-11s %-6s %-6s %s\n",
		"FOLDER", "LIVE", "WINDOW", "OLDER", "UNCLS", "SENDERS", "DOMAINS", "S/90%", "SHAPE",
		"LABEL", "AREA", "FEEDS DESK")
	for _, c := range census {
		name := c.Name
		if c.Role == "inbox" {
			name += " (inbox)"
		}
		fmt.Printf("%-24s %7d %7d %7d %7d %8d %8d %8d %-11s %-6s %-6s %v\n",
			truncateDayOne(name, 24), c.Live, c.InWindow, c.Older, c.Unclassified,
			c.DistinctSenders, c.DistinctDomains, c.SendersFor90, c.Shape,
			yesNoDayOne(c.ExistingLabelID != nil), yesNoDayOne(c.ExistingAreaID != nil), c.FeedsDesk)
	}
	fmt.Println()
	fmt.Println("UNCLS is mail with no From address — counted, never hidden: a folder that is " +
		"largely unclassifiable is a folder whose SHAPE word means much less than it looks like it does.")
	fmt.Println("S/90% is the fewest senders covering 90% of LIVE. SHAPE is a word for the numbers " +
		"beside it, not an authority.")
}

func printDayOnePreview(plan store.SetupPlan, counts store.SetupCounts, isDefault bool) {
	if isDefault {
		fmt.Println("\n=== the DEFAULT plan: label and archive everything older than the window ===")
		fmt.Println("(nothing is brought to the desk and nothing is left alone; " +
			"supply --plan FILE to depart from this)")
	} else {
		fmt.Println("\n=== the plan you supplied ===")
	}
	fmt.Printf("window: %d days\n\n", plan.WindowDays)
	fmt.Printf("%-24s %-13s %7s %8s %10s %7s\n",
		"FOLDER", "ACTION", "OLDER", "LABELS", "ARCHIVED", "MOVES")
	for _, f := range counts.PerFolder {
		fmt.Printf("%-24s %-13s %7d %8d %10d %7d\n",
			truncateDayOne(f.Name, 24), f.Action, f.Older, f.Labels, f.Archived, f.Moves)
	}
	fmt.Println()
	fmt.Printf("TOTAL: %s\n", dayOneSummaryLine(counts))
	fmt.Printf("of the mail this plan would touch, %d message(s) carry no From address "+
		"(counted, not hidden)\n", counts.Unclassified)
}

// dayOneSummaryLine is the one sentence the whole feature is judged by, in
// drawer-report's register. It ALWAYS names "nothing moves on the server",
// including when moves is 0: a promise that is only visible when it is being
// broken is not a promise anybody can check.
func dayOneSummaryLine(c store.SetupCounts) string {
	return fmt.Sprintf("write %s labels · archive %s in Mailroom (nothing moves on the server) · "+
		"move %s on the server · leave %s · about %s",
		commaDayOne(c.Labels), commaDayOne(c.Archived), commaDayOne(c.Moves), commaDayOne(c.Leave),
		dayOneDuration(c.EstimatedSeconds))
}

// dayOneDuration renders the ESTIMATE as prose. It is a stated planning rate,
// not a measurement (store.setupWritesPerSecond) — hence "about".
func dayOneDuration(seconds int) string {
	switch {
	case seconds <= 0:
		return "no time at all"
	case seconds < 60:
		return fmt.Sprintf("%d second(s)", seconds)
	default:
		return fmt.Sprintf("%d minute(s)", (seconds+59)/60)
	}
}

func printDayOneRuns(ctx context.Context, st *store.Store, userID ulid.ULID) error {
	var runs []store.SetupRun
	if err := st.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		runs, e = tx.ListSetupRuns(ctx)
		return e
	}); err != nil {
		return fmt.Errorf("day-one: --runs: %w", err)
	}
	fmt.Println("=== day-one setup runs, newest first ===")
	if len(runs) == 0 {
		fmt.Println("none: this login has never run day-one setup.")
		return nil
	}
	fmt.Printf("%-26s %-8s %6s %-19s %-19s %s\n", "RUN", "STATUS", "WINDOW", "CREATED", "FINISHED",
		"LABELS/ARCHIVED/MOVED (planned)")
	for _, r := range runs {
		fmt.Printf("%-26s %-8s %6d %-19s %-19s %d/%d/%d (%d/%d/%d)\n",
			r.ID, r.Status, r.WindowDays,
			r.CreatedAt.UTC().Format("2006-01-02 15:04:05"), dayOneTimeOrDash(r.FinishedAt),
			r.LabelsWritten, r.Archived, r.Moved,
			r.PlannedLabels, r.PlannedArchived, r.PlannedMoves)
		if r.Error != "" {
			fmt.Printf("  ! %s\n", r.Error)
		}
	}
	return nil
}

// printDayOneRun re-reads ONE run and prints planned beside actual — A CHECKED
// FACT, NOT THE COUNTERS THE LOOP ACCUMULATED (drawer-report's own discipline):
// the loop's totals and the run's columns are written by different code, and
// printing the run's own row is what makes a disagreement visible.
func printDayOneRun(ctx context.Context, st *store.Store, userID, runID ulid.ULID) error {
	var (
		run   store.SetupRun
		found bool
	)
	if err := st.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		run, found, e = tx.GetSetupRun(ctx, runID)
		return e
	}); err != nil {
		return fmt.Errorf("day-one: read run back: %w", err)
	}
	if !found {
		return fmt.Errorf("day-one: run %s vanished", runID)
	}
	fmt.Printf("\nrun %s: %s\n", run.ID, run.Status)
	fmt.Printf("  labels   planned %d, written %d\n", run.PlannedLabels, run.LabelsWritten)
	fmt.Printf("  archived planned %d, actual  %d   (in Mailroom — nothing moved on the server)\n",
		run.PlannedArchived, run.Archived)
	fmt.Printf("  moved    planned %d, actual  %d   (server-side moves, from bringToDesk folders only)\n",
		run.PlannedMoves, run.Moved)
	fmt.Printf("  left for later %d · unclassified %d\n", run.LeftForLater, run.Unclassified)
	if run.Error != "" {
		fmt.Printf("  ! %s\n", run.Error)
	}
	// A DISAGREEMENT IS A WARNING, NEVER HIDDEN (CLAUDE.md). The planned figures
	// came from the preview and the actual ones from the run; they can legitimately
	// differ (mail arrived, the user labelled something by hand in between), but
	// the operator has to be told rather than left to compare two numbers.
	if run.Status == store.SetupStatusDone &&
		(run.LabelsWritten != run.PlannedLabels || run.Archived != run.PlannedArchived || run.Moved != run.PlannedMoves) {
		fmt.Println("  ⚠ the run and the preview disagree. That is possible without anything being " +
			"wrong (mail arrived, or a label was applied by hand, between the preview and the apply), " +
			"but it is worth knowing which of the three moved and by how much.")
	}
	return nil
}

func dayOneTimeOrDash(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return t.UTC().Format("2006-01-02 15:04:05")
}

func yesNoDayOne(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func truncateDayOne(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}

// commaDayOne groups thousands, so "4,404" reads as a number rather than as a
// string of digits — the drawer-report register.
func commaDayOne(n int) string {
	s := fmt.Sprintf("%d", n)
	if n < 0 || len(s) <= 3 {
		return s
	}
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	return b.String()
}
