// SPDX-License-Identifier: AGPL-3.0-only

package api

// Contact/merge and Contact/separate (#816, ADR-041 §4): the user says two
// contacts are one person, or that a merged contact's addresses are not.
//
//	Contact/merge {accountId, into, from, ifInState?}
//	  -> {accountId, oldState, newState, id (= into), destroyedId (= from),
//	      addressesMoved, keysMoved, keysCombined}
//
//	Contact/separate {accountId, id, addressIds: [addressId], ifInState?}
//	  -> {accountId, oldState, newState, id, created: [contactId],
//	      addressContacts: {addressId: contactId}, keysMoved,
//	      restored, keptFields: [property]}
//
// restored (#834): the separate took back every address of each merge the
// named addresses came in by, so those merges were undone on `id`: each field
// the merge wrote is back to its pre-merge value. keptFields names the fields
// a merge wrote that were changed after it (displayName, userName, notes,
// isVip, isSaved, isBlocked) and were therefore kept. A partial separate
// restores nothing on `id` and says restored: false, keptFields: [].
//
// Errors (method-level): invalidArguments (a malformed or missing id, into ==
// from, no addressIds, every address named, or an address that is not one of
// the contact's); notFound (a contact that does not exist, was merged away, or
// is another user's: RLS makes the three one case); stateMismatch
// (ifInState); notesTooLong (merge only: the joined notes would pass 4,000
// characters, and the merge is refused rather than cutting the user's text).
//
// Change rows (store, one savepoint each): merge writes Contact `updated` for
// into and `destroyed` for from; separate writes `created` for each new
// contact and `updated` for the one it left (and for another live contact a
// nested merge's undo restored, when it changed).

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

const errNotesTooLong = "notesTooLong"

type contactMergeArgs struct {
	AccountID string  `json:"accountId"`
	Into      string  `json:"into"`
	From      string  `json:"from"`
	IfInState *string `json:"ifInState"`
}

type contactMergeResult struct {
	AccountID      string `json:"accountId"`
	OldState       string `json:"oldState"`
	NewState       string `json:"newState"`
	ID             string `json:"id"`
	DestroyedID    string `json:"destroyedId"`
	AddressesMoved int    `json:"addressesMoved"`
	KeysMoved      int    `json:"keysMoved"`
	KeysCombined   int    `json:"keysCombined"`
}

func parseContactID(raw, name string) (ulid.ULID, *MethodError) {
	id, err := ulid.Parse(raw)
	if err != nil {
		return ulid.ULID{}, &MethodError{Type: errInvalidArguments, Description: name + " must be a contact id"}
	}
	return id, nil
}

// contactWriteError maps the store's merge/separate errors to method errors.
func contactWriteError(err error) *MethodError {
	switch {
	case errors.Is(err, store.ErrContactNotFound):
		return &MethodError{Type: errNotFound, Description: "no such contact"}
	case errors.Is(err, store.ErrMergeNotesTooLong):
		return &MethodError{Type: errNotesTooLong, Description: err.Error()}
	case errors.Is(err, store.ErrMergeSelf), errors.Is(err, store.ErrContactAddressNotFound),
		errors.Is(err, store.ErrSeparateAll), errors.Is(err, store.ErrSeparateNone):
		return &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	return &MethodError{Type: errServerFail, Description: err.Error()}
}

// contactStateCheck reads the Contact state and enforces ifInState.
func contactStateCheck(ctx context.Context, tx *store.UserTx, ifInState *string) (string, *MethodError) {
	st, err := tx.ChangeState(ctx, store.ObjectContact)
	if err != nil {
		return "", &MethodError{Type: errServerFail, Description: err.Error()}
	}
	if ifInState != nil && *ifInState != encodeState(st) {
		return "", &MethodError{Type: errStateMismatch}
	}
	return encodeState(st), nil
}

func (a *API) contactMerge(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	var args contactMergeArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	if mErr := resolveAccount(userID, args.AccountID); mErr != nil {
		return nil, mErr
	}
	into, mErr := parseContactID(args.Into, "into")
	if mErr != nil {
		return nil, mErr
	}
	from, mErr := parseContactID(args.From, "from")
	if mErr != nil {
		return nil, mErr
	}
	res := contactMergeResult{AccountID: userID.String(), ID: into.String(), DestroyedID: from.String()}
	mErr = a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
		old, mErr := contactStateCheck(ctx, tx, args.IfInState)
		if mErr != nil {
			return mErr
		}
		res.OldState = old
		m, err := tx.MergeContacts(ctx, into, from)
		if err != nil {
			return contactWriteError(err)
		}
		res.AddressesMoved, res.KeysMoved, res.KeysCombined = m.Addresses, m.Keys, m.KeysCombined
		st, err := tx.ChangeState(ctx, store.ObjectContact)
		if err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		res.NewState = encodeState(st)
		return nil
	})
	if mErr != nil {
		return nil, mErr
	}
	return mustMarshal(res), nil
}

type contactSeparateArgs struct {
	AccountID  string   `json:"accountId"`
	ID         string   `json:"id"`
	AddressIDs []string `json:"addressIds"`
	IfInState  *string  `json:"ifInState"`
}

type contactSeparateResult struct {
	AccountID       string            `json:"accountId"`
	OldState        string            `json:"oldState"`
	NewState        string            `json:"newState"`
	ID              string            `json:"id"`
	Created         []string          `json:"created"`
	AddressContacts map[string]string `json:"addressContacts"`
	KeysMoved       int               `json:"keysMoved"`
	Restored        bool              `json:"restored"`
	KeptFields      []string          `json:"keptFields"`
}

func (a *API) contactSeparate(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	var args contactSeparateArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	if mErr := resolveAccount(userID, args.AccountID); mErr != nil {
		return nil, mErr
	}
	id, mErr := parseContactID(args.ID, "id")
	if mErr != nil {
		return nil, mErr
	}
	if len(args.AddressIDs) == 0 {
		return nil, &MethodError{Type: errInvalidArguments, Description: "addressIds must name at least one address"}
	}
	addrIDs := make([]ulid.ULID, 0, len(args.AddressIDs))
	for _, raw := range args.AddressIDs {
		aid, err := ulid.Parse(raw)
		if err != nil {
			return nil, &MethodError{Type: errInvalidArguments, Description: "addressIds must be address ids"}
		}
		addrIDs = append(addrIDs, aid)
	}
	res := contactSeparateResult{AccountID: userID.String(), ID: id.String(),
		Created: []string{}, AddressContacts: map[string]string{}, KeptFields: []string{}}
	mErr = a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
		old, mErr := contactStateCheck(ctx, tx, args.IfInState)
		if mErr != nil {
			return mErr
		}
		res.OldState = old
		s, err := tx.SeparateContact(ctx, id, addrIDs)
		if err != nil {
			return contactWriteError(err)
		}
		res.Created = ulidStrings(s.Created)
		for a, c := range s.AddressContact {
			res.AddressContacts[a.String()] = c.String()
		}
		res.KeysMoved = s.Keys
		res.Restored = s.Restored
		if s.KeptFields != nil {
			res.KeptFields = s.KeptFields
		}
		st, err := tx.ChangeState(ctx, store.ObjectContact)
		if err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		res.NewState = encodeState(st)
		return nil
	})
	if mErr != nil {
		return nil, mErr
	}
	return mustMarshal(res), nil
}

// jmapDateOrZero is a UTCDate, or null for the zero time (a contact with no
// addresses has no mail span).
func jmapDateOrZero(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return jmapDate(t)
}
