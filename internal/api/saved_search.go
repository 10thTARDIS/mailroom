// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// SavedSearch/* — named queries, and the sidebar's smart folders (#57, matrix §7).
//
// A saved search is a NAMED QUERY plus a boolean for whether it is pinned to the nav rail.
// The matrix lists "saved searches" and "smart folders" as two rows; they are one object
// and a flag (see migration 00024).
//
// THE QUERY IS THE OPERATOR STRING, verbatim, and this layer does not parse it. Parsing
// lives in internal/search and runs at Email/query time, so a saved search executes
// through EXACTLY the path typing it does — there is no second interpretation that could
// disagree with the first. A client runs one by putting its `query` into an Email/query
// filter, which is what the search box already does with typed text.
//
// So there is deliberately no `SavedSearch/query` method: it would be a second way to run
// a search, and the two could drift. One search path, one meaning.

type savedSearchGetArgs struct {
	AccountID string   `json:"accountId"`
	IDs       []string `json:"ids"`
}

func savedSearchObject(s store.SavedSearch) map[string]any {
	return map[string]any{
		"id":        s.ID.String(),
		"name":      s.Name,
		"query":     s.Query,
		"inSidebar": s.InSidebar,
		"position":  s.Position,
	}
}

func (a *API) savedSearchGet(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	var args savedSearchGetArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	if mErr := resolveAccount(userID, args.AccountID); mErr != nil {
		return nil, mErr
	}

	var (
		state    int64
		all      []store.SavedSearch
		notFound = []string{}
	)
	if err := a.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		if state, e = tx.ChangeState(ctx, store.ObjectSavedSearch); e != nil {
			return e
		}
		all, e = tx.ListSavedSearches(ctx)
		return e
	}); err != nil {
		return nil, &MethodError{Type: errServerFail, Description: err.Error()}
	}

	list := make([]map[string]any, 0, len(all))
	if args.IDs == nil {
		for _, s := range all {
			list = append(list, savedSearchObject(s))
		}
	} else {
		byID := make(map[string]store.SavedSearch, len(all))
		for _, s := range all {
			byID[s.ID.String()] = s
		}
		for _, want := range args.IDs {
			if s, ok := byID[want]; ok {
				list = append(list, savedSearchObject(s))
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

func (a *API) savedSearchChanges(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	return a.handleChanges(ctx, userID, rawArgs, store.ObjectSavedSearch)
}

// savedSearchWrite is the create/update form. A FULL OBJECT, not a patch — name, query and
// inSidebar together decide what the thing is and where it appears, so a partial write
// that changed one without the others is the #170 class of bug.
type savedSearchWrite struct {
	Name      *string `json:"name"`
	Query     *string `json:"query"`
	InSidebar *bool   `json:"inSidebar"`
	Position  *int    `json:"position"`
}

func (w savedSearchWrite) toStore() store.SavedSearch {
	s := store.SavedSearch{
		// Pinned by DEFAULT when a client does not say. Saving a search whose point is
		// to be a folder and then having to find a second control to show it would make
		// the common case two steps; an unpinned one is the deliberate variant.
		InSidebar: true,
	}
	if w.Name != nil {
		s.Name = *w.Name
	}
	if w.Query != nil {
		s.Query = *w.Query
	}
	if w.InSidebar != nil {
		s.InSidebar = *w.InSidebar
	}
	if w.Position != nil {
		s.Position = *w.Position
	}
	return s
}

func (a *API) savedSearchSet(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
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
		oldState, err := tx.ChangeState(ctx, store.ObjectSavedSearch)
		if err != nil {
			return err
		}
		res.OldState = encodeState(oldState)
		if args.IfInState != nil && *args.IfInState != encodeState(oldState) {
			stateErr = &MethodError{Type: errStateMismatch}
			return errMethodRollback
		}

		created := map[string]ulid.ULID{}
		for cid, raw := range args.Create {
			var w savedSearchWrite
			if e := json.Unmarshal(raw, &w); e != nil {
				res.NotCreated[cid] = SetError{Type: setInvalidProps, Description: e.Error()}
				continue
			}
			s := w.toStore()
			id, e := tx.CreateSavedSearch(ctx, s)
			if e != nil {
				res.NotCreated[cid] = savedSearchSetError(e)
				continue
			}
			s.ID = id
			created[cid] = id
			res.Created[cid] = savedSearchObject(s)
		}

		for id, raw := range args.Update {
			sid, ok := resolveSetID(id, created)
			if !ok {
				res.NotUpdated[id] = SetError{Type: setNotFound}
				continue
			}
			var w savedSearchWrite
			if e := json.Unmarshal(raw, &w); e != nil {
				res.NotUpdated[id] = SetError{Type: setInvalidProps, Description: e.Error()}
				continue
			}
			found, e := tx.UpdateSavedSearch(ctx, sid, w.toStore())
			if e != nil {
				res.NotUpdated[id] = savedSearchSetError(e)
				continue
			}
			if !found {
				res.NotUpdated[id] = SetError{Type: setNotFound}
				continue
			}
			res.Updated[id] = nil
		}

		for _, id := range args.Destroy {
			sid, ok := resolveSetID(id, created)
			if !ok {
				res.NotDestroyed[id] = SetError{Type: setNotFound}
				continue
			}
			found, e := tx.DeleteSavedSearch(ctx, sid)
			if e != nil {
				return e
			}
			if !found {
				res.NotDestroyed[id] = SetError{Type: setNotFound}
				continue
			}
			res.Destroyed = append(res.Destroyed, id)
		}

		newState, err := tx.ChangeState(ctx, store.ObjectSavedSearch)
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

// savedSearchSetError surfaces the store's validation as a per-object SetError.
//
// A taken name reports `invalidProperties` with `name` named, matching ruleSetError —
// RFC 8620 §5.3 defines no "already exists" SetError type, and the PROPERTY is what lets a
// client point at the field the user has to change. Consistency with the existing
// convention matters more here than a bespoke type would.
//
// The store owns the rules (a name, a query, the length bounds, uniqueness) so the API and
// any future non-JMAP caller cannot disagree about them.
func savedSearchSetError(err error) SetError {
	if errors.Is(err, store.ErrSavedSearchNameTaken) {
		return SetError{Type: setInvalidProps,
			Description: "a saved search with that name already exists",
			Properties:  []string{"name"}}
	}
	return SetError{Type: setInvalidProps, Description: err.Error(),
		Properties: []string{"name", "query"}}
}
