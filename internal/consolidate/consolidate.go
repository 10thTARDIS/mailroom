// SPDX-License-Identifier: AGPL-3.0-only

// Package consolidate reshapes an account's folder tree ON THE PROVIDER (#647):
// every message in each folder matching a pattern is moved into one destination
// folder, and folders matching another pattern are deleted once they are empty.
// It is the engine behind `mailroom-admin consolidate-folders`.
//
// It writes to a real mailbox, so its shape is "report, then act":
//
//   - BuildPlan is READ-ONLY. It LISTs, STATUSes every folder it would touch,
//     and decides per folder: move N, nothing to move, delete, delete once its
//     move lands, or refuse — with the reason.
//   - Apply acts on a plan one folder at a time and re-reads the server around
//     every step: STATUS before and after each `UID MOVE 1:*`, requiring the
//     source at 0 and the destination at exactly D+N, and STOPPING on any
//     mismatch; STATUS immediately before each DELETE, deleting only at 0.
//
// It writes NOTHING to Mailroom's database. Mailroom's sync observes the moves
// and deletions the way it observes any other IMAP client's.
//
// Re-running is the recovery path: every decision is re-derived from the
// server, so a run that stopped (a mismatch, --max-folders, a dropped
// connection) is continued by running it again.
package consolidate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	"mailroom/internal/sync"
)

// Session is what consolidation needs from the server. sync.FolderAdmin
// satisfies it; the unit tests use a fake.
type Session interface {
	ListMailboxes(ctx context.Context) ([]sync.Mailbox, error)
	Status(ctx context.Context, mailbox string) (sync.FolderStatus, error)
	MoveAll(ctx context.Context, source, dest string) error
	DeleteMailbox(ctx context.Context, mailbox string) error
	SupportsMove() bool
}

// Options is one consolidation run.
type Options struct {
	// From selects the folders to empty, as an IMAP LIST pattern (see Match).
	// Empty means no moves.
	From string
	// Into is the destination folder. Required with From.
	Into string
	// DeleteEmpty selects folders to delete once empty (LIST patterns).
	DeleteEmpty []string
	// MaxFolders bounds how many folder operations (a folder's move, a
	// folder's delete) one Apply performs. 0 means no bound.
	MaxFolders int
}

// Validate checks the options without any I/O.
func (o Options) Validate() error {
	if o.From == "" && len(o.DeleteEmpty) == 0 {
		return errors.New("nothing to do: give --from (with --into) and/or --delete-empty")
	}
	if o.From != "" && o.Into == "" {
		return errors.New("--from needs --into")
	}
	for _, g := range o.DeleteEmpty {
		if g == "" {
			return errors.New("--delete-empty must not be empty")
		}
	}
	if o.MaxFolders < 0 {
		return errors.New("--max-folders must not be negative")
	}
	return nil
}

// MailroomCount is what Mailroom's database holds for one folder
// (store.MailboxLiveCount): its live provenance count and resolved role.
type MailroomCount struct {
	Live int64
	Role string
}

// Action is the plan's decision for one folder.
type Action string

const (
	// ActionMove: the folder holds N > 0 messages and they move to the destination.
	ActionMove Action = "move"
	// ActionNothingToMove: a matched source that is already empty.
	ActionNothingToMove Action = "nothing to move"
	// ActionNotSelectable: a matched source flagged \Noselect — a hierarchy node
	// that cannot hold messages, so there is nothing to move.
	ActionNotSelectable Action = "not selectable"
	// ActionDelete: a deletion target that is empty now.
	ActionDelete Action = "delete"
	// ActionDeleteAfterMove: a deletion target that is also a move source; it
	// is deleted only after its move lands, and only if STATUS then reads 0.
	ActionDeleteAfterMove Action = "delete after its move"
	// ActionRefuse: see Folder.Refusal.
	ActionRefuse Action = "REFUSE"
)

// Refusal classes. Totals are reported per class.
const (
	RefuseInbox               = "inbox"
	RefuseSpecialUse          = "special-use"
	RefuseMailroomRole        = "mailroom-role"
	RefuseIsDestination       = "is-destination"
	RefuseContainsDestination = "contains-destination"
	RefuseDestination         = "destination-unusable"
	RefuseNoMove              = "no-move-capability"
	RefuseHasChildren         = "has-children"
	RefuseNotEmpty            = "not-empty"
)

// Refusal says why a folder will not be touched.
type Refusal struct {
	Class  string
	Detail string
}

func (r *Refusal) String() string { return r.Class + ": " + r.Detail }

// Folder is one folder's line in the plan.
type Folder struct {
	Name       string
	Delim      rune
	Selectable bool
	// Server is the STATUS read while planning; nil when the folder is not
	// selectable (STATUS is not defined for it).
	Server *sync.FolderStatus
	// Mailroom is Mailroom's own count for the folder; nil when Mailroom holds
	// no row for it (never synced, or sync_scope made it invisible).
	Mailroom *MailroomCount
	Action   Action
	Refusal  *Refusal
}

// Plan is BuildPlan's result: what Apply would do, and why.
type Plan struct {
	Opts          Options
	MoveSupported bool
	// Dest is the destination folder, nil without --into or when it is unusable.
	Dest *Folder
	// DestRefusal is set when --into names a folder that cannot receive mail.
	DestRefusal *Refusal
	// Moves lists every folder --from matched, in name order.
	Moves []*Folder
	// Deletes lists every folder --delete-empty matched, leaves before parents.
	Deletes []*Folder
}

// BuildPlan reads the server (LIST, then STATUS of every matched folder and the
// destination) and decides what Apply would do. It changes nothing.
//
// mailroom maps IMAP names to Mailroom's own counts; nil is allowed.
func BuildPlan(ctx context.Context, s Session, mailroom map[string]MailroomCount, opts Options) (*Plan, error) {
	if err := opts.Validate(); err != nil {
		return nil, err
	}
	boxes, err := s.ListMailboxes(ctx)
	if err != nil {
		return nil, fmt.Errorf("list folders: %w", err)
	}
	sort.Slice(boxes, func(i, j int) bool { return boxes[i].Name < boxes[j].Name })

	p := &Plan{Opts: opts, MoveSupported: s.SupportsMove()}
	statuses := map[string]sync.FolderStatus{}
	folderOf := func(mb sync.Mailbox) (*Folder, error) {
		f := &Folder{Name: mb.Name, Delim: mb.Delimiter, Selectable: selectable(mb)}
		if c, ok := lookupCount(mailroom, mb.Name); ok {
			f.Mailroom = &c
		}
		if f.Selectable {
			st, ok := statuses[mb.Name]
			if !ok {
				st, err = s.Status(ctx, mb.Name)
				if err != nil {
					return nil, fmt.Errorf("status %q: %w", mb.Name, err)
				}
				statuses[mb.Name] = st
			}
			f.Server = &st
		}
		return f, nil
	}

	if opts.Into != "" {
		mb, ok := findMailbox(boxes, opts.Into)
		switch {
		case !ok:
			p.DestRefusal = &Refusal{RefuseDestination, fmt.Sprintf("the destination %q does not exist on the server", opts.Into)}
		case !selectable(mb):
			p.DestRefusal = &Refusal{RefuseDestination, fmt.Sprintf("the destination %q cannot hold messages (\\Noselect)", opts.Into)}
		default:
			if p.Dest, err = folderOf(mb); err != nil {
				return nil, err
			}
		}
	}

	moveByName := map[string]*Folder{}
	if opts.From != "" {
		for _, mb := range boxes {
			if !Match(opts.From, mb.Name, mb.Delimiter) {
				continue
			}
			f, err := folderOf(mb)
			if err != nil {
				return nil, err
			}
			p.planSource(f, mb)
			p.Moves = append(p.Moves, f)
			moveByName[f.Name] = f
		}
	}

	if len(opts.DeleteEmpty) > 0 {
		var targets []sync.Mailbox
		isTarget := map[string]bool{}
		for _, mb := range boxes {
			for _, g := range opts.DeleteEmpty {
				if Match(g, mb.Name, mb.Delimiter) {
					targets = append(targets, mb)
					isTarget[mb.Name] = true
					break
				}
			}
		}
		// Leaves before parents: deepest first, then by name for a stable order.
		sort.SliceStable(targets, func(i, j int) bool {
			di, dj := depth(targets[i]), depth(targets[j])
			if di != dj {
				return di > dj
			}
			return targets[i].Name < targets[j].Name
		})
		byName := map[string]*Folder{}
		for _, mb := range targets {
			f, err := folderOf(mb)
			if err != nil {
				return nil, err
			}
			p.planDelete(f, mb, boxes, isTarget, byName, moveByName)
			p.Deletes = append(p.Deletes, f)
			byName[f.Name] = f
		}
	}
	return p, nil
}

// planSource decides one --from folder.
func (p *Plan) planSource(f *Folder, mb sync.Mailbox) {
	if r := protectedRefusal(mb, f); r != nil {
		f.Action, f.Refusal = ActionRefuse, r
		return
	}
	if into := p.Opts.Into; sameName(f.Name, into) {
		f.Action, f.Refusal = ActionRefuse, &Refusal{RefuseIsDestination, "it is the destination"}
		return
	}
	if f.Delim != 0 && strings.HasPrefix(canonInbox(p.Opts.Into, f.Delim), canonInbox(f.Name, f.Delim)+string(f.Delim)) {
		f.Action, f.Refusal = ActionRefuse, &Refusal{RefuseContainsDestination,
			fmt.Sprintf("the destination %q is inside it", p.Opts.Into)}
		return
	}
	if !f.Selectable {
		f.Action = ActionNotSelectable
		return
	}
	if f.Server.Messages == 0 {
		f.Action = ActionNothingToMove
		return
	}
	if p.DestRefusal != nil {
		f.Action, f.Refusal = ActionRefuse, &Refusal{RefuseDestination, p.DestRefusal.Detail}
		return
	}
	if !p.MoveSupported {
		f.Action, f.Refusal = ActionRefuse, &Refusal{RefuseNoMove,
			"the server does not advertise MOVE; this command never falls back to COPY + EXPUNGE"}
		return
	}
	f.Action = ActionMove
}

// planDelete decides one --delete-empty folder. Targets arrive leaves first,
// so every child target is already decided in byName.
func (p *Plan) planDelete(f *Folder, mb sync.Mailbox, boxes []sync.Mailbox, isTarget map[string]bool,
	byName, moveByName map[string]*Folder) {
	if r := protectedRefusal(mb, f); r != nil {
		f.Action, f.Refusal = ActionRefuse, r
		return
	}
	if p.Opts.Into != "" && sameName(f.Name, p.Opts.Into) {
		f.Action, f.Refusal = ActionRefuse, &Refusal{RefuseIsDestination, "it is the destination"}
		return
	}
	// A child that is not itself being deleted — or whose own deletion is
	// refused — keeps this folder. Deleting a parent on a server that allows
	// it would take the child with it (or leave a \Noselect husk).
	var keep []string
	for _, c := range childrenOf(f, boxes) {
		cf, ok := byName[c]
		if !isTarget[c] || !ok || cf.Action == ActionRefuse {
			keep = append(keep, c)
		}
	}
	if len(keep) > 0 {
		f.Action, f.Refusal = ActionRefuse, &Refusal{RefuseHasChildren, "it has child folders that are not being deleted: " + listSome(keep, 3)}
		return
	}
	if !f.Selectable {
		f.Action = ActionDelete
		return
	}
	if f.Server.Messages == 0 {
		f.Action = ActionDelete
		return
	}
	if m, ok := moveByName[f.Name]; ok && m.Action == ActionMove {
		f.Action = ActionDeleteAfterMove
		return
	}
	f.Action, f.Refusal = ActionRefuse, &Refusal{RefuseNotEmpty, fmt.Sprintf("it holds %d messages", f.Server.Messages)}
}

// protectedRefusal is the refusal every source and deletion target shares:
// INBOX, any SPECIAL-USE folder, and any folder Mailroom itself maps to a role.
func protectedRefusal(mb sync.Mailbox, f *Folder) *Refusal {
	if strings.EqualFold(mb.Name, "INBOX") {
		return &Refusal{RefuseInbox, "INBOX is never emptied or deleted"}
	}
	for _, a := range mb.Attrs {
		if isSpecialUse(a) {
			return &Refusal{RefuseSpecialUse, fmt.Sprintf("the server marks it %s", a)}
		}
	}
	// Defence in depth for a provider that advertises no SPECIAL-USE: Mailroom
	// may still have mapped the folder to a role (a manual mapping), and a
	// folder Mailroom files Sent or Drafts into is not a folder to empty.
	if f.Mailroom != nil && f.Mailroom.Role != "" {
		return &Refusal{RefuseMailroomRole, fmt.Sprintf("Mailroom uses it as %q", f.Mailroom.Role)}
	}
	return nil
}

// isSpecialUse reports an RFC 6154 role attribute, plus \Important (RFC 8457).
func isSpecialUse(a sync.MailboxAttr) bool {
	_, ok := sync.Mailbox{Attrs: []sync.MailboxAttr{a}}.SpecialUse()
	return ok
}

// ---- apply ------------------------------------------------------------------

// Result is what Apply did.
type Result struct {
	MovedFolders  []string
	MovedMessages uint64
	Deleted       []string
	// NotDeleted lists deletions refused at the moment of acting (a message
	// arrived, a child appeared) — each with its reason.
	NotDeleted []string
	// AlreadyGone counts deletion targets no longer on the server.
	AlreadyGone int
	// Bounded is true when --max-folders ended the run before the plan did.
	Bounded bool
}

func (r *Result) summary() string {
	moved := "none"
	if len(r.MovedFolders) > 0 {
		moved = fmt.Sprintf("%d folders (%d messages): %s", len(r.MovedFolders), r.MovedMessages, listSome(r.MovedFolders, 10))
	}
	deleted := "none"
	if len(r.Deleted) > 0 {
		deleted = fmt.Sprintf("%d folders: %s", len(r.Deleted), listSome(r.Deleted, 10))
	}
	return "already done this run — moved: " + moved + "; deleted: " + deleted
}

// StopError is Apply stopping because the server did not end up where a step
// said it would, or a step failed. Nothing after it was attempted.
type StopError struct {
	Folder string
	Step   string // "move" or "delete"
	Detail string
	Done   Result
}

func (e *StopError) Error() string {
	return fmt.Sprintf("STOPPED at %s %q: %s\n  %s\n  Nothing after this folder was attempted. Check the folder on the "+
		"provider; re-running re-reads every count and continues from where the server is now.",
		e.Step, e.Folder, e.Detail, e.Done.summary())
}

// Apply performs the plan: every move first, one folder at a time, then every
// deletion, leaves before parents. It prints one line per step to w.
func Apply(ctx context.Context, s Session, p *Plan, w io.Writer) (Result, error) {
	var res Result
	used := 0
	budgetLeft := func() bool {
		if p.Opts.MaxFolders > 0 && used >= p.Opts.MaxFolders {
			res.Bounded = true
			_, _ = fmt.Fprintf(w, "reached --max-folders %d; re-run to continue (the report will show what remains)\n", p.Opts.MaxFolders)
			return false
		}
		return true
	}
	stop := func(folder, step, detail string) (Result, error) {
		return res, &StopError{Folder: folder, Step: step, Detail: detail, Done: res}
	}

	for _, f := range p.Moves {
		if f.Action != ActionMove {
			continue
		}
		if !budgetLeft() {
			return res, nil
		}
		dest := p.Dest.Name
		src, err := s.Status(ctx, f.Name)
		if err != nil {
			return stop(f.Name, "move", "STATUS of the source failed before moving: "+err.Error())
		}
		if src.Messages == 0 {
			_, _ = fmt.Fprintf(w, "skip    %s: empty now, nothing to move\n", f.Name)
			continue
		}
		before, err := s.Status(ctx, dest)
		if err != nil {
			return stop(f.Name, "move", "STATUS of the destination failed before moving: "+err.Error())
		}
		if err := s.MoveAll(ctx, f.Name, dest); err != nil {
			return stop(f.Name, "move", fmt.Sprintf("UID MOVE 1:* into %q failed: %v (the source held %d, the destination %d; "+
				"the move may have partly happened)", dest, err, src.Messages, before.Messages))
		}
		used++
		after, err := s.Status(ctx, f.Name)
		if err != nil {
			return stop(f.Name, "move", "STATUS of the source failed AFTER the move: "+err.Error())
		}
		afterDest, err := s.Status(ctx, dest)
		if err != nil {
			return stop(f.Name, "move", "STATUS of the destination failed AFTER the move: "+err.Error())
		}
		want := before.Messages + src.Messages
		if after.Messages != 0 || afterDest.Messages != want || afterDest.UIDValidity != before.UIDValidity {
			return stop(f.Name, "move", fmt.Sprintf(
				"after UID MOVE the source holds %d (want 0) and %q holds %d (want %d = %d before + %d moved)%s. "+
					"If new mail arrived in %q meanwhile, that alone explains a higher count",
				after.Messages, dest, afterDest.Messages, want, before.Messages, src.Messages,
				uidValidityNote(before.UIDValidity, afterDest.UIDValidity), dest))
		}
		res.MovedFolders = append(res.MovedFolders, f.Name)
		res.MovedMessages += uint64(src.Messages)
		_, _ = fmt.Fprintf(w, "moved   %s: %d messages into %s (%d -> %d); source now 0\n",
			f.Name, src.Messages, dest, before.Messages, afterDest.Messages)
	}

	var deletes []*Folder
	for _, f := range p.Deletes {
		if f.Action == ActionDelete || f.Action == ActionDeleteAfterMove {
			deletes = append(deletes, f)
		}
	}
	if len(deletes) == 0 {
		return res, nil
	}

	// A fresh LIST: the tree as it is now, not as it was when planned.
	boxes, err := s.ListMailboxes(ctx)
	if err != nil {
		return stop(deletes[0].Name, "delete", "re-LIST before deleting failed: "+err.Error())
	}
	live := map[string]sync.Mailbox{}
	for _, mb := range boxes {
		live[mb.Name] = mb
	}
	deleted := map[string]bool{}
	notDeleted := func(f *Folder, why string) {
		res.NotDeleted = append(res.NotDeleted, f.Name+": "+why)
		_, _ = fmt.Fprintf(w, "KEEP    %s: not deleted — %s\n", f.Name, why)
	}
	for _, f := range deletes {
		mb, ok := live[f.Name]
		if !ok {
			res.AlreadyGone++
			_, _ = fmt.Fprintf(w, "skip    %s: already gone\n", f.Name)
			continue
		}
		if r := protectedRefusal(mb, f); r != nil {
			notDeleted(f, r.String())
			continue
		}
		var kids []string
		for _, c := range childrenOf(f, boxes) {
			if !deleted[c] {
				kids = append(kids, c)
			}
		}
		if len(kids) > 0 {
			notDeleted(f, "it still has child folders: "+listSome(kids, 3))
			continue
		}
		if selectable(mb) {
			// THE RE-READ. Immediately before DELETE, whatever the plan said:
			// mail can arrive in a folder between the plan and now, and a
			// server-side filter can file into it at any moment.
			st, err := s.Status(ctx, f.Name)
			if err != nil {
				return stop(f.Name, "delete", "STATUS immediately before DELETE failed: "+err.Error())
			}
			if st.Messages != 0 {
				notDeleted(f, fmt.Sprintf("STATUS reads %d messages immediately before DELETE", st.Messages))
				continue
			}
		}
		if !budgetLeft() {
			return res, nil
		}
		if err := s.DeleteMailbox(ctx, f.Name); err != nil {
			return stop(f.Name, "delete", "DELETE failed: "+err.Error())
		}
		used++
		deleted[f.Name] = true
		res.Deleted = append(res.Deleted, f.Name)
		_, _ = fmt.Fprintf(w, "deleted %s\n", f.Name)
	}
	return res, nil
}

func uidValidityNote(before, after uint32) string {
	if before == after {
		return ""
	}
	return fmt.Sprintf(", and the destination's UIDVALIDITY changed %d -> %d", before, after)
}

// ---- report -----------------------------------------------------------------

// WriteReport prints the plan: every folder with the server's STATUS,
// Mailroom's count and the decision, then totals per category.
func WriteReport(w io.Writer, p *Plan) error {
	var b strings.Builder
	tw := tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
	// Every write lands in b (a strings.Builder, via tabwriter); the one error
	// worth reporting is the final WriteString to w.
	pf := func(w io.Writer, format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }

	if p.Opts.Into != "" {
		switch {
		case p.DestRefusal != nil:
			pf(tw, "destination\t%s\tREFUSE: %s\n", p.Opts.Into, p.DestRefusal.Detail)
		default:
			pf(tw, "destination\t%s\tserver MESSAGES=%d UIDNEXT=%d\tMailroom live=%s\n",
				p.Dest.Name, p.Dest.Server.Messages, p.Dest.Server.UIDNext, mailroomCell(p.Dest))
		}
		moveCap := "advertised"
		if !p.MoveSupported {
			moveCap = "NOT advertised — every move is refused"
		}
		pf(tw, "server MOVE\t%s\n", moveCap)
	}
	_ = tw.Flush()

	section := func(title string, folders []*Folder) {
		pf(&b, "\n%s\n", title)
		if len(folders) == 0 {
			pf(&b, "  (no folder matches)\n")
			return
		}
		tw := tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
		pf(tw, "  FOLDER\tSERVER\tUIDNEXT\tMAILROOM\tPLAN\n")
		for _, f := range folders {
			server, uidnext := "-", "-"
			if f.Server != nil {
				server = fmt.Sprintf("%d", f.Server.Messages)
				uidnext = fmt.Sprintf("%d", f.Server.UIDNext)
			}
			pf(tw, "  %s\t%s\t%s\t%s\t%s\n", f.Name, server, uidnext, mailroomCell(f), planCell(f))
		}
		_ = tw.Flush()
	}
	if p.Opts.From != "" {
		section(fmt.Sprintf("MOVE into %s  (--from %q)", p.Opts.Into, p.Opts.From), p.Moves)
	}
	if len(p.Deletes) > 0 || len(p.Opts.DeleteEmpty) > 0 {
		section(fmt.Sprintf("DELETE when empty, leaves before parents  (--delete-empty %s)", quoteAll(p.Opts.DeleteEmpty)), p.Deletes)
	}

	t := p.Totals()
	pf(&b, "\nTOTALS\n")
	tw = tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
	if p.Opts.From != "" {
		pf(tw, "  move\t%d folders\t%d messages\n", t.MoveFolders, t.MoveMessages)
		pf(tw, "  nothing to move (empty)\t%d folders\n", t.NothingToMove)
		pf(tw, "  not selectable (hold no mail)\t%d folders\n", t.NotSelectable)
		pf(tw, "  refused moves\t%d folders\t%s\n", t.RefusedMoves, classes(t.RefusedMoveClasses))
	}
	if len(p.Opts.DeleteEmpty) > 0 {
		pf(tw, "  delete now (empty)\t%d folders\n", t.DeleteNow)
		pf(tw, "  delete after its move\t%d folders\n", t.DeleteAfterMove)
		pf(tw, "  refused deletions\t%d folders\t%s\n", t.RefusedDeletes, classes(t.RefusedDeleteClasses))
	}
	pf(tw, "  Mailroom count differs from server\t%d folders\t(marked *; informational — Mailroom re-reads the server after any change)\n", t.MailroomDiffers)
	pf(tw, "  Mailroom holds no row for\t%d folders\t(never synced, or not visible to sync)\n", t.MailroomMissing)
	_ = tw.Flush()
	if p.Dest != nil && t.MoveMessages > 0 {
		pf(&b, "\n%s would go from %d to %d messages (plus any mail that arrives meanwhile).\n",
			p.Dest.Name, p.Dest.Server.Messages, uint64(p.Dest.Server.Messages)+t.MoveMessages)
	}
	ops := t.MoveFolders + t.DeleteNow + t.DeleteAfterMove
	switch {
	case ops == 0:
		pf(&b, "\nNothing to do: no folder would be moved or deleted.\n")
	case p.Opts.MaxFolders > 0 && ops > p.Opts.MaxFolders:
		pf(&b, "\n%d folder operations planned; --max-folders %d bounds one --apply run to the first %d (moves first).\n",
			ops, p.Opts.MaxFolders, p.Opts.MaxFolders)
	default:
		pf(&b, "\n%d folder operations planned.\n", ops)
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func mailroomCell(f *Folder) string {
	if f.Mailroom == nil {
		return "-"
	}
	s := fmt.Sprintf("%d", f.Mailroom.Live)
	if f.Server != nil && int64(f.Server.Messages) != f.Mailroom.Live {
		s += "*"
	}
	return s
}

func planCell(f *Folder) string {
	switch f.Action {
	case ActionMove:
		return fmt.Sprintf("move %d", f.Server.Messages)
	case ActionRefuse:
		return "REFUSE (" + f.Refusal.String() + ")"
	case ActionDelete:
		if !f.Selectable {
			return "delete (\\Noselect: holds no mail)"
		}
		return "delete"
	case ActionDeleteAfterMove:
		return "delete after its move, if STATUS then reads 0"
	default:
		return string(f.Action)
	}
}

// Totals is the plan's per-category count — never one number, so "nothing to
// do" and "everything refused" cannot look alike.
type Totals struct {
	MoveFolders, NothingToMove, NotSelectable, RefusedMoves int
	MoveMessages                                            uint64
	DeleteNow, DeleteAfterMove, RefusedDeletes              int
	RefusedMoveClasses, RefusedDeleteClasses                map[string]int
	MailroomDiffers, MailroomMissing                        int
}

// Totals counts the plan per category.
func (p *Plan) Totals() Totals {
	t := Totals{RefusedMoveClasses: map[string]int{}, RefusedDeleteClasses: map[string]int{}}
	seen := map[string]bool{}
	note := func(f *Folder) {
		if seen[f.Name] {
			return
		}
		seen[f.Name] = true
		switch {
		case f.Mailroom == nil:
			t.MailroomMissing++
		case f.Server != nil && int64(f.Server.Messages) != f.Mailroom.Live:
			t.MailroomDiffers++
		}
	}
	for _, f := range p.Moves {
		note(f)
		switch f.Action {
		case ActionMove:
			t.MoveFolders++
			t.MoveMessages += uint64(f.Server.Messages)
		case ActionNothingToMove:
			t.NothingToMove++
		case ActionNotSelectable:
			t.NotSelectable++
		case ActionRefuse:
			t.RefusedMoves++
			t.RefusedMoveClasses[f.Refusal.Class]++
		}
	}
	for _, f := range p.Deletes {
		note(f)
		switch f.Action {
		case ActionDelete:
			t.DeleteNow++
		case ActionDeleteAfterMove:
			t.DeleteAfterMove++
		case ActionRefuse:
			t.RefusedDeletes++
			t.RefusedDeleteClasses[f.Refusal.Class]++
		}
	}
	return t
}

func classes(m map[string]int) string {
	if len(m) == 0 {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s %d", k, m[k])
	}
	return "(" + strings.Join(parts, ", ") + ")"
}

func quoteAll(ss []string) string {
	q := make([]string, len(ss))
	for i, s := range ss {
		q[i] = fmt.Sprintf("%q", s)
	}
	return strings.Join(q, " ")
}

// ---- names ------------------------------------------------------------------

// Match reports whether name matches an IMAP LIST pattern (RFC 9051 §6.3.9):
// `*` matches any run of characters INCLUDING the hierarchy delimiter, `%`
// any run EXCLUDING it; every other character matches itself, case-sensitively
// — except a leading INBOX, which IMAP treats case-insensitively. So with "/"
// as the delimiter, "INBOX/*" matches "INBOX/a" and "INBOX/a/b" but not
// "INBOX", and "INBOX/%" matches "INBOX/a" but not "INBOX/a/b".
func Match(pattern, name string, delim rune) bool {
	return match([]rune(canonInbox(pattern, delim)), []rune(canonInbox(name, delim)), delim)
}

func match(p, n []rune, delim rune) bool {
	for len(p) > 0 {
		switch p[0] {
		case '*':
			for i := 0; i <= len(n); i++ {
				if match(p[1:], n[i:], delim) {
					return true
				}
			}
			return false
		case '%':
			for i := 0; i <= len(n); i++ {
				if match(p[1:], n[i:], delim) {
					return true
				}
				if i < len(n) && delim != 0 && n[i] == delim {
					return false
				}
			}
			return false
		default:
			if len(n) == 0 || n[0] != p[0] {
				return false
			}
			p, n = p[1:], n[1:]
		}
	}
	return len(n) == 0
}

// canonInbox upper-cases a leading INBOX component.
func canonInbox(s string, delim rune) string {
	if len(s) < 5 || !strings.EqualFold(s[:5], "INBOX") {
		return s
	}
	if len(s) == 5 || (delim != 0 && strings.HasPrefix(s[5:], string(delim))) {
		return "INBOX" + s[5:]
	}
	return s
}

func sameName(a, b string) bool {
	if strings.EqualFold(a, "INBOX") && strings.EqualFold(b, "INBOX") {
		return true
	}
	return a == b
}

func findMailbox(boxes []sync.Mailbox, name string) (sync.Mailbox, bool) {
	for _, mb := range boxes {
		if sameName(mb.Name, name) {
			return mb, true
		}
	}
	return sync.Mailbox{}, false
}

func lookupCount(m map[string]MailroomCount, name string) (MailroomCount, bool) {
	if c, ok := m[name]; ok {
		return c, true
	}
	if strings.EqualFold(name, "INBOX") {
		for k, c := range m {
			if strings.EqualFold(k, "INBOX") {
				return c, true
			}
		}
	}
	return MailroomCount{}, false
}

func selectable(mb sync.Mailbox) bool {
	return !mb.HasAttr(sync.MailboxAttrNoSelect) && !mb.HasAttr(sync.MailboxAttrNonExistent)
}

func depth(mb sync.Mailbox) int {
	if mb.Delimiter == 0 {
		return 0
	}
	return strings.Count(mb.Name, string(mb.Delimiter))
}

// childrenOf lists every folder below f (any depth), by name.
func childrenOf(f *Folder, boxes []sync.Mailbox) []string {
	if f.Delim == 0 {
		return nil
	}
	prefix := canonInbox(f.Name, f.Delim) + string(f.Delim)
	var out []string
	for _, mb := range boxes {
		if strings.HasPrefix(canonInbox(mb.Name, f.Delim), prefix) {
			out = append(out, mb.Name)
		}
	}
	return out
}

func listSome(names []string, n int) string {
	if len(names) <= n {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s (and %d more)", strings.Join(names[:n], ", "), len(names)-n)
}
