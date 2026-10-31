// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
)

// DAY-ONE SETUP: THE BULK IMPORT, ITS PREVIEW, AND ITS UNDO (#446, migration
// 00050).
//
// A folder-sorted mailbox becomes a desk. Mail from the last N days (default
// 30) is newly arrived and stays on the desk; everything OLDER gets its area
// and its labels and is ARCHIVED IN MAILROOM — and NOTHING MOVES ON THE
// SERVER. Per folder the operator may instead bring the mail to the desk (a
// real server-side move to INBOX) or leave it entirely alone.
//
// # Report before apply, and the report is the same code as the apply
//
// SetupPreview and SetupApplyChunk build their per-folder predicates from THE
// SAME builders — setupChosenSQL, setupLiveMessageSQL, setupLabelMissingSQL and
// setupMoveNeededSQL — for the reason #155 records: a preview that can disagree with the engine is worse
// than none, because it is a confident wrong answer. The preview says "write
// N labels, archive M in Mailroom, move 0" and the run then records
// what it actually wrote in its own columns beside the planned ones, so the
// two can be compared rather than trusted.
//
// # A message belongs to exactly ONE folder of the plan
//
// Mail can hold live provenance in several folders at once (Gmail's labels are
// IMAP folders, so this is the common case, not the exotic one). If each folder
// counted its own provenance independently, a message in two planned folders
// would be counted twice in the preview and written once by the apply, and the
// promised numbers would simply be wrong.
//
// So setupChosenSQL assigns every message to ONE folder: the EARLIEST in the
// plan's own order that holds it. Planned folders come first, in plan order,
// then every other census folder as an implicit `leave` — which makes the
// per-folder counts a true PARTITION of the population, so Labels + Archived +
// Moves + Leave describe disjoint sets and the totals are the sums. A folder
// the plan does not mention can therefore never take a message away from one it
// does.
//
// # The journal is what makes the run undoable, and what makes it retryable
//
// One app.setup_journal row per message per run, written even when nothing
// changed, records what THIS RUN did: the label it inserted (or NULL, when the
// message already carried it), whether it set desk_archived_at, and the folder
// a moved message came out of. Undo replays the inverse from those rows and
// nothing else, so it can never take away a label the user had already applied
// by hand, and it never un-archives a message that was already archived.
//
// The same rows are the CHUNK CURSOR: the chunk query skips any message that
// already has a journal row for this run. That is what makes the job safe to
// retry — a crashed or dead-lettered chunk resumes exactly where it stopped,
// with no bookkeeping outside the rows the run itself wrote.

// SetupJobKind is the durable-job kind the day-one import worker registers for
// (internal/setup). ONE-SHOT, unlike DrawerJobKind: it is enqueued by
// Setup/apply and Setup/undo and nothing seeds it at boot.
const SetupJobKind = "setup_import"

// SetupRunKind values (app.setup_runs.kind). Only the import has a run: the
// census, the preview and the rule induction write nothing.
const SetupRunKindImport = "import"

// The run lifecycle (app.setup_runs.status). 'undoing' is a STATUS rather than
// a flag because the same job kind drains both directions and reads this to
// decide which way it is going.
const (
	SetupStatusQueued  = "queued"
	SetupStatusRunning = "running"
	SetupStatusDone    = "done"
	SetupStatusFailed  = "failed"
	SetupStatusUndoing = "undoing"
	SetupStatusUndone  = "undone"
)

// The per-folder actions of step 2.
const (
	// SetupActionLabelArchive is the default and the point of the feature: the
	// folder's older mail gets its label (and, through it, its area) and is
	// ARCHIVED IN MAILROOM. Nothing moves on the server.
	SetupActionLabelArchive = "labelArchive"
	// SetupActionBringToDesk is the per-folder override for a folder whose mail
	// is genuinely unactioned: a REAL server-side move to the account's INBOX,
	// with the writeback intent a user's own move would enqueue. It is the one
	// action here that changes anything on the provider.
	SetupActionBringToDesk = "bringToDesk"
	// SetupActionLeave touches the folder not at all. It is also what a folder
	// absent from the plan gets.
	SetupActionLeave = "leave"
)

// setupWritesPerSecond is the rate SetupCounts.EstimatedSeconds is computed at.
//
// ⚠ IT IS A STATED PLANNING RATE, NOT A MEASUREMENT. Nothing has benchmarked
// this; it is a deliberately conservative round number for three indexed writes
// and a journal row per message inside a chunked transaction, and it exists so
// the wizard can say "about two minutes" instead of showing a bare count. Treat
// a disagreement with reality as a wrong estimate, never as a wrong count — the
// counts beside it are measured.
const setupWritesPerSecond = 400

// ErrSetupPlanInvalid wraps the reason a plan cannot be previewed or applied:
// an unknown mailbox, a special-use mailbox, bringToDesk on INBOX itself, a
// non-positive window. Typed so the API can answer invalidArguments with the
// reason rather than a raw SQL failure.
var ErrSetupPlanInvalid = errors.New("store: invalid setup plan")

// ErrSetupRunBusy is returned when a run cannot be created because one is
// already outstanding for this login, and when an undo is asked for a run that
// is still running (the client disables Undo until the run is done).
var ErrSetupRunBusy = errors.New("store: setup run busy")

// ErrSetupRunNotFound is returned for a run id that does not resolve for the
// caller. RLS makes another user's run indistinguishable from a missing one,
// which is the point: fail closed.
var ErrSetupRunNotFound = errors.New("store: setup run not found")

// SetupFolderChoice is one folder's line of the plan.
type SetupFolderChoice struct {
	MailboxID ulid.ULID `json:"mailboxId"`
	// Action is one of the SetupAction* constants.
	Action string `json:"action"`
	// LabelName is the label this folder's older mail gets; it defaults to the
	// folder's own display name, because that is the word the user already
	// chose. IGNORED FOR INBOX, whose label is the message's own resolved
	// area's label instead (there is no single word for "the inbox").
	LabelName string `json:"labelName,omitempty"`
	// AreaID names an EXISTING area the folder's label should feed. Nil to use
	// AreaName instead.
	AreaID *ulid.ULID `json:"areaId,omitempty"`
	// AreaName creates (or reuses, by case-insensitive name) an area of this
	// name owning the folder's label. EMPTY means no area at all — the mail
	// gets a label and nothing else, which is a legitimate choice for a folder
	// that is an archive rather than a topic.
	AreaName string `json:"areaName,omitempty"`
}

// SetupPlan is the whole of step 2's decision: the window, and a line per
// folder. A folder in the census but absent from Folders is `leave`.
type SetupPlan struct {
	WindowDays int                 `json:"windowDays"`
	Folders    []SetupFolderChoice `json:"folders"`
}

// SetupFolderCounts is one folder's line of the preview. Older is the folder's
// share of the population — the denominator the other three are counted
// against.
type SetupFolderCounts struct {
	MailboxID ulid.ULID
	Name      string
	Action    string
	Older     int
	Labels    int
	Archived  int
	Moves     int
}

// SetupCounts is the preview: what applying this plan would write, per folder
// and in total.
//
// PER-FOLDER, NEVER ONE TOTAL (CLAUDE.md): a single number cannot tell "this
// plan does nothing because the window covers everything" from "this plan does
// nothing because every message is already labelled", and an operator would act
// on those two findings completely differently.
type SetupCounts struct {
	Labels   int
	Archived int
	Moves    int
	// Leave is how many older messages the plan deliberately does not touch —
	// the folders marked `leave` plus every census folder the plan omits.
	Leave int
	// Unclassified is how many of the messages the plan WOULD touch carry no
	// From address, so nothing about their sender is known. Counted, never
	// hidden.
	Unclassified     int
	PerFolder        []SetupFolderCounts
	EstimatedSeconds int
}

// SetupProgress is one chunk's outcome, in either direction.
//
// In the APPLY direction Labels/Archived/Moves count what was written; in the
// UNDO direction they count what was REVERSED (labels removed, messages
// un-archived, messages moved back). Processed is journal rows written (apply)
// or consumed (undo), which is what advances the cursor.
type SetupProgress struct {
	Done      bool
	Processed int
	Labels    int
	Archived  int
	Moves     int
	// Changed is how many messages got an Email change_log row this chunk —
	// exactly the messages something actually happened to.
	Changed int
}

// SetupRun mirrors app.setup_runs.
type SetupRun struct {
	ID         ulid.ULID
	Kind       string
	Status     string
	WindowDays int
	Plan       SetupPlan
	// PlannedLabels/PlannedArchived/PlannedMoves are what the preview promised;
	// LabelsWritten/Archived/Moved are what the run did. Both, so they can be
	// compared.
	PlannedLabels   int
	LabelsWritten   int
	PlannedArchived int
	Archived        int
	PlannedMoves    int
	Moved           int
	LeftForLater    int
	Unclassified    int
	Error           string
	CreatedAt       time.Time
	StartedAt       *time.Time
	FinishedAt      *time.Time
	UndoneAt        *time.Time
}

// setupFolderFacts is one folder of the effective plan, resolved against the
// database: the choice, the folder's identity, the label its mail gets, and —
// for bringToDesk — the account's INBOX the move targets.
type setupFolderFacts struct {
	Choice    SetupFolderChoice
	MailboxID ulid.ULID
	AccountID ulid.ULID
	Name      string
	Role      string
	// LabelID is the label this folder's mail gets, when one already exists.
	// Nil means either "INBOX, so the label is per-message" or "the plan names
	// a label that has not been minted yet" — the preview then counts every
	// older message as needing it, which is exactly right.
	LabelID *ulid.ULID
	// InboxID is the account's INBOX; set only for bringToDesk.
	InboxID *ulid.ULID
	// Planned is false for a census folder the plan omits, which is an implicit
	// `leave`. Such folders take part in the message-to-folder assignment (so
	// the counts stay a partition) but are never acted on.
	Planned bool
}

func (f setupFolderFacts) action() string {
	if !f.Planned {
		return SetupActionLeave
	}
	return f.Choice.Action
}

// actionable reports whether this folder's mail is written to at all.
func (f setupFolderFacts) actionable() bool {
	a := f.action()
	return a == SetupActionLabelArchive || a == SetupActionBringToDesk
}

// resolveSetupPlan validates plan and returns the EFFECTIVE folder order: the
// plan's own folders first, in the order the plan lists them, then every other
// census folder as an implicit `leave`.
//
// THAT ORDER IS THE MESSAGE-TO-FOLDER PRECEDENCE (see the file header), which
// is why it is computed once, here, and handed to both the preview and the
// apply rather than rebuilt by each.
func (tx *UserTx) resolveSetupPlan(ctx context.Context, plan SetupPlan) ([]setupFolderFacts, error) {
	if plan.WindowDays <= 0 {
		return nil, fmt.Errorf("%w: windowDays must be positive, got %d", ErrSetupPlanInvalid, plan.WindowDays)
	}
	census, err := tx.MailboxCensus(ctx, nil, plan.WindowDays)
	if err != nil {
		return nil, err
	}
	byID := make(map[ulid.ULID]FolderCensus, len(census))
	for _, c := range census {
		byID[c.MailboxID] = c
	}
	labels, err := tx.labelIDsByLowerName(ctx)
	if err != nil {
		return nil, err
	}

	out := make([]setupFolderFacts, 0, len(census))
	seen := make(map[ulid.ULID]bool, len(plan.Folders))
	for _, ch := range plan.Folders {
		switch ch.Action {
		case SetupActionLabelArchive, SetupActionBringToDesk, SetupActionLeave:
		default:
			return nil, fmt.Errorf("%w: unknown action %q for mailbox %s", ErrSetupPlanInvalid, ch.Action, ch.MailboxID)
		}
		if seen[ch.MailboxID] {
			return nil, fmt.Errorf("%w: mailbox %s appears twice", ErrSetupPlanInvalid, ch.MailboxID)
		}
		seen[ch.MailboxID] = true
		c, ok := byID[ch.MailboxID]
		if !ok {
			// Either the mailbox does not exist for this caller (RLS makes those
			// indistinguishable, deliberately) or it is a SPECIAL-USE folder,
			// which the census excludes because those are "mapped without
			// asking" — labelling Sent "Sent" is not a decision anybody should
			// be asked to make.
			return nil, fmt.Errorf("%w: mailbox %s is not a folder day-one setup can act on "+
				"(unknown, or a special-use folder, which is mapped without asking)", ErrSetupPlanInvalid, ch.MailboxID)
		}
		f := setupFolderFacts{
			Choice: ch, MailboxID: c.MailboxID, AccountID: c.AccountID,
			Name: c.Name, Role: c.Role, Planned: true,
		}
		if ch.Action == SetupActionBringToDesk && c.Role == "inbox" {
			return nil, fmt.Errorf("%w: bringToDesk on INBOX (%s) is meaningless — the inbox IS the desk",
				ErrSetupPlanInvalid, ch.MailboxID)
		}
		if ch.Action == SetupActionBringToDesk {
			inbox, found, ierr := tx.accountInboxID(ctx, c.AccountID)
			if ierr != nil {
				return nil, ierr
			}
			if !found {
				return nil, fmt.Errorf("%w: bringToDesk on %s, but its account has no INBOX to move to",
					ErrSetupPlanInvalid, ch.MailboxID)
			}
			f.InboxID = &inbox
		}
		// INBOX gets no folder label: its mail is labelled with its own resolved
		// area's label, per message (setupInboxLabels).
		if c.Role != "inbox" && ch.Action != SetupActionLeave {
			name := strings.TrimSpace(ch.LabelName)
			if name == "" {
				name = c.Name
			}
			f.Choice.LabelName = name
			if id, ok := labels[strings.ToLower(name)]; ok {
				lid := id
				f.LabelID = &lid
			}
		}
		out = append(out, f)
	}
	for _, c := range census {
		if seen[c.MailboxID] {
			continue
		}
		out = append(out, setupFolderFacts{
			MailboxID: c.MailboxID, AccountID: c.AccountID, Name: c.Name, Role: c.Role,
		})
	}
	return out, nil
}

// accountInboxID finds an account's INBOX by the reserved IMAP name (RFC 3501
// §5.1, case-insensitive) — the same rule roleForMailbox applies, and the same
// one deskLiveSQL's own INBOX test uses.
func (tx *UserTx) accountInboxID(ctx context.Context, accountID ulid.ULID) (ulid.ULID, bool, error) {
	var idStr string
	err := tx.tx.QueryRow(ctx, `
		SELECT id FROM app.mailboxes
		 WHERE user_id = $1 AND account_id = $2 AND upper(imap_name) = 'INBOX'
		 ORDER BY id
		 LIMIT 1`,
		tx.userID.String(), accountID.String()).Scan(&idStr)
	if err != nil {
		if isNoRows(err) {
			return ulid.ULID{}, false, nil
		}
		return ulid.ULID{}, false, fmt.Errorf("store: account inbox: %w", err)
	}
	id, perr := ulid.Parse(idStr)
	if perr != nil {
		return ulid.ULID{}, false, fmt.Errorf("store: parse account inbox id %q: %w", idStr, perr)
	}
	return id, true, nil
}

// setupChosenSQL is the message-to-folder assignment, as a CTE body: for every
// message with live provenance in one of `order`'s folders, the ONE folder that
// decides it — the earliest in `order`. See the file header for why one folder
// and not all of them.
//
// `order` is resolveSetupPlan's output order (planned folders first, in plan
// order), so the same message always lands on the same folder in the preview
// and in every chunk of the apply.
func setupChosenSQL(userPH string, bind func(any) string, order []ulid.ULID) string {
	idsPH := bind(ulidStrings(order))
	return `SELECT DISTINCT ON (mp.message_id) mp.message_id, mp.mailbox_id
	          FROM app.message_provenance mp
	         WHERE mp.user_id = ` + userPH + ` AND mp.state = 'live'
	           AND mp.mailbox_id = ANY(` + idsPH + `)
	         ORDER BY mp.message_id, array_position(` + idsPH + `::text[], mp.mailbox_id::text)`
}

// setupLiveMessageSQL is the message-side predicate both directions share: not
// junk, not trashed, not vanished, not a draft. `m` is the caller's alias.
//
// It is deliberately NOT deskMessageSQL: day-one acts on mail because of the
// FOLDER it is in, whether or not that folder feeds the desk — a "Receipts"
// folder that has never counted as unactioned is exactly the mail this feature
// exists to label.
const setupLiveMessageSQL = `NOT m.is_junk AND m.trashed_at IS NULL AND m.vanished_at IS NULL AND NOT m.is_draft`

// setupLabelMissingSQL is the "this message does not yet carry the label this
// folder would give it" predicate — the ONE definition the preview's Labels
// count and the apply's INSERT ... ON CONFLICT DO NOTHING both express, so the
// promised count and the written count are answers to the same question.
//
// For INBOX it is the area resolver's two arms read directly: the label arm being
// NULL while the tab arm names an area means "no label of this message feeds
// any area, but its TAB VERDICT places it in one" — which is EXACTLY
// applyAreaLabel's rule (area_labels.go), including the part that matters: a
// message already routed into an area BY A LABEL is already written down, and
// giving it a second area's own label on top would manufacture the tie
// areas.Resolution.Contested exists to name.
func setupLabelMissingSQL(f setupFolderFacts, bind func(any) string) string {
	if !f.actionable() {
		return boolLit(false)
	}
	if f.Role == "inbox" {
		// "$1" is the user id in every query this is built into (SetupPreview
		// binds it first). areaMatchSQL needs it because this predicate sits in
		// a FILTER clause, which Postgres plans on its own: it cannot borrow the
		// outer `m.user_id = $1` the way a top-level WHERE semi-join does.
		return areaMatchSQL(bind, "$1", "area_la.area_id IS NULL AND area_tf.area_id IS NOT NULL")
	}
	if f.LabelID == nil {
		// The label does not exist yet (CreateSetupRun mints it), so every older
		// message in the folder needs it.
		return boolLit(true)
	}
	return `NOT EXISTS (SELECT 1 FROM app.message_labels ml
	          WHERE ml.user_id = m.user_id AND ml.message_id = m.id
	            AND ml.label_id = ` + bind(f.LabelID.String()) + `)`
}

// setupMoveNeededSQL is "this message is not already effectively in the
// account's INBOX", through inMailboxSQL — the SAME membership derivation
// Email/query's `inMailbox` filter uses (jmap_email.go). One implementation, so
// the preview cannot promise a move the apply then declines to make.
func setupMoveNeededSQL(f setupFolderFacts, bind func(any) string, userID ulid.ULID) string {
	if f.action() != SetupActionBringToDesk || f.InboxID == nil {
		return boolLit(false)
	}
	return "NOT " + inMailboxSQL(bind, userID, *f.InboxID)
}

// SetupPreview counts, per folder and in total, what applying plan would write.
// IT WRITES NOTHING — the report that has to exist before the bulk import does
// (CLAUDE.md).
func (tx *UserTx) SetupPreview(ctx context.Context, plan SetupPlan) (SetupCounts, error) {
	facts, err := tx.resolveSetupPlan(ctx, plan)
	if err != nil {
		return SetupCounts{}, err
	}
	order := make([]ulid.ULID, len(facts))
	for i, f := range facts {
		order[i] = f.MailboxID
	}

	var counts SetupCounts
	for _, f := range facts {
		args := []any{tx.userID.String()}
		bind := bindFunc(&args)
		chosen := setupChosenSQL("$1", bind, order)
		cutoff := "now() - " + bind(plan.WindowDays) + "::int * interval '1 day'"
		labelMissing := setupLabelMissingSQL(f, bind)
		moveNeeded := setupMoveNeededSQL(f, bind, tx.userID)
		archived := boolLit(false)
		if f.action() == SetupActionLabelArchive {
			archived = "m.desk_archived_at IS NULL"
		}
		folderPH := bind(f.MailboxID.String())

		row := SetupFolderCounts{MailboxID: f.MailboxID, Name: f.Name, Action: f.action()}
		var unclassified int
		if err := tx.tx.QueryRow(ctx, `
			WITH chosen AS (`+chosen+`)
			SELECT count(*),
			       count(*) FILTER (WHERE m.from_address IS NULL),
			       count(*) FILTER (WHERE `+labelMissing+`),
			       count(*) FILTER (WHERE `+archived+`),
			       count(*) FILTER (WHERE `+moveNeeded+`)
			  FROM chosen c
			  JOIN app.messages m ON m.user_id = $1 AND m.id = c.message_id
			 WHERE c.mailbox_id = `+folderPH+`
			   AND `+setupLiveMessageSQL+`
			   AND m.received_at < `+cutoff,
			args...).Scan(&row.Older, &unclassified, &row.Labels, &row.Archived, &row.Moves); err != nil {
			return SetupCounts{}, fmt.Errorf("store: setup preview (%s): %w", f.Name, err)
		}
		if !f.actionable() {
			// A `leave` folder reports its Older anyway — that is the number the
			// operator is deciding about — but contributes nothing else.
			counts.Leave += row.Older
			row.Labels, row.Archived, row.Moves = 0, 0, 0
		} else {
			counts.Labels += row.Labels
			counts.Archived += row.Archived
			counts.Moves += row.Moves
			// The denominator for Unclassified is the mail the plan WOULD TOUCH:
			// "we could not classify N of the M we are about to label" is
			// the useful sentence; counting mail the plan leaves alone would
			// inflate it with folders nobody is acting on.
			counts.Unclassified += unclassified
		}
		counts.PerFolder = append(counts.PerFolder, row)
	}
	writes := counts.Labels + counts.Archived + counts.Moves
	counts.EstimatedSeconds = int(math.Ceil(float64(writes) / float64(setupWritesPerSecond)))
	return counts, nil
}

// CreateSetupRun records the run, MINTS THE LABELS AND AREAS THE PLAN NEEDS
// (once, here, rather than per chunk), and enqueues the one-shot import job.
// Returns ErrSetupRunBusy when a run is already outstanding for this login.
//
// WHY THE LABELS AND AREAS ARE CREATED HERE and not lazily in the chunks: a
// chunk that had to mint them would do it under whatever identity happened to
// run first, could race another chunk into a duplicate-name failure, and — the
// real problem — would make the run's own plan unreadable, because "label
// Receipts" would name a label that may or may not exist yet depending on how
// far the job had got. Minting them in the same transaction as the run row
// means the plan describes objects that exist from the moment it is stored.
func (tx *UserTx) CreateSetupRun(ctx context.Context, plan SetupPlan, counts SetupCounts) (ulid.ULID, error) {
	facts, err := tx.resolveSetupPlan(ctx, plan)
	if err != nil {
		return ulid.ULID{}, err
	}
	if id, busy, err := tx.outstandingSetupRun(ctx); err != nil {
		return ulid.ULID{}, err
	} else if busy {
		return ulid.ULID{}, fmt.Errorf("%w: run %s is already %s", ErrSetupRunBusy, id, "queued, running or undoing")
	}

	for i := range facts {
		if !facts[i].actionable() || facts[i].Role == "inbox" {
			continue
		}
		if err := tx.setupEnsureLabelAndArea(ctx, &facts[i]); err != nil {
			return ulid.ULID{}, err
		}
	}

	// The plan is stored with the label names RESOLVED (a blank LabelName
	// defaulted to the folder's own name), so the row records what was actually
	// done rather than what was left implicit.
	stored := SetupPlan{WindowDays: plan.WindowDays}
	for _, f := range facts {
		if f.Planned {
			stored.Folders = append(stored.Folders, f.Choice)
		}
	}
	payload, err := json.Marshal(stored)
	if err != nil {
		return ulid.ULID{}, fmt.Errorf("store: create setup run (plan): %w", err)
	}

	id := NewULID()
	if _, err := tx.tx.Exec(ctx, `
		INSERT INTO app.setup_runs (id, user_id, kind, status, window_days, plan,
		    planned_labels, planned_archived, planned_moves, left_for_later, unclassified)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		id.String(), tx.userID.String(), SetupRunKindImport, SetupStatusQueued, plan.WindowDays, payload,
		counts.Labels, counts.Archived, counts.Moves, counts.Leave, counts.Unclassified,
	); err != nil {
		return ulid.ULID{}, fmt.Errorf("store: create setup run: %w", err)
	}
	if _, err := tx.AppendChange(ctx, ObjectSetupRun, id, OpCreated); err != nil {
		return ulid.ULID{}, err
	}
	// The payload carries the RUN ID AND NOTHING ELSE (jobPayloadKeys): the
	// worker re-reads the run, and the plan inside it, under the target user's
	// identity, which is the only context RLS can check.
	jobPayload, err := json.Marshal(map[string]string{"run_id": id.String()})
	if err != nil {
		return ulid.ULID{}, fmt.Errorf("store: create setup run (job payload): %w", err)
	}
	if _, err := tx.EnqueueJob(ctx, SetupJobKind, time.Now().UTC(), jobPayload, "setup:"+id.String()); err != nil {
		return ulid.ULID{}, err
	}
	return id, nil
}

// setupEnsureLabelAndArea mints what one folder's line needs: the label, and
// the area that label feeds.
//
// THE FOLDER'S LABEL BECOMES THE AREA'S OWN LABEL when the area is new — which
// is what makes "labelled Receipts" and "in the Receipts area" the same fact
// ("areas are labels", #442) rather than two things that have to be kept in
// step. When the line names an area that already exists, the label is added as
// an ORDINARY FEED of it instead; the existing area's own label is never
// disturbed.
func (tx *UserTx) setupEnsureLabelAndArea(ctx context.Context, f *setupFolderFacts) error {
	if f.LabelID == nil {
		id, err := tx.CreateLabel(ctx, LabelRow{Name: f.Choice.LabelName, MirrorToIMAP: false})
		if err != nil {
			return fmt.Errorf("store: setup run: label %q: %w", f.Choice.LabelName, err)
		}
		f.LabelID = &id
	}

	target := f.Choice.AreaID
	if target == nil && strings.TrimSpace(f.Choice.AreaName) != "" {
		areas, err := tx.areaIDsByLowerName(ctx)
		if err != nil {
			return err
		}
		if id, ok := areas[strings.ToLower(strings.TrimSpace(f.Choice.AreaName))]; ok {
			target = &id
		} else {
			// A NEW area owning this folder's label. Position 0 would push it to
			// the front of the rail and change which area wins a label tie for
			// every existing area (areaJoinSQL orders by position), so day-one
			// appends: a folder becoming an area sits AFTER the arrangement the
			// user already has.
			pos, err := tx.nextAreaPosition(ctx)
			if err != nil {
				return err
			}
			if _, err := tx.CreateArea(ctx, AreaRow{
				Name: strings.TrimSpace(f.Choice.AreaName), Position: pos, LabelID: f.LabelID,
			}); err != nil {
				return fmt.Errorf("store: setup run: area %q: %w", f.Choice.AreaName, err)
			}
			return nil
		}
	}
	if target == nil {
		return nil // label only: a legitimate choice for a folder that is an archive, not a topic
	}
	return tx.setupAddAreaFeed(ctx, *target, *f.LabelID)
}

// nextAreaPosition is one past the caller's last area, so a day-one area is
// APPENDED to the rail rather than inserted into it — see the tie-break
// reasoning in setupEnsureLabelAndArea.
func (tx *UserTx) nextAreaPosition(ctx context.Context) (int, error) {
	var pos int
	if err := tx.tx.QueryRow(ctx,
		`SELECT COALESCE(max(position), -1) + 1 FROM app.areas WHERE user_id = $1`,
		tx.userID.String()).Scan(&pos); err != nil {
		return 0, fmt.Errorf("store: next area position: %w", err)
	}
	return pos, nil
}

// setupAddAreaFeed adds labelID as a feed of areaID through the ordinary areas
// write path (UpdateArea/SetFeeds), so the "a label feeds at most one area"
// invariant, the change_log row and the savepoint discipline are all the ones
// Area/set already has — never a direct INSERT into app.area_feeds.
func (tx *UserTx) setupAddAreaFeed(ctx context.Context, areaID, labelID ulid.ULID) error {
	views, err := tx.GetAreas(ctx, []ulid.ULID{areaID})
	if err != nil {
		return err
	}
	if len(views) == 0 {
		return fmt.Errorf("%w: area %s does not exist", ErrSetupPlanInvalid, areaID)
	}
	v := views[0]
	if v.LabelID == labelID {
		return nil // already the area's own label; nothing to add
	}
	feeds := make([]AreaFeed, 0, len(v.Feeds)+1)
	for _, feed := range v.Feeds {
		if feed.LabelID != nil && *feed.LabelID == v.LabelID {
			continue // the own label is implicit; UpdateArea always keeps it
		}
		if feed.LabelID != nil && *feed.LabelID == labelID {
			return nil // already a feed of this area
		}
		feeds = append(feeds, feed)
	}
	lid := labelID
	feeds = append(feeds, AreaFeed{LabelID: &lid})
	if _, err := tx.UpdateArea(ctx, areaID, AreaPatch{SetFeeds: true, Feeds: feeds}); err != nil {
		return fmt.Errorf("store: setup run: feed area %s: %w", areaID, err)
	}
	return nil
}

// outstandingSetupRun reports a run that is queued, running or undoing — one
// the login has not finished with. Setup/apply refuses to create a second, and
// so does CreateSetupRun itself, in the same transaction, which is what makes
// the check meaningful rather than advisory.
func (tx *UserTx) outstandingSetupRun(ctx context.Context) (ulid.ULID, bool, error) {
	var idStr string
	err := tx.tx.QueryRow(ctx, `
		SELECT id FROM app.setup_runs
		 WHERE user_id = $1 AND status IN ('queued','running','undoing')
		 ORDER BY created_at DESC
		 LIMIT 1`, tx.userID.String()).Scan(&idStr)
	if err != nil {
		if isNoRows(err) {
			return ulid.ULID{}, false, nil
		}
		return ulid.ULID{}, false, fmt.Errorf("store: outstanding setup run: %w", err)
	}
	id, perr := ulid.Parse(idStr)
	if perr != nil {
		return ulid.ULID{}, false, fmt.Errorf("store: parse setup run id %q: %w", idStr, perr)
	}
	return id, true, nil
}

const setupRunColumns = `id, kind, status, window_days, plan,
	planned_labels, labels_written, planned_archived, archived,
	planned_moves, moved, left_for_later, unclassified,
	COALESCE(error, ''), created_at, started_at, finished_at, undone_at`

func scanSetupRun(scan func(...any) error) (SetupRun, error) {
	var (
		r      SetupRun
		idStr  string
		planJS []byte
	)
	if err := scan(&idStr, &r.Kind, &r.Status, &r.WindowDays, &planJS,
		&r.PlannedLabels, &r.LabelsWritten, &r.PlannedArchived, &r.Archived,
		&r.PlannedMoves, &r.Moved, &r.LeftForLater, &r.Unclassified,
		&r.Error, &r.CreatedAt, &r.StartedAt, &r.FinishedAt, &r.UndoneAt); err != nil {
		return SetupRun{}, err
	}
	id, err := ulid.Parse(idStr)
	if err != nil {
		return SetupRun{}, fmt.Errorf("store: parse setup run id %q: %w", idStr, err)
	}
	r.ID = id
	if err := json.Unmarshal(planJS, &r.Plan); err != nil {
		return SetupRun{}, fmt.Errorf("store: setup run %s plan: %w", idStr, err)
	}
	return r, nil
}

// ListSetupRuns returns the caller's runs, newest first.
func (tx *UserTx) ListSetupRuns(ctx context.Context) ([]SetupRun, error) {
	rows, err := tx.tx.Query(ctx,
		`SELECT `+setupRunColumns+` FROM app.setup_runs WHERE user_id = $1 ORDER BY created_at DESC, id DESC`,
		tx.userID.String())
	if err != nil {
		return nil, fmt.Errorf("store: list setup runs: %w", err)
	}
	defer rows.Close()
	var out []SetupRun
	for rows.Next() {
		r, serr := scanSetupRun(rows.Scan)
		if serr != nil {
			return nil, serr
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetSetupRun reads one run. RLS makes another user's run indistinguishable
// from a missing one — fail closed.
func (tx *UserTx) GetSetupRun(ctx context.Context, id ulid.ULID) (SetupRun, bool, error) {
	row := tx.tx.QueryRow(ctx,
		`SELECT `+setupRunColumns+` FROM app.setup_runs WHERE user_id = $1 AND id = $2`,
		tx.userID.String(), id.String())
	r, err := scanSetupRun(row.Scan)
	if err != nil {
		if isNoRows(err) {
			return SetupRun{}, false, nil
		}
		return SetupRun{}, false, err
	}
	return r, true, nil
}

// MarkSetupRunFailed records why a run stopped and appends the run's change row
// so the client stops waiting. The run stays RESUMABLE: the journal is left
// exactly as it is, so a retry (or a re-apply) picks up from the same cursor.
func (tx *UserTx) MarkSetupRunFailed(ctx context.Context, id ulid.ULID, msg string) error {
	tag, err := tx.tx.Exec(ctx, `
		UPDATE app.setup_runs SET status = $3, error = $4, finished_at = now()
		 WHERE user_id = $1 AND id = $2`,
		tx.userID.String(), id.String(), SetupStatusFailed, msg)
	if err != nil {
		return fmt.Errorf("store: mark setup run failed: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: %s", ErrSetupRunNotFound, id)
	}
	_, err = tx.AppendChange(ctx, ObjectSetupRun, id, OpUpdated)
	return err
}

// BeginSetupUndo flips a finished run to 'undoing' and enqueues the same
// one-shot job kind; the worker reads the status to decide direction. Refuses
// with ErrSetupRunBusy while the run is still queued or running — the client
// disables Undo until it is done, and this is the server making that a rule
// rather than a convention.
func (tx *UserTx) BeginSetupUndo(ctx context.Context, id ulid.ULID) error {
	run, found, err := tx.GetSetupRun(ctx, id)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("%w: %s", ErrSetupRunNotFound, id)
	}
	switch run.Status {
	case SetupStatusDone, SetupStatusFailed:
	case SetupStatusUndoing:
		return nil // already undoing; the job is already enqueued
	case SetupStatusUndone:
		return nil // nothing left to reverse
	default:
		return fmt.Errorf("%w: run %s is %s; undo is only offered once it has finished",
			ErrSetupRunBusy, id, run.Status)
	}
	if _, err := tx.tx.Exec(ctx, `
		UPDATE app.setup_runs SET status = $3, error = NULL WHERE user_id = $1 AND id = $2`,
		tx.userID.String(), id.String(), SetupStatusUndoing); err != nil {
		return fmt.Errorf("store: begin setup undo: %w", err)
	}
	if _, err := tx.AppendChange(ctx, ObjectSetupRun, id, OpUpdated); err != nil {
		return err
	}
	payload, err := json.Marshal(map[string]string{"run_id": id.String()})
	if err != nil {
		return fmt.Errorf("store: begin setup undo (job payload): %w", err)
	}
	// A fresh idempotency key: the apply's "setup:<id>" job row still exists
	// (done), and the undo is a second, distinct piece of work on the same run.
	_, err = tx.EnqueueJob(ctx, SetupJobKind, time.Now().UTC(), payload, "setup-undo:"+id.String())
	return err
}

// SetupApplyChunk performs ONE chunk of a run, in the caller's transaction.
//
// It selects up to `limit` older messages of the run's labelArchive and
// bringToDesk folders that have NO JOURNAL ROW FOR THIS RUN YET, oldest first,
// and per message: inserts the folder's label (ON CONFLICT DO NOTHING — an
// existing row's attribution is never overwritten, exactly as applyRuleAction
// explains), sets desk_archived_at if it was NULL, and — for bringToDesk —
// performs THE SAME STORE MOVE Email/set performs, so a writeback intent is
// enqueued exactly as a user's own move would. Then exactly ONE Email change
// row per message that changed in any way, and a journal row for every message
// examined.
//
// ⚠ A RETRY IS SAFE AND RESUMES WHERE IT STOPPED. The chunk query skips
// journaled messages, so a crashed chunk (whose transaction rolled back
// wholesale) is simply re-selected, and a chunk that committed is never
// re-selected. There is no cursor outside the rows the run wrote.
func (tx *UserTx) SetupApplyChunk(ctx context.Context, runID ulid.ULID, limit int) (SetupProgress, error) {
	if limit <= 0 {
		limit = 200
	}
	run, found, err := tx.GetSetupRun(ctx, runID)
	if err != nil {
		return SetupProgress{}, err
	}
	if !found {
		return SetupProgress{}, fmt.Errorf("%w: %s", ErrSetupRunNotFound, runID)
	}
	switch run.Status {
	case SetupStatusQueued, SetupStatusRunning, SetupStatusFailed:
		// 'failed' is resumable on purpose: MarkSetupRunFailed leaves the journal
		// alone, so the dispatcher's own retry has to be able to carry on.
	case SetupStatusDone:
		return SetupProgress{Done: true}, nil
	default:
		return SetupProgress{}, fmt.Errorf("%w: run %s is %s, not appliable", ErrSetupRunBusy, runID, run.Status)
	}

	facts, err := tx.resolveSetupPlan(ctx, run.Plan)
	if err != nil {
		return SetupProgress{}, err
	}
	order := make([]ulid.ULID, len(facts))
	byFolder := make(map[ulid.ULID]setupFolderFacts, len(facts))
	var actionable []ulid.ULID
	for i, f := range facts {
		order[i] = f.MailboxID
		byFolder[f.MailboxID] = f
		if f.actionable() {
			actionable = append(actionable, f.MailboxID)
		}
	}
	if len(actionable) == 0 {
		return tx.finishSetupApply(ctx, run)
	}

	// THE CUTOFF IS PINNED TO THE RUN'S CREATION, not to now(): every chunk and
	// every retry must see the SAME population, or a message would age into the
	// run between chunks and the actual counters could exceed the planned ones
	// the preview promised.
	cutoff := run.CreatedAt.UTC().AddDate(0, 0, -run.WindowDays)

	args := []any{tx.userID.String()}
	bind := bindFunc(&args)
	chosen := setupChosenSQL("$1", bind, order)
	// move_needed is computed HERE, in the same query and from the same
	// inMailboxSQL the preview counted with, rather than re-derived per message
	// in Go: ApplyEmailUpdate would happily perform (and enqueue an intent for)
	// a redundant move, so "does this message need moving" has to be the
	// preview's own answer.
	moveArms := ""
	for _, f := range facts {
		if f.action() != SetupActionBringToDesk {
			continue
		}
		moveArms += " WHEN c.mailbox_id = " + bind(f.MailboxID.String()) +
			" THEN " + setupMoveNeededSQL(f, bind, tx.userID)
	}
	moveExpr := boolLit(false)
	if moveArms != "" {
		moveExpr = "CASE" + moveArms + " ELSE false END"
	}
	cutoffPH := bind(cutoff)
	actionablePH := bind(ulidStrings(actionable))
	runPH := bind(runID.String())
	limitPH := bind(limit)

	rows, err := tx.tx.Query(ctx, `
		WITH chosen AS (`+chosen+`)
		SELECT m.id, c.mailbox_id, `+moveExpr+`
		  FROM chosen c
		  JOIN app.messages m ON m.user_id = $1 AND m.id = c.message_id
		 WHERE c.mailbox_id = ANY(`+actionablePH+`)
		   AND `+setupLiveMessageSQL+`
		   AND m.received_at < `+cutoffPH+`::timestamptz
		   AND NOT EXISTS (SELECT 1 FROM app.setup_journal j
		                    WHERE j.user_id = m.user_id AND j.run_id = `+runPH+`
		                      AND j.message_id = m.id)
		 ORDER BY m.received_at, m.id
		 LIMIT `+limitPH, args...)
	if err != nil {
		return SetupProgress{}, fmt.Errorf("store: setup apply chunk (scan): %w", err)
	}
	type chunkRow struct {
		msgID, folderID ulid.ULID
		moveNeeded      bool
	}
	var batch []chunkRow
	for rows.Next() {
		var msgStr, mbStr string
		var moveNeeded bool
		if err := rows.Scan(&msgStr, &mbStr, &moveNeeded); err != nil {
			rows.Close()
			return SetupProgress{}, fmt.Errorf("store: scan setup chunk row: %w", err)
		}
		msgID, perr := ulid.Parse(msgStr)
		if perr != nil {
			rows.Close()
			return SetupProgress{}, fmt.Errorf("store: parse setup chunk message id %q: %w", msgStr, perr)
		}
		mbID, perr := ulid.Parse(mbStr)
		if perr != nil {
			rows.Close()
			return SetupProgress{}, fmt.Errorf("store: parse setup chunk mailbox id %q: %w", mbStr, perr)
		}
		batch = append(batch, chunkRow{msgID: msgID, folderID: mbID, moveNeeded: moveNeeded})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return SetupProgress{}, fmt.Errorf("store: setup apply chunk (scan): %w", err)
	}
	rows.Close()

	if len(batch) == 0 {
		return tx.finishSetupApply(ctx, run)
	}

	// INBOX's label is per message: the resolved area's OWN label, and only when
	// the TAB VERDICT decided the area (applyAreaLabel's rule). Resolved for the
	// whole chunk in one round trip through ResolveAreas — THE resolver, not a
	// second copy of it.
	var inboxMsgs []ulid.ULID
	for _, b := range batch {
		if byFolder[b.folderID].Role == "inbox" {
			inboxMsgs = append(inboxMsgs, b.msgID)
		}
	}
	inboxLabels, err := tx.setupInboxLabels(ctx, inboxMsgs)
	if err != nil {
		return SetupProgress{}, err
	}

	var progress SetupProgress
	for _, b := range batch {
		f := byFolder[b.folderID]
		labelID := f.LabelID
		if f.Role == "inbox" {
			if lid, ok := inboxLabels[b.msgID]; ok {
				labelID = &lid
			} else {
				// A Needs-sorting message gets NO label but IS still archived:
				// day-one's promise is about the desk, not about labelling
				// everything, and inventing a label for mail no area claims
				// would be filing it somewhere nobody asked for.
				labelID = nil
			}
		}

		var journalLabel *ulid.ULID
		if labelID != nil {
			appliedBy, ruleID := SetupLabelSource().columns()
			tag, xerr := tx.tx.Exec(ctx, `
				INSERT INTO app.message_labels (user_id, message_id, label_id, applied_by, applied_by_rule_id)
				VALUES ($1, $2, $3, $4, $5)
				ON CONFLICT (user_id, message_id, label_id) DO NOTHING`,
				tx.userID.String(), b.msgID.String(), labelID.String(), appliedBy, ruleID)
			if xerr != nil {
				return SetupProgress{}, fmt.Errorf("store: setup apply (label %s): %w", labelID, xerr)
			}
			if tag.RowsAffected() > 0 {
				lid := *labelID
				journalLabel = &lid
				progress.Labels++
			}
		}

		deskArchived := false
		if f.action() == SetupActionLabelArchive {
			tag, xerr := tx.tx.Exec(ctx, `
				UPDATE app.messages SET desk_archived_at = now()
				 WHERE user_id = $1 AND id = $2 AND desk_archived_at IS NULL`,
				tx.userID.String(), b.msgID.String())
			if xerr != nil {
				return SetupProgress{}, fmt.Errorf("store: setup apply (archive): %w", xerr)
			}
			if tag.RowsAffected() > 0 {
				deskArchived = true
				progress.Archived++
			}
		}

		var movedFrom *ulid.ULID
		movedAppendedChange := false
		if b.moveNeeded && f.action() == SetupActionBringToDesk && f.InboxID != nil {
			appended, xerr := tx.setupMoveMessage(ctx, b.msgID, *f.InboxID)
			if xerr != nil {
				return SetupProgress{}, xerr
			}
			from := b.folderID
			movedFrom = &from
			movedAppendedChange = appended
			progress.Moves++
		}

		// ALWAYS a journal row, even an all-false one: it IS the chunk cursor.
		if _, xerr := tx.tx.Exec(ctx, `
			INSERT INTO app.setup_journal (id, user_id, run_id, message_id, label_id, desk_archived, moved_from_mailbox_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			NewULID().String(), tx.userID.String(), runID.String(), b.msgID.String(),
			ulidArg(journalLabel), deskArchived, ulidArg(movedFrom)); xerr != nil {
			return SetupProgress{}, fmt.Errorf("store: setup apply (journal): %w", xerr)
		}
		progress.Processed++

		// EXACTLY ONE Email change row per message that changed. The move path
		// (ApplyEmailUpdate) already appended one, so this covers the
		// label-and-archive-only case; appending a second would make a client
		// replay the same message twice.
		if (journalLabel != nil || deskArchived) && !movedAppendedChange {
			if _, xerr := tx.AppendChange(ctx, ObjectEmail, b.msgID, OpUpdated); xerr != nil {
				return SetupProgress{}, xerr
			}
			progress.Changed++
		} else if movedAppendedChange {
			progress.Changed++
		}
	}

	if err := tx.bumpSetupRunProgress(ctx, run, progress); err != nil {
		return SetupProgress{}, err
	}
	return progress, nil
}

// setupInboxLabels resolves, for INBOX messages, the AREA'S OWN LABEL each one
// should get — and only for the messages applyAreaLabel would label: the area
// must have been decided by the TAB VERDICT, not by a label the message already
// carries (which is already written down) and not by nothing at all (Needs
// sorting).
//
// It goes through ResolveAreas, THE resolver both Email.area and AreaSummary
// use, so day-one cannot place a message in an area the desk then disagrees
// about.
func (tx *UserTx) setupInboxLabels(ctx context.Context, ids []ulid.ULID) (map[ulid.ULID]ulid.ULID, error) {
	out := map[ulid.ULID]ulid.ULID{}
	if len(ids) == 0 {
		return out, nil
	}
	res, err := tx.ResolveAreas(ctx, ids)
	if err != nil {
		return nil, err
	}
	wanted := map[ulid.ULID][]ulid.ULID{} // area id -> messages
	for _, id := range ids {
		r, ok := res[id]
		if !ok || r.AreaID == nil || r.ByLabel {
			continue
		}
		wanted[*r.AreaID] = append(wanted[*r.AreaID], id)
	}
	if len(wanted) == 0 {
		return out, nil
	}
	areaIDs := make([]ulid.ULID, 0, len(wanted))
	for id := range wanted {
		areaIDs = append(areaIDs, id)
	}
	rows, err := tx.tx.Query(ctx,
		`SELECT id, label_id FROM app.areas WHERE user_id = $1 AND id = ANY($2)`,
		tx.userID.String(), ulidStrings(areaIDs))
	if err != nil {
		return nil, fmt.Errorf("store: setup inbox labels: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var areaStr, labelStr string
		if err := rows.Scan(&areaStr, &labelStr); err != nil {
			return nil, fmt.Errorf("store: scan setup inbox label: %w", err)
		}
		areaID, perr := ulid.Parse(areaStr)
		if perr != nil {
			return nil, fmt.Errorf("store: parse setup area id %q: %w", areaStr, perr)
		}
		labelID, perr := ulid.Parse(labelStr)
		if perr != nil {
			return nil, fmt.Errorf("store: parse setup area label id %q: %w", labelStr, perr)
		}
		for _, msgID := range wanted[areaID] {
			out[msgID] = labelID
		}
	}
	return out, rows.Err()
}

// setupMoveMessage performs THE SAME STORE MOVE Email/set performs, and
// reports whether it appended the Email change row.
//
// It resolves the target's ROLE first, exactly as Email/set's own setMailbox
// does, because the role is what the per-account writeback intent carries
// (mailbox names differ per account) while the named mailbox is the canonical
// placement. Going through ApplyEmailUpdate rather than writing
// moved_mailbox_id directly is what makes a day-one bringToDesk indistinguishable
// from a user dragging the message to the inbox — same canonical placement,
// same intent, same one change row.
//
// The change row is DETECTED rather than assumed: ApplyEmailUpdate appends none
// when nothing actually changed (a message already canonically in the target),
// and the caller still owes one for a label it inserted.
func (tx *UserTx) setupMoveMessage(ctx context.Context, msgID, target ulid.ULID) (appendedChange bool, err error) {
	role, found, err := tx.MailboxRole(ctx, target)
	if err != nil {
		return false, err
	}
	if !found {
		return false, fmt.Errorf("%w: mailbox %s vanished mid-run", ErrSetupPlanInvalid, target)
	}
	before, err := tx.ChangeState(ctx, ObjectEmail)
	if err != nil {
		return false, err
	}
	mb := target
	msgFound, after, err := tx.ApplyEmailUpdate(ctx, msgID, EmailUpdate{MoveToRole: role, MoveToMailboxID: &mb})
	if err != nil {
		return false, err
	}
	if !msgFound {
		return false, fmt.Errorf("store: setup move: message %s vanished mid-run", msgID)
	}
	return after > before, nil
}

// bumpSetupRunProgress adds one chunk's actuals to the run and, on the first
// chunk, marks it running. Exactly ONE SetupRun change row per chunk that
// changed the run — the invariant applies to this object like any other, and a
// client polling /changes is how the wizard's progress bar moves.
func (tx *UserTx) bumpSetupRunProgress(ctx context.Context, run SetupRun, p SetupProgress) error {
	if _, err := tx.tx.Exec(ctx, `
		UPDATE app.setup_runs
		   SET labels_written = labels_written + $3,
		       archived = archived + $4,
		       moved = moved + $5,
		       status = $6,
		       started_at = COALESCE(started_at, now()),
		       -- Cleared along with error: MarkSetupRunFailed set both, and a
		       -- resumed run that still carried a finished_at would render as
		       -- "finished" beside a status of 'running'. finishSetupApply sets it
		       -- again when the run really is done.
		       finished_at = NULL,
		       error = NULL
		 WHERE user_id = $1 AND id = $2`,
		tx.userID.String(), run.ID.String(), p.Labels, p.Archived, p.Moves, SetupStatusRunning); err != nil {
		return fmt.Errorf("store: setup run progress: %w", err)
	}
	_, err := tx.AppendChange(ctx, ObjectSetupRun, run.ID, OpUpdated)
	return err
}

// finishSetupApply marks a run done. Idempotent: a run already done appends no
// second change row, so the worker's final "is there more?" chunk is free.
func (tx *UserTx) finishSetupApply(ctx context.Context, run SetupRun) (SetupProgress, error) {
	if run.Status == SetupStatusDone {
		return SetupProgress{Done: true}, nil
	}
	if _, err := tx.tx.Exec(ctx, `
		UPDATE app.setup_runs
		   SET status = $3, finished_at = now(), started_at = COALESCE(started_at, now()), error = NULL
		 WHERE user_id = $1 AND id = $2`,
		tx.userID.String(), run.ID.String(), SetupStatusDone); err != nil {
		return SetupProgress{}, fmt.Errorf("store: finish setup run: %w", err)
	}
	if _, err := tx.AppendChange(ctx, ObjectSetupRun, run.ID, OpUpdated); err != nil {
		return SetupProgress{}, err
	}
	return SetupProgress{Done: true}, nil
}

// SetupUndoChunk reverses ONE chunk of a run, from its journal, newest journal
// rows first — so a message that was labelled and then moved comes apart in the
// reverse order it was put together.
//
// PER JOURNAL ROW, and ONLY what the row records:
//
//   - the label is deleted ONLY IF IT IS STILL applied_by = 'setup'. A label the
//     user has re-applied by hand since the run is THEIRS, and an undo that
//     took it away would destroy the very filing this feature exists to
//     respect. This is the whole reason LabelBySetup is its own attribution.
//   - desk_archived_at is cleared only when the run set it, so a message that
//     was already archived in Mailroom before the run stays archived.
//   - a moved message goes back to the folder it came out of, through the same
//     store move path (and therefore the same writeback intent) the apply used.
//
// Then the journal row is deleted, which is what advances the cursor; when the
// journal is empty the run is 'undone'.
func (tx *UserTx) SetupUndoChunk(ctx context.Context, runID ulid.ULID, limit int) (SetupProgress, error) {
	if limit <= 0 {
		limit = 200
	}
	run, found, err := tx.GetSetupRun(ctx, runID)
	if err != nil {
		return SetupProgress{}, err
	}
	if !found {
		return SetupProgress{}, fmt.Errorf("%w: %s", ErrSetupRunNotFound, runID)
	}
	switch run.Status {
	case SetupStatusUndoing, SetupStatusDone, SetupStatusFailed:
	case SetupStatusUndone:
		return SetupProgress{Done: true}, nil
	default:
		// queued or running: the run is still writing, and reversing it
		// concurrently would race its own chunks. The client disables Undo until
		// the run is done; this is the server making that a rule.
		return SetupProgress{}, fmt.Errorf("%w: run %s is %s; undo is refused until it has finished",
			ErrSetupRunBusy, runID, run.Status)
	}

	rows, err := tx.tx.Query(ctx, `
		SELECT id, message_id, label_id, desk_archived, moved_from_mailbox_id
		  FROM app.setup_journal
		 WHERE user_id = $1 AND run_id = $2
		 ORDER BY id DESC
		 LIMIT $3`,
		tx.userID.String(), runID.String(), limit)
	if err != nil {
		return SetupProgress{}, fmt.Errorf("store: setup undo chunk (scan): %w", err)
	}
	type journalRow struct {
		id, msgID    ulid.ULID
		labelID      *ulid.ULID
		deskArchived bool
		movedFrom    *ulid.ULID
	}
	var batch []journalRow
	for rows.Next() {
		var (
			idStr, msgStr      string
			labelStr, movedStr *string
			deskArchived       bool
		)
		if err := rows.Scan(&idStr, &msgStr, &labelStr, &deskArchived, &movedStr); err != nil {
			rows.Close()
			return SetupProgress{}, fmt.Errorf("store: scan setup journal row: %w", err)
		}
		jr := journalRow{deskArchived: deskArchived}
		var perr error
		if jr.id, perr = ulid.Parse(idStr); perr != nil {
			rows.Close()
			return SetupProgress{}, fmt.Errorf("store: parse setup journal id %q: %w", idStr, perr)
		}
		if jr.msgID, perr = ulid.Parse(msgStr); perr != nil {
			rows.Close()
			return SetupProgress{}, fmt.Errorf("store: parse setup journal message id %q: %w", msgStr, perr)
		}
		if labelStr != nil {
			lid, lerr := ulid.Parse(*labelStr)
			if lerr != nil {
				rows.Close()
				return SetupProgress{}, fmt.Errorf("store: parse setup journal label id %q: %w", *labelStr, lerr)
			}
			jr.labelID = &lid
		}
		if movedStr != nil {
			mid, merr := ulid.Parse(*movedStr)
			if merr != nil {
				rows.Close()
				return SetupProgress{}, fmt.Errorf("store: parse setup journal mailbox id %q: %w", *movedStr, merr)
			}
			jr.movedFrom = &mid
		}
		batch = append(batch, jr)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return SetupProgress{}, fmt.Errorf("store: setup undo chunk (scan): %w", err)
	}
	rows.Close()

	if len(batch) == 0 {
		return tx.finishSetupUndo(ctx, run)
	}

	var progress SetupProgress
	for _, jr := range batch {
		changed := false
		if jr.labelID != nil {
			tag, xerr := tx.tx.Exec(ctx, `
				DELETE FROM app.message_labels
				 WHERE user_id = $1 AND message_id = $2 AND label_id = $3 AND applied_by = $4`,
				tx.userID.String(), jr.msgID.String(), jr.labelID.String(), LabelBySetup)
			if xerr != nil {
				return SetupProgress{}, fmt.Errorf("store: setup undo (label): %w", xerr)
			}
			if tag.RowsAffected() > 0 {
				progress.Labels++
				changed = true
			}
		}
		if jr.deskArchived {
			tag, xerr := tx.tx.Exec(ctx, `
				UPDATE app.messages SET desk_archived_at = NULL
				 WHERE user_id = $1 AND id = $2 AND desk_archived_at IS NOT NULL`,
				tx.userID.String(), jr.msgID.String())
			if xerr != nil {
				return SetupProgress{}, fmt.Errorf("store: setup undo (archive): %w", xerr)
			}
			if tag.RowsAffected() > 0 {
				progress.Archived++
				changed = true
			}
		}
		movedAppendedChange := false
		if jr.movedFrom != nil {
			appended, xerr := tx.setupMoveMessage(ctx, jr.msgID, *jr.movedFrom)
			if xerr != nil {
				return SetupProgress{}, xerr
			}
			movedAppendedChange = appended
			progress.Moves++
		}
		if changed && !movedAppendedChange {
			if _, xerr := tx.AppendChange(ctx, ObjectEmail, jr.msgID, OpUpdated); xerr != nil {
				return SetupProgress{}, xerr
			}
			progress.Changed++
		} else if movedAppendedChange {
			progress.Changed++
		}
		if _, xerr := tx.tx.Exec(ctx,
			`DELETE FROM app.setup_journal WHERE user_id = $1 AND id = $2`,
			tx.userID.String(), jr.id.String()); xerr != nil {
			return SetupProgress{}, fmt.Errorf("store: setup undo (journal): %w", xerr)
		}
		progress.Processed++
	}

	// The run's ACTUAL counters are left exactly as they are: they record what
	// the run DID, which stays true after it is reversed. The status and
	// undone_at are how a reader knows it no longer holds.
	if _, err := tx.tx.Exec(ctx, `
		UPDATE app.setup_runs SET status = $3, error = NULL WHERE user_id = $1 AND id = $2`,
		tx.userID.String(), runID.String(), SetupStatusUndoing); err != nil {
		return SetupProgress{}, fmt.Errorf("store: setup undo (status): %w", err)
	}
	if _, err := tx.AppendChange(ctx, ObjectSetupRun, runID, OpUpdated); err != nil {
		return SetupProgress{}, err
	}
	return progress, nil
}

// DeskArchivedAt returns, for the given message ids, WHEN day-one setup archived
// them in Mailroom. Ids never archived are simply absent from the map — this is
// how Email.deskArchivedAt renders null. One round trip, the same shape
// DrawerArchivedAt (drawer_sweep.go) and ResolveAreas already take.
func (tx *UserTx) DeskArchivedAt(ctx context.Context, ids []ulid.ULID) (map[ulid.ULID]time.Time, error) {
	out := make(map[ulid.ULID]time.Time, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := tx.tx.Query(ctx, `
		SELECT id, desk_archived_at FROM app.messages
		 WHERE user_id = $1 AND id = ANY($2) AND desk_archived_at IS NOT NULL`,
		tx.userID.String(), ulidStrings(ids))
	if err != nil {
		return nil, fmt.Errorf("store: desk archived at: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var idStr string
		var at time.Time
		if err := rows.Scan(&idStr, &at); err != nil {
			return nil, fmt.Errorf("store: scan desk archived at: %w", err)
		}
		id, perr := ulid.Parse(idStr)
		if perr != nil {
			return nil, fmt.Errorf("store: parse desk archived message id %q: %w", idStr, perr)
		}
		out[id] = at
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: desk archived at: %w", err)
	}
	return out, nil
}

// finishSetupUndo marks a run undone once its journal is empty. Idempotent, for
// finishSetupApply's reason.
func (tx *UserTx) finishSetupUndo(ctx context.Context, run SetupRun) (SetupProgress, error) {
	if run.Status == SetupStatusUndone {
		return SetupProgress{Done: true}, nil
	}
	if _, err := tx.tx.Exec(ctx, `
		UPDATE app.setup_runs SET status = $3, undone_at = now(), error = NULL
		 WHERE user_id = $1 AND id = $2`,
		tx.userID.String(), run.ID.String(), SetupStatusUndone); err != nil {
		return SetupProgress{}, fmt.Errorf("store: finish setup undo: %w", err)
	}
	if _, err := tx.AppendChange(ctx, ObjectSetupRun, run.ID, OpUpdated); err != nil {
		return SetupProgress{}, err
	}
	return SetupProgress{Done: true}, nil
}
