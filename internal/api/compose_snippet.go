// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// ComposeSnippet/* — saved signatures and templates (#56, spec §5).
//
// One object for both, because they are the same thing wearing two hats: a named
// piece of body text the user wrote once and reuses. See internal/store's
// compose_snippets.go for why that is one table rather than two.
//
// identities.signature_html is untouched and still read by Identity.htmlSignature.
// It is the identity's default when this object has nothing for that identity, so a
// user who never creates a second signature sees no change at all.

type composeSnippetGetArgs struct {
	AccountID string   `json:"accountId"`
	IDs       []string `json:"ids"`
}

func composeSnippetObject(s store.ComposeSnippet) map[string]any {
	var identity any
	if s.IdentityID != nil {
		identity = s.IdentityID.String()
	}
	return map[string]any{
		"id":         s.ID.String(),
		"kind":       string(s.Kind),
		"identityId": identity,
		"name":       s.Name,
		"bodyHtml":   s.BodyHTML,
		"bodyText":   s.BodyText,
		"isDefault":  s.IsDefault,
	}
}

func (a *API) composeSnippetGet(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	var args composeSnippetGetArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	if mErr := resolveAccount(userID, args.AccountID); mErr != nil {
		return nil, mErr
	}

	var (
		state    int64
		all      []store.ComposeSnippet
		notFound = []string{}
	)
	if err := a.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		if state, e = tx.ChangeState(ctx, store.ObjectComposeSnippet); e != nil {
			return e
		}
		all, e = tx.ListComposeSnippets(ctx)
		return e
	}); err != nil {
		return nil, &MethodError{Type: errServerFail, Description: err.Error()}
	}

	list := make([]map[string]any, 0, len(all))
	if args.IDs == nil {
		for _, s := range all {
			list = append(list, composeSnippetObject(s))
		}
	} else {
		byID := make(map[string]store.ComposeSnippet, len(all))
		for _, s := range all {
			byID[s.ID.String()] = s
		}
		for _, want := range args.IDs {
			if s, ok := byID[want]; ok {
				list = append(list, composeSnippetObject(s))
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

func (a *API) composeSnippetChanges(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	return a.handleChanges(ctx, userID, rawArgs, store.ObjectComposeSnippet)
}

// composeSnippetWrite is the create/update form. A FULL OBJECT, not a patch — see
// the note on store.UpdateComposeSnippet: kind and identityId together decide which
// picker offers a snippet, and letting one change without the other is the class of
// bug #170 hit when a partial save wiped a field it never meant to touch.
type composeSnippetWrite struct {
	Kind       *string `json:"kind"`
	IdentityID *string `json:"identityId"`
	Name       *string `json:"name"`
	BodyHTML   *string `json:"bodyHtml"`
	BodyText   *string `json:"bodyText"`
	IsDefault  *bool   `json:"isDefault"`
}

func (w composeSnippetWrite) toStore() (store.ComposeSnippet, *SetError) {
	s := store.ComposeSnippet{}
	if w.Kind == nil {
		return s, &SetError{Type: setInvalidProps,
			Description: `kind is required: "signature" or "template"`, Properties: []string{"kind"}}
	}
	s.Kind = store.SnippetKind(*w.Kind)
	if w.Name != nil {
		s.Name = *w.Name
	}
	if w.BodyHTML != nil {
		s.BodyHTML = *w.BodyHTML
	}
	if w.BodyText != nil {
		s.BodyText = *w.BodyText
	}
	if w.IsDefault != nil {
		s.IsDefault = *w.IsDefault
	}
	// An empty-string identityId means "any identity", the same as omitting it — a
	// client clearing a select box should not have to know the difference.
	if w.IdentityID != nil && *w.IdentityID != "" {
		iid, err := ulid.Parse(*w.IdentityID)
		if err != nil {
			return s, &SetError{Type: setInvalidProps,
				Description: "identityId must be a valid id", Properties: []string{"identityId"}}
		}
		s.IdentityID = &iid
	}
	return s, nil
}

func (a *API) composeSnippetSet(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
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
		oldState, err := tx.ChangeState(ctx, store.ObjectComposeSnippet)
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
			var w composeSnippetWrite
			if e := json.Unmarshal(raw, &w); e != nil {
				res.NotCreated[cid] = SetError{Type: setInvalidProps, Description: e.Error()}
				continue
			}
			snip, serr := w.toStore()
			if serr != nil {
				res.NotCreated[cid] = *serr
				continue
			}
			id, e := tx.CreateComposeSnippet(ctx, snip)
			if e != nil {
				res.NotCreated[cid] = snippetSetError(e)
				continue
			}
			snip.ID = id
			created[cid] = id
			res.Created[cid] = composeSnippetObject(snip)
		}

		for id, raw := range args.Update {
			sid, ok := resolveSetID(id, created)
			if !ok {
				res.NotUpdated[id] = SetError{Type: setNotFound}
				continue
			}
			var w composeSnippetWrite
			if e := json.Unmarshal(raw, &w); e != nil {
				res.NotUpdated[id] = SetError{Type: setInvalidProps, Description: e.Error()}
				continue
			}
			snip, serr := w.toStore()
			if serr != nil {
				res.NotUpdated[id] = *serr
				continue
			}
			found, e := tx.UpdateComposeSnippet(ctx, sid, snip)
			if e != nil {
				res.NotUpdated[id] = snippetSetError(e)
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
			found, e := tx.DeleteComposeSnippet(ctx, sid)
			if e != nil {
				return e
			}
			if !found {
				res.NotDestroyed[id] = SetError{Type: setNotFound}
				continue
			}
			res.Destroyed = append(res.Destroyed, id)
		}

		newState, err := tx.ChangeState(ctx, store.ObjectComposeSnippet)
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

// snippetSetError surfaces the store's validation as a clean per-object SetError.
// The store owns the rules (a name, a body, a template with no identity, the length
// bounds) so that the API and a future non-JMAP caller cannot disagree about them.
func snippetSetError(err error) SetError {
	// A taken name names `name`, so a client can point at the field to change — the same
	// convention ruleSetError and savedSearchSetError use.
	if errors.Is(err, store.ErrSnippetNameTaken) {
		return SetError{Type: setInvalidProps,
			Description: "a signature or template with that name already exists",
			Properties:  []string{"name"}}
	}
	return SetError{Type: setInvalidProps, Description: err.Error()}
}
