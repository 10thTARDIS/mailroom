// SPDX-License-Identifier: AGPL-3.0-only

package pgpkeys

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
)

// The ways a user-supplied key can be refused by Import. Each maps to one
// ContactKey/import error type, so the user is told which thing is wrong.
var (
	// ErrMultipleKeys: more than one primary key in the blob. A correspondent's
	// key is one transferable key; a keyring dump is not accepted because the
	// user could not tell which of its keys was meant.
	ErrMultipleKeys = errors.New("pgpkeys: the input holds more than one primary key")

	// ErrExpiredOrRevoked: the key, or every key it could be encrypted to, is
	// expired or revoked at the time of the import.
	ErrExpiredOrRevoked = errors.New("pgpkeys: the key is expired or revoked")

	// ErrNoEncryptionSubkey: the key is valid but nothing in it can be encrypted
	// to (a signing-only key).
	ErrNoEncryptionSubkey = errors.New("pgpkeys: the key has no encryption subkey")
)

// Imported is a key accepted by Import.
type Imported struct {
	// Binary is the key in binary transferable form, the form every other route
	// stores in contact_keys.public_key (armor is removed).
	Binary []byte
	// Fingerprint is the primary key's fingerprint, UPPERCASE hex.
	Fingerprint string
	// UserID is the raw user ID that names the address.
	UserID string
}

// Import validates a key the USER pasted or uploaded for address (#47, ADR-038
// "imported" source). input is ASCII-armored or binary.
//
// It refuses, in this order: input over MaxKeyBytes (ErrTooLarge); anything
// unreadable (ErrMalformed); any secret-key material, whether in a PRIVATE KEY
// armor block or in the packets themselves (ErrHasPrivateKey: never stored);
// more than one primary key (ErrMultipleKeys); an expired or revoked key
// (ErrExpiredOrRevoked); a key with no encryption subkey (ErrNoEncryptionSubkey);
// and a key with no valid user ID whose email is address (ErrNamesOtherAddress).
//
// ⚠ THE NAMES-THE-ADDRESS CHECK IS A GATE HERE, as it is for encryption: an
// imported key becomes the pin, so one that does not name the address would
// send that address's mail to whoever owns the key.
func Import(input []byte, address string, now time.Time) (Imported, error) {
	// Bound BEFORE any decoding, armor expands but never beyond this by much, and
	// the decoded size is bounded again below.
	if len(input) > MaxKeyBytes*2 {
		return Imported{}, ErrTooLarge
	}
	// ⚠ Trim only to LOOK for armor. A binary key can end in a byte that is
	// whitespace (0x0a, 0x20...), and trimming it corrupts the key: it then fails
	// to parse, intermittently, depending on the key.
	binary := input
	if trimmed := bytes.TrimSpace(input); bytes.HasPrefix(trimmed, []byte("-----BEGIN")) {
		block, err := armor.Decode(bytes.NewReader(trimmed))
		if err != nil {
			return Imported{}, fmt.Errorf("%w: %v", ErrMalformed, err)
		}
		if strings.Contains(strings.ToUpper(block.Type), "PRIVATE") {
			return Imported{}, ErrHasPrivateKey
		}
		binary, err = io.ReadAll(io.LimitReader(block.Body, MaxKeyBytes+1))
		if err != nil {
			return Imported{}, fmt.Errorf("%w: %v", ErrMalformed, err)
		}
	}
	k, err := Parse(binary) // size, malformed, secret material
	if err != nil {
		return Imported{}, err
	}
	entities, err := openpgp.ReadKeyRing(bytes.NewReader(binary))
	if err != nil || len(entities) == 0 {
		return Imported{}, ErrMalformed
	}
	if len(entities) > 1 {
		return Imported{}, ErrMultipleKeys
	}
	e := entities[0]

	if _, ok := e.EncryptionKey(now); !ok {
		self, ident := e.PrimarySelfSignature()
		switch {
		case self == nil:
			return Imported{}, ErrMalformed
		case e.PrimaryKey.KeyExpired(self, now), e.Revoked(now), self.SigExpired(now),
			ident != nil && ident.Revoked(now):
			return Imported{}, ErrExpiredOrRevoked
		}
		// Valid primary. Is there an encryption subkey that is merely expired or
		// revoked, or none at all?
		for _, sub := range e.Subkeys {
			if sub.Sig.FlagsValid && sub.Sig.FlagEncryptCommunications {
				return Imported{}, ErrExpiredOrRevoked
			}
		}
		return Imported{}, ErrNoEncryptionSubkey
	}
	uid, ok := namingUserID(e, address, now)
	if !ok {
		return Imported{}, ErrNamesOtherAddress
	}
	return Imported{Binary: binary, Fingerprint: k.Fingerprint, UserID: uid}, nil
}
