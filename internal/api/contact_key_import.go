// SPDX-License-Identifier: AGPL-3.0-only

package api

// ContactKey/import (#47, ADR-038 "imported" source): the user hands us a
// correspondent's public key.
//
//	{accountId, address, publicKey, dryRun?}  ->  {accountId, address, fingerprint, userId, pinnedFingerprint}
//
// publicKey is an ASCII-armored key ("-----BEGIN PGP PUBLIC KEY BLOCK-----"), or
// the BASE64 of a binary OpenPGP key (JSON cannot carry raw bytes).
//
// dryRun: true runs every check and returns the fingerprint and userId (the user
// ID that names the address) while storing nothing; pinnedFingerprint is null.
//
// The key becomes an eligible key of source `manual` under the contact that owns
// `address` (created if there is none, as the WKD path does), and it REPLACES the
// pin for the address: importing is an explicit act, which ADR-038 lets override
// the trust-on-first-use pin. `authenticated` is not meaningful for a manual key
// and is stored false.
//
// ERRORS (method errors, each with a distinct type):
//
//	secretKeyRefused       the input carries secret-key material; nothing is stored
//	keyDoesNotNameAddress  no valid, non-revoked user ID has this email
//	noEncryptionSubkey     nothing in the key can be encrypted to
//	keyExpiredOrRevoked    the key (or its only encryption subkey) is expired or revoked
//	invalidKey             unreadable, or more than one primary key
//	tooLarge               over the size cap (256 KiB decoded)
//	ownAddress             the address is one the user sends as; a key for it is
//	                       the user's own (UserKey), not a correspondent's
//
// One transaction: key row, pin and one Contact `updated` change_log row (plus
// the contact's own `created` row when this call had to create it).

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/pgpkeys"
	"mailroom/internal/store"
)

const (
	errSecretKeyRefused    = "secretKeyRefused"
	errNoEncryptionSubkey  = "noEncryptionSubkey"
	errKeyExpiredOrRevoked = "keyExpiredOrRevoked"
	errInvalidKey          = "invalidKey"
	errTooLarge            = "tooLarge"
	errOwnAddress          = "ownAddress"
)

type contactKeyImportArgs struct {
	AccountID string `json:"accountId"`
	Address   string `json:"address"`
	PublicKey string `json:"publicKey"`
	// DryRun validates and reports the fingerprint and user ID, stores nothing
	// and writes no change row. The web dialog uses it to show the key BEFORE the
	// user confirms (the browser has no OpenPGP parser to do it itself).
	DryRun bool `json:"dryRun"`
}

func (a *API) contactKeyImport(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	var args contactKeyImportArgs
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
	if len(args.PublicKey) > 2*pgpkeys.MaxKeyBytes {
		return nil, &MethodError{Type: errTooLarge, Description: "that key is too large to import"}
	}
	raw := []byte(strings.TrimSpace(args.PublicKey))
	if len(raw) == 0 {
		return nil, &MethodError{Type: errInvalidArguments, Description: "publicKey is required"}
	}
	if !strings.HasPrefix(string(raw), "-----BEGIN") {
		dec, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(string(raw)), ""))
		if err != nil {
			return nil, &MethodError{Type: errInvalidKey,
				Description: "publicKey must be an armored key or the base64 of a binary key"}
		}
		raw = dec
	}

	imp, err := pgpkeys.Import(raw, addr, time.Now().UTC())
	if err != nil {
		return nil, importKeyError(err)
	}
	if args.DryRun {
		// The own-address refusal applies to a dry run too, or the dialog would show
		// a confirmation that the real call then refuses.
		if mErr := a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
			return rejectOwnAddress(ctx, tx, addr)
		}); mErr != nil {
			return nil, mErr
		}
		return mustMarshal(map[string]any{
			"accountId":         userID.String(),
			"address":           addr,
			"fingerprint":       imp.Fingerprint,
			"userId":            imp.UserID,
			"dryRun":            true,
			"pinnedFingerprint": nil,
		}), nil
	}

	var pinned string
	mErr := a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
		if mErr := rejectOwnAddress(ctx, tx, addr); mErr != nil {
			return mErr
		}
		cid, ok, err := tx.ContactIDForAddress(ctx, addr)
		if err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		if !ok {
			// A recipient typed into compose, or a sender we hold nothing for, is not
			// a contact yet: make one the way the WKD path does.
			if err := tx.UpsertContactsFromMessage(ctx, store.ContactSourceMessage{
				Recipients: []store.ContactParticipant{{Address: addr}},
			}); err != nil {
				return &MethodError{Type: errServerFail, Description: err.Error()}
			}
			cid, ok, err = tx.ContactIDForAddress(ctx, addr)
			if err != nil {
				return &MethodError{Type: errServerFail, Description: err.Error()}
			}
			if !ok {
				return &MethodError{Type: errInvalidArguments, Description: "no contact can be made for that address"}
			}
		}
		if err := tx.ImportContactKey(ctx, cid, addr, store.ContactKeyRow{
			Kind: store.KeyPGP, Fingerprint: imp.Fingerprint, PublicKey: imp.Binary,
		}); err != nil {
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
	return mustMarshal(map[string]any{
		"accountId":         userID.String(),
		"address":           addr,
		"fingerprint":       imp.Fingerprint,
		"userId":            imp.UserID,
		"pinnedFingerprint": pinned,
	}), nil
}

func importKeyError(err error) *MethodError {
	switch {
	case errors.Is(err, pgpkeys.ErrHasPrivateKey):
		return &MethodError{Type: errSecretKeyRefused,
			Description: "that is a SECRET key; it was not stored. Export and paste the public key instead"}
	case errors.Is(err, pgpkeys.ErrTooLarge):
		return &MethodError{Type: errTooLarge, Description: "that key is too large to import"}
	case errors.Is(err, pgpkeys.ErrNamesOtherAddress):
		return &MethodError{Type: errKeyDoesNotNameAddress,
			Description: "that key has no valid user ID naming this address"}
	case errors.Is(err, pgpkeys.ErrNoEncryptionSubkey):
		return &MethodError{Type: errNoEncryptionSubkey, Description: "that key has no encryption subkey"}
	case errors.Is(err, pgpkeys.ErrExpiredOrRevoked):
		return &MethodError{Type: errKeyExpiredOrRevoked, Description: "that key is expired or revoked"}
	case errors.Is(err, pgpkeys.ErrMultipleKeys):
		return &MethodError{Type: errInvalidKey, Description: "that file holds more than one key; import one key at a time"}
	default:
		return &MethodError{Type: errInvalidKey, Description: "that is not a readable OpenPGP public key"}
	}
}

// rejectOwnAddress refuses an address the user sends as. It is never a
// correspondent (the contact table excludes it), so there is nowhere to keep the
// key; say so, rather than silently storing nothing.
func rejectOwnAddress(ctx context.Context, tx *store.UserTx, addr string) *MethodError {
	_, err := tx.ResolveSendIdentity(ctx, addr)
	switch {
	case err == nil:
		return &MethodError{Type: errOwnAddress,
			Description: "that is one of your own addresses; a key for it belongs in your own keys, not a contact's"}
	case errors.Is(err, store.ErrFromNotVerified):
		return nil
	default:
		return &MethodError{Type: errServerFail, Description: err.Error()}
	}
}
