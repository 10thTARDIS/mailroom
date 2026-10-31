// SPDX-License-Identifier: AGPL-3.0-only

// Package pgpkeys inspects OpenPGP PUBLIC keys that arrived from outside —
// an Autocrypt header, a WKD lookup — far enough to decide whether they are
// storable and what to file them under.
//
// It is shared by every key-discovery route so they cannot disagree about what
// a valid key is, and so the checks below exist in exactly one place.
//
// ⚠ PUBLIC KEYS ONLY. This runs inside the app, not the crypto process, which
// is consistent with ADR-024: the boundary exists to keep PRIVATE key material
// on one side of it, and internal/pgpverify already parses public keys here for
// the same reason. Nothing in this package may ever be handed a secret key —
// see ErrHasPrivateKey.
package pgpkeys

import (
	"bytes"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
)

// MaxKeyBytes bounds a single serialized key, matching store.maxPublicKey.
// Key material from a stranger is attacker-sized, so the ceiling is ours.
const MaxKeyBytes = 256 * 1024

var (
	// ErrMalformed means the bytes are not a readable OpenPGP key.
	ErrMalformed = errors.New("pgpkeys: not a readable OpenPGP public key")

	// ErrHasPrivateKey means the "public" key carries SECRET key packets.
	//
	// ⚠ THIS IS A REAL CASE, NOT A THEORETICAL ONE. `gpg --export-secret-keys`
	// output is a valid transferable key, and a correspondent can paste it into
	// an Autocrypt header or serve it over WKD by mistake. Storing it would put
	// someone else's private key in our database, in a column called
	// public_key, where nothing would ever look for it again. Refuse instead.
	ErrHasPrivateKey = errors.New("pgpkeys: refusing a key that carries private key material")

	// ErrTooLarge means the input exceeds MaxKeyBytes.
	ErrTooLarge = errors.New("pgpkeys: key is too large")
)

// Key is the storable summary of a discovered public key.
type Key struct {
	// Fingerprint is the primary key's fingerprint as UPPERCASE hex, the same
	// form app.user_keys.fingerprint and internal/pgpverify already use. A
	// different case here would silently defeat the (user, contact,
	// fingerprint) uniqueness that stops one key being stored twice.
	Fingerprint string

	// UserIDs are the key's raw UID strings, e.g. "Alice <alice@example.com>".
	UserIDs []string

	// Addresses are the email addresses parsed out of those UIDs, lowercased
	// and de-duplicated. A caller checks the address it expects against this —
	// see CoversAddress — rather than trusting the envelope alone.
	Addresses []string
}

// CoversAddress reports whether addr appears in one of the key's UIDs.
//
// ⚠ ADVISORY, NOT A GATE, and the distinction is deliberate. Autocrypt Level 1
// says a key SHOULD carry a UID matching the address but does not require it,
// and real keys are published with no UID address at all. So a caller may
// record this as a quality signal; refusing on it would drop usable keys.
func (k Key) CoversAddress(addr string) bool {
	want := strings.ToLower(strings.TrimSpace(addr))
	for _, a := range k.Addresses {
		if a == want {
			return true
		}
	}
	return false
}

// Parse reads a BINARY OpenPGP transferable public key — the form Autocrypt
// base64s, WKD serves, and store.UserKey holds — and returns what is needed to
// store it. Armored input is rejected as malformed, deliberately: every
// producer in this codebase deals in binary, and quietly accepting armor here
// would let armored bytes reach a column documented as binary.
func Parse(binary []byte) (Key, error) {
	if len(binary) == 0 {
		return Key{}, ErrMalformed
	}
	if len(binary) > MaxKeyBytes {
		return Key{}, ErrTooLarge
	}

	entities, err := openpgp.ReadKeyRing(bytes.NewReader(binary))
	if err != nil {
		return Key{}, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	if len(entities) == 0 {
		return Key{}, ErrMalformed
	}

	// ⚠ THE PRIVATE-KEY CHECK RUNS OVER EVERY ENTITY AND EVERY SUBKEY, not just
	// the first primary key. A ring can carry one innocuous public key followed
	// by a secret one, and checking only entities[0].PrivateKey would wave that
	// through.
	for _, e := range entities {
		if e.PrivateKey != nil {
			return Key{}, ErrHasPrivateKey
		}
		for _, sub := range e.Subkeys {
			if sub.PrivateKey != nil {
				return Key{}, ErrHasPrivateKey
			}
		}
	}

	// The first entity is the key; extra entities in one blob are unusual and
	// their UIDs are not this key's UIDs, so they are not merged in.
	e := entities[0]
	if e.PrimaryKey == nil {
		return Key{}, ErrMalformed
	}

	k := Key{Fingerprint: strings.ToUpper(fmt.Sprintf("%X", e.PrimaryKey.Fingerprint))}
	seen := map[string]bool{}
	names := make([]string, 0, len(e.Identities))
	for name := range e.Identities {
		names = append(names, name)
	}
	sort.Strings(names) // map order is random; the lists must be stable
	for _, name := range names {
		k.UserIDs = append(k.UserIDs, name)
		addr := strings.ToLower(userIDAddress(name))
		if addr != "" && !seen[addr] {
			seen[addr] = true
			k.Addresses = append(k.Addresses, addr)
		}
	}
	return k, nil
}

// ErrNoEncryptionKey means the key is readable but cannot be encrypted to at the
// given time: it is expired or revoked (primary key, user id or self-signature),
// or it has no unexpired, unrevoked subkey flagged for encryption.
var ErrNoEncryptionKey = errors.New("pgpkeys: no valid encryption key")

// EncryptionFingerprint reports whether binary can be encrypted to at `now` and,
// if so, returns the primary key's fingerprint (uppercase hex, as Parse reports
// it).
//
// ⚠ THIS IS THE APPLICATION'S PRE-CHECK, NOT THE AUTHORITY. It answers "should
// the compose window offer Encrypt for this address" and "which stored key do we
// pick". The crypto process re-applies the same rule (go-crypto's
// Entity.EncryptionKey) when it actually encrypts, so a key that expired between
// the two is still refused, by address, at the boundary that does the work.
func EncryptionFingerprint(binary []byte, now time.Time) (string, error) {
	k, err := Parse(binary) // size, malformed and private-material checks
	if err != nil {
		return "", err
	}
	entities, err := openpgp.ReadKeyRing(bytes.NewReader(binary))
	if err != nil || len(entities) == 0 {
		return "", ErrMalformed
	}
	if _, ok := entities[0].EncryptionKey(now); !ok {
		return "", ErrNoEncryptionKey
	}
	return k.Fingerprint, nil
}

// ErrNamesOtherAddress means the key is otherwise usable but carries no valid
// user ID whose email is the address it would be encrypted for.
var ErrNamesOtherAddress = errors.New("pgpkeys: no valid user ID names this address")

// EncryptionFingerprintFor is EncryptionFingerprint plus the rule that the key
// must NAME THE ADDRESS: at least one user ID that is not revoked, whose
// self-signature has not expired, and whose email equals address
// (case-insensitive).
//
// ⚠ THIS IS A GATE FOR ENCRYPTION, WHILE Key.CoversAddress STAYS ADVISORY FOR
// STORAGE. Storing a key that names another address is harmless; encrypting a
// message for alice@ to a key that does not name alice@ is how a planted key
// reads her mail.
func EncryptionFingerprintFor(binary []byte, address string, now time.Time) (string, error) {
	fpr, err := EncryptionFingerprint(binary, now)
	if err != nil {
		return "", err
	}
	entities, err := openpgp.ReadKeyRing(bytes.NewReader(binary))
	if err != nil || len(entities) == 0 {
		return "", ErrMalformed
	}
	if !entityNamesAddress(entities[0], address, now) {
		return "", ErrNamesOtherAddress
	}
	return fpr, nil
}

// NamesAddress reports whether binary is a readable public key (no private
// material) with a valid user ID naming address, at `now`: a user ID that is not
// revoked, whose self-signature has not expired, and whose email equals address.
// It is the gate for attributing a SIGNATURE to an address (#47, ADR-039), where
// EncryptionFingerprintFor is the gate for encrypting to one; unlike that, it
// does not require an encryption subkey.
func NamesAddress(binary []byte, address string, now time.Time) bool {
	if _, err := Parse(binary); err != nil {
		return false
	}
	entities, err := openpgp.ReadKeyRing(bytes.NewReader(binary))
	if err != nil || len(entities) == 0 {
		return false
	}
	return entityNamesAddress(entities[0], address, now)
}

func entityNamesAddress(e *openpgp.Entity, address string, now time.Time) bool {
	_, ok := namingUserID(e, address, now)
	return ok
}

// namingUserID returns the user ID of e that validly names address at now (not
// revoked, self-signature not expired, email equal to address, case-insensitive).
func namingUserID(e *openpgp.Entity, address string, now time.Time) (string, bool) {
	want := strings.ToLower(strings.TrimSpace(address))
	if want == "" {
		return "", false
	}
	for name, ident := range e.Identities {
		if ident == nil || ident.SelfSignature == nil ||
			ident.Revoked(now) || ident.SelfSignature.SigExpired(now) {
			continue
		}
		if strings.ToLower(userIDAddress(name)) == want {
			return name, true
		}
	}
	return "", false
}
