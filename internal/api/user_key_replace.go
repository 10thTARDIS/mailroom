// SPDX-License-Identifier: AGPL-3.0-only

package api

// UserKey/replace (#817, ADR-040 §14): make a new key for a bound key's address,
// put it in the old key's place, and retire the old one, in one step.
//
//	UserKey/replace {accountId, id, expiresDays? | expiresAt?}
//	  -> {accountId, id, replacedId, fingerprint, published, boundTo, key, oldKey}
//
// TWO STEPS, AND THE SECOND IS ATOMIC. The crypto process generates the new key
// UNBOUND (custody writes it, with its Key `created` row). Then ONE store
// savepoint (store.ReplaceUserKey) binds it to the old key's identity,
// publishes it if the old key was published and this instance serves the
// address, retires the old key and records retired_at and replaced_by, with one
// change row per object that moved: Key (new), Key (old), Identity. A failure
// in that step leaves the old key exactly as it was; the new key then exists
// unbound, and the error says so.
//
// Refused before anything is generated: a revoked key (keyRevoked: revoking for
// compromise stays its own act), a retired key (keyRetired), an unbound key
// (keyNotBound), a key bound to a wildcard (wildcardIdentity). It spends the
// generate rate-limit token. The old key certifying the new one is out of scope
// (#817).

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// Method error types of UserKey/replace.
const (
	errKeyRevoked  = "keyRevoked"
	errKeyRetired  = "keyRetired"
	errKeyNotBound = "keyNotBound"
)

type userKeyReplaceArgs struct {
	AccountID   string  `json:"accountId"`
	ID          string  `json:"id"`
	ExpiresDays *int    `json:"expiresDays"`
	ExpiresAt   *string `json:"expiresAt"`
}

func (a *API) userKeyReplace(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	var args userKeyReplaceArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	if mErr := resolveAccount(userID, args.AccountID); mErr != nil {
		return nil, mErr
	}
	if mErr := a.needCustody(); mErr != nil {
		return nil, mErr
	}
	oldID, mErr := parseKeyID(args.ID)
	if mErr != nil {
		return nil, mErr
	}
	exp, mErr := generateExpiry(args.ExpiresDays, args.ExpiresAt, time.Now().UTC())
	if mErr != nil {
		return nil, mErr
	}

	// The refusals, before the crypto process is asked for anything.
	var (
		ident   store.Identity
		publish bool
	)
	if mErr := a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
		old, found, err := tx.GetUserKey(ctx, oldID)
		if err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		if !found {
			return &MethodError{Type: errNotFound, Description: "no such key"}
		}
		if mErr := replaceRefusal(old); mErr != nil {
			return mErr
		}
		isDefault, err := tx.KeyIsIdentityDefault(ctx, oldID, *old.IdentityID)
		if err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		if !isDefault {
			return &MethodError{Type: errKeyNotBound,
				Description: "that key is not its address's signing key (another key is bound there); replace that one instead"}
		}
		list, err := tx.GetIdentities(ctx, []ulid.ULID{*old.IdentityID})
		if err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		if len(list) == 0 {
			return &MethodError{Type: errKeyNotBound, Description: "that key's address no longer exists; generate a key instead"}
		}
		ident = list[0]
		if ident.Kind != store.IdentityFixed || ident.Address == "" {
			return &MethodError{Type: errWildcardIdentity,
				Description: "that key is bound to a domain wildcard; a new key names one address, so generate it for a fixed address"}
		}
		_, servable := wkdLocation(ident, a.wkdDomain)
		publish = old.WKDPublished && servable
		return nil
	}); mErr != nil {
		return nil, mErr
	}
	if mErr := a.spendKeyOp(userID); mErr != nil {
		return nil, mErr
	}

	ck, err := a.custody.GenerateKey(ctx, userID, ident.DisplayName, ident.Address, exp)
	if err != nil {
		return nil, custodyMethodError(ctx, "replace", userID, err, errInvalidArguments)
	}
	newID, perr := ulid.Parse(ck.KeyID)
	if perr != nil {
		return nil, &MethodError{Type: errServerFail, Description: "the crypto process returned an unreadable key id"}
	}

	var res store.ReplaceResult
	if mErr := a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
		var err error
		res, err = tx.ReplaceUserKey(ctx, oldID, newID, publish)
		switch {
		case err == nil && !res.Found:
			return &MethodError{Type: errNotFound, Description: "the key disappeared before it could be replaced"}
		case err == nil:
			return nil
		}
		// The new key exists, unbound; the old key is exactly as it was.
		left := " A new key (" + newID.String() + ") was made but not put in place; your current key is unchanged."
		switch {
		case errors.Is(err, store.ErrKeyRevoked):
			return &MethodError{Type: errKeyRevoked, Description: "that key was revoked meanwhile." + left}
		case errors.Is(err, store.ErrKeyRetired):
			return &MethodError{Type: errKeyRetired, Description: "that key was retired meanwhile." + left}
		case errors.Is(err, store.ErrKeyNotBound):
			return &MethodError{Type: errKeyNotBound, Description: "that key was unbound meanwhile." + left}
		default:
			slog.ErrorContext(ctx, "UserKey/replace: the store step failed after generation",
				"user_id", userID.String(), "old_key_id", oldID.String(), "new_key_id", newID.String(), "err", err)
			return &MethodError{Type: errServerFail, Description: "the replacement could not be completed." + left}
		}
	}); mErr != nil {
		return nil, mErr
	}

	newObj, mErr := a.keyObjectAfter(ctx, userID, newID)
	if mErr != nil {
		return nil, mErr
	}
	oldObj, mErr := a.keyObjectAfter(ctx, userID, oldID)
	if mErr != nil {
		return nil, mErr
	}
	return mustMarshal(map[string]any{
		"accountId":   userID.String(),
		"id":          newID.String(),
		"replacedId":  oldID.String(),
		"fingerprint": ck.Fingerprint,
		"published":   res.Published,
		"boundTo":     res.IdentityID.String(),
		"key":         newObj,
		"oldKey":      oldObj,
	}), nil
}

// replaceRefusal is why a key cannot be replaced, or nil.
func replaceRefusal(k store.UserKey) *MethodError {
	switch {
	case k.RevokedAt != nil:
		return &MethodError{Type: errKeyRevoked,
			Description: "that key is revoked; generate a new key for the address instead"}
	case k.DecryptOnly:
		return &MethodError{Type: errKeyRetired,
			Description: "that key is already retired; generate a new key for the address instead"}
	case k.IdentityID == nil:
		return &MethodError{Type: errKeyNotBound,
			Description: "that key is not bound to an address, so there is nothing to put a new key in place of; generate one instead"}
	}
	return nil
}
