// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// Untangling accidental multi-labelling from the batch-triage surface
// (mailroom#219).
//
// The batch-triage surface answers a group of messages at once, and each
// answer ADDS a label (actions.setLabel(ids, atom, true)). A message that
// showed up in several batch groups therefore accumulated one label per
// answer, even though the operator never intended more than one label on any
// message. This command applies the operator's own after-the-fact decision —
// supplied as a plan file naming the ONE label to KEEP per message — by
// removing every other label already on it.
//
// It is a repair of the same family as repair-contacts and backfill-alias:
// DRY-RUN BY DEFAULT, and it mutates through store.ApplyEmailUpdate ONLY —
// the single path that appends the Email change_log row and enqueues
// writeback in the same transaction as the canonical change (the load-bearing
// invariant, CLAUDE.md). RemovalIsUndo is set on every removal: taking back a
// label the operator applied to a batch by mistake is not the operator
// disagreeing with a rule (#170's correction machinery), it is undoing a
// batch operation, same as apply-rules --undo (internal/api/rule_apply.go).
//
// ApplyEmailUpdate already supports a removal-only patch with RemovalIsUndo
// set — apply-rules' undoRule (cmd/mailroom-admin/applyrules.go) exercises
// exactly that combination today. Nothing needed to change there.
//
// FAIL CLOSED, PER MESSAGE, is the entire point (see decide below): a
// message this command cannot be certain about is refused and reported, never
// guessed at, and one refusal must not abort the run — the report has to name
// every bad line so a partial plan cannot pass silently. That is also why the
// command returns a non-zero exit whenever anything was refused, dry run or
// not: "would refuse" is exactly as much a signal that the plan needs fixing
// as "refused" is.

// untanglePlanEntry is one validated line of a plan file: the message to
// reduce, and the ONE label to keep on it.
type untanglePlanEntry struct {
	Line      int
	MessageID ulid.ULID
	KeepLabel string
}

// untangleLineError is a plan line rejected before it ever reaches the
// database — malformed, missing a column, or naming a message id that
// appears on more than one line. Collected rather than fatal: a plan
// describing dozens of messages has to name every bad line in one pass, not
// stop at the first.
type untangleLineError struct {
	Line   int
	Raw    string
	Reason string
}

// parseUntanglePlan reads an untangle-labels plan file: comment lines
// starting '#' and blank lines are ignored; every other line is
// "<message-id ULID><TAB><label name>" — the label to KEEP on that message.
// Every other label currently on the message is a removal candidate.
//
// A line with no tab, an empty id or label column, or an id that is not a
// valid ULID becomes an untangleLineError rather than aborting the read. So
// does a message id that is named on more than one line: the plan disagrees
// with itself about that message, and guessing which line is right is
// exactly what this command refuses to do, so EVERY occurrence is rejected.
func parseUntanglePlan(r io.Reader) (entries []untanglePlanEntry, errs []untangleLineError, err error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)

	lineNo := 0
	for sc.Scan() {
		lineNo++
		raw := sc.Text()
		// bufio.Scanner's default split already strips a trailing \r before \n
		// (CRLF); trimming again here is a no-op on such input and a safety net
		// for anything that slipped through with a bare \r.
		line := strings.TrimRight(raw, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}

		tab := strings.IndexByte(line, '\t')
		if tab < 0 {
			errs = append(errs, untangleLineError{Line: lineNo, Raw: raw,
				Reason: "no tab between the message id and the label name"})
			continue
		}
		idPart := strings.TrimSpace(line[:tab])
		labelPart := strings.TrimSpace(line[tab+1:])
		if idPart == "" {
			errs = append(errs, untangleLineError{Line: lineNo, Raw: raw, Reason: "missing message id"})
			continue
		}
		if labelPart == "" {
			errs = append(errs, untangleLineError{Line: lineNo, Raw: raw, Reason: "missing label column"})
			continue
		}
		id, perr := ulid.Parse(idPart)
		if perr != nil {
			errs = append(errs, untangleLineError{Line: lineNo, Raw: raw,
				Reason: fmt.Sprintf("message id %q is not a valid ULID: %v", idPart, perr)})
			continue
		}
		entries = append(entries, untanglePlanEntry{Line: lineNo, MessageID: id, KeepLabel: labelPart})
	}
	if serr := sc.Err(); serr != nil {
		return nil, nil, fmt.Errorf("read plan: %w", serr)
	}

	byID := map[ulid.ULID][]int{}
	for _, e := range entries {
		byID[e.MessageID] = append(byID[e.MessageID], e.Line)
	}
	clean := make([]untanglePlanEntry, 0, len(entries))
	for _, e := range entries {
		if lines := byID[e.MessageID]; len(lines) > 1 {
			errs = append(errs, untangleLineError{
				Line: e.Line,
				Raw:  fmt.Sprintf("%s\t%s", e.MessageID, e.KeepLabel),
				Reason: fmt.Sprintf("message id %s appears on lines %v — refusing every occurrence",
					e.MessageID, lines),
			})
			continue
		}
		clean = append(clean, e)
	}
	sort.Slice(errs, func(i, j int) bool { return errs[i].Line < errs[j].Line })
	return clean, errs, nil
}

// untangleOutcome is what decide concluded about one plan entry.
type untangleOutcome int

const (
	// outcomeChanged is a message that would be (dry run) or was (apply)
	// reduced to its kept label.
	outcomeChanged untangleOutcome = iota
	// outcomeAlreadySingle is a message with fewer than two labels: the
	// command must be incapable of stripping a message to nothing, or of
	// touching one that is already correctly labelled.
	outcomeAlreadySingle
	// outcomeRefused is every other fail-closed reason (message not found,
	// keep-label absent, label name unresolved).
	outcomeRefused
)

// untangleResult is one plan entry's decision, plus what it needs for the
// report and (for outcomeChanged) the write.
type untangleResult struct {
	entry   untanglePlanEntry
	outcome untangleOutcome
	reason  string
	keepID  ulid.ULID
	// current is every label id on the message BEFORE this run, whoever
	// applied it — reported even on a refusal, so the operator can see what
	// the message actually carries.
	current []ulid.ULID
	// removed is the label ids taken off (or, in dry run, that would be).
	removed []ulid.ULID
}

// untangleRun is one invocation: its plan and everything needed to decide and
// report on it. The MATCH LOOP IS WRITTEN ONCE (decide) and shared by both dry
// run and apply — the same discipline applyrules.go's backfillRun uses, for
// the same reason: a report computed by different code than the write is a
// report that can disagree with what actually happens.
type untangleRun struct {
	ctx      context.Context
	userID   ulid.ULID
	entries  []untanglePlanEntry
	lineErrs []untangleLineError
	apply    bool
	batch    int

	labelByName map[string]ulid.ULID
	labelName   map[ulid.ULID]string
	// meta holds report metadata (sender, subject) for every plan message that
	// exists for this user. Absence from this map, after loadLabelsAndMeta has
	// run, IS the "message id not found" signal (store.GetEmails: ids not
	// returned were not found or belong to another user — RLS makes the two
	// indistinguishable by design).
	meta map[ulid.ULID]store.EmailView
	// current is every label id currently on each plan message, whoever
	// applied it (store.LabelsOnMessages) — the set decide() validates against.
	current map[ulid.ULID][]ulid.ULID

	results []untangleResult
}

// runUntanglePlan drives one run against the real store: a single read
// transaction loads this user's labels and the plan's message metadata, then
// decide() classifies every line the same way whether or not anything gets
// written, and — only when apply is true — the extra labels come off, ONE
// MESSAGE PER TRANSACTION so a single bad write cannot roll back every
// message already fixed (the same reasoning as repair-sent-drafts).
//
// Exported at package scope, not buried inside flag parsing, so integration
// tests drive exactly the code path the CLI uses.
func runUntanglePlan(
	ctx context.Context,
	st *store.Store,
	userID ulid.ULID,
	entries []untanglePlanEntry,
	lineErrs []untangleLineError,
	apply bool,
	batch int,
) (*untangleRun, error) {
	if batch <= 0 {
		batch = 200
	}
	r := &untangleRun{ctx: ctx, userID: userID, entries: entries, lineErrs: lineErrs, apply: apply, batch: batch}
	if err := st.UserTx(ctx, userID, r.loadLabelsAndMeta); err != nil {
		return r, err
	}
	r.decideAll()
	if err := r.write(st); err != nil {
		return r, err
	}
	return r, nil
}

// loadLabelsAndMeta reads this user's labels (to resolve each plan line's
// keep-label name) and, for every planned message, its current sender/subject
// (GetEmails, for the report) and its current label set (LabelsOnMessages,
// what decide validates against) — read-only, entirely through existing store
// methods.
func (r *untangleRun) loadLabelsAndMeta(tx *store.UserTx) error {
	labels, err := tx.ListLabels(r.ctx)
	if err != nil {
		return fmt.Errorf("list labels: %w", err)
	}
	r.labelByName = make(map[string]ulid.ULID, len(labels))
	r.labelName = make(map[ulid.ULID]string, len(labels))
	for _, l := range labels {
		r.labelByName[l.Name] = l.ID
		r.labelName[l.ID] = l.Name
	}

	ids := make([]ulid.ULID, 0, len(r.entries))
	for _, e := range r.entries {
		ids = append(ids, e.MessageID)
	}
	r.meta = make(map[ulid.ULID]store.EmailView, len(ids))
	r.current = make(map[ulid.ULID][]ulid.ULID, len(ids))
	for _, chunk := range chunkULIDs(ids, r.batch) {
		views, err := tx.GetEmails(r.ctx, chunk)
		if err != nil {
			return fmt.Errorf("get emails: %w", err)
		}
		for _, v := range views {
			r.meta[v.ID] = v
		}
		onMsgs, err := tx.LabelsOnMessages(r.ctx, chunk)
		if err != nil {
			return fmt.Errorf("labels on messages: %w", err)
		}
		for id, lids := range onMsgs {
			r.current[id] = lids
		}
	}
	return nil
}

// decideAll classifies every plan entry.
func (r *untangleRun) decideAll() {
	r.results = make([]untangleResult, len(r.entries))
	for i, e := range r.entries {
		r.results[i] = r.decide(e)
	}
}

// decide is THE correctness of this command: it refuses and never guesses,
// checking — in order — that the keep-label names a real label of this
// user's, that the message exists for this user, that it currently carries
// two or more labels, and that the keep-label is actually one of them. Only
// when every check passes does it compute what would be removed: every
// OTHER label currently on the message, and nothing else. It never adds a
// label, never resolves a label that is not already present, and never
// touches a message this run has not been told about.
func (r *untangleRun) decide(e untanglePlanEntry) untangleResult {
	res := untangleResult{entry: e}

	keepID, ok := r.labelByName[e.KeepLabel]
	if !ok {
		res.outcome = outcomeRefused
		res.reason = fmt.Sprintf("label %q does not resolve to one of this user's labels", e.KeepLabel)
		return res
	}
	res.keepID = keepID

	if _, found := r.meta[e.MessageID]; !found {
		res.outcome = outcomeRefused
		res.reason = "message id not found for this user"
		return res
	}

	current := r.current[e.MessageID]
	res.current = current
	if len(current) < 2 {
		res.outcome = outcomeAlreadySingle
		res.reason = fmt.Sprintf("message has %d label(s) — nothing to untangle", len(current))
		return res
	}

	keepPresent := false
	for _, lid := range current {
		if lid == keepID {
			keepPresent = true
			break
		}
	}
	if !keepPresent {
		res.outcome = outcomeRefused
		res.reason = fmt.Sprintf("keep-label %q is not currently on this message", e.KeepLabel)
		return res
	}

	remove := make([]ulid.ULID, 0, len(current)-1)
	for _, lid := range current {
		if lid != keepID {
			remove = append(remove, lid)
		}
	}
	res.removed = remove
	res.outcome = outcomeChanged
	return res
}

// write performs the removals decide() computed, one message per
// transaction. RemovalIsUndo is set on every call: this is undoing a batch
// mistake, not the operator disagreeing with a rule, so it must not record a
// rule correction (#170) — the same reasoning as apply-rules' --undo
// (internal/api/rule_apply.go:292).
func (r *untangleRun) write(st *store.Store) error {
	if !r.apply {
		return nil
	}
	for i := range r.results {
		res := &r.results[i]
		if res.outcome != outcomeChanged {
			continue
		}
		if err := st.UserTx(r.ctx, r.userID, func(tx *store.UserTx) error {
			found, _, e := tx.ApplyEmailUpdate(r.ctx, res.entry.MessageID, store.EmailUpdate{
				RemoveLabelIDs: res.removed,
				RemovalIsUndo:  true,
			})
			if e != nil {
				return e
			}
			if !found {
				// The message existed moments ago when loadLabelsAndMeta read it and
				// does not now (deleted account, a concurrent purge). Report it as a
				// refusal rather than silently counting a write that never happened.
				res.outcome = outcomeRefused
				res.reason = "message disappeared before the write — re-run the plan"
			}
			return nil
		}); err != nil {
			return fmt.Errorf("message %s: %w", res.entry.MessageID, err)
		}
	}
	return nil
}

// refusedTotal is every plan line that did NOT end in a clean removal:
// malformed/duplicate lines caught before the database, every outcomeRefused
// decision, and every outcomeAlreadySingle one. All three mean the plan was
// not fully applied, which is exactly what a non-zero exit has to say — in
// dry run as much as in apply, since "would refuse" is the same signal that
// the plan needs fixing before anyone runs --apply.
func (r *untangleRun) refusedTotal() int {
	n := len(r.lineErrs)
	for _, res := range r.results {
		if res.outcome != outcomeChanged {
			n++
		}
	}
	return n
}

// report prints, per message, the id, sender, subject, its current labels,
// the label being kept, and the labels being removed — then totals, a
// per-label removal tally, and (dry run) the re-run reminder.
func (r *untangleRun) report() {
	fmt.Printf("plan file: %d line(s) parsed, %d line(s) rejected before touching the database\n",
		len(r.entries), len(r.lineErrs))
	for _, e := range r.lineErrs {
		fmt.Printf("  REFUSED  line %-4d %s  (%q)\n", e.Line, e.Reason, strings.TrimRight(e.Raw, "\r\n"))
	}
	fmt.Println()

	var changed, already, refused int
	removals := map[string]int{}
	for _, res := range r.results {
		view := r.meta[res.entry.MessageID]
		from := view.FromAddress
		if from == "" {
			from = "(unknown sender)"
		}
		subj := trunc(view.Subject, 40)
		switch res.outcome {
		case outcomeChanged:
			changed++
			verb := "would remove"
			if r.apply {
				verb = "removed"
			}
			fmt.Printf("  %s  %-26s %-42s has=%-24s keep=%-16s %s=%s\n",
				shortULID(res.entry.MessageID), trunc(from, 26), subj,
				r.labelNames(res.current), res.entry.KeepLabel, verb, r.labelNames(res.removed))
			for _, lid := range res.removed {
				removals[r.labelName[lid]]++
			}
		case outcomeAlreadySingle:
			already++
			fmt.Printf("  %s  %-26s %-42s has=%-24s (already single-labelled — skipped)\n",
				shortULID(res.entry.MessageID), trunc(from, 26), subj, r.labelNames(res.current))
		case outcomeRefused:
			refused++
			fmt.Printf("  %s  %-26s %-42s REFUSED: %s\n",
				shortULID(res.entry.MessageID), trunc(from, 26), subj, res.reason)
		}
	}

	verbTotal := "would change"
	if r.apply {
		verbTotal = "changed"
	}
	fmt.Printf("\ntotals: in plan=%d  %s=%d  already single-labelled=%d  refused=%d\n",
		len(r.entries)+len(r.lineErrs), verbTotal, changed, already, refused+len(r.lineErrs))

	if len(removals) > 0 {
		fmt.Println("\nremovals by label:")
		printCounts(removals, 50)
	}

	if !r.apply {
		fmt.Printf("\nDRY RUN — nothing written. Re-run with --apply to persist.\n")
	}
}

// labelNames renders a set of label ids as a sorted, comma-joined name list
// for the report; an id with no known name (should not happen — these all
// came from this user's own labels) falls back to the raw id rather than
// hiding it.
func (r *untangleRun) labelNames(ids []ulid.ULID) string {
	if len(ids) == 0 {
		return "(none)"
	}
	names := make([]string, 0, len(ids))
	for _, id := range ids {
		if n, ok := r.labelName[id]; ok {
			names = append(names, n)
		} else {
			names = append(names, id.String())
		}
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}

// shortULID renders a message id at report-friendly length. Full ids are
// still available from the plan file itself for anything that needs one.
func shortULID(id ulid.ULID) string {
	s := id.String()
	if len(s) > 10 {
		return s[:10]
	}
	return s
}

// chunkULIDs splits ids into slices of at most size, preserving order —
// bounds the batch reads a very large plan file would otherwise issue as one
// query.
func chunkULIDs(ids []ulid.ULID, size int) [][]ulid.ULID {
	if size <= 0 || len(ids) == 0 {
		if len(ids) == 0 {
			return nil
		}
		size = len(ids)
	}
	out := make([][]ulid.ULID, 0, (len(ids)+size-1)/size)
	for i := 0; i < len(ids); i += size {
		end := i + size
		if end > len(ids) {
			end = len(ids)
		}
		out = append(out, ids[i:end])
	}
	return out
}

// runUntangleLabels is the CLI entry point: parse flags and the plan file,
// run it against the store, print the report, and — if anything was refused,
// in either mode — return a non-zero-exit error so a partial plan cannot
// pass silently (mailroom#219).
func runUntangleLabels(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("untangle-labels", flag.ContinueOnError)
	var (
		userIDStr = fs.String("user-id", "", "operator user id (ULID)")
		planPath  = fs.String("plan", "", "plan file: <message-id ULID><TAB><label to keep>, one per line")
		apply     = fs.Bool("apply", false, "remove every OTHER label on each planned message (default: dry-run)")
		batch     = fs.Int("batch", 200, "how many plan messages to read per round trip")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	userID, err := parseUserID(*userIDStr)
	if err != nil {
		return err
	}
	if strings.TrimSpace(*planPath) == "" {
		return errors.New("untangle-labels: --plan is required")
	}

	f, err := os.Open(*planPath)
	if err != nil {
		return fmt.Errorf("untangle-labels: open plan: %w", err)
	}
	entries, lineErrs, perr := parseUntanglePlan(f)
	_ = f.Close()
	if perr != nil {
		return fmt.Errorf("untangle-labels: %w", perr)
	}

	d, err := openDeps(ctx)
	if err != nil {
		return err
	}
	defer d.close()

	r, err := runUntanglePlan(ctx, d.store, userID, entries, lineErrs, *apply, *batch)
	if err != nil {
		return fmt.Errorf("untangle-labels: %w", err)
	}
	r.report()

	if n := r.refusedTotal(); n > 0 {
		return fmt.Errorf("untangle-labels: %d of %d plan line(s) were not applied cleanly — see the report above",
			n, len(r.entries)+len(r.lineErrs))
	}
	return nil
}
