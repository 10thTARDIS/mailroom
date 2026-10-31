// SPDX-License-Identifier: AGPL-3.0-only

package crypto

import (
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
)

// DeriveKey deterministically derives a 32-byte symmetric key from the master
// key via HKDF-SHA256, bound to salt and a domain-separation info string. It is
// the storage-free alternative to the wrapped random-DEK hierarchy: the same
// (master, salt, info) always yields the same key, so no wrapped key needs to
// be persisted. Use for keys whose rotation is handled by re-encrypting rather
// than re-wrapping (e.g. the per-user account-credentials key: salt=userID,
// info="mailroom-credentials-v1"). For blob DEKs that must rotate cheaply,
// prefer the random KEK/DEK wrap path instead.
func DeriveKey(master, salt []byte, info string) ([]byte, error) {
	if len(master) != KeySize {
		return nil, fmt.Errorf("crypto: derive key: master must be %d bytes, got %d", KeySize, len(master))
	}
	out, err := hkdf.Key(sha256.New, master, salt, info, KeySize)
	if err != nil {
		return nil, fmt.Errorf("crypto: derive key: %w", err)
	}
	return out, nil
}

// Domain-separation associated data for the wrap operations. These are bound
// into the AEAD tag so a wrapped KEK can never be opened as a DEK (or vice
// versa), and a DEK for one purpose can never be opened as a DEK for another.
const (
	kekWrapAAD       = "mailroom-kek-wrap-v1"
	dekWrapAADPrefix = "mailroom-dek-wrap-v1:"
)

// GenerateKEK returns a fresh random 32-byte per-user key-encryption key.
func GenerateKEK() ([]byte, error) { return randomKey("kek") }

// GenerateDEK returns a fresh random 32-byte per-purpose data-encryption key.
func GenerateDEK() ([]byte, error) { return randomKey("dek") }

func randomKey(what string) ([]byte, error) {
	k := make([]byte, KeySize)
	if _, err := rand.Read(k); err != nil {
		return nil, fmt.Errorf("crypto: generate %s: %w", what, err)
	}
	return k, nil
}

// WrapKEK wraps a per-user KEK under the master key. The result is a
// versioned envelope; persist it as the user's wrapped KEK. Master-key
// rotation re-wraps this (RewrapKEK) without touching any DEK or data.
func WrapKEK(master, kek []byte) ([]byte, error) {
	if len(kek) != KeySize {
		return nil, fmt.Errorf("crypto: wrap kek: kek must be %d bytes, got %d", KeySize, len(kek))
	}
	wrapped, err := Seal(master, nil, kek, []byte(kekWrapAAD))
	if err != nil {
		return nil, fmt.Errorf("crypto: wrap kek: %w", err)
	}
	return wrapped, nil
}

// UnwrapKEK recovers a per-user KEK from its master-wrapped form.
func UnwrapKEK(master, wrapped []byte) ([]byte, error) {
	kek, err := Open(master, wrapped, []byte(kekWrapAAD))
	if err != nil {
		return nil, fmt.Errorf("crypto: unwrap kek: %w", err)
	}
	if len(kek) != KeySize {
		return nil, fmt.Errorf("crypto: unwrap kek: unexpected key length %d", len(kek))
	}
	return kek, nil
}

// RewrapKEK performs a master-key rotation for a single wrapped KEK: it
// unwraps the KEK under oldMaster and re-wraps it under newMaster. The KEK
// value is unchanged, so every DEK wrapped by that KEK — and all data sealed
// under those DEKs — remains valid and untouched. This is the cheap rotation
// path from ADR-005.
func RewrapKEK(oldMaster, newMaster, wrapped []byte) ([]byte, error) {
	kek, err := UnwrapKEK(oldMaster, wrapped)
	if err != nil {
		return nil, fmt.Errorf("crypto: rewrap kek: %w", err)
	}
	defer Zero(kek)
	newWrapped, err := WrapKEK(newMaster, kek)
	if err != nil {
		return nil, fmt.Errorf("crypto: rewrap kek: %w", err)
	}
	return newWrapped, nil
}

// WrapDEK wraps a per-purpose DEK under a user KEK, binding the purpose label
// ("mail-blob", "credentials", "private-keys", ...) into the AEAD associated
// data. UnwrapDEK must be given the identical purpose; a DEK wrapped for one
// purpose cannot be unwrapped for another.
func WrapDEK(kek, dek []byte, purpose string) ([]byte, error) {
	if len(dek) != KeySize {
		return nil, fmt.Errorf("crypto: wrap dek: dek must be %d bytes, got %d", KeySize, len(dek))
	}
	if purpose == "" {
		return nil, errors.New("crypto: wrap dek: purpose must not be empty")
	}
	wrapped, err := Seal(kek, nil, dek, dekWrapAAD(purpose))
	if err != nil {
		return nil, fmt.Errorf("crypto: wrap dek: %w", err)
	}
	return wrapped, nil
}

// UnwrapDEK recovers a per-purpose DEK from its KEK-wrapped form. purpose must
// match the value passed to WrapDEK, or authentication fails.
func UnwrapDEK(kek, wrapped []byte, purpose string) ([]byte, error) {
	if purpose == "" {
		return nil, errors.New("crypto: unwrap dek: purpose must not be empty")
	}
	dek, err := Open(kek, wrapped, dekWrapAAD(purpose))
	if err != nil {
		return nil, fmt.Errorf("crypto: unwrap dek (purpose %q): %w", purpose, err)
	}
	if len(dek) != KeySize {
		return nil, fmt.Errorf("crypto: unwrap dek: unexpected key length %d", len(dek))
	}
	return dek, nil
}

func dekWrapAAD(purpose string) []byte {
	return append([]byte(dekWrapAADPrefix), purpose...)
}
