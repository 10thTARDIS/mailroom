// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// Mailbox method handlers (RFC 8621 §2). Folders are Mailboxes; get/query/
// changes are the 0.5 surface. Mailbox/set is intentionally minimal — creating,
// renaming, or destroying a mailbox (a label-as-mailbox) is out of scope for
// 0.5, so every create/update/destroy is refused with a SetError.

type mailboxGetArgs struct {
	AccountID  string    `json:"accountId"`
	IDs        *[]string `json:"ids"`
	Properties []string  `json:"properties"`
}

func (a *API) mailboxGet(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	var args mailboxGetArgs
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
		state int64
		byID  = make(map[ulid.ULID]store.MailboxView)
		// requested preserves order for the response + notFound bookkeeping.
		requested []requestedID
		notFound  = []string{}
	)
	mErr := a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
		var want []ulid.ULID
		if args.IDs == nil {
			// ids:null means "all mailboxes" (RFC 8620 §5.1).
			all, err := tx.ListMailboxIDs(ctx)
			if err != nil {
				return &MethodError{Type: errServerFail, Description: err.Error()}
			}
			want = all
			for _, id := range all {
				requested = append(requested, requestedID{raw: id.String(), id: id, ok: true})
			}
		} else {
			var valid []ulid.ULID
			valid, notFound, requested = parseIDs(*args.IDs)
			want = valid
		}
		views, err := tx.GetMailboxes(ctx, want)
		if err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		for _, v := range views {
			// An account-restricted API key sees only its accounts' mailboxes
			// (#49, ADR-042 §5); the rest are notFound.
			if !keyAllowsAccount(ctx, v.AccountID) {
				continue
			}
			byID[v.ID] = v
		}
		if keyAccounts(ctx) != nil && args.IDs == nil {
			kept := requested[:0]
			for _, r := range requested {
				if _, ok := byID[r.id]; ok {
					kept = append(kept, r)
				}
			}
			requested = kept
		}
		state, err = tx.ChangeState(ctx, store.ObjectMailbox)
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
		mv, ok := byID[req.id]
		if !ok {
			notFound = append(notFound, req.raw)
			continue
		}
		list = append(list, mailboxObject(mv, props))
	}
	return mustMarshal(getResult{
		AccountID: userID.String(),
		State:     encodeState(state),
		List:      list,
		NotFound:  notFound,
	}), nil
}

// mailboxObject renders a MailboxView as a JMAP Mailbox object. totalThreads /
// unreadThreads are approximated by the email counts in 0.5 (documented); a
// per-thread count is a later refinement.
func mailboxObject(mv store.MailboxView, props map[string]bool) map[string]any {
	var role any
	if mv.Role != "" {
		role = mv.Role
	}
	full := map[string]any{
		"id": mv.ID.String(),
		// accountId is a urn:mailroom:accounts extension property (#488), read-only:
		// the owning account. Folder NAMES collide across accounts — every account
		// has an INBOX, a Sent, an Archive — so a client cannot filter or group
		// mailboxes by account without this; it would otherwise have to guess from
		// name, which is exactly the unmeasured claim #488 was opened to avoid.
		"accountId":     mv.AccountID.String(),
		"name":          mv.Name,
		"parentId":      nil,
		"role":          role,
		"sortOrder":     mv.SortOrder,
		"totalEmails":   mv.TotalEmails,
		"unreadEmails":  mv.UnreadEmails,
		"totalThreads":  mv.TotalEmails,
		"unreadThreads": mv.UnreadEmails,
		"isSubscribed":  true,
		// syncScope is a urn:mailroom:accounts extension property (#62): "full" or
		// "off". It is here rather than merely in an operator tool because a client
		// MUST be able to tell a live folder from a frozen one — an "off" folder
		// keeps everything it already had and gains nothing, so drawing it like any
		// other folder would present a snapshot as current mail.
		"syncScope": mv.SyncScope,
		// feedsDesk is a urn:mailroom:desk extension property (#442/#446):
		// "folders feed the desk" — whether live mail in THIS mailbox counts
		// toward the desk's Surface/Drawer/Unscored split exactly like INBOX
		// does. On the object (not merely settable) so a client configuring
		// the desk can read the setting back rather than trusting its own
		// last write.
		"feedsDesk": mv.FeedsDesk,
		"myRights": map[string]any{
			"mayReadItems":   true,
			"mayAddItems":    true,
			"mayRemoveItems": true,
			"maySetSeen":     true,
			"maySetKeywords": true,
			"mayCreateChild": false,
			"mayRename":      false,
			"mayDelete":      false,
			"maySubmit":      false,
		},
	}
	if props == nil {
		return full
	}
	out := map[string]any{"id": mv.ID.String()}
	for k, v := range full {
		if props[k] {
			out[k] = v
		}
	}
	return out
}

func (a *API) mailboxQuery(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
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
		var err error
		ids, err = tx.ListMailboxIDs(ctx)
		if err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		if keyAccounts(ctx) != nil {
			views, verr := tx.GetMailboxes(ctx, ids)
			if verr != nil {
				return &MethodError{Type: errServerFail, Description: verr.Error()}
			}
			allowed := map[ulid.ULID]bool{}
			for _, v := range views {
				if keyAllowsAccount(ctx, v.AccountID) {
					allowed[v.ID] = true
				}
			}
			kept := ids[:0]
			for _, id := range ids {
				if allowed[id] {
					kept = append(kept, id)
				}
			}
			ids = kept
		}
		state, err = tx.ChangeState(ctx, store.ObjectMailbox)
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
	// A key's query is paged like every key query (#850): position and the
	// limit its admission charged for, the server's limit echoed.
	if principalFrom(ctx) != nil {
		if probe.Limit != nil && *probe.Limit < 0 {
			return nil, &MethodError{Type: errInvalidArguments, Description: "limit must be a non-negative integer"}
		}
		page, pos := keyQueryWindow("Mailbox/query", ids, probe.Position, probe.Limit)
		out.IDs, out.Position, out.Limit = ulidStrings(page), pos, keyServerLimit("Mailbox/query", probe.Limit)
	}
	return mustMarshal(out), nil
}

func (a *API) mailboxChanges(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	return a.handleChanges(ctx, userID, rawArgs, store.ObjectMailbox)
}

// mailboxSet accepts exactly TWO mutations: `syncScope` (#62) and `feedsDesk`
// (#442/#446). Creating, renaming and destroying mailboxes are still refused —
// those are IMAP operations with remote consequences and no story yet — so
// every other patched property comes back as an `invalidProperties` SetError
// naming itself.
//
// Deliberately narrow rather than a general Mailbox update path: a client that
// tries to rename a folder must be told no, not have the rename silently ignored
// while a `syncScope`/`feedsDesk` in the same patch succeeds.
func (a *API) mailboxSet(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	var args emailSetArgs // same {create, update, destroy, ifInState} shape
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
		oldState, err := tx.ChangeState(ctx, store.ObjectMailbox)
		if err != nil {
			return err
		}
		res.OldState = encodeState(oldState)
		if args.IfInState != nil && *args.IfInState != encodeState(oldState) {
			stateErr = &MethodError{Type: errStateMismatch}
			return errMethodRollback
		}

		for id, patch := range args.Update {
			if serr := updateMailboxPatch(ctx, tx, id, patch); serr != nil {
				res.NotUpdated[id] = *serr
				continue
			}
			// null: the server changed nothing beyond what was asked (RFC 8620 §5.3).
			res.Updated[id] = nil
		}

		// READ AGAIN. A scope change appends a Mailbox change row, so the state
		// moved; reporting the pre-update state as newState would make a client
		// re-fetch the same changes forever, or skip them.
		newState, err := tx.ChangeState(ctx, store.ObjectMailbox)
		if err != nil {
			return err
		}
		res.NewState = encodeState(newState)
		return nil
	})
	if stateErr != nil {
		return nil, stateErr
	}
	if err != nil {
		return nil, &MethodError{Type: errServerFail, Description: err.Error()}
	}

	for cid := range args.Create {
		res.NotCreated[cid] = SetError{Type: setNotImplemented,
			Description: "creating mailboxes is not supported in this milestone"}
	}
	for _, id := range args.Destroy {
		res.NotDestroyed[id] = SetError{Type: setNotImplemented,
			Description: "destroying mailboxes is not supported in this milestone"}
	}
	return mustMarshal(res), nil
}

// updateMailboxPatch applies a Mailbox patch, which may set `syncScope`
// and/or `feedsDesk` (#442/#446: "folders feed the desk") and nothing else.
//
// A patch mentioning any other property is REFUSED WHOLE — including the
// syncScope/feedsDesk in it. Applying the half we understand and dropping the
// rest would leave the client believing a rename landed.
func updateMailboxPatch(ctx context.Context, tx *store.UserTx, id string, raw json.RawMessage) *SetError {
	mid, err := ulid.Parse(id)
	if err != nil {
		return &SetError{Type: setNotFound}
	}
	var patch map[string]any
	if err := json.Unmarshal(raw, &patch); err != nil {
		return &SetError{Type: setInvalidProps, Description: err.Error()}
	}
	if len(patch) == 0 {
		return nil // a no-op patch is not an error (RFC 8620 §5.3)
	}

	var (
		scope     *string
		feedsDesk *bool
	)
	for k, v := range patch {
		switch k {
		case "syncScope":
			s, ok := v.(string)
			if !ok {
				return &SetError{Type: setInvalidProps,
					Description: "syncScope must be a string", Properties: []string{"syncScope"}}
			}
			scope = &s
		case "feedsDesk":
			b, ok := v.(bool)
			if !ok {
				return &SetError{Type: setInvalidProps,
					Description: "feedsDesk must be a boolean", Properties: []string{"feedsDesk"}}
			}
			feedsDesk = &b
		default:
			return &SetError{Type: setInvalidProps,
				Description: "only syncScope and feedsDesk may be changed on a mailbox; creating, " +
					"renaming and destroying mailboxes are not supported",
				Properties: []string{k}}
		}
	}

	if scope != nil {
		// The set is closed, and the value that is NOT here is the interesting
		// one: "headers" was advertised in the schema for a long time and
		// never implemented — the sync engine tests only for "off", so
		// "headers" produced a FULL sync, i.e. the opposite of its name. It is
		// refused rather than quietly accepted (migration 00026 also
		// constrains the column).
		if *scope != store.SyncScopeFull && *scope != store.SyncScopeOff {
			return &SetError{Type: setInvalidProps,
				Description: `syncScope must be "full" or "off"`,
				Properties:  []string{"syncScope"}}
		}
		if _, err := tx.SetMailboxSyncScope(ctx, mid, *scope); err != nil {
			if errors.Is(err, store.ErrMailboxNotFound) {
				return &SetError{Type: setNotFound}
			}
			return &SetError{Type: setForbidden, Description: err.Error()}
		}
	}
	if feedsDesk != nil {
		if _, err := tx.SetMailboxFeedsDesk(ctx, mid, *feedsDesk); err != nil {
			if errors.Is(err, store.ErrMailboxNotFound) {
				return &SetError{Type: setNotFound}
			}
			return &SetError{Type: setForbidden, Description: err.Error()}
		}
	}
	return nil
}
