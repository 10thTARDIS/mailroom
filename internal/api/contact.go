// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// Contact method handlers for the `urn:mailroom:contacts` extension (spec §10a,
// #37). Contacts are AUTO-DERIVED from ingest, not hand-maintained (see
// store/contacts.go): the read surface exposes them so a client can drive
// recipient autocomplete and see who a message's alias resolves to.
//
// Contact/set (#59, #816) is deliberately NARROW: it updates the user's own
// state on an existing contact — isVip, isBlocked, blockDestination, isSaved,
// userName (the user's own name for them) and notes — and nothing else.
// Contacts are still created only by ingest (and by Contact/separate) and
// never destroyed by a client; the learned displayName or an address in a
// patch is refused by name. Contact/merge and Contact/separate (#816,
// ADR-041) are their own methods, in contact_merge.go.
//
// Registration: Contact/get, Contact/query, Contact/changes and Contact/set
// are wired into the dispatch map in service.go, gated on capContacts in
// capability.go, and pushed over EventSource via eventsource.go — the same
// four-corner wiring every extension type needs.

// contactObject renders a store.Contact as a JMAP Contact object, honoring a
// requested property subset (id is always present). addresses[] is a list of
// {address, reachedMeAt}; reachedMeAt is null when the correspondent has no
// known reply-from alias.
//
// isVip, isBlocked, blockDestination ("junk" | "trash") and blockedAt (#59) are
// the user's own markers (store/contact_flags.go). blockDestination is where
// NEW mail from a blocked contact goes; it is reported whether or not the
// contact is blocked (it is the choice a re-block would reuse).
//
// #816 (ADR-041) adds the saved-contact surface: isSaved/savedAt; name (the
// displayed name: userName when set, else displayName); userName (the user's
// own name for them, null when unset; ingest never writes it); notes;
// inboundCount/outboundCount and firstMailAt/lastMailAt over all the
// contact's addresses. The four statistics are NOT synced state (ADR-014,
// ADR-041 §3): ingest moves them without a change row, so a client that shows
// them re-reads the contact rather than waiting for /changes. addresses[].id
// is what Contact/separate names.
//
// keys[] is #448/#374's addition: one entry per app.contact_keys row this
// contact holds, {fingerprint, kind, source, fetchedAt} — built field-by-field
// here rather than by serialising a store.ContactKey, so the key MATERIAL
// (PublicKey) has no path onto the wire even if a future struct field is added
// carelessly. keys is built by the caller (contactGet) and passed in per
// contact, never fetched inside this function, so it stays a pure renderer.
func contactObject(c store.Contact, keys []store.ContactKey, props map[string]bool) map[string]any {
	addrs := make([]map[string]any, 0, len(c.Addresses))
	for _, a := range c.Addresses {
		var reached any
		if a.ReachedMeAt != nil && *a.ReachedMeAt != "" {
			reached = *a.ReachedMeAt
		}
		addrs = append(addrs, map[string]any{
			"id":          a.ID.String(),
			"address":     a.Address,
			"reachedMeAt": reached,
		})
	}
	keyObjs := make([]map[string]any, 0, len(keys))
	for _, k := range keys {
		keyObjs = append(keyObjs, map[string]any{
			"fingerprint": k.Fingerprint,
			"kind":        string(k.Kind),
			"source":      k.Source,
			"refused":     k.Refused,
			"fetchedAt":   jmapDate(k.FetchedAt),
		})
	}
	inbound, outbound := c.MailCounts()
	first, last := c.MailSpan()
	var userName any
	if c.UserDisplayName != nil && *c.UserDisplayName != "" {
		userName = *c.UserDisplayName
	}
	full := map[string]any{
		"id":               c.ID.String(),
		"displayName":      c.DisplayName,
		"name":             c.DisplayedName(),
		"userName":         userName,
		"notes":            c.Notes,
		"isSaved":          c.SavedAt != nil,
		"savedAt":          jmapDatePtr(c.SavedAt),
		"inboundCount":     inbound,
		"outboundCount":    outbound,
		"firstMailAt":      jmapDateOrZero(first),
		"lastMailAt":       jmapDateOrZero(last),
		"addresses":        addrs,
		"keys":             keyObjs,
		"isVip":            c.VIP,
		"isBlocked":        c.Blocked,
		"blockDestination": c.BlockDestination,
		"blockedAt":        jmapDatePtr(c.BlockedAt),
	}
	if props == nil {
		return full
	}
	out := map[string]any{"id": c.ID.String()}
	for k, v := range full {
		if props[k] {
			out[k] = v
		}
	}
	return out
}

// ---- Contact/get ----------------------------------------------------------

type contactGetArgs struct {
	AccountID  string    `json:"accountId"`
	IDs        *[]string `json:"ids"`
	Properties []string  `json:"properties"`
}

func (a *API) contactGet(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	var args contactGetArgs
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
		byID      = make(map[ulid.ULID]store.Contact)
		keysByID  map[ulid.ULID][]store.ContactKey
		requested []requestedID
		notFound  = []string{}
	)
	mErr := a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
		var list []store.Contact
		var err error
		if args.IDs == nil {
			// ids:null means "all contacts" (RFC 8620 §5.1).
			list, err = tx.ListContacts(ctx)
			if err != nil {
				return &MethodError{Type: errServerFail, Description: err.Error()}
			}
			for _, v := range list {
				requested = append(requested, requestedID{raw: v.ID.String(), id: v.ID, ok: true})
			}
		} else {
			var valid []ulid.ULID
			valid, notFound, requested = parseIDs(*args.IDs)
			list, err = tx.GetContacts(ctx, valid)
			if err != nil {
				return &MethodError{Type: errServerFail, Description: err.Error()}
			}
		}
		ids := make([]ulid.ULID, 0, len(list))
		for _, v := range list {
			byID[v.ID] = v
			ids = append(ids, v.ID)
		}
		// ONE query for the whole id list (#448/#374) — see ContactKeysForMany's
		// own comment for why a Contact/get batch must not fan out per contact.
		keysByID, err = tx.ContactKeysForMany(ctx, ids)
		if err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		state, err = tx.ChangeState(ctx, store.ObjectContact)
		if err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		return nil
	})
	if mErr != nil {
		return nil, mErr
	}

	out := make([]map[string]any, 0, len(requested))
	for _, req := range requested {
		if !req.ok {
			continue
		}
		cv, ok := byID[req.id]
		if !ok {
			notFound = append(notFound, req.raw)
			continue
		}
		out = append(out, contactObject(cv, keysByID[req.id], props))
	}
	return mustMarshal(getResult{
		AccountID: userID.String(),
		State:     encodeState(state),
		List:      out,
		NotFound:  notFound,
	}), nil
}

// ---- Contact/query --------------------------------------------------------

// contactQueryArgs is the autocomplete surface: filter.text is matched (case-
// insensitive substring) against address and display name; filter.hasKey
// (#448/#374) additionally requires (true) or forbids (false) at least one
// app.contact_keys row for the contact, and combines with text. limit bounds
// the page. This is a deliberately small subset of RFC 8620 §5.5 Foo/query —
// no sort (results come back most-corresponded-with first), no anchor
// windowing — because it exists to serve typeahead, not paged browsing.
//
// calculateTotal is accepted for RFC-shape parity with Email/query, but — like
// Email/query's own field — does not gate anything: `total` is always the real
// count of matching contacts (see the LimitSet note below), computed in the
// same query as the page, so there is no cost to always returning it.
//
// #59 adds three more: address (one exact address — the sender card's "who is
// this From"), isVip and isBlocked (Settings' two lists). All combine with AND.
//
// #816 adds the Contacts page's: filter.isSaved and filter.onlyCoRecipient;
// sort by name (case-insensitive) or lastMailAt (one comparator; isAscending
// defaults to true as RFC 8620 says, so the page sends false for "latest
// first"); position (negative counts from the end). With none of them the
// query is the autocomplete it always was, over ALL contacts, saved or not.
type contactQueryArgs struct {
	AccountID string `json:"accountId"`
	Filter    struct {
		Text            string `json:"text"`
		HasKey          *bool  `json:"hasKey"`
		Address         string `json:"address"`
		IsVIP           *bool  `json:"isVip"`
		IsBlocked       *bool  `json:"isBlocked"`
		IsSaved         *bool  `json:"isSaved"`
		OnlyCoRecipient *bool  `json:"onlyCoRecipient"`
	} `json:"filter"`
	Sort []struct {
		Property    string `json:"property"`
		IsAscending *bool  `json:"isAscending"`
	} `json:"sort"`
	Position       int  `json:"position"`
	Limit          *int `json:"limit"`
	CalculateTotal bool `json:"calculateTotal"`
}

// contactQueryResult is queryResult plus RFC 8620 §5.5's limit, present only
// when the server used a smaller limit than the one requested.
type contactQueryResult struct {
	queryResult
	Limit *int `json:"limit,omitempty"`
}

func (a *API) contactQuery(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	var args contactQueryArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	if mErr := resolveAccount(userID, args.AccountID); mErr != nil {
		return nil, mErr
	}
	limit := 0
	// LimitSet, not just limit != 0: an explicit limit: 0 is a legitimate RFC
	// 8620 §5.5 request for zero ids plus the total, and limit alone cannot
	// distinguish it from "no limit sent" — the #469 lesson, applied here (see
	// store.EmailQuery.LimitSet for the sibling query this mirrors).
	limitSet := args.Limit != nil
	if limitSet {
		if *args.Limit < 0 {
			return nil, &MethodError{Type: errInvalidArguments, Description: "limit must be a non-negative integer"}
		}
		limit = *args.Limit
	}
	// A key's query always has a limit, the one its admission charged for
	// (#850): never the autocomplete default by omission.
	keyCaller := principalFrom(ctx) != nil
	if keyCaller {
		limit, limitSet = keyQueryLimitFor("Contact/query", args.Limit), true
	}

	page := store.ContactPage{Position: args.Position, Limit: limit, LimitSet: limitSet}
	if len(args.Sort) > 1 {
		return nil, &MethodError{Type: errUnsupportedSort, Description: "Contact/query sorts by one property"}
	}
	if len(args.Sort) == 1 {
		switch args.Sort[0].Property {
		case store.ContactSortName, store.ContactSortLastMailAt:
			page.Sort = args.Sort[0].Property
		default:
			return nil, &MethodError{Type: errUnsupportedSort, Description: "Contact/query sorts by name or lastMailAt"}
		}
		page.Ascending = args.Sort[0].IsAscending == nil || *args.Sort[0].IsAscending
	}

	var (
		ids   []ulid.ULID
		total int
		pos   int
		state int64
	)
	mErr := a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
		var err error
		ids, total, pos, err = tx.QueryContactsPage(ctx, store.ContactQueryFilter{
			Text:            args.Filter.Text,
			HasKey:          args.Filter.HasKey,
			Address:         args.Filter.Address,
			IsVIP:           args.Filter.IsVIP,
			IsBlocked:       args.Filter.IsBlocked,
			IsSaved:         args.Filter.IsSaved,
			OnlyCoRecipient: args.Filter.OnlyCoRecipient,
		}, page)
		if err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		state, err = tx.ChangeState(ctx, store.ObjectContact)
		if err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		return nil
	})
	if mErr != nil {
		return nil, mErr
	}
	out := contactQueryResult{queryResult: queryResult{
		AccountID:           userID.String(),
		QueryState:          encodeState(state),
		CanCalculateChanges: false,
		Position:            pos,
		Total:               total,
		IDs:                 ulidStrings(ids),
	}}
	if limitSet && limit > store.MaxContactQueryLimit {
		used := store.MaxContactQueryLimit
		out.Limit = &used
	}
	if keyCaller {
		out.Limit = keyServerLimit("Contact/query", args.Limit)
	}
	return mustMarshal(out), nil
}

// ---- Contact/set ----------------------------------------------------------

// contactSet updates contact markers (#59) and, since #816, saved, the user's
// name and notes. Every update runs in the caller's
// UserTx; a changed contact appends exactly one Contact change_log row and a
// no-op none (store.SetContactMarkers). create and destroy are refused per id.
func (a *API) contactSet(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
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
		oldState, err := tx.ChangeState(ctx, store.ObjectContact)
		if err != nil {
			return err
		}
		res.OldState = encodeState(oldState)
		if args.IfInState != nil && *args.IfInState != encodeState(oldState) {
			stateErr = &MethodError{Type: errStateMismatch}
			return errMethodRollback
		}
		for cid := range args.Create {
			res.NotCreated[cid] = SetError{Type: setForbidden,
				Description: "contacts are created from your mail; add one to your contacts with isSaved"}
		}
		for _, id := range args.Destroy {
			res.NotDestroyed[id] = SetError{Type: setForbidden,
				Description: "contacts are created from your mail and cannot be destroyed"}
		}
		for id, raw := range args.Update {
			if serr := updateContactMarkers(ctx, tx, id, raw); serr != nil {
				res.NotUpdated[id] = *serr
				continue
			}
			res.Updated[id] = nil
		}
		newState, err := tx.ChangeState(ctx, store.ObjectContact)
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

func updateContactMarkers(ctx context.Context, tx *store.UserTx, id string, raw json.RawMessage) *SetError {
	cid, err := ulid.Parse(id)
	if err != nil {
		return &SetError{Type: setNotFound}
	}
	var patch map[string]json.RawMessage
	if err := json.Unmarshal(raw, &patch); err != nil {
		return &SetError{Type: setInvalidPatch, Description: err.Error()}
	}
	var p store.ContactMarkersPatch
	for key, val := range patch {
		// WHOSE MAIL IS BLOCKED, OR WAKES THE USER, IS A PERSON'S DECISION (#49
		// review), as a rule is (Rule/set is session-only). A block with
		// blockDestination "trash" sends every future arrival from that sender
		// to Trash at ingest and on to the purge: a delete by proxy, with no
		// mail:delete, no confirmation, no delete count and no anomaly. Unblock
		// undoes the user's own blocks, and VIP lets a sender through quiet
		// hours. A key may still save a contact, name it and keep notes.
		if principalFrom(ctx) != nil && (key == "isVip" || key == "isBlocked" || key == "blockDestination") {
			return &SetError{Type: setForbidden, Properties: []string{key},
				Description: key + " is set by a person, not an API key: it decides whose mail is blocked or wakes you"}
		}
		switch key {
		case "isVip", "isBlocked":
			var b bool
			if err := json.Unmarshal(val, &b); err != nil {
				return &SetError{Type: setInvalidProps, Description: key + " must be a boolean", Properties: []string{key}}
			}
			if key == "isVip" {
				p.VIP = &b
			} else {
				p.Blocked = &b
			}
		case "blockDestination":
			var s string
			if err := json.Unmarshal(val, &s); err != nil || (s != store.BlockToJunk && s != store.BlockToTrash) {
				return &SetError{Type: setInvalidProps,
					Description: "blockDestination must be junk or trash", Properties: []string{key}}
			}
			p.BlockDestination = &s
		case "isSaved":
			var b bool
			if err := json.Unmarshal(val, &b); err != nil {
				return &SetError{Type: setInvalidProps, Description: key + " must be a boolean", Properties: []string{key}}
			}
			p.Saved = &b
		case "userName":
			// A string, or null to clear. Cleaned in the store (trimmed, control
			// and directional characters removed, at most 200 characters).
			var sp *string
			if err := json.Unmarshal(val, &sp); err != nil {
				return &SetError{Type: setInvalidProps, Description: "userName must be a string or null", Properties: []string{key}}
			}
			p.UserNameSet, p.UserName = true, sp
		case "notes":
			var s string
			if err := json.Unmarshal(val, &s); err != nil {
				return &SetError{Type: setInvalidProps, Description: "notes must be a string", Properties: []string{key}}
			}
			p.Notes = &s
		default:
			return &SetError{Type: setInvalidProps,
				Description: "only isVip, isBlocked, blockDestination, isSaved, userName and notes can be set on a contact",
				Properties:  []string{key}}
		}
	}
	if _, err := tx.SetContactMarkers(ctx, cid, p); err != nil {
		switch {
		case errors.Is(err, store.ErrContactNotFound):
			return &SetError{Type: setNotFound}
		case errors.Is(err, store.ErrBlockDestinationInvalid):
			return &SetError{Type: setInvalidProps, Description: err.Error(), Properties: []string{"blockDestination"}}
		case errors.Is(err, store.ErrContactUserNameTooLong):
			return &SetError{Type: setInvalidProps, Description: err.Error(), Properties: []string{"userName"}}
		case errors.Is(err, store.ErrContactNotesTooLong):
			return &SetError{Type: setInvalidProps, Description: err.Error(), Properties: []string{"notes"}}
		case errors.Is(err, store.ErrContactSaveConflict):
			return &SetError{Type: setInvalidProps, Description: err.Error(), Properties: []string{"isVip", "isSaved"}}
		}
		return &SetError{Type: setForbidden, Description: err.Error()}
	}
	return nil
}

// ---- Contact/changes ------------------------------------------------------

func (a *API) contactChanges(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	return a.handleChanges(ctx, userID, rawArgs, store.ObjectContact)
}
