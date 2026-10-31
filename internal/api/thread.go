// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"encoding/json"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// Thread method handlers (RFC 8621 §3). A Thread is an ordered list of its Email
// ids; get/changes are the 0.5 surface.

type threadGetArgs struct {
	AccountID string    `json:"accountId"`
	IDs       *[]string `json:"ids"`
}

func (a *API) threadGet(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	var args threadGetArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	if mErr := resolveAccount(userID, args.AccountID); mErr != nil {
		return nil, mErr
	}
	if args.IDs == nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: "Thread/get requires ids"}
	}

	valid, notFound, requested := parseIDs(*args.IDs)
	var (
		state int64
		byID  = make(map[ulid.ULID]store.ThreadView)
	)
	mErr := a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
		views, err := tx.GetThreads(ctx, valid)
		if err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		for _, v := range views {
			byID[v.ID] = v
		}
		state, err = tx.ChangeState(ctx, store.ObjectThread)
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
		tv, ok := byID[req.id]
		if !ok {
			notFound = append(notFound, req.raw)
			continue
		}
		list = append(list, map[string]any{
			"id":       tv.ID.String(),
			"emailIds": ulidStrings(tv.EmailIDs),
			// Mailroom extension (urn:mailroom:organize, spec §6): a muted thread
			// is one the user has told us to stop surfacing. Reported on every
			// Thread/get — the object has only two RFC properties, so there is no
			// property-selection machinery here to gate it behind, and a boolean
			// costs nothing to a client that ignores it.
			"isMuted": tv.Muted,
		})
	}
	return mustMarshal(getResult{
		AccountID: userID.String(),
		State:     encodeState(state),
		List:      list,
		NotFound:  notFound,
	}), nil
}

func (a *API) threadChanges(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	return a.handleChanges(ctx, userID, rawArgs, store.ObjectThread)
}

// threadSet is the Mailroom extension that makes a thread's mute settable
// (urn:mailroom:organize, spec §6 / #51). RFC 8621 defines no Thread/set, which
// is exactly why this is gated by capOrganize rather than the mail capability
// (capability.go): a mail-only client keeps precisely the RFC's Thread surface.
//
// UPDATE ONLY. Threads are derived objects — membership lives on
// messages.thread_id and is recomputable (spec §4) — so `create` and `destroy`
// are meaningless here and are rejected rather than silently ignored. A client
// that asks for something we will not do deserves to be told.
//
// The only settable property is isMuted. Anything else in the patch is a
// SetError on that id rather than a whole-method failure, which is what RFC 8620
// §5.3 prescribes: per-object errors belong in notUpdated.
func (a *API) threadSet(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	var args emailSetArgs // the shared {create, update, destroy, ifInState} shape
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
	for cid := range args.Create {
		res.NotCreated[cid] = SetError{Type: setForbidden,
			Description: "threads are derived from message membership and cannot be created"}
	}
	for _, id := range args.Destroy {
		res.NotDestroyed[id] = SetError{Type: setForbidden,
			Description: "threads are derived from message membership and cannot be destroyed"}
	}

	mErr := a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
		state, err := tx.ChangeState(ctx, store.ObjectThread)
		if err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		res.OldState = encodeState(state)

		for rawID, patch := range args.Update {
			tid, perr := ulid.Parse(rawID)
			if perr != nil {
				res.NotUpdated[rawID] = SetError{Type: setNotFound, Description: "not a valid id"}
				continue
			}
			muted, serr := parseThreadPatch(patch)
			if serr != nil {
				res.NotUpdated[rawID] = *serr
				continue
			}
			// A thread that does not exist — or belongs to another user, which RLS
			// makes indistinguishable — is notFound, not a silent success.
			ok, err := tx.SetThreadMuted(ctx, tid, muted)
			if err != nil {
				return &MethodError{Type: errServerFail, Description: err.Error()}
			}
			if !ok {
				// Either the thread is absent, or the value already matched. Tell
				// the two apart so a redundant set is reported as the success it is
				// rather than as a bogus notFound.
				views, gerr := tx.GetThreads(ctx, []ulid.ULID{tid})
				if gerr != nil {
					return &MethodError{Type: errServerFail, Description: gerr.Error()}
				}
				if len(views) == 0 {
					res.NotUpdated[rawID] = SetError{Type: setNotFound, Description: "thread not found"}
					continue
				}
			}
			res.Updated[rawID] = nil
		}

		newState, err := tx.ChangeState(ctx, store.ObjectThread)
		if err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		res.NewState = encodeState(newState)
		return nil
	})
	if mErr != nil {
		return nil, mErr
	}
	return mustMarshal(res), nil
}

// parseThreadPatch reads the one property Thread/set accepts. Both the plain
// ("isMuted") and JMAP patch ("isMuted/") spellings are unnecessary here — the
// property is a scalar boolean, not a nested map — so only the plain form is
// recognized, and anything else is reported rather than ignored.
func parseThreadPatch(patch json.RawMessage) (bool, *SetError) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(patch, &raw); err != nil {
		return false, &SetError{Type: setInvalidPatch, Description: err.Error()}
	}
	val, ok := raw["isMuted"]
	if !ok || len(raw) != 1 {
		for key := range raw {
			if key != "isMuted" {
				return false, &SetError{Type: setInvalidProps,
					Description: "unsupported Thread property in patch: " + key,
					Properties:  []string{key}}
			}
		}
		return false, &SetError{Type: setInvalidPatch, Description: "Thread/set requires isMuted"}
	}
	var muted bool
	if err := json.Unmarshal(val, &muted); err != nil {
		return false, &SetError{Type: setInvalidProps,
			Description: "isMuted must be a boolean", Properties: []string{"isMuted"}}
	}
	return muted, nil
}
