// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// Setup/* — DAY-ONE SETUP (#446): the four steps that turn a folder-sorted
// mailbox into a desk without moving anything by accident. All under capDesk.
//
// THREE OF THE FIVE METHODS WRITE NOTHING. Setup/census, Setup/preview and
// Setup/get are reports, and that ordering is the feature (CLAUDE.md, "build
// the report before the fix"): the wizard shows the folders with their counts,
// then the exact per-folder consequence of the plan, and only then offers
// Setup/apply. A mailbox arriving from another client can hold thousands of
// messages older than the window, and the operator has to see the number before
// the write, not after it.
//
// WHY Type/verb METHODS RATHER THAN A get/query/changes/set OBJECT. Two of them
// answer COUNTS (census, preview), which is Area/getSummary's own precedent for
// a Mailroom extension verb, and the other three are LIFECYCLE OPERATIONS on a
// job (apply, undo) plus a read of it. Only the RUN is an object with sync
// semantics, and it is deliberately read-only over JMAP: a client cannot create
// a SetupRun with a /set — Setup/apply is the one door, because creating a run
// also mints labels and areas and enqueues a job, none of which a patch could
// express honestly. Hence Setup/get rather than SetupRun/get: the whole surface
// is one namespace the client uses once.
//
// THE RUN IS STILL A CHANGE-LOGGED OBJECT (store.ObjectSetupRun) and the client
// SHOULD poll it while the import drains — with Setup/get, or through
// /changes on the SetupRun type like any other object.

// ---- Setup/census ---------------------------------------------------------

// setupDefaultWindowDays is the window Setup/census and the wizard default to:
// mail from the last 30 days is "newly arrived" and stays on the desk.
// #446 offers 7/14/30/90; the SERVER accepts any positive number, because the
// closed set is a presentation choice and a server refusing 45 would be
// enforcing a UI decision.
const setupDefaultWindowDays = 30

type setupCensusArgs struct {
	AccountID string `json:"accountId"`
	// MailAccountID narrows the census to one mail account, the same shape
	// Area/getSummary's own mailAccountId takes (JMAP's accountId is the LOGIN;
	// a Mailroom login can hold several mail accounts).
	MailAccountID *string `json:"mailAccountId"`
	WindowDays    *int    `json:"windowDays"`
}

func folderCensusObject(c store.FolderCensus) map[string]any {
	var existingLabel, existingArea any
	if c.ExistingLabelID != nil {
		existingLabel = c.ExistingLabelID.String()
	}
	if c.ExistingAreaID != nil {
		existingArea = c.ExistingAreaID.String()
	}
	return map[string]any{
		"mailboxId": c.MailboxID.String(),
		// The MAIL account, not the login: a census row belongs to the account
		// whose folder it is.
		"accountId": c.AccountID.String(),
		"name":      c.Name,
		"role":      c.Role,
		"feedsDesk": c.FeedsDesk,
		// Live is the denominator every other count here is against; inWindow +
		// older = live, always.
		"live":         c.Live,
		"inWindow":     c.InWindow,
		"older":        c.Older,
		"unclassified": c.Unclassified,
		// float64, and deliberately not rounded here: the client decides how to
		// render "84%".
		"distinctSenders": c.DistinctSenders,
		"distinctDomains": c.DistinctDomains,
		"sendersFor90":    c.SendersFor90,
		"bulkShare":       c.BulkShare,
		// shape is a WORD FOR THE COUNTS BESIDE IT, not an authority — see
		// store.classifyShape. Render it with the numbers, never instead of them.
		"shape":           c.Shape,
		"existingLabelId": existingLabel,
		"existingAreaId":  existingArea,
	}
}

func (a *API) setupCensus(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	var args setupCensusArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	if mErr := resolveAccount(userID, args.AccountID); mErr != nil {
		return nil, mErr
	}
	var mailAccount *ulid.ULID
	if args.MailAccountID != nil {
		id, err := ulid.Parse(*args.MailAccountID)
		if err != nil {
			return nil, &MethodError{Type: errInvalidArguments, Description: "mailAccountId is not a valid id"}
		}
		mailAccount = &id
	}
	windowDays := setupDefaultWindowDays
	if args.WindowDays != nil {
		windowDays = *args.WindowDays
		if windowDays <= 0 {
			return nil, &MethodError{Type: errInvalidArguments, Description: "windowDays must be a positive integer"}
		}
	}

	var folders []store.FolderCensus
	if mErr := a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
		var err error
		folders, err = tx.MailboxCensus(ctx, mailAccount, windowDays)
		if err != nil {
			return setupStoreError(err)
		}
		return nil
	}); mErr != nil {
		return nil, mErr
	}

	list := make([]map[string]any, 0, len(folders))
	for _, c := range folders {
		list = append(list, folderCensusObject(c))
	}
	return mustMarshal(map[string]any{
		"accountId":  userID.String(),
		"windowDays": windowDays,
		"folders":    list,
	}), nil
}

// ---- the plan, shared by preview and apply --------------------------------

// setupPlanArgs is the wire form of store.SetupPlan. Its own type rather than
// reusing the store struct's JSON tags, for the reason every other method here
// follows: the wire shape is a contract with the client and the store shape is
// free to change.
type setupPlanArgs struct {
	WindowDays int                     `json:"windowDays"`
	Folders    []setupFolderChoiceArgs `json:"folders"`
}

type setupFolderChoiceArgs struct {
	MailboxID string `json:"mailboxId"`
	Action    string `json:"action"`
	LabelName string `json:"labelName"`
	// AreaID names an EXISTING area to feed; areaName creates (or reuses by
	// name) one. Both absent means label-only — no area at all, which is a
	// legitimate choice for a folder that is an archive rather than a topic.
	AreaID   *string `json:"areaId"`
	AreaName string  `json:"areaName"`
}

// parseSetupPlan validates the wire plan into the store's, naming the offending
// property rather than letting a bad id surface as a store error.
func parseSetupPlan(args setupPlanArgs) (store.SetupPlan, *MethodError) {
	plan := store.SetupPlan{WindowDays: args.WindowDays}
	if plan.WindowDays == 0 {
		// Omitted rather than wrong: the wizard always sends one, but a plan with
		// no window is far better answered with the documented default than with
		// an error a client cannot act on.
		plan.WindowDays = setupDefaultWindowDays
	}
	if plan.WindowDays < 0 {
		return store.SetupPlan{}, &MethodError{Type: errInvalidArguments,
			Description: "plan.windowDays must be a positive integer"}
	}
	for _, f := range args.Folders {
		mid, err := ulid.Parse(strings.TrimSpace(f.MailboxID))
		if err != nil {
			return store.SetupPlan{}, &MethodError{Type: errInvalidArguments,
				Description: "plan.folders[].mailboxId is not a valid id"}
		}
		switch f.Action {
		case store.SetupActionLabelArchive, store.SetupActionBringToDesk, store.SetupActionLeave:
		default:
			return store.SetupPlan{}, &MethodError{Type: errInvalidArguments,
				Description: `plan.folders[].action must be one of "labelArchive", "bringToDesk", "leave"`}
		}
		choice := store.SetupFolderChoice{
			MailboxID: mid, Action: f.Action,
			LabelName: strings.TrimSpace(f.LabelName),
			AreaName:  strings.TrimSpace(f.AreaName),
		}
		if f.AreaID != nil {
			aid, aerr := ulid.Parse(strings.TrimSpace(*f.AreaID))
			if aerr != nil {
				return store.SetupPlan{}, &MethodError{Type: errInvalidArguments,
					Description: "plan.folders[].areaId is not a valid id"}
			}
			choice.AreaID = &aid
		}
		plan.Folders = append(plan.Folders, choice)
	}
	return plan, nil
}

func setupCountsObject(c store.SetupCounts) map[string]any {
	perFolder := make([]map[string]any, 0, len(c.PerFolder))
	for _, f := range c.PerFolder {
		perFolder = append(perFolder, map[string]any{
			"mailboxId": f.MailboxID.String(),
			"name":      f.Name,
			"action":    f.Action,
			"older":     f.Older,
			"labels":    f.Labels,
			"archived":  f.Archived,
			"moves":     f.Moves,
		})
	}
	return map[string]any{
		"labels":   c.Labels,
		"archived": c.Archived,
		// moves is the ONLY number here that changes anything on the provider,
		// and it is 0 for a plan with no bringToDesk folder. A client should say
		// so in words.
		"moves":        c.Moves,
		"leave":        c.Leave,
		"unclassified": c.Unclassified,
		"perFolder":    perFolder,
		// A STATED PLANNING RATE, not a measurement — see
		// store.setupWritesPerSecond.
		"estimatedSeconds": c.EstimatedSeconds,
	}
}

type setupPreviewArgs struct {
	AccountID string        `json:"accountId"`
	Plan      setupPlanArgs `json:"plan"`
}

func (a *API) setupPreview(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	var args setupPreviewArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	if mErr := resolveAccount(userID, args.AccountID); mErr != nil {
		return nil, mErr
	}
	plan, mErr := parseSetupPlan(args.Plan)
	if mErr != nil {
		return nil, mErr
	}

	var counts store.SetupCounts
	if mErr := a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
		var err error
		counts, err = tx.SetupPreview(ctx, plan)
		if err != nil {
			return setupStoreError(err)
		}
		return nil
	}); mErr != nil {
		return nil, mErr
	}
	return mustMarshal(map[string]any{
		"accountId": userID.String(),
		"counts":    setupCountsObject(counts),
	}), nil
}

// ---- Setup/apply ----------------------------------------------------------

type setupApplyArgs struct {
	AccountID string        `json:"accountId"`
	Plan      setupPlanArgs `json:"plan"`
}

// setupApply creates the run and enqueues the one-shot import job. It returns
// the COUNTS IT RECORDED AS PLANNED alongside the run id, so the client can show
// the promise it is about to be held to and compare it against the run's own
// actual counters as they arrive.
//
// The preview is re-run inside the SAME transaction as the create rather than
// trusting counts the client echoes back: a client-supplied planned figure would
// make "planned N, wrote N" a tautology instead of a check.
func (a *API) setupApply(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	var args setupApplyArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	if mErr := resolveAccount(userID, args.AccountID); mErr != nil {
		return nil, mErr
	}
	plan, mErr := parseSetupPlan(args.Plan)
	if mErr != nil {
		return nil, mErr
	}

	var (
		runID  ulid.ULID
		counts store.SetupCounts
	)
	if mErr := a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
		var err error
		if counts, err = tx.SetupPreview(ctx, plan); err != nil {
			return setupStoreError(err)
		}
		if runID, err = tx.CreateSetupRun(ctx, plan, counts); err != nil {
			return setupStoreError(err)
		}
		return nil
	}); mErr != nil {
		return nil, mErr
	}
	return mustMarshal(map[string]any{
		"accountId": userID.String(),
		"runId":     runID.String(),
		"counts":    setupCountsObject(counts),
	}), nil
}

// ---- Setup/get ------------------------------------------------------------

func setupRunObject(r store.SetupRun) map[string]any {
	folders := make([]map[string]any, 0, len(r.Plan.Folders))
	for _, f := range r.Plan.Folders {
		var areaID any
		if f.AreaID != nil {
			areaID = f.AreaID.String()
		}
		folders = append(folders, map[string]any{
			"mailboxId": f.MailboxID.String(),
			"action":    f.Action,
			"labelName": f.LabelName,
			"areaId":    areaID,
			"areaName":  f.AreaName,
		})
	}
	var errMsg any
	if r.Error != "" {
		errMsg = r.Error
	}
	return map[string]any{
		"id":         r.ID.String(),
		"kind":       r.Kind,
		"status":     r.Status,
		"windowDays": r.WindowDays,
		"plan":       map[string]any{"windowDays": r.Plan.WindowDays, "folders": folders},
		// PLANNED beside ACTUAL, deliberately both: "planned N, wrote N"
		// is the only form in which the preview's promise can be checked rather
		// than trusted.
		"plannedLabels":   r.PlannedLabels,
		"labelsWritten":   r.LabelsWritten,
		"plannedArchived": r.PlannedArchived,
		"archived":        r.Archived,
		"plannedMoves":    r.PlannedMoves,
		"moved":           r.Moved,
		"leftForLater":    r.LeftForLater,
		"unclassified":    r.Unclassified,
		"error":           errMsg,
		"createdAt":       jmapDate(r.CreatedAt),
		"startedAt":       jmapDateOrNil(r.StartedAt),
		"finishedAt":      jmapDateOrNil(r.FinishedAt),
		"undoneAt":        jmapDateOrNil(r.UndoneAt),
	}
}

type setupGetArgs struct {
	AccountID string   `json:"accountId"`
	IDs       []string `json:"ids"`
}

func (a *API) setupGet(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	var args setupGetArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	if mErr := resolveAccount(userID, args.AccountID); mErr != nil {
		return nil, mErr
	}

	var (
		runs  []store.SetupRun
		state int64
	)
	if mErr := a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
		var err error
		if runs, err = tx.ListSetupRuns(ctx); err != nil {
			return setupStoreError(err)
		}
		if state, err = tx.ChangeState(ctx, store.ObjectSetupRun); err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		return nil
	}); mErr != nil {
		return nil, mErr
	}

	notFound := []string{}
	list := []map[string]any{}
	if args.IDs == nil {
		for _, r := range runs {
			list = append(list, setupRunObject(r))
		}
	} else {
		byID := make(map[string]store.SetupRun, len(runs))
		for _, r := range runs {
			byID[r.ID.String()] = r
		}
		for _, want := range args.IDs {
			if r, ok := byID[want]; ok {
				list = append(list, setupRunObject(r))
				continue
			}
			notFound = append(notFound, want)
		}
	}
	return mustMarshal(getResult{
		AccountID: userID.String(),
		State:     encodeState(state),
		List:      list,
		NotFound:  notFound,
	}), nil
}

func (a *API) setupChanges(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	return a.handleChanges(ctx, userID, rawArgs, store.ObjectSetupRun)
}

// ---- Setup/undo -----------------------------------------------------------

type setupUndoArgs struct {
	AccountID string `json:"accountId"`
	RunID     string `json:"runId"`
}

// setupUndo flips the run to 'undoing' and enqueues the SAME job kind; the
// worker reads the status to decide direction. It refuses (setupRunBusy) while
// the run is still queued or running — the client disables Undo until it is
// done, and this is the server making that a rule rather than a convention.
func (a *API) setupUndo(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	var args setupUndoArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	if mErr := resolveAccount(userID, args.AccountID); mErr != nil {
		return nil, mErr
	}
	runID, err := ulid.Parse(strings.TrimSpace(args.RunID))
	if err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: "runId must be a valid id"}
	}

	if mErr := a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
		if err := tx.BeginSetupUndo(ctx, runID); err != nil {
			return setupStoreError(err)
		}
		return nil
	}); mErr != nil {
		return nil, mErr
	}
	return mustMarshal(map[string]any{
		"accountId": userID.String(),
		"runId":     runID.String(),
	}), nil
}

// errSetupRunBusy is the Mailroom method error for "a run is already
// outstanding" and "that run has not finished yet".
//
// Its own type rather than invalidArguments, because the client's remedy is
// different in kind: the request was well formed and would be valid later. A
// wizard that got invalidArguments here would show a validation message; what it
// should do is show the run that is already going.
const errSetupRunBusy = "setupRunBusy"

// setupStoreError maps the store's typed setup errors onto JMAP method errors,
// so a plan a client can fix is answered with the reason rather than a 500.
func setupStoreError(err error) *MethodError {
	switch {
	case errors.Is(err, store.ErrSetupPlanInvalid):
		return &MethodError{Type: errInvalidArguments, Description: err.Error()}
	case errors.Is(err, store.ErrSetupRunBusy):
		return &MethodError{Type: errSetupRunBusy, Description: err.Error()}
	case errors.Is(err, store.ErrSetupRunNotFound):
		// notFound is not a request-level error shape here: Setup/undo names one
		// run, so "there is no such run" is the whole answer.
		return &MethodError{Type: errInvalidArguments, Description: err.Error()}
	default:
		return &MethodError{Type: errServerFail, Description: err.Error()}
	}
}
