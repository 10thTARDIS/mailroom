// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
	"mailroom/internal/tabs"
)

// Area/* — the desk's own partition of mail (#442), wrapping the store layer
// increment 1 shipped (internal/store/areas.go, areas_resolve.go).
//
// AN AREA IS THE USER'S OWN ARRANGEMENT OF THE DESK: a name, a position, an
// OWN LABEL (placing a message in the area IS applying that label — "areas
// are labels"), and any number of ADDITIONAL feeds (tabs and/or other
// labels) that also route mail into it. That is a THIRD axis alongside tabs
// (function) and labels (topic) — see capDesk's doc comment in service.go —
// which is why this file, not category_tab.go or label.go, owns it.
//
// areaMaxNameRunes bounds a name at the API layer; the store enforces no
// length limit of its own (only case-insensitive uniqueness), so this is
// where "too long" is actually refused.
const areaMaxNameRunes = 64

// areaObject renders a store.AreaView as a JMAP Area object.
//
// `labelIds` EXCLUDES the area's own label (`labelId`) — the spec is explicit
// that the own label is never repeated in it, because it is already named by
// `labelId` and listing it twice would make a client wonder whether removing
// it from `labelIds` unmaps the area's identity (it would not: `labelId` is
// immutable, see updateArea). Both `tabs` and `labelIds` are always present,
// possibly empty, never null — the same "a bounded/empty result must report
// its bound" discipline every other list property here follows.
func areaObject(v store.AreaView) map[string]any {
	tabNames := []string{}
	labelIDs := []string{}
	for _, f := range v.Feeds {
		switch {
		case f.Tab != "":
			tabNames = append(tabNames, string(f.Tab))
		case f.LabelID != nil && *f.LabelID != v.LabelID:
			labelIDs = append(labelIDs, f.LabelID.String())
		}
	}
	var autoArchiveDays any
	if v.AutoArchiveDays != nil {
		autoArchiveDays = *v.AutoArchiveDays
	}
	return map[string]any{
		"id":       v.ID.String(),
		"name":     v.Name,
		"position": v.Position,
		"labelId":  v.LabelID.String(),
		"tabs":     tabNames,
		"labelIds": labelIDs,
		// autoArchive (#443) is whether this drawer sweeps at all, default true;
		// autoArchiveDays is only the PERIOD — null now means "the login
		// default" (desk_preferences.drawer_default_days), never "off". "off" is
		// autoArchive: false.
		"autoArchive":     v.AutoArchive,
		"autoArchiveDays": autoArchiveDays,
		"hiddenFromRail":  v.HiddenFromRail,
	}
}

// ---- Area/get ---------------------------------------------------------

type areaGetArgs struct {
	AccountID string   `json:"accountId"`
	IDs       []string `json:"ids"`
}

func (a *API) areaGet(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	var args areaGetArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	if mErr := resolveAccount(userID, args.AccountID); mErr != nil {
		return nil, mErr
	}

	var (
		state    int64
		all      []store.AreaView
		notFound = []string{}
	)
	if err := a.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		// ORDER MATTERS, exactly as categoryTabGet explains: ListAreas
		// materialises the default plan (and appends its `created` change_log
		// rows) on a first-ever call, so the state must be read AFTER it —
		// otherwise the client would be handed a cursor from before the
		// creates it is about to be shown, and its next /changes would replay
		// them.
		var e error
		if all, e = tx.ListAreas(ctx); e != nil {
			return e
		}
		state, e = tx.ChangeState(ctx, store.ObjectArea)
		return e
	}); err != nil {
		return nil, &MethodError{Type: errServerFail, Description: err.Error()}
	}

	list := make([]map[string]any, 0, len(all))
	if args.IDs == nil {
		for _, v := range all {
			list = append(list, areaObject(v))
		}
	} else {
		byID := make(map[string]store.AreaView, len(all))
		for _, v := range all {
			byID[v.ID.String()] = v
		}
		for _, want := range args.IDs {
			if v, ok := byID[want]; ok {
				list = append(list, areaObject(v))
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

// ---- Area/changes -------------------------------------------------------

func (a *API) areaChanges(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	return a.handleChanges(ctx, userID, rawArgs, store.ObjectArea)
}

// ---- Area/set -----------------------------------------------------------

// areaCreate is the Area/set create form. `labelId` names an EXISTING label
// to reuse as the area's own; absent, the store mints one named `name`.
type areaCreate struct {
	Name     *string  `json:"name"`
	Position *int     `json:"position"`
	LabelID  *string  `json:"labelId"`
	Tabs     []string `json:"tabs"`
	LabelIDs []string `json:"labelIds"`
	// AutoArchive (#443) defaults to true, matching store.AreaRow's own nil ->
	// true default, so a create that omits it gets a sweeping drawer.
	AutoArchive     *bool `json:"autoArchive"`
	AutoArchiveDays *int  `json:"autoArchiveDays"`
	HiddenFromRail  *bool `json:"hiddenFromRail"`
}

// validateAreaFeeds parses and validates a create/update's `tabs` and
// `labelIds` into store.AreaFeeds: every tab must parse (tabs.Parse), every
// label id must parse as a ULID and EXIST for this user (tx.GetLabels — RLS
// makes another user's label indistinguishable from a nonexistent one), and
// none may be `own` — the area's own label, which is already implied and
// must never be asserted as an additional feed of itself.
//
// own is nil on a create with no explicit labelId (the store will mint one,
// and there is nothing yet to compare against); it is the current LabelID on
// an update, since that property is immutable.
func validateAreaFeeds(ctx context.Context, tx *store.UserTx, tabNames, labelIDStrs []string, own *ulid.ULID) ([]store.AreaFeed, *SetError) {
	feeds := make([]store.AreaFeed, 0, len(tabNames)+len(labelIDStrs))
	for _, name := range tabNames {
		t, ok := tabs.Parse(name)
		if !ok {
			return nil, &SetError{Type: setInvalidProps,
				Description: fmt.Sprintf("%q is not a category tab", name), Properties: []string{"tabs"}}
		}
		feeds = append(feeds, store.AreaFeed{Tab: t})
	}
	if len(labelIDStrs) == 0 {
		return feeds, nil
	}

	parsed := make([]ulid.ULID, 0, len(labelIDStrs))
	byRaw := make(map[string]ulid.ULID, len(labelIDStrs))
	var missing []string
	for _, s := range labelIDStrs {
		lid, err := ulid.Parse(s)
		if err != nil {
			missing = append(missing, s)
			continue
		}
		if own != nil && lid == *own {
			return nil, &SetError{Type: setInvalidProps,
				Description: "the area's own label is already its feed", Properties: []string{"labelIds"}}
		}
		byRaw[s] = lid
		parsed = append(parsed, lid)
	}
	if len(parsed) > 0 {
		views, err := tx.GetLabels(ctx, parsed)
		if err != nil {
			return nil, &SetError{Type: setForbidden, Description: err.Error()}
		}
		found := make(map[ulid.ULID]bool, len(views))
		for _, v := range views {
			found[v.ID] = true
		}
		for s, lid := range byRaw {
			if !found[lid] {
				missing = append(missing, s)
			}
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing) // deterministic description
		return nil, &SetError{Type: setInvalidProps,
			Description: "labelIds not found for this account: " + strings.Join(missing, ", "),
			Properties:  []string{"labelIds"}}
	}
	for _, lid := range parsed {
		feeds = append(feeds, store.AreaFeed{LabelID: &lid})
	}
	return feeds, nil
}

// areaStoreSetError maps a Create/UpdateArea error to a per-object SetError.
//
// store.ErrFeedTaken's own message already names the feed and the area
// holding it (feedDescription in areas.go), so it is surfaced verbatim; only
// the PROPERTY has to be worked out, by sniffing whether the message
// describes a tab or a label.
//
// store.ErrAreaNameTaken and store.ErrAreaLabelTaken (#442 increment 3) are
// the sentinels the increment-2 version of this comment said were missing: the
// store now reads pgconn.PgError.ConstraintName (areas.go) to tell a name
// collision from an own-label collision, so each maps to exactly the one
// property that was wrong instead of naming both.
//
// Everything else falls back to naming BOTH `name` and `labelId`, since a
// store error this function does not recognise carries no way to tell which
// one caused it.
func areaStoreSetError(err error) *SetError {
	if errors.Is(err, store.ErrFeedTaken) {
		props := []string{"labelIds"}
		if strings.Contains(err.Error(), `tab "`) {
			props = []string{"tabs"}
		}
		return &SetError{Type: setInvalidProps, Description: err.Error(), Properties: props}
	}
	if errors.Is(err, store.ErrAreaNameTaken) {
		return &SetError{Type: setInvalidProps, Description: err.Error(), Properties: []string{"name"}}
	}
	if errors.Is(err, store.ErrAreaLabelTaken) {
		return &SetError{Type: setInvalidProps, Description: err.Error(), Properties: []string{"labelId"}}
	}
	return &SetError{Type: setInvalidProps, Description: err.Error(), Properties: []string{"name", "labelId"}}
}

// createArea parses one Area/set create patch and inserts it. nextPos is the
// batch's running "one past the current max position" counter: the first
// create without an explicit `position` in a call takes it, and every
// subsequent one in the SAME call takes the next integer after — so a batch
// of several new areas gets a stable, non-colliding order rather than every
// one landing on the same default.
func createArea(ctx context.Context, tx *store.UserTx, raw json.RawMessage, nextPos *int) (map[string]any, ulid.ULID, *SetError) {
	var c areaCreate
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, ulid.ULID{}, &SetError{Type: setInvalidProps, Description: err.Error()}
	}

	name := ""
	if c.Name != nil {
		name = strings.TrimSpace(*c.Name)
	}
	if name == "" {
		return nil, ulid.ULID{}, &SetError{Type: setInvalidProps,
			Description: "name is required", Properties: []string{"name"}}
	}
	if utf8.RuneCountInString(name) > areaMaxNameRunes {
		return nil, ulid.ULID{}, &SetError{Type: setInvalidProps,
			Description: fmt.Sprintf("name must be at most %d characters", areaMaxNameRunes),
			Properties:  []string{"name"}}
	}

	row := store.AreaRow{Name: name}
	if c.Position != nil {
		row.Position = *c.Position
	} else {
		row.Position = *nextPos
	}
	*nextPos = row.Position + 1

	var ownLabel *ulid.ULID
	if c.LabelID != nil {
		lid, err := ulid.Parse(*c.LabelID)
		if err != nil {
			return nil, ulid.ULID{}, &SetError{Type: setInvalidProps,
				Description: "labelId is not a valid id", Properties: []string{"labelId"}}
		}
		views, err := tx.GetLabels(ctx, []ulid.ULID{lid})
		if err != nil {
			return nil, ulid.ULID{}, &SetError{Type: setForbidden, Description: err.Error()}
		}
		if len(views) == 0 {
			return nil, ulid.ULID{}, &SetError{Type: setInvalidProps,
				Description: "labelId names no label of this account", Properties: []string{"labelId"}}
		}
		row.LabelID = &lid
		ownLabel = &lid
	}

	if c.AutoArchive != nil {
		row.AutoArchive = c.AutoArchive
	}
	if c.AutoArchiveDays != nil {
		if *c.AutoArchiveDays <= 0 {
			return nil, ulid.ULID{}, &SetError{Type: setInvalidProps,
				Description: "autoArchiveDays must be a positive integer or null",
				Properties:  []string{"autoArchiveDays"}}
		}
		row.AutoArchiveDays = c.AutoArchiveDays
	}
	if c.HiddenFromRail != nil {
		row.HiddenFromRail = *c.HiddenFromRail
	}

	feeds, serr := validateAreaFeeds(ctx, tx, c.Tabs, c.LabelIDs, ownLabel)
	if serr != nil {
		return nil, ulid.ULID{}, serr
	}
	row.Feeds = feeds

	id, err := tx.CreateArea(ctx, row)
	if err != nil {
		return nil, ulid.ULID{}, areaStoreSetError(err)
	}
	// RFC 8620 §5.3: `created` reports the whole server-set object, not just
	// the id — re-read rather than reconstruct, so it cannot disagree with
	// what a subsequent Area/get would show.
	views, err := tx.GetAreas(ctx, []ulid.ULID{id})
	if err != nil || len(views) == 0 {
		return map[string]any{"id": id.String()}, id, nil
	}
	return areaObject(views[0]), id, nil
}

// updateArea parses one Area/set update patch and applies it. `id` and
// `labelId` are immutable: an area's own label IS its identity (placing a
// message in the area is applying that label), so repointing it would
// silently change what "this area" means to a client already syncing it —
// making a new area is how you use a different label.
//
// `tabs`/`labelIds` are REPLACE-ALL when either is present in the patch: the
// one not mentioned keeps its current value (read fresh, since a batch update
// may target an area a `create` in the SAME call just made — see the
// `resolveSetID` call site in areaSet).
func updateArea(ctx context.Context, tx *store.UserTx, id ulid.ULID, raw json.RawMessage) *SetError {
	var patch map[string]json.RawMessage
	if err := json.Unmarshal(raw, &patch); err != nil {
		return &SetError{Type: setInvalidPatch, Description: err.Error()}
	}

	var (
		p                    store.AreaPatch
		tabNames, labelIDStr []string
		feedsTouched         bool
	)
	for key, val := range patch {
		switch key {
		case "name":
			var s string
			if err := json.Unmarshal(val, &s); err != nil {
				return &SetError{Type: setInvalidProps,
					Description: "name must be a string", Properties: []string{"name"}}
			}
			s = strings.TrimSpace(s)
			if s == "" {
				return &SetError{Type: setInvalidProps,
					Description: "name must not be empty", Properties: []string{"name"}}
			}
			if utf8.RuneCountInString(s) > areaMaxNameRunes {
				return &SetError{Type: setInvalidProps,
					Description: fmt.Sprintf("name must be at most %d characters", areaMaxNameRunes),
					Properties:  []string{"name"}}
			}
			p.Name = &s
		case "position":
			var n int
			if err := json.Unmarshal(val, &n); err != nil {
				return &SetError{Type: setInvalidProps,
					Description: "position must be an integer", Properties: []string{"position"}}
			}
			p.Position = &n
		case "tabs":
			if err := json.Unmarshal(val, &tabNames); err != nil {
				return &SetError{Type: setInvalidProps,
					Description: "tabs must be an array of strings", Properties: []string{"tabs"}}
			}
			if tabNames == nil {
				tabNames = []string{}
			}
			feedsTouched = true
		case "labelIds":
			if err := json.Unmarshal(val, &labelIDStr); err != nil {
				return &SetError{Type: setInvalidProps,
					Description: "labelIds must be an array of strings", Properties: []string{"labelIds"}}
			}
			if labelIDStr == nil {
				labelIDStr = []string{}
			}
			feedsTouched = true
		case "autoArchive":
			var b bool
			if err := json.Unmarshal(val, &b); err != nil {
				return &SetError{Type: setInvalidProps,
					Description: "autoArchive must be a boolean", Properties: []string{"autoArchive"}}
			}
			p.SetAutoArchive = true
			p.AutoArchive = b
		case "autoArchiveDays":
			p.SetAutoArchiveDays = true
			if !isJSONNull(val) {
				var n int
				if err := json.Unmarshal(val, &n); err != nil || n <= 0 {
					return &SetError{Type: setInvalidProps,
						Description: "autoArchiveDays must be a positive integer or null",
						Properties:  []string{"autoArchiveDays"}}
				}
				p.AutoArchiveDays = &n
			}
		case "hiddenFromRail":
			var b bool
			if err := json.Unmarshal(val, &b); err != nil {
				return &SetError{Type: setInvalidProps,
					Description: "hiddenFromRail must be a boolean", Properties: []string{"hiddenFromRail"}}
			}
			p.HiddenFromRail = &b
		case "id", "labelId":
			return &SetError{Type: setInvalidProps,
				Description: key + " is immutable: an area's own label is its identity; " +
					"make a new area to use a different label",
				Properties: []string{key}}
		default:
			return &SetError{Type: setInvalidProps,
				Description: "unsupported Area property in patch: " + key, Properties: []string{key}}
		}
	}

	if feedsTouched {
		views, err := tx.GetAreas(ctx, []ulid.ULID{id})
		if err != nil {
			return &SetError{Type: setForbidden, Description: err.Error()}
		}
		if len(views) == 0 {
			return &SetError{Type: setNotFound}
		}
		current := views[0]
		// A property this patch did not mention keeps its CURRENT value —
		// read fresh rather than from a snapshot taken before this call's
		// other creates/updates, so a batch that creates an area and then
		// edits only its `labelIds` does not accidentally wipe `tabs`.
		if tabNames == nil {
			for _, f := range current.Feeds {
				if f.Tab != "" {
					tabNames = append(tabNames, string(f.Tab))
				}
			}
		}
		if labelIDStr == nil {
			for _, f := range current.Feeds {
				if f.LabelID != nil && *f.LabelID != current.LabelID {
					labelIDStr = append(labelIDStr, f.LabelID.String())
				}
			}
		}
		feeds, serr := validateAreaFeeds(ctx, tx, tabNames, labelIDStr, &current.LabelID)
		if serr != nil {
			return serr
		}
		p.SetFeeds = true
		p.Feeds = feeds
	}

	found, err := tx.UpdateArea(ctx, id, p)
	if err != nil {
		return areaStoreSetError(err)
	}
	if !found {
		return &SetError{Type: setNotFound}
	}
	return nil
}

func (a *API) areaSet(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	var args emailSetArgs // the shared {accountId, ifInState, create, update, destroy} shape
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	if mErr := resolveAccount(userID, args.AccountID); mErr != nil {
		return nil, mErr
	}

	res := setResult{
		AccountID:    userID.String(),
		Created:      map[string]any{},
		Updated:      map[string]any{},
		Destroyed:    []string{},
		NotCreated:   map[string]SetError{},
		NotUpdated:   map[string]SetError{},
		NotDestroyed: map[string]SetError{},
	}

	var stateErr *MethodError
	err := a.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		// Materialise before reading oldState, for categoryTabGet's ORDER
		// MATTERS reason: the default plan's creates must be inside the state
		// bracket, not before it.
		existing, err := tx.ListAreas(ctx)
		if err != nil {
			return err
		}
		oldState, err := tx.ChangeState(ctx, store.ObjectArea)
		if err != nil {
			return err
		}
		res.OldState = encodeState(oldState)
		if args.IfInState != nil && *args.IfInState != encodeState(oldState) {
			stateErr = &MethodError{Type: errStateMismatch}
			return errMethodRollback
		}

		nextPos := 0
		for _, v := range existing {
			if v.Position >= nextPos {
				nextPos = v.Position + 1
			}
		}

		// Track created ids so a later update/destroy in the same call can
		// reference them by creation id (RFC 8620 §5.3 #creationId).
		created := map[string]ulid.ULID{}

		for cid, raw := range args.Create {
			obj, id, serr := createArea(ctx, tx, raw, &nextPos)
			if serr != nil {
				res.NotCreated[cid] = *serr
				continue
			}
			created[cid] = id
			res.Created[cid] = obj
		}

		for id, raw := range args.Update {
			aid, ok := resolveSetID(id, created)
			if !ok {
				res.NotUpdated[id] = SetError{Type: setNotFound}
				continue
			}
			if serr := updateArea(ctx, tx, aid, raw); serr != nil {
				res.NotUpdated[id] = *serr
				continue
			}
			res.Updated[id] = nil
		}

		// The area is destroyed; ITS OWN LABEL SURVIVES, and every message
		// that carried it keeps the label and everything else about it
		// (DeleteArea's own doc comment in areas.go) — giving up the area is
		// not the same as giving up what it was made of.
		for _, id := range args.Destroy {
			aid, ok := resolveSetID(id, created)
			if !ok {
				res.NotDestroyed[id] = SetError{Type: setNotFound}
				continue
			}
			found, derr := tx.DeleteArea(ctx, aid)
			if derr != nil {
				return derr
			}
			if !found {
				res.NotDestroyed[id] = SetError{Type: setNotFound}
				continue
			}
			res.Destroyed = append(res.Destroyed, id)
		}

		newState, err := tx.ChangeState(ctx, store.ObjectArea)
		if err != nil {
			return err
		}
		res.NewState = encodeState(newState)
		return nil
	})
	if stateErr != nil {
		return nil, stateErr
	}
	if err != nil && err != errMethodRollback {
		return nil, &MethodError{Type: errServerFail, Description: err.Error()}
	}
	return mustMarshal(res), nil
}

// ---- Area/getSummary ------------------------------------------------------

// areaGetSummaryArgs is Area/getSummary's request: a Mailroom extension
// method (RFC 8620 allows a capability to define its own Type/verb), not a
// standard get/query/changes/set shape, because its answer is a set of
// COUNTS, not a list of objects.
type areaGetSummaryArgs struct {
	AccountID     string   `json:"accountId"`
	MailAccountID *string  `json:"mailAccountId"`
	Threshold     *float64 `json:"threshold"`
}

func (a *API) areaGetSummary(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	var args areaGetSummaryArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	if mErr := resolveAccount(userID, args.AccountID); mErr != nil {
		return nil, mErr
	}

	var scope store.DeskScope
	if args.MailAccountID != nil {
		id, err := ulid.Parse(*args.MailAccountID)
		if err != nil {
			return nil, &MethodError{Type: errInvalidArguments, Description: "mailAccountId is not a valid id"}
		}
		scope.AccountID = &id
	}

	thresholdGiven := args.Threshold != nil
	var threshold float64
	if thresholdGiven {
		threshold = *args.Threshold
		if threshold < 0 || threshold > 1 {
			return nil, &MethodError{Type: errInvalidArguments, Description: "threshold must be between 0 and 1"}
		}
	}

	var (
		state         int64
		counts        []store.AreaCounts
		archivedCount map[ulid.ULID]int
	)
	mErr := a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
		// Materialise first: a first-contact summary is over the materialised
		// plan, exactly as areaGet's ORDER MATTERS comment explains.
		if _, err := tx.ListAreas(ctx); err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		if !thresholdGiven {
			prefs, err := tx.GetDeskPreferences(ctx)
			if err != nil {
				return &MethodError{Type: errServerFail, Description: err.Error()}
			}
			threshold = float64(prefs.LineThreshold)
		}
		scope.Threshold = float32(threshold)
		var err error
		if counts, err = tx.AreaSummary(ctx, scope); err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		// archivedByDrawer (#443): an all-time count per area, alongside the
		// live Surface/Drawer/Unscored split — the same one round trip AreaSummary
		// already pays, not a second query per area.
		if archivedCount, err = tx.DrawerArchiveCounts(ctx); err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		state, err = tx.ChangeState(ctx, store.ObjectArea)
		if err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		return nil
	})
	if mErr != nil {
		return nil, mErr
	}

	list := summaryList(counts, archivedCount)

	return mustMarshal(map[string]any{
		"accountId": userID.String(),
		"state":     encodeState(state),
		// Reported as the float32 scope.Threshold actually used for the
		// query, not the float64 `threshold` local — a float64 widening of a
		// float32 DeskPreferences value prints as a long, ugly decimal
		// (0.8999999761581421 for "0.9"), because encoding/json's shortest
		// round-trip algorithm is precision-AWARE: it looks at the Go type
		// being marshalled, and a float32 field marshals to the shortest
		// string that round-trips at 32-bit precision, matching
		// DeskPreferences/get's own lineThreshold (deskPreferencesObject),
		// which never hit this because it marshals the float32 field
		// directly.
		"threshold": scope.Threshold,
		"list":      list,
	}), nil
}

// summaryList renders Area/getSummary's `list` — one row per area in the
// store's order plus the Needs-sorting row — shared with Area/getDesk so the
// two methods' summary rows are one shape.
func summaryList(counts []store.AreaCounts, archivedCount map[ulid.ULID]int) []map[string]any {
	list := make([]map[string]any, 0, len(counts))
	for _, c := range counts {
		var areaID any
		// archivedByDrawer is 0 for the Needs-sorting row (c.AreaID nil): nothing
		// sweeps unsorted mail — the drawer sweep only ever resolves a message
		// through the SAME area resolver AreaSummary itself uses, and Needs
		// sorting has no area id to key app.drawer_archives on.
		archived := 0
		if c.AreaID != nil {
			areaID = c.AreaID.String()
			archived = archivedCount[*c.AreaID]
		}
		list = append(list, map[string]any{
			"areaId":           areaID,
			"surface":          c.Surface,
			"drawer":           c.Drawer,
			"unscored":         c.Unscored,
			"archivedByDrawer": archived,
		})
	}
	return list
}
