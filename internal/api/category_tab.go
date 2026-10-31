// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// CategoryTab/* — the Gmail-style category tabs (#219, spec §8).
//
// A tab is a FUNCTION category: what a message DOES, decided from RFC-defined
// headers and public sending infrastructure. That is a different axis from
// labels, which are TOPIC and come from identity and rules. Both work; asking
// one mechanism to serve both is why #156 stalled. See internal/tabs.
//
// THIS OBJECT IS THE USER'S ARRANGEMENT OF THE STRIP, nothing more: which tabs
// are switched on and what order they sit in. The other half — WHICH TAB EACH
// LABEL FEEDS — is the `tab` property on Label (label.go), because a label's tab
// is a property of that label, like its colour.
//
// NO CREATE, NO DESTROY. The vocabulary is closed (five tabs, the app.category_tab
// enum), and a tab is only useful if something can decide it — the decider is
// code, so a sixth tab is a release either way. A client that wants a tab gone
// disables it; the rows themselves are materialised by the server on first read
// and are not the client's to remove.
//
// NO /query either. Five rows the client reads whole; a filterable query over
// them would be answering a question nobody asks (the same reasoning as
// RemoteAllow and ComposeSnippet).

func categoryTabObject(t store.CategoryTabView) map[string]any {
	return map[string]any{
		"id":       t.ID.String(),
		"tab":      string(t.Tab),
		"enabled":  t.Enabled,
		"position": t.Position,
	}
}

type categoryTabGetArgs struct {
	AccountID string   `json:"accountId"`
	IDs       []string `json:"ids"`
}

func (a *API) categoryTabGet(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	var args categoryTabGetArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	if mErr := resolveAccount(userID, args.AccountID); mErr != nil {
		return nil, mErr
	}

	var (
		state    int64
		all      []store.CategoryTabView
		notFound = []string{}
	)
	if err := a.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		// ORDER MATTERS: the list read materialises the rows on a first-ever call
		// and appends their `created` change_log rows, so the state must be read
		// AFTER it. Reading state first would hand the client a state string from
		// before the five creates it is about to be shown — i.e. a cursor that
		// replays them on the next /changes.
		var e error
		if all, e = tx.ListCategoryTabs(ctx); e != nil {
			return e
		}
		state, e = tx.ChangeState(ctx, store.ObjectCategoryTab)
		return e
	}); err != nil {
		return nil, &MethodError{Type: errServerFail, Description: err.Error()}
	}

	list := make([]map[string]any, 0, len(all))
	if args.IDs == nil {
		for _, t := range all {
			list = append(list, categoryTabObject(t))
		}
	} else {
		byID := make(map[string]store.CategoryTabView, len(all))
		for _, t := range all {
			byID[t.ID.String()] = t
		}
		for _, want := range args.IDs {
			if t, ok := byID[want]; ok {
				list = append(list, categoryTabObject(t))
				continue
			}
			notFound = append(notFound, want)
		}
	}

	return mustMarshal(map[string]any{
		"accountId": userID.String(),
		"state":     encodeState(state),
		"list":      list,
		"notFound":  notFound,
	}), nil
}

func (a *API) categoryTabChanges(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	return a.handleChanges(ctx, userID, rawArgs, store.ObjectCategoryTab)
}

// categoryTabUpdate is the patch form. `tab` and `id` are immutable: a row's tab
// IS its identity, so repointing it would silently change what a client had
// already synced under that id.
type categoryTabUpdate struct {
	Enabled  *bool `json:"enabled"`
	Position *int  `json:"position"`
}

// parseCategoryTabPatch reads a CategoryTab/set update patch, rejecting anything
// outside {enabled, position}. An unknown property is refused rather than
// ignored: a client that thinks it moved a tab and silently did not is worse off
// than one told its patch was not understood.
func parseCategoryTabPatch(patch map[string]json.RawMessage) (categoryTabUpdate, *SetError) {
	var u categoryTabUpdate
	for key, val := range patch {
		switch key {
		case "enabled":
			var b bool
			if err := json.Unmarshal(val, &b); err != nil {
				return u, &SetError{Type: setInvalidProps,
					Description: "enabled must be a boolean", Properties: []string{"enabled"}}
			}
			u.Enabled = &b
		case "position":
			var n int
			if err := json.Unmarshal(val, &n); err != nil {
				return u, &SetError{Type: setInvalidProps,
					Description: "position must be an integer", Properties: []string{"position"}}
			}
			u.Position = &n
		case "id", "tab":
			return u, &SetError{Type: setInvalidProps,
				Description: key + " is immutable", Properties: []string{key}}
		default:
			return u, &SetError{Type: setInvalidProps,
				Description: "unsupported CategoryTab property in patch: " + key,
				Properties:  []string{key}}
		}
	}
	return u, nil
}

func (a *API) categoryTabSet(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
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
		// Materialise before reading oldState, for the reason categoryTabGet
		// explains: the creates must be inside the bracket, not before it.
		if _, err := tx.ListCategoryTabs(ctx); err != nil {
			return err
		}
		oldState, err := tx.ChangeState(ctx, store.ObjectCategoryTab)
		if err != nil {
			return err
		}
		res.OldState = encodeState(oldState)
		if args.IfInState != nil && *args.IfInState != encodeState(oldState) {
			stateErr = &MethodError{Type: errStateMismatch}
			return errMethodRollback
		}

		// The five rows are server-owned: a client neither creates nor destroys
		// them. Reported per object rather than failing the whole method, so a
		// batch that also contains valid updates still applies them.
		for cid := range args.Create {
			res.NotCreated[cid] = SetError{Type: setForbidden,
				Description: "the category tabs are a fixed set; enable or disable them instead of creating one"}
		}
		for _, id := range args.Destroy {
			res.NotDestroyed[id] = SetError{Type: setForbidden,
				Description: "the category tabs are a fixed set; disable a tab instead of destroying it"}
		}

		for id, raw := range args.Update {
			tid, err := ulid.Parse(id)
			if err != nil {
				res.NotUpdated[id] = SetError{Type: setNotFound}
				continue
			}
			var patch map[string]json.RawMessage
			if err := json.Unmarshal(raw, &patch); err != nil {
				res.NotUpdated[id] = SetError{Type: setInvalidPatch, Description: err.Error()}
				continue
			}
			u, serr := parseCategoryTabPatch(patch)
			if serr != nil {
				res.NotUpdated[id] = *serr
				continue
			}
			found, err := tx.UpdateCategoryTab(ctx, tid,
				store.CategoryTabPatch{Enabled: u.Enabled, Position: u.Position})
			if err != nil {
				if errors.Is(err, store.ErrPrimaryTabRequired) {
					res.NotUpdated[id] = SetError{Type: setInvalidProps,
						Description: "the Primary tab cannot be disabled: mail from a disabled tab appears there",
						Properties:  []string{"enabled"}}
					continue
				}
				return err
			}
			if !found {
				res.NotUpdated[id] = SetError{Type: setNotFound}
				continue
			}
			res.Updated[id] = nil
		}

		newState, err := tx.ChangeState(ctx, store.ObjectCategoryTab)
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
