// SPDX-License-Identifier: AGPL-3.0-only

package crypto

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"os"
)

// KeyProvider supplies the unsealed 32-byte master key. The interface exists
// so a file-based secret (FileKeyProvider) can later be swapped for a KMS/HSM
// without touching callers (ADR-005). Implementations must never log key
// material. Returned buffers are owned by the caller; zeroize when done.
type KeyProvider interface {
	// MasterKey returns the 32-byte unsealed master key. It is read fresh on
	// each call (no long-lived in-memory copy is held by the provider).
	MasterKey(ctx context.Context) ([]byte, error)
}

// FileKeyProvider reads the master key from a file — a Docker/loaded secret
// mounted at e.g. /run/secrets/mailroom_master_key. Unseal is automatic at
// boot (ADR-005): whoever owns the running box owns the key.
//
// File format (documented, unambiguous):
//
//   - If the file is exactly 32 bytes, it is taken as the raw key verbatim
//     (no trimming — a raw key may legitimately contain whitespace bytes).
//   - Otherwise the contents are ASCII-whitespace-trimmed and must be exactly
//     64 hex characters (case-insensitive), decoding to the 32-byte key. This
//     allows a trailing newline, so the common `... > secret` idiom works.
//
// Consequence: a raw 32-byte key must be written with no trailing newline
// (use printf, not echo). The hex form is recommended for hand-managed files.
type FileKeyProvider struct {
	Path string
}

// NewFileKeyProvider constructs a FileKeyProvider for the given path.
func NewFileKeyProvider(path string) *FileKeyProvider {
	return &FileKeyProvider{Path: path}
}

// MasterKey implements KeyProvider.
func (p *FileKeyProvider) MasterKey(ctx context.Context) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return ReadKeyFile(p.Path, "master key")
}

// ReadKeyFile reads a 32-byte secret from path using the file-format rules
// documented on FileKeyProvider (32 raw bytes verbatim, or 64 hex characters
// with surrounding whitespace trimmed). label names the secret in error
// messages so an operator learns WHICH secret is malformed — there is more than
// one root secret in the deployment now: the master key and the crypto
// process's independent PGP root (ADR-024). Never log the returned bytes.
func ReadKeyFile(path, label string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("crypto: read %s %q: %w", label, path, err)
	}
	defer Zero(raw)
	key, err := parseKeyMaterial(raw, label)
	if err != nil {
		return nil, err
	}
	return key, nil
}

func parseKeyMaterial(raw []byte, label string) ([]byte, error) {
	if len(raw) == KeySize {
		key := make([]byte, KeySize)
		copy(key, raw)
		return key, nil
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == hex.EncodedLen(KeySize) {
		key := make([]byte, KeySize)
		if _, err := hex.Decode(key, trimmed); err != nil {
			Zero(key)
			return nil, fmt.Errorf("crypto: %s: not valid hex: %w", label, err)
		}
		return key, nil
	}
	return nil, fmt.Errorf(
		"crypto: %s: want %d raw bytes or %d hex chars (optionally newline-terminated), got %d bytes",
		label, KeySize, hex.EncodedLen(KeySize), len(raw),
	)
}
