// SPDX-License-Identifier: AGPL-3.0-only

package api

// ContactKey/get and ContactKey/set (#796, #795): the correspondents' keys a
// user holds, with what their packets say, and removing one. Gated on capPGP.
//
//	ContactKey/get     {accountId, contactIds?, includeRemoved?} -> {accountId, state, list, notFound}
//	ContactKey/set     {accountId, destroy: [id]}     -> {accountId, oldState, newState, destroyed, notDestroyed}
//	ContactKey/history {accountId, id}                -> {accountId, id, list: [{kind, source, at, expiresAt}]}
//
// NOT A SYNCED OBJECT OF ITS OWN. A correspondent key is part of the Contact
// (Contact.keys), and every change to the keys a contact lists is recorded as a
// Contact `updated` change_log row (ContactKey/import, /decide, a renewal merge,
// and now removal). So there is no ContactKey/changes: a client syncing
// Contact/changes already learns that a contact's keys moved, and re-asks
// ContactKey/get for that contact. `state` here is a hash of the rows and the
// clock-derived flags, for cache-busting only, like UserKey/get's.
//
// One deliberate gap in that rule predates this file and is kept: a brand-new
// key from ingest or WKD discovery writes no change_log row
// (store.UpsertContactKey). Contact.keys can therefore grow without a Contact
// change; a client that needs the full list asks ContactKey/get.
//
// NEVER KEY MATERIAL. Each entry is built field by field, like contactObject.
//
// READ-TIME FACTS (#795): expiresAt, expired, revoked, usable, expiringSoon and
// hasEncryptionKey are computed from the stored packets at the call.
// `createdAt` is the key's own creation time from its packets (when the key was
// MADE; null when unreadable), and `fetchedAt` is when Mailroom last saw it.
//
// ContactKey/set DESTROY removes the key and, when it was the encryption pin
// for any of the contact's addresses, the pin too, with ONE Contact `updated`
// change_log row (store.RemoveContactKey). Since #815 the removal is a SOFT
// delete: the row and its history stay, ContactKey/get lists it only with
// includeRemoved, and nothing ever encrypts to or verifies with it again unless
// the key is seen again (ADR-040 §11). create and update are refused:
// adding a key is ContactKey/import, trusting or refusing one is
// ContactKey/decide.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/pgpkeys"
	"mailroom/internal/store"
)

type contactKeyGetArgs struct {
	AccountID  string    `json:"accountId"`
	ContactIDs *[]string `json:"contactIds"`
	// IncludeRemoved also lists keys the user removed (#815), with removedAt
	// set: the history view. Default false, the keys the user holds.
	IncludeRemoved bool `json:"includeRemoved"`
}

// contactKeyObject renders one held correspondent key. pins is the contact's
// address → pinned fingerprint map.
func contactKeyObject(k store.ContactKey, pins map[string]string, now time.Time) map[string]any {
	var pinnedFor []string
	for addr, fpr := range pins {
		if fpr == k.Fingerprint {
			pinnedFor = append(pinnedFor, addr)
		}
	}
	sort.Strings(pinnedFor)
	if pinnedFor == nil {
		pinnedFor = []string{}
	}
	var (
		created, expiresAt             any
		expired, revoked, usable, soon bool
		hasEncryption                  bool
		addresses                      = []string{}
	)
	if v, err := pgpkeys.Assess(k.PublicKey, now); err == nil {
		created = jmapDate(v.Created)
		if until, never := v.UsableUntil(); !never {
			expiresAt = jmapDate(until)
		}
		expired, revoked, usable, soon = v.Expired(now), v.Revoked, v.Usable(now), v.ExpiringSoon(now)
		hasEncryption = v.HasEncryptionKey
	}
	if parsed, err := pgpkeys.Parse(k.PublicKey); err == nil && parsed.Addresses != nil {
		addresses = parsed.Addresses
		sort.Strings(addresses)
	}
	return map[string]any{
		"id":               k.ID.String(),
		"contactId":        k.ContactID.String(),
		"fingerprint":      k.Fingerprint,
		"kind":             string(k.Kind),
		"source":           k.Source,
		"authenticated":    k.Authenticated,
		"refused":          k.Refused,
		"pinned":           len(pinnedFor) > 0,
		"pinnedFor":        pinnedFor,
		"addresses":        addresses,
		"createdAt":        created,
		"fetchedAt":        jmapDate(k.FetchedAt),
		"expiresAt":        expiresAt,
		"expired":          expired,
		"revoked":          revoked,
		"usable":           usable,
		"expiringSoon":     soon,
		"hasEncryptionKey": hasEncryption,
		// #815: set only for a removed key (includeRemoved).
		"removedAt": jmapDateOrNil(k.RemovedAt),
		// #812: the key's self-signed user IDs (untrusted, cleaned text).
		"userIds": userIDsJSON(k.PublicKey, now),
	}
}

func (a *API) contactKeyGet(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	var args contactKeyGetArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	if mErr := resolveAccount(userID, args.AccountID); mErr != nil {
		return nil, mErr
	}
	var (
		keys     []store.ContactKey
		pins     map[ulid.ULID]map[string]string
		notFound = []string{}
	)
	mErr := a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
		var err error
		if args.ContactIDs == nil {
			all := tx.AllContactKeys
			if args.IncludeRemoved {
				all = tx.AllContactKeysIncludingRemoved
			}
			if keys, err = all(ctx); err != nil {
				return &MethodError{Type: errServerFail, Description: err.Error()}
			}
			if pins, err = tx.ContactKeyPinsFor(ctx, nil); err != nil {
				return &MethodError{Type: errServerFail, Description: err.Error()}
			}
			return nil
		}
		valid, _, _ := parseIDs(*args.ContactIDs)
		for _, raw := range *args.ContactIDs {
			if _, err := ulid.Parse(raw); err != nil {
				notFound = append(notFound, raw)
			}
		}
		contacts, err := tx.GetContacts(ctx, valid)
		if err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		exists := map[ulid.ULID]bool{}
		for _, c := range contacts {
			exists[c.ID] = true
		}
		var ids []ulid.ULID
		for _, id := range valid {
			if exists[id] {
				ids = append(ids, id)
			} else {
				notFound = append(notFound, id.String())
			}
		}
		many := tx.ContactKeysForMany
		if args.IncludeRemoved {
			many = tx.ContactKeysForManyIncludingRemoved
		}
		byContact, err := many(ctx, ids)
		if err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		for _, id := range ids {
			keys = append(keys, byContact[id]...)
		}
		if pins, err = tx.ContactKeyPinsFor(ctx, ids); err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		return nil
	})
	if mErr != nil {
		return nil, mErr
	}
	now := time.Now().UTC()
	list := make([]map[string]any, 0, len(keys))
	for _, k := range keys {
		list = append(list, contactKeyObject(k, pins[k.ContactID], now))
	}
	return mustMarshal(getResult{
		AccountID: userID.String(),
		State:     contactKeyState(list),
		List:      list,
		NotFound:  notFound,
	}), nil
}

// contactKeyState hashes the rendered entries (which carry every stored fact
// and the clock-derived flags, but no key material), so any change to what a
// client would be shown moves it.
func contactKeyState(list []map[string]any) string {
	b, _ := json.Marshal(list)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:16]
}

// ---- ContactKey/set ---------------------------------------------------------

type contactKeySetArgs struct {
	AccountID string                     `json:"accountId"`
	Create    map[string]json.RawMessage `json:"create"`
	Update    map[string]json.RawMessage `json:"update"`
	Destroy   []string                   `json:"destroy"`
}

func (a *API) contactKeySet(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	var args contactKeySetArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	if mErr := resolveAccount(userID, args.AccountID); mErr != nil {
		return nil, mErr
	}
	res := setResult{AccountID: userID.String()}
	if len(args.Create) > 0 {
		res.NotCreated = map[string]SetError{}
		for cid := range args.Create {
			res.NotCreated[cid] = SetError{Type: setForbidden, Description: "add a correspondent's key with ContactKey/import"}
		}
	}
	if len(args.Update) > 0 {
		res.NotUpdated = map[string]SetError{}
		for id := range args.Update {
			res.NotUpdated[id] = SetError{Type: setForbidden, Description: "trust or refuse a key with ContactKey/decide"}
		}
	}
	// The state is the hash of what ContactKey/get with no ids would render.
	render := func(tx *store.UserTx) (string, error) {
		keys, err := tx.AllContactKeys(ctx)
		if err != nil {
			return "", err
		}
		pins, err := tx.ContactKeyPinsFor(ctx, nil)
		if err != nil {
			return "", err
		}
		now := time.Now().UTC()
		list := make([]map[string]any, 0, len(keys))
		for _, k := range keys {
			list = append(list, contactKeyObject(k, pins[k.ContactID], now))
		}
		return contactKeyState(list), nil
	}
	mErr := a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
		var err error
		if res.OldState, err = render(tx); err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		for _, raw := range args.Destroy {
			id, perr := ulid.Parse(raw)
			if perr != nil {
				addNotDestroyed(&res, raw, SetError{Type: setNotFound, Description: "no such key"})
				continue
			}
			r, err := tx.RemoveContactKey(ctx, id)
			if err != nil {
				return &MethodError{Type: errServerFail, Description: err.Error()}
			}
			if !r.Found {
				addNotDestroyed(&res, raw, SetError{Type: setNotFound, Description: "no such key"})
				continue
			}
			res.Destroyed = append(res.Destroyed, raw)
		}
		if res.NewState, err = render(tx); err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		return nil
	})
	if mErr != nil {
		return nil, mErr
	}
	return mustMarshal(res), nil
}

func addNotDestroyed(res *setResult, id string, e SetError) {
	if res.NotDestroyed == nil {
		res.NotDestroyed = map[string]SetError{}
	}
	res.NotDestroyed[id] = e
}

// ---- ContactKey/history -----------------------------------------------------

type contactKeyHistoryArgs struct {
	AccountID string `json:"accountId"`
	ID        string `json:"id"`
}

// contactKeyHistory reads one held key's events (#815), oldest first: what
// happened to it on this instance (first_seen, renewed, updated, revoked_seen,
// pinned, refused, removed), by which route, when, and what its end of
// usability was afterwards. A removed key's history is readable: that is what
// the history is for. Read-only.
func (a *API) contactKeyHistory(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	var args contactKeyHistoryArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	if mErr := resolveAccount(userID, args.AccountID); mErr != nil {
		return nil, mErr
	}
	keyID, mErr := parseKeyID(args.ID)
	if mErr != nil {
		return nil, mErr
	}
	var events []store.ContactKeyEvent
	if mErr := a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
		ev, found, err := tx.ContactKeyHistory(ctx, keyID)
		if err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		if !found {
			return &MethodError{Type: errNotFound, Description: "no such key"}
		}
		events = ev
		return nil
	}); mErr != nil {
		return nil, mErr
	}
	list := make([]map[string]any, 0, len(events))
	for _, e := range events {
		list = append(list, map[string]any{
			"kind":      e.Kind,
			"source":    e.Source,
			"at":        jmapDate(e.At),
			"expiresAt": jmapDateOrNil(e.ExpiresAt),
		})
	}
	return mustMarshal(map[string]any{
		"accountId": userID.String(),
		"id":        keyID.String(),
		"list":      list,
	}), nil
}
