// SPDX-License-Identifier: AGPL-3.0-only

package blob

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	"github.com/oklog/ulid/v2"
)

// ErrNotFound is returned by Get when no blob exists at the derived path for
// the given user and content hash.
var ErrNotFound = errors.New("blob: not found")

// ContentHash is the SHA-256 digest of a blob's PLAINTEXT. It is the
// content-address used to derive a blob's on-disk path; it is never a
// digest of the ciphertext.
type ContentHash [32]byte

// String renders h as lowercase hex.
func (h ContentHash) String() string {
	return hex.EncodeToString(h[:])
}

// ParseContentHash parses the hex encoding produced by ContentHash.String.
func ParseContentHash(s string) (ContentHash, error) {
	var h ContentHash
	b, err := hex.DecodeString(s)
	if err != nil {
		return ContentHash{}, fmt.Errorf("blob: parse content hash: %w", err)
	}
	if len(b) != len(h) {
		return ContentHash{}, fmt.Errorf("blob: parse content hash: want %d bytes, got %d", len(h), len(b))
	}
	copy(h[:], b)
	return h, nil
}

// BlobStore is Mailroom's blob storage abstraction (ADR-004): content
// addressed by the plaintext's SHA-256, stored as ciphertext only, isolated
// per user by both a non-enumerable path prefix and a caller-supplied DEK.
// A filesystem implementation (FSBlobStore) ships first; a MinIO/S3-style
// backend can implement this interface later without callers changing.
type BlobStore interface {
	// Put streams r, encrypting it under dek as it is read, and returns the
	// SHA-256 hash of the plaintext. Content already stored for userID under
	// the resulting hash is deduplicated; Put still succeeds in that case.
	Put(ctx context.Context, userID ulid.ULID, dek []byte, r io.Reader) (ContentHash, error)

	// Get returns a reader over the decrypted plaintext for h, owned by
	// userID and encrypted under dek. The returned reader's terminating Read
	// surfaces any authentication or content-hash-verification failure;
	// callers must not treat partially-read output as trustworthy until the
	// reader returns a clean io.EOF. Returns ErrNotFound if no such blob
	// exists.
	Get(ctx context.Context, userID ulid.ULID, dek []byte, h ContentHash) (io.ReadCloser, error)

	// Exists reports whether a blob is stored for userID under h.
	Exists(ctx context.Context, userID ulid.ULID, h ContentHash) (bool, error)

	// StoredSize returns the on-disk (ciphertext, envelope-included) byte size
	// of the blob stored for userID under h. Returns ErrNotFound if absent.
	StoredSize(ctx context.Context, userID ulid.ULID, h ContentHash) (int64, error)

	// Delete removes the blob stored for userID under h. Deleting a blob
	// that does not exist is not an error (idempotent).
	Delete(ctx context.Context, userID ulid.ULID, h ContentHash) error
}
