// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/notify"
	"mailroom/internal/store"
)

// DeskPreferences/* and ClientPreferences/* (#442) — the desk's two settings
// surfaces increment 1 stored (internal/store/desk_preferences.go).
//
// DeskPreferences IS A SINGLETON: one record per user, and its wire `id` is
// the USER's id (the change_log object_id the store already appends
// DeskPreferences rows under — GetDeskPreferences' own doc comment). There is
// exactly one valid id to /get or /set by, so both refuse anything else.
//
// ClientPreferences IS NOT: a phone and a browser legitimately want different
// density and pane defaults, so there is one row per client family, and
// /get with no ids returns all three.

// ---- DeskPreferences ------------------------------------------------------

func deskPreferencesObject(userID ulid.ULID, p store.DeskPreferences) map[string]any {
	return map[string]any{
		"id":                 userID.String(),
		"lineThreshold":      p.LineThreshold,
		"drawerDefaultDays":  p.DrawerDefaultDays,
		"trayNagDays":        p.TrayNagDays,
		"drawerSweepEnabled": p.DrawerSweepEnabled,
		// setupCompletedAt (#446) is NULL for a login that has not been through
		// day-one setup, and that is how the client decides to open the WIZARD
		// rather than the desk. Migration 00050 backfilled every login that
		// already had preferences, so an established desk never sees it null.
		"setupCompletedAt": jmapDateOrNil(p.SetupCompletedAt),
		// learnedPlacementEnabled (#714): learned placement's off switch,
		// enforced by the store at ingest, default true.
		"learnedPlacementEnabled": p.LearnedPlacementEnabled,
		// quietHours (#59): off by default. See quietHoursObject.
		"quietHours": quietHoursObject(p.QuietHours),
		// followUp (#58): the follow-up nudges. See followUpObject.
		"followUp": followUpObject(p.FollowUp),
	}
}

// followUpObject renders the follow-up nudge settings: outbound ("no one
// answered your mail", default on), inbound ("you haven't replied to this",
// default off — opt-in) and inboundDays, the inbound delay (1-14, default 3).
func followUpObject(f store.FollowUpSettings) map[string]any {
	return map[string]any{
		"outbound":    f.Outbound,
		"inbound":     f.Inbound,
		"inboundDays": f.InboundDays,
	}
}

// parseFollowUp validates a whole followUp object; every field is required,
// for parseQuietHours' reason (one form, no guessing which half was meant).
func parseFollowUp(raw json.RawMessage) (store.FollowUpSettings, *SetError) {
	bad := func(why string) *SetError {
		return &SetError{Type: setInvalidProps, Description: "followUp: " + why, Properties: []string{"followUp"}}
	}
	var in struct {
		Outbound    *bool `json:"outbound"`
		Inbound     *bool `json:"inbound"`
		InboundDays *int  `json:"inboundDays"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return store.FollowUpSettings{}, bad("must be an object with outbound, inbound and inboundDays")
	}
	if in.Outbound == nil || in.Inbound == nil || in.InboundDays == nil {
		return store.FollowUpSettings{}, bad("outbound, inbound and inboundDays are all required")
	}
	if *in.InboundDays < 1 || *in.InboundDays > 14 {
		return store.FollowUpSettings{}, bad("inboundDays is a whole number of days from 1 to 14")
	}
	return store.FollowUpSettings{Outbound: *in.Outbound, Inbound: *in.Inbound, InboundDays: *in.InboundDays}, nil
}

// quietHoursObject renders the quiet-hours setting. start and end are MINUTES
// after local midnight (0..1439) in timeZone, an IANA name; days lists the
// weekdays a quiet period STARTS on, 0 = Sunday … 6 = Saturday, so an
// overnight period that starts on Friday also covers Saturday morning.
// allowVip lets VIP mail through while it is quiet (default true).
func quietHoursObject(q store.QuietHours) map[string]any {
	days := make([]int, 0, 7)
	for d := 0; d < 7; d++ {
		if q.Days&(1<<d) != 0 {
			days = append(days, d)
		}
	}
	return map[string]any{
		"enabled":  q.Enabled,
		"start":    q.Start,
		"end":      q.End,
		"days":     days,
		"timeZone": q.Timezone,
		"allowVip": q.AllowVIP,
	}
}

// parseQuietHours validates a whole quietHours object. Every field is
// required: the setting is one form, and a partial object would leave the
// server to guess which half the client meant.
func parseQuietHours(raw json.RawMessage) (store.QuietHours, *SetError) {
	bad := func(why string) *SetError {
		return &SetError{Type: setInvalidProps, Description: "quietHours: " + why, Properties: []string{"quietHours"}}
	}
	var in struct {
		Enabled  *bool   `json:"enabled"`
		Start    *int    `json:"start"`
		End      *int    `json:"end"`
		Days     *[]int  `json:"days"`
		TimeZone *string `json:"timeZone"`
		AllowVIP *bool   `json:"allowVip"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return store.QuietHours{}, bad("must be an object with enabled, start, end, days, timeZone and allowVip")
	}
	if in.Enabled == nil || in.Start == nil || in.End == nil || in.Days == nil || in.TimeZone == nil || in.AllowVIP == nil {
		return store.QuietHours{}, bad("enabled, start, end, days, timeZone and allowVip are all required")
	}
	if *in.Start < 0 || *in.Start >= 1440 || *in.End < 0 || *in.End >= 1440 {
		return store.QuietHours{}, bad("start and end are minutes after midnight, 0 to 1439")
	}
	if *in.Start == *in.End {
		return store.QuietHours{}, bad("start and end must differ")
	}
	mask := 0
	for _, d := range *in.Days {
		if d < 0 || d > 6 {
			return store.QuietHours{}, bad("days are 0 (Sunday) to 6 (Saturday)")
		}
		mask |= 1 << d
	}
	if err := notify.ValidateZone(*in.TimeZone); err != nil {
		return store.QuietHours{}, bad("timeZone must be an IANA time zone name such as Europe/London")
	}
	return store.QuietHours{
		Enabled:  *in.Enabled,
		Start:    *in.Start,
		End:      *in.End,
		Days:     mask,
		Timezone: *in.TimeZone,
		AllowVIP: *in.AllowVIP,
	}, nil
}

type deskPreferencesGetArgs struct {
	AccountID string   `json:"accountId"`
	IDs       []string `json:"ids"`
}

func (a *API) deskPreferencesGet(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	var args deskPreferencesGetArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	if mErr := resolveAccount(userID, args.AccountID); mErr != nil {
		return nil, mErr
	}

	var (
		state int64
		prefs store.DeskPreferences
	)
	if err := a.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		// Materialise BEFORE reading state, for categoryTabGet's ORDER MATTERS
		// reason: GetDeskPreferences appends a `created` row on first contact.
		var e error
		if prefs, e = tx.GetDeskPreferences(ctx); e != nil {
			return e
		}
		state, e = tx.ChangeState(ctx, store.ObjectDeskPreferences)
		return e
	}); err != nil {
		return nil, &MethodError{Type: errServerFail, Description: err.Error()}
	}

	notFound := []string{}
	list := []map[string]any{}
	if args.IDs == nil {
		list = append(list, deskPreferencesObject(userID, prefs))
	} else {
		for _, want := range args.IDs {
			if want == userID.String() {
				list = append(list, deskPreferencesObject(userID, prefs))
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

func (a *API) deskPreferencesChanges(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	return a.handleChanges(ctx, userID, rawArgs, store.ObjectDeskPreferences)
}

// updateDeskPreferences parses and applies a DeskPreferences/set update
// patch, validating each property in Go BEFORE the store's CHECK constraint
// ever fires, so a rejection names the property rather than surfacing raw
// constraint text.
func updateDeskPreferences(ctx context.Context, tx *store.UserTx, raw json.RawMessage) *SetError {
	var patch map[string]json.RawMessage
	if err := json.Unmarshal(raw, &patch); err != nil {
		return &SetError{Type: setInvalidPatch, Description: err.Error()}
	}
	var p store.DeskPreferencesPatch
	for key, val := range patch {
		switch key {
		case "lineThreshold":
			var f float64
			if err := json.Unmarshal(val, &f); err != nil || f < 0 || f > 1 {
				return &SetError{Type: setInvalidProps,
					Description: "lineThreshold must be a number between 0 and 1",
					Properties:  []string{"lineThreshold"}}
			}
			v := float32(f)
			p.LineThreshold = &v
		case "drawerDefaultDays":
			var n int
			if err := json.Unmarshal(val, &n); err != nil || n <= 0 {
				return &SetError{Type: setInvalidProps,
					Description: "drawerDefaultDays must be a positive integer",
					Properties:  []string{"drawerDefaultDays"}}
			}
			p.DrawerDefaultDays = &n
		case "trayNagDays":
			var n int
			if err := json.Unmarshal(val, &n); err != nil || n <= 0 {
				return &SetError{Type: setInvalidProps,
					Description: "trayNagDays must be a positive integer",
					Properties:  []string{"trayNagDays"}}
			}
			p.TrayNagDays = &n
		case "drawerSweepEnabled":
			var b bool
			if err := json.Unmarshal(val, &b); err != nil {
				return &SetError{Type: setInvalidProps,
					Description: "drawerSweepEnabled must be a boolean",
					Properties:  []string{"drawerSweepEnabled"}}
			}
			p.DrawerSweepEnabled = &b
		case "learnedPlacementEnabled":
			var b bool
			if err := json.Unmarshal(val, &b); err != nil {
				return &SetError{Type: setInvalidProps,
					Description: "learnedPlacementEnabled must be a boolean",
					Properties:  []string{"learnedPlacementEnabled"}}
			}
			p.LearnedPlacementEnabled = &b
		case "quietHours":
			q, serr := parseQuietHours(val)
			if serr != nil {
				return serr
			}
			p.QuietHours = &q
		case "followUp":
			f, serr := parseFollowUp(val)
			if serr != nil {
				return serr
			}
			p.FollowUp = &f
		case "setupCompletedAt":
			// SETTABLE TO A DATE OR TO NULL, and null has to be expressible: the
			// wizard writes a date when it finishes, and "show me the wizard
			// again" — an operator re-running day-one on a mailbox that has
			// changed shape — is exactly setting it back to null. Hence the
			// store's three-state patch (leave / set / set-null) rather than a
			// nil pointer meaning both.
			if isJSONNull(val) {
				p.SetSetupCompletedAt = true
				p.SetupCompletedAt = nil
				continue
			}
			var str string
			if err := json.Unmarshal(val, &str); err != nil {
				return &SetError{Type: setInvalidProps,
					Description: "setupCompletedAt must be a UTCDate string or null",
					Properties:  []string{"setupCompletedAt"}}
			}
			at, ok := jmapTime(str)
			if !ok {
				return &SetError{Type: setInvalidProps,
					Description: "setupCompletedAt is not a valid UTCDate",
					Properties:  []string{"setupCompletedAt"}}
			}
			p.SetSetupCompletedAt = true
			p.SetupCompletedAt = &at
		case "id":
			return &SetError{Type: setInvalidProps, Description: "id is immutable", Properties: []string{"id"}}
		default:
			return &SetError{Type: setInvalidProps,
				Description: "unsupported DeskPreferences property in patch: " + key, Properties: []string{key}}
		}
	}
	if err := tx.UpdateDeskPreferences(ctx, p); err != nil {
		if errors.Is(err, store.ErrDeskPreferencesInvalid) {
			return &SetError{Type: setInvalidProps, Description: err.Error()}
		}
		return &SetError{Type: setForbidden, Description: err.Error()}
	}
	return nil
}

func (a *API) deskPreferencesSet(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
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
		oldState, err := tx.ChangeState(ctx, store.ObjectDeskPreferences)
		if err != nil {
			return err
		}
		res.OldState = encodeState(oldState)
		if args.IfInState != nil && *args.IfInState != encodeState(oldState) {
			stateErr = &MethodError{Type: errStateMismatch}
			return errMethodRollback
		}

		// The record is one fixed row per user: a client neither creates nor
		// destroys it.
		for cid := range args.Create {
			res.NotCreated[cid] = SetError{Type: setForbidden,
				Description: "the desk's preferences are one fixed record; update it"}
		}
		for _, id := range args.Destroy {
			res.NotDestroyed[id] = SetError{Type: setForbidden,
				Description: "the desk's preferences are one fixed record; update it"}
		}

		for id, raw := range args.Update {
			if id != userID.String() {
				res.NotUpdated[id] = SetError{Type: setNotFound}
				continue
			}
			if serr := updateDeskPreferences(ctx, tx, raw); serr != nil {
				res.NotUpdated[id] = *serr
				continue
			}
			res.Updated[id] = nil
		}

		newState, err := tx.ChangeState(ctx, store.ObjectDeskPreferences)
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

// ---- ClientPreferences ------------------------------------------------

// clientFamilyOrder is the fixed order ClientPreferences/get with nil ids
// returns all three families in — the same order deskClientFamilies
// advertises, so a client zipping the two together gets a stable pairing.
var clientFamilyOrder = []store.ClientFamily{store.ClientWeb, store.ClientPhone, store.ClientDesktop}

func clientPreferencesObject(p store.ClientPreferences) map[string]any {
	return map[string]any{
		"id":           p.ID.String(),
		"family":       string(p.Family),
		"theme":        p.Theme,
		"density":      p.Density,
		"messageOpens": p.MessageOpens,
		"paneSplitPct": p.PaneSplitPct,
		"textSize":     p.TextSize,
		"rowLayout":    p.RowLayout,
	}
}

type clientPreferencesGetArgs struct {
	AccountID string   `json:"accountId"`
	IDs       []string `json:"ids"`
}

func (a *API) clientPreferencesGet(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	var args clientPreferencesGetArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	if mErr := resolveAccount(userID, args.AccountID); mErr != nil {
		return nil, mErr
	}

	var (
		state int64
		all   []store.ClientPreferences
	)
	if err := a.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		all = make([]store.ClientPreferences, 0, len(clientFamilyOrder))
		for _, fam := range clientFamilyOrder {
			// Materialise each family BEFORE reading state, same ORDER
			// MATTERS reason as everywhere else in this increment.
			p, e := tx.GetClientPreferences(ctx, fam)
			if e != nil {
				return e
			}
			all = append(all, p)
		}
		var e error
		state, e = tx.ChangeState(ctx, store.ObjectClientPreferences)
		return e
	}); err != nil {
		return nil, &MethodError{Type: errServerFail, Description: err.Error()}
	}

	notFound := []string{}
	list := []map[string]any{}
	if args.IDs == nil {
		for _, p := range all {
			list = append(list, clientPreferencesObject(p))
		}
	} else {
		byID := make(map[string]store.ClientPreferences, len(all))
		for _, p := range all {
			byID[p.ID.String()] = p
		}
		for _, want := range args.IDs {
			if p, ok := byID[want]; ok {
				list = append(list, clientPreferencesObject(p))
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

func (a *API) clientPreferencesChanges(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	return a.handleChanges(ctx, userID, rawArgs, store.ObjectClientPreferences)
}

// updateClientPreferences validates a patch against the closed vocabularies
// desk.go defines — the same ones deskCapability advertises — before the
// store's own CHECK constraint ever runs.
func updateClientPreferences(ctx context.Context, tx *store.UserTx, fam store.ClientFamily, raw json.RawMessage) *SetError {
	var patch map[string]json.RawMessage
	if err := json.Unmarshal(raw, &patch); err != nil {
		return &SetError{Type: setInvalidPatch, Description: err.Error()}
	}
	var p store.ClientPreferencesPatch
	for key, val := range patch {
		switch key {
		case "theme":
			var s string
			if err := json.Unmarshal(val, &s); err != nil || !inVocabulary(deskThemes, s) {
				return &SetError{Type: setInvalidProps,
					Description: "theme must be one of " + strings.Join(deskThemes, ", "),
					Properties:  []string{"theme"}}
			}
			p.Theme = &s
		case "density":
			var s string
			if err := json.Unmarshal(val, &s); err != nil || !inVocabulary(deskDensities, s) {
				return &SetError{Type: setInvalidProps,
					Description: "density must be one of " + strings.Join(deskDensities, ", "),
					Properties:  []string{"density"}}
			}
			p.Density = &s
		case "messageOpens":
			var s string
			if err := json.Unmarshal(val, &s); err != nil || !inVocabulary(deskMessageOpens, s) {
				return &SetError{Type: setInvalidProps,
					Description: "messageOpens must be one of " + strings.Join(deskMessageOpens, ", "),
					Properties:  []string{"messageOpens"}}
			}
			p.MessageOpens = &s
		case "paneSplitPct":
			var n int
			if err := json.Unmarshal(val, &n); err != nil || n < 20 || n > 80 {
				return &SetError{Type: setInvalidProps,
					Description: "paneSplitPct must be between 20 and 80",
					Properties:  []string{"paneSplitPct"}}
			}
			p.PaneSplitPct = &n
		case "textSize":
			var s string
			if err := json.Unmarshal(val, &s); err != nil || !inVocabulary(deskTextSizes, s) {
				return &SetError{Type: setInvalidProps,
					Description: "textSize must be one of " + strings.Join(deskTextSizes, ", "),
					Properties:  []string{"textSize"}}
			}
			p.TextSize = &s
		case "rowLayout":
			var s string
			if err := json.Unmarshal(val, &s); err != nil || !inVocabulary(deskRowLayouts, s) {
				return &SetError{Type: setInvalidProps,
					Description: "rowLayout must be one of " + strings.Join(deskRowLayouts, ", "),
					Properties:  []string{"rowLayout"}}
			}
			p.RowLayout = &s
		case "id", "family":
			return &SetError{Type: setInvalidProps, Description: key + " is immutable", Properties: []string{key}}
		default:
			return &SetError{Type: setInvalidProps,
				Description: "unsupported ClientPreferences property in patch: " + key, Properties: []string{key}}
		}
	}
	if err := tx.UpdateClientPreferences(ctx, fam, p); err != nil {
		if errors.Is(err, store.ErrClientPreferencesInvalid) {
			return &SetError{Type: setInvalidProps, Description: err.Error()}
		}
		return &SetError{Type: setForbidden, Description: err.Error()}
	}
	return nil
}

func (a *API) clientPreferencesSet(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	var args emailSetArgs
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
		oldState, err := tx.ChangeState(ctx, store.ObjectClientPreferences)
		if err != nil {
			return err
		}
		res.OldState = encodeState(oldState)
		if args.IfInState != nil && *args.IfInState != encodeState(oldState) {
			stateErr = &MethodError{Type: errStateMismatch}
			return errMethodRollback
		}

		// The three families are a fixed set: a client neither creates nor
		// destroys a row.
		for cid := range args.Create {
			res.NotCreated[cid] = SetError{Type: setForbidden,
				Description: "the client families are a fixed set; update one instead of creating a row"}
		}
		for _, id := range args.Destroy {
			res.NotDestroyed[id] = SetError{Type: setForbidden,
				Description: "the client families are a fixed set; update one instead of destroying it"}
		}

		// Materialise (and identify) all three BEFORE applying any update, the
		// same way /get does, so an update id can be matched to its family
		// even on a caller's very first call.
		famByID := make(map[string]store.ClientFamily, len(clientFamilyOrder))
		for _, fam := range clientFamilyOrder {
			p, e := tx.GetClientPreferences(ctx, fam)
			if e != nil {
				return e
			}
			famByID[p.ID.String()] = fam
		}

		for id, raw := range args.Update {
			fam, ok := famByID[id]
			if !ok {
				res.NotUpdated[id] = SetError{Type: setNotFound}
				continue
			}
			if serr := updateClientPreferences(ctx, tx, fam, raw); serr != nil {
				res.NotUpdated[id] = *serr
				continue
			}
			res.Updated[id] = nil
		}

		newState, err := tx.ChangeState(ctx, store.ObjectClientPreferences)
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
