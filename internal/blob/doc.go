// SPDX-License-Identifier: AGPL-3.0-only

// Package blob implements Mailroom's content-addressed, encrypted blob
// storage (ADR-004, spec v0.4 §10/§14). Blobs are addressed by the SHA-256
// hash of their PLAINTEXT and stored on disk as CIPHERTEXT ONLY, under a
// per-user prefix derived from HMAC-SHA256(serverKey, userID) — derived, not
// stored, so paths are non-enumerable without the server key.
//
// # Isolation (ADR-004, spec §10)
//
// Per-user DEKs are the real isolation backstop: a path-construction bug
// leaks ciphertext under the wrong key, not plaintext under the wrong user.
// Different users hash to different HMAC prefixes, so cross-user
// content-addressed dedup is impossible by construction — this is
// deliberate, not an oversight (ADR-004 consequences).
//
// This package is deliberately unaware of DEK provenance: callers (wired up
// by the crypto key hierarchy and the store/API layers) supply a raw DEK per
// call. blob never touches the database.
//
// # Filesystem backend
//
// FSBlobStore is the v1 backend; a MinIO/S3-compatible backend can implement
// the same BlobStore interface later without callers changing (ADR-004).
// Path layout: root/<hmac-userid-hex>/<hash[:2]>/<hash-hex>, sharded by the
// first byte of the content hash to bound directory fan-out. All directories
// are created 0700 and blob files 0600.
//
// Put streams the plaintext through crypto.SealStream in one pass: the
// source is teed through a SHA-256 hasher to compute the content hash
// without buffering the whole plaintext, while ciphertext is written to a
// temp file in the destination shard directory. Once the hash (and
// therefore the final path) is known, the temp file is atomically renamed
// into place. If the destination already exists, the content is already
// stored for this user (content-addressed dedup) and the temp file is
// discarded. Get streams ciphertext through crypto.OpenStream and verifies
// the recovered plaintext's hash matches the requested ContentHash; because
// OpenStream can emit authenticated leading chunks before a later chunk
// fails, Get returns a reader whose terminating Read surfaces any auth or
// hash-mismatch error (via io.Pipe) rather than buffering the full
// plaintext to make the result all-or-nothing.
//
// # Key binding
//
// SealStream/OpenStream's keyID is left empty: the caller-supplied DEK is
// already selected out-of-band (one DEK per call; no per-stream key
// selection is needed here). The AEAD associated data is the raw user ID
// (userID[:]), binding every chunk to the owning user — a blob's ciphertext
// cannot be reattributed to a different user's DEK context.
package blob
