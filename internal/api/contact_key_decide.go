// SPDX-License-Identifier: AGPL-3.0-only

package api

// ContactKey/decide (#47, #626 section E, ADR-039): the user's answer to "this
// sender's key is not the one I had".
//
//	{accountId, address, fingerprint, decision: "trust" | "keep"}
//
//	trust  the key becomes the pin for the address and an eligible key of source
//	       `manual` (so ADR-038 ranks it above WKD and Autocrypt), and a refusal
//	       is cleared. This is also what lets encryption proceed: RecipientKey/
//	       lookup stops reporting keyChanged and Submit encrypts to the new key.
//	keep   the key is recorded as refused. It is never encrypted to, and a
//	       signature by it keeps showing as keyChanged with `refused: true`. The
//	       pin is untouched, so encryption keeps using the old key.
//
// A NARROW METHOD, NOT ContactKey/set. The only thing a client may decide is trust
// in a key we ALREADY HOLD for the address; there is no way through this to add a
// key (that is the importer's job) or to delete one. `fingerprint` must name a key
// held under the contact that owns `address`, else notFound.
//
// `trust` additionally requires that the key NAMES the address (a valid user ID
// with that email, ADR-038 rule 3): trusting a key for an address it does not name
// would make it the pin, and the next encrypted mail to that address would go to
// whoever owns the key. A key that does not name the address is refused with
// keyDoesNotNameAddress. `keep` has no such requirement.
//
// WRITES: the key row and the pin (trust) or the refusal (keep) plus a Contact
// `updated` change_log row (Contact.keys is part of the synced Contact), in one
// transaction. Deciding again is idempotent; trust after keep reverses it.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/pgpkeys"
	"mailroom/internal/store"
)

const (
	errKeyDoesNotNameAddress = "keyDoesNotNameAddress"
	errContactKeyNotFound    = "notFound"
)

type contactKeyDecideArgs struct {
	AccountID   string `json:"accountId"`
	Address     string `json:"address"`
	Fingerprint string `json:"fingerprint"`
	Decision    string `json:"decision"`
}

func (a *API) contactKeyDecide(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	var args contactKeyDecideArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	if mErr := resolveAccount(userID, args.AccountID); mErr != nil {
		return nil, mErr
	}
	addr := store.NormalizeAddress(args.Address)
	if addr == "" || strings.ContainsAny(addr, " \t\r\n,<>\"") {
		return nil, &MethodError{Type: errInvalidArguments, Description: "address must be a plain email address"}
	}
	fpr := strings.ToUpper(strings.TrimSpace(args.Fingerprint))
	if fpr == "" {
		return nil, &MethodError{Type: errInvalidArguments, Description: "fingerprint is required"}
	}
	if args.Decision != "trust" && args.Decision != "keep" {
		return nil, &MethodError{Type: errInvalidArguments, Description: `decision must be "trust" or "keep"`}
	}

	var pinned string
	mErr := a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
		cid, ok, err := tx.ContactIDForAddress(ctx, addr)
		if err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		if !ok {
			return &MethodError{Type: errContactKeyNotFound, Description: "no key is held for that address"}
		}
		keys, err := tx.ContactKeysFor(ctx, cid)
		if err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		var held *store.ContactKey
		for i := range keys {
			if keys[i].Kind == store.KeyPGP && keys[i].Fingerprint == fpr {
				held = &keys[i]
				break
			}
		}
		if held == nil {
			return &MethodError{Type: errContactKeyNotFound, Description: "no such key is held for that address"}
		}
		if args.Decision == "trust" {
			if !pgpkeys.NamesAddress(held.PublicKey, addr, time.Now().UTC()) {
				return &MethodError{Type: errKeyDoesNotNameAddress,
					Description: "that key has no user ID naming this address, so it cannot be trusted for it"}
			}
			err = tx.TrustContactKey(ctx, cid, addr, fpr)
		} else {
			err = tx.RefuseContactKey(ctx, cid, fpr)
		}
		if err != nil {
			if errors.Is(err, store.ErrContactKeyNotFound) {
				return &MethodError{Type: errContactKeyNotFound, Description: "no such key is held for that address"}
			}
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		pinned, _, err = tx.ContactKeyPin(ctx, addr)
		if err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		return nil
	})
	if mErr != nil {
		return nil, mErr
	}
	var pin any
	if pinned != "" {
		pin = pinned
	}
	return mustMarshal(map[string]any{
		"accountId":         userID.String(),
		"address":           addr,
		"fingerprint":       fpr,
		"decision":          args.Decision,
		"pinnedFingerprint": pin,
	}), nil
}
