// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
	"mailroom/internal/tabs"
)

// Label method handlers for the `urn:mailroom:labels` extension (spec §6/§13).
// Labels are canonical objects in Postgres that mirror to IMAP keywords; the
// core Email object still carries them as JMAP keywords (email.go). This
// extension exposes the label metadata itself — color and nesting (parent) —
// which are LOCAL-ONLY and deliberately do NOT round-trip to IMAP (only the
// leaf keyword atom mirrors; other clients see the raw atom, spec §6).
//
// Extensions add methods and properties but NEVER change core method semantics
// (spec §13), so these live entirely alongside the RFC surface.
//
// Registration: Label/get, Label/query, Label/changes and Label/set are wired
// into the dispatch map in service.go (the only place methods register). The
// capability is advertised per-account in session.go; it must also be added to
// the top-level session `capabilities` set for a client to `using` it — see the
// note in the task report (session.go is owned by the session milestone).

// labelObject renders a LabelView as a JMAP Label object, honoring a requested
// property subset (id is always present). `keyword` is the server-assigned IMAP
// mirror atom (read-only, "" when the label is local-only); `parentId` and
// `color` are the local-only nesting/color metadata.
func labelObject(lv store.LabelView, props map[string]bool) map[string]any {
	var parent any
	if lv.ParentID != nil {
		parent = lv.ParentID.String()
	}
	var color any
	if lv.Color != "" {
		color = lv.Color
	}
	var keyword any
	if lv.KeywordAtom != "" {
		keyword = lv.KeywordAtom
	}
	// `tab` is the label→category-tab mapping (#219): which Gmail-style tab this
	// label's mail feeds, or null when the label is unmapped — which falls through
	// to the tab classifier exactly as unlabelled mail does. Local-only, like
	// color and parentId: there is no IMAP concept to mirror it onto.
	var tab any
	if lv.Tab != "" {
		tab = string(lv.Tab)
	}
	full := map[string]any{
		"id":           lv.ID.String(),
		"name":         lv.Name,
		"color":        color,
		"parentId":     parent,
		"mirrorToImap": lv.MirrorToIMAP,
		"keyword":      keyword,
		"tab":          tab,
	}
	if props == nil {
		return full
	}
	out := map[string]any{"id": lv.ID.String()}
	for k, v := range full {
		if props[k] {
			out[k] = v
		}
	}
	return out
}

// ---- Label/get ------------------------------------------------------------

type labelGetArgs struct {
	AccountID  string    `json:"accountId"`
	IDs        *[]string `json:"ids"`
	Properties []string  `json:"properties"`
}

func (a *API) labelGet(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	var args labelGetArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	if mErr := resolveAccount(userID, args.AccountID); mErr != nil {
		return nil, mErr
	}
	var props map[string]bool
	if args.Properties != nil {
		props = make(map[string]bool, len(args.Properties))
		for _, p := range args.Properties {
			props[p] = true
		}
	}

	var (
		state     int64
		byID      = make(map[ulid.ULID]store.LabelView)
		requested []requestedID
		notFound  = []string{}
	)
	mErr := a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
		var views []store.LabelView
		var err error
		if args.IDs == nil {
			// ids:null means "all labels" (RFC 8620 §5.1).
			views, err = tx.ListLabels(ctx)
			if err != nil {
				return &MethodError{Type: errServerFail, Description: err.Error()}
			}
			for _, v := range views {
				requested = append(requested, requestedID{raw: v.ID.String(), id: v.ID, ok: true})
			}
		} else {
			var valid []ulid.ULID
			valid, notFound, requested = parseIDs(*args.IDs)
			views, err = tx.GetLabels(ctx, valid)
			if err != nil {
				return &MethodError{Type: errServerFail, Description: err.Error()}
			}
		}
		for _, v := range views {
			byID[v.ID] = v
		}
		state, err = tx.ChangeState(ctx, store.ObjectLabel)
		if err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		return nil
	})
	if mErr != nil {
		return nil, mErr
	}

	list := make([]map[string]any, 0, len(requested))
	for _, req := range requested {
		if !req.ok {
			continue
		}
		lv, ok := byID[req.id]
		if !ok {
			notFound = append(notFound, req.raw)
			continue
		}
		list = append(list, labelObject(lv, props))
	}
	return mustMarshal(getResult{
		AccountID: userID.String(),
		State:     encodeState(state),
		List:      list,
		NotFound:  notFound,
	}), nil
}

// ---- Label/query ----------------------------------------------------------

// labelQuery returns every label id in the stable name order the store defines.
// Filtering/sorting is not offered in 0.5 (the set is small); the response is a
// well-formed Foo/query the client windows itself.
func (a *API) labelQuery(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	var probe struct {
		AccountID string `json:"accountId"`
		Position  int    `json:"position"`
		Limit     *int   `json:"limit"`
	}
	if err := json.Unmarshal(rawArgs, &probe); err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	if mErr := resolveAccount(userID, probe.AccountID); mErr != nil {
		return nil, mErr
	}

	var (
		ids   []ulid.ULID
		state int64
	)
	mErr := a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
		views, err := tx.ListLabels(ctx)
		if err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		ids = make([]ulid.ULID, len(views))
		for i, v := range views {
			ids[i] = v.ID
		}
		state, err = tx.ChangeState(ctx, store.ObjectLabel)
		if err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		return nil
	})
	if mErr != nil {
		return nil, mErr
	}
	out := queryResult{
		AccountID:           userID.String(),
		QueryState:          encodeState(state),
		CanCalculateChanges: false,
		Position:            0,
		Total:               len(ids),
		IDs:                 ulidStrings(ids),
	}
	// A key's query is paged like every key query (#850).
	if principalFrom(ctx) != nil {
		if probe.Limit != nil && *probe.Limit < 0 {
			return nil, &MethodError{Type: errInvalidArguments, Description: "limit must be a non-negative integer"}
		}
		page, pos := keyQueryWindow("Label/query", ids, probe.Position, probe.Limit)
		out.IDs, out.Position, out.Limit = ulidStrings(page), pos, keyServerLimit("Label/query", probe.Limit)
	}
	return mustMarshal(out), nil
}

// ---- Label/changes --------------------------------------------------------

func (a *API) labelChanges(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	return a.handleChanges(ctx, userID, rawArgs, store.ObjectLabel)
}

// ---- Label/set ------------------------------------------------------------

// labelCreate is the parsed create form. Name is required; mirrorToImap
// defaults to true (spec §6). color/parentId are local-only metadata.
type labelCreate struct {
	Name         *string `json:"name"`
	Color        *string `json:"color"`
	ParentID     *string `json:"parentId"`
	MirrorToIMAP *bool   `json:"mirrorToImap"`
	Tab          *string `json:"tab"`
}

func (a *API) labelSet(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	var args emailSetArgs // shared {accountId, ifInState, create, update, destroy} shape
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

	// The whole set runs in ONE UserTx: oldState/newState bracket every mutation
	// and per-object failures still commit the successful ones. A stateMismatch
	// aborts the whole method (RFC 8620 §5.3).
	var stateErr *MethodError
	err := a.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		oldState, err := tx.ChangeState(ctx, store.ObjectLabel)
		if err != nil {
			return err
		}
		res.OldState = encodeState(oldState)
		if args.IfInState != nil && *args.IfInState != encodeState(oldState) {
			stateErr = &MethodError{Type: errStateMismatch}
			return errMethodRollback
		}

		// Track created ids so a later update/destroy in the same call can
		// reference them by creation id (JMAP #ref within a /set, RFC 8620 §5.3).
		created := map[string]ulid.ULID{}

		for cid, raw := range args.Create {
			obj, id, serr := createLabel(ctx, tx, raw)
			if serr != nil {
				res.NotCreated[cid] = *serr
				continue
			}
			created[cid] = id
			res.Created[cid] = obj
		}

		for id, patch := range args.Update {
			lid, ok := resolveSetID(id, created)
			if !ok {
				res.NotUpdated[id] = SetError{Type: setNotFound}
				continue
			}
			if serr := updateLabel(ctx, tx, lid, patch); serr != nil {
				res.NotUpdated[id] = *serr
				continue
			}
			res.Updated[id] = nil
		}

		for _, id := range args.Destroy {
			lid, ok := resolveSetID(id, created)
			if !ok {
				res.NotDestroyed[id] = SetError{Type: setNotFound}
				continue
			}
			found, derr := tx.DeleteLabel(ctx, lid)
			if derr != nil {
				return derr
			}
			if !found {
				res.NotDestroyed[id] = SetError{Type: setNotFound}
				continue
			}
			res.Destroyed = append(res.Destroyed, id)
		}

		newState, err := tx.ChangeState(ctx, store.ObjectLabel)
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

// resolveSetID resolves a Label/set update/destroy target id, which may be a
// real ULID or a "#creationId" reference to a label created earlier in the same
// call (RFC 8620 §5.3).
func resolveSetID(id string, created map[string]ulid.ULID) (ulid.ULID, bool) {
	if strings.HasPrefix(id, "#") {
		lid, ok := created[strings.TrimPrefix(id, "#")]
		return lid, ok
	}
	lid, err := ulid.Parse(id)
	if err != nil {
		return ulid.ULID{}, false
	}
	return lid, true
}

// createLabel parses a Label/set create patch and inserts the label, returning
// the created JMAP object (with the server-assigned id + keyword atom) on
// success. mirrorToImap defaults to true (spec §6).
func createLabel(ctx context.Context, tx *store.UserTx, raw json.RawMessage) (map[string]any, ulid.ULID, *SetError) {
	var c labelCreate
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, ulid.ULID{}, &SetError{Type: setInvalidProps, Description: err.Error()}
	}
	if c.Name == nil || *c.Name == "" {
		return nil, ulid.ULID{}, &SetError{Type: setInvalidProps,
			Description: "label name is required", Properties: []string{"name"}}
	}
	row := store.LabelRow{Name: *c.Name, MirrorToIMAP: true}
	if c.MirrorToIMAP != nil {
		row.MirrorToIMAP = *c.MirrorToIMAP
	}
	if c.Color != nil {
		row.Color = *c.Color
	}
	if c.ParentID != nil {
		pid, err := ulid.Parse(*c.ParentID)
		if err != nil {
			return nil, ulid.ULID{}, &SetError{Type: setInvalidProps,
				Description: "parentId is not a valid id", Properties: []string{"parentId"}}
		}
		row.ParentID = &pid
	}
	if c.Tab != nil && *c.Tab != "" {
		t, ok := tabs.Parse(*c.Tab)
		if !ok {
			return nil, ulid.ULID{}, &SetError{Type: setInvalidProps,
				Description: "tab is not a category tab", Properties: []string{"tab"}}
		}
		row.Tab = t
	}
	id, err := tx.CreateLabel(ctx, row)
	if err != nil {
		if errors.Is(err, store.ErrLabelNameTaken) {
			return nil, ulid.ULID{}, &SetError{Type: setInvalidProps,
				Description: "a label with that name already exists", Properties: []string{"name"}}
		}
		if errors.Is(err, store.ErrLabelTabInvalid) {
			return nil, ulid.ULID{}, &SetError{Type: setInvalidProps,
				Description: "tab is not a category tab", Properties: []string{"tab"}}
		}
		return nil, ulid.ULID{}, &SetError{Type: setForbidden, Description: err.Error()}
	}
	// Re-read to surface the server-set keyword atom in the created object.
	views, err := tx.GetLabels(ctx, []ulid.ULID{id})
	if err != nil || len(views) == 0 {
		// The row exists; fall back to the id-only server object.
		return map[string]any{"id": id.String()}, id, nil
	}
	return labelObject(views[0], nil), id, nil
}

// updateLabel parses a Label/set update patch and applies it. It accepts the
// top-level Label properties (name, color, parentId, mirrorToImap); the
// server-set `keyword` and `id` are rejected as immutable.
func updateLabel(ctx context.Context, tx *store.UserTx, id ulid.ULID, patch json.RawMessage) *SetError {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(patch, &raw); err != nil {
		return &SetError{Type: setInvalidPatch, Description: err.Error()}
	}
	var p store.LabelPatch
	for key, val := range raw {
		switch key {
		case "name":
			var s string
			if err := json.Unmarshal(val, &s); err != nil || s == "" {
				return &SetError{Type: setInvalidProps,
					Description: "name must be a non-empty string", Properties: []string{"name"}}
			}
			p.Name = &s
		case "color":
			p.SetColor = true
			if !isJSONNull(val) {
				if err := json.Unmarshal(val, &p.Color); err != nil {
					return &SetError{Type: setInvalidProps,
						Description: "color must be a string", Properties: []string{"color"}}
				}
			}
		case "parentId":
			p.SetParent = true
			if !isJSONNull(val) {
				var s string
				if err := json.Unmarshal(val, &s); err != nil {
					return &SetError{Type: setInvalidProps,
						Description: "parentId must be a string", Properties: []string{"parentId"}}
				}
				pid, err := ulid.Parse(s)
				if err != nil {
					return &SetError{Type: setInvalidProps,
						Description: "parentId is not a valid id", Properties: []string{"parentId"}}
				}
				p.ParentID = &pid
			}
		case "mirrorToImap":
			var b bool
			if err := json.Unmarshal(val, &b); err != nil {
				return &SetError{Type: setInvalidProps,
					Description: "mirrorToImap must be a boolean", Properties: []string{"mirrorToImap"}}
			}
			p.MirrorToIMAP = &b
		case "tab":
			// null (or "") unmaps the label, which is how the operator takes a label
			// back out of a tab — the same shape as clearing `color`.
			p.SetTab = true
			if !isJSONNull(val) {
				var s string
				if err := json.Unmarshal(val, &s); err != nil {
					return &SetError{Type: setInvalidProps,
						Description: "tab must be a string or null", Properties: []string{"tab"}}
				}
				if s != "" {
					t, ok := tabs.Parse(s)
					if !ok {
						return &SetError{Type: setInvalidProps,
							Description: "tab is not a category tab", Properties: []string{"tab"}}
					}
					p.Tab = t
				}
			}
		case "id", "keyword":
			return &SetError{Type: setInvalidProps,
				Description: key + " is immutable", Properties: []string{key}}
		default:
			return &SetError{Type: setInvalidProps,
				Description: "unsupported Label property in patch: " + key, Properties: []string{key}}
		}
	}
	found, err := tx.UpdateLabel(ctx, id, p)
	if err != nil {
		if errors.Is(err, store.ErrLabelNameTaken) {
			return &SetError{Type: setInvalidProps,
				Description: "a label with that name already exists", Properties: []string{"name"}}
		}
		if errors.Is(err, store.ErrLabelTabInvalid) {
			return &SetError{Type: setInvalidProps,
				Description: "tab is not a category tab", Properties: []string{"tab"}}
		}
		return &SetError{Type: setForbidden, Description: err.Error()}
	}
	if !found {
		return &SetError{Type: setNotFound}
	}
	return nil
}
