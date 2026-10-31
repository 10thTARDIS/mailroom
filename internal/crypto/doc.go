// SPDX-License-Identifier: AGPL-3.0-only

// Package crypto implements Mailroom's envelope-encryption v1: key material
// (delivered from and derived from a file-based master key) and the versioned
// AEAD envelope + chunked-stream formats that protect data at rest
// (spec v0.4 §9/§11, ADR-004, ADR-005). Issue #7.
//
// This package is deliberately persistence-free. It operates on raw byte
// key material only; it never touches the database and MUST NOT import
// internal/store. Ciphertext and any persisted key bytes are handled by
// callers (the store / crypto process); this package just produces and
// consumes the bytes.
//
// # Key derivation, as actually used in 0.5/1.0 (ADR-013)
//
// Spec §9/ADR-005 describe a WRAPPED hierarchy (master key -> per-user KEK ->
// per-purpose DEK, with the KEK stored). ADR-013 records the deviation: for
// 0.5/1.0 every per-user key is instead HKDF-DERIVED straight from the master
// key, with no per-user KEK and no wrapped-key store —
//
//   - credentials key = DeriveKey(master, salt=userID, info="mailroom-credentials-v1"),
//   - mail-blob DEK   = DeriveKey(master, salt=userID, info="mailroom-mail-blob-v1"),
//   - blob paths       = an HMAC of a key derived from the master.
//
// DeriveKey (keys.go) is the function actually called along these paths. The
// wrapped-KEK/DEK primitives below (GenerateKEK, WrapKEK/UnwrapKEK, RewrapKEK,
// WrapDEK/UnwrapDEK) still exist in this package but have no production
// caller today — they are the "1.x hardening" ADR-013 defers to, kept here so
// that work is an addition rather than a rewrite when it lands.
//
// # The primitives, when the wrapped hierarchy is used (1.x)
//
//   - Master key: 32 bytes, delivered as a file-based secret at boot behind
//     the KeyProvider interface (FileKeyProvider). Never persisted by this
//     package; escrowed offline per docs/key-escrow.md.
//   - Per-user KEK: a random 32-byte key wrapped by the master key. Master
//     rotation would then be a cheap re-wrap of KEKs only (RewrapKEK); the
//     DEKs and the data they seal untouched.
//   - Per-purpose DEK: a random 32-byte key wrapped by the user KEK, with the
//     purpose label ("mail-blob", "credentials", ...) bound into the wrap as
//     AEAD associated data so a DEK cannot be confused across purposes.
//
// # Formats
//
// Two self-describing, versioned binary formats are defined here:
//
//   - The single-shot envelope (see envelope.go) for keys and small payloads.
//   - The chunked stream (see stream.go) for large blobs (attachments, raw
//     MIME), which resists truncation, reordering and chunk-swapping.
//
// # Honest framing (spec §9)
//
// This is strong at-rest encryption, NOT end-to-end encryption. Because KEKs
// are wrapped by the master key, whoever holds the master key can decrypt
// everything; admin blindness is software policy + audit, not cryptography.
//
// # Security notes
//
//   - Keys are 32 bytes; nonces come from crypto/rand and are never reused.
//   - Key material is never logged. Callers own returned key buffers; helpers
//     zeroize temporary copies where practical (see Zero).
package crypto
