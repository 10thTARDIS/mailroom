// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"encoding/json"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// RemoteAllow/* — remembering "show remote content", per sender (#202, spec §11).
//
// Remote images are blocked by default and the affordance that loads them was
// per-message and non-persistent, because "I trusted this one newsletter once"
// must not become "this sender may track me forever". That holds for a BLANKET
// allow. What was missing was anywhere to record a NARROW one, leaving only "ask
// every single time" and "never ask" — and asking every time about the same
// newsletter is how a safe default becomes something users route around.
//
// WHAT THIS SURFACE DOES NOT DO, and the distinction matters: it does not decide
// whether images load. internal/api/sanitize.go still parks every remote URL in a
// data-attribute UNCONDITIONALLY — a sanitizer that branched on a preference would
// stop being the only path, and the whole hostile-content posture rests on it
// having no branches. These methods only read and write the LIST; the client
// consults it to decide what to un-park (docs/hostile-content.md §2.3).
//
// So the security properties are unchanged by design. What is new is an audit
// surface: RemoteAllow/get is what the settings list reads, and a grant that could
// not be listed and destroyed would be exactly the invisible permanent consent the
// original decision was protecting against.
//
// NO /query. The list is a handful of rows a client reads whole; a filterable query
// over "senders whose images I load" answers a question nobody asks.

type remoteAllowGetArgs struct {
	AccountID string   `json:"accountId"`
	IDs       []string `json:"ids"`
}

// remoteAllowObject is the JMAP representation. Deliberately three fields: there
// is nothing else to know about a grant, and anything more would invite storing
// policy here that belongs in the schema's CHECK.
func remoteAllowObject(a store.RemoteAllow) map[string]any {
	return map[string]any{
		"id":    a.ID.String(),
		"kind":  string(a.Kind),
		"value": a.Value,
	}
}

func (a *API) remoteAllowGet(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	var args remoteAllowGetArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	if mErr := resolveAccount(userID, args.AccountID); mErr != nil {
		return nil, mErr
	}

	var (
		state    int64
		all      []store.RemoteAllow
		notFound = []string{}
	)
	if err := a.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		if state, e = tx.ChangeState(ctx, store.ObjectRemoteAllow); e != nil {
			return e
		}
		all, e = tx.ListRemoteAllows(ctx)
		return e
	}); err != nil {
		return nil, &MethodError{Type: errServerFail, Description: err.Error()}
	}

	list := make([]map[string]any, 0, len(all))
	if args.IDs == nil {
		// The whole list — the ordinary call. A client wants all of them because it
		// has to match every sender it renders against every grant.
		for _, one := range all {
			list = append(list, remoteAllowObject(one))
		}
	} else {
		byID := make(map[string]store.RemoteAllow, len(all))
		for _, one := range all {
			byID[one.ID.String()] = one
		}
		for _, want := range args.IDs {
			if one, ok := byID[want]; ok {
				list = append(list, remoteAllowObject(one))
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

func (a *API) remoteAllowChanges(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	return a.handleChanges(ctx, userID, rawArgs, store.ObjectRemoteAllow)
}

// remoteAllowCreate is the create form: both fields required, no defaults.
//
// NO DEFAULT KIND, deliberately. Defaulting to `domain` would silently widen a
// grant, and defaulting to `address` would silently narrow one the user thought
// they made — either way the client would be able to omit the most consequential
// field in the request. It has to say which it means.
type remoteAllowCreate struct {
	Kind  *string `json:"kind"`
	Value *string `json:"value"`
}

func (a *API) remoteAllowSet(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
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
		oldState, err := tx.ChangeState(ctx, store.ObjectRemoteAllow)
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
			obj, id, serr := createRemoteAllow(ctx, tx, raw)
			if serr != nil {
				res.NotCreated[cid] = *serr
				continue
			}
			created[cid] = id
			res.Created[cid] = obj
		}

		// THERE IS NO UPDATE. A grant has no mutable part: changing its value or its
		// kind is revoking one decision and making a different one, and saying so
		// keeps the settings list honest about what was actually granted and when.
		for id := range args.Update {
			res.NotUpdated[id] = SetError{Type: setInvalidProps,
				Description: "a remote-content allow cannot be edited; destroy it and create the one you want"}
		}

		for _, id := range args.Destroy {
			rid, ok := resolveSetID(id, created)
			if !ok {
				res.NotDestroyed[id] = SetError{Type: setNotFound}
				continue
			}
			found, derr := tx.RemoveRemoteAllow(ctx, rid)
			if derr != nil {
				return derr
			}
			if !found {
				res.NotDestroyed[id] = SetError{Type: setNotFound}
				continue
			}
			res.Destroyed = append(res.Destroyed, id)
		}

		newState, err := tx.ChangeState(ctx, store.ObjectRemoteAllow)
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

// createRemoteAllow parses one create patch and records the grant.
//
// The store normalises and validates: a value with a wildcard, a space, a leading
// dot, or the wrong shape for its kind is refused there rather than stored, and
// comes back here as a clean invalidProperties SetError. A repeat of an existing
// grant is NOT an error — it is the same decision, so the existing row is returned
// and no change_log row is appended.
func createRemoteAllow(ctx context.Context, tx *store.UserTx, raw json.RawMessage) (map[string]any, ulid.ULID, *SetError) {
	var c remoteAllowCreate
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, ulid.ULID{}, &SetError{Type: setInvalidProps, Description: err.Error()}
	}
	if c.Kind == nil || *c.Kind == "" {
		return nil, ulid.ULID{}, &SetError{Type: setInvalidProps,
			Description: "kind is required: \"address\" or \"domain\"", Properties: []string{"kind"}}
	}
	if c.Value == nil || *c.Value == "" {
		return nil, ulid.ULID{}, &SetError{Type: setInvalidProps,
			Description: "value is required", Properties: []string{"value"}}
	}
	kind := store.RemoteAllowKind(*c.Kind)
	if kind != store.RemoteAllowAddress && kind != store.RemoteAllowDomain {
		return nil, ulid.ULID{}, &SetError{Type: setInvalidProps,
			Description: "kind must be \"address\" or \"domain\"", Properties: []string{"kind"}}
	}

	allow, err := tx.AddRemoteAllow(ctx, kind, *c.Value)
	if err != nil {
		return nil, ulid.ULID{}, &SetError{Type: setInvalidProps,
			Description: err.Error(), Properties: []string{"value"}}
	}
	return remoteAllowObject(allow), allow.ID, nil
}
