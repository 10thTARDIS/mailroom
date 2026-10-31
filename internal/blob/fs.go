// SPDX-License-Identifier: AGPL-3.0-only

package blob

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/crypto"
)

// shardPrefixLen is the number of hex characters of the content hash used to
// shard a user's blob directory (one byte = two hex chars), bounding
// per-directory fan-out.
const shardPrefixLen = 2

// FSBlobStore is the filesystem BlobStore backend (ADR-004). It stores
// ciphertext only, under root/<hmac-userid-hex>/<hash[:2]>/<hash-hex>; see
// the package doc for the full layout and threat-model discussion.
type FSBlobStore struct {
	root      string
	serverKey []byte
}

var _ BlobStore = (*FSBlobStore)(nil)

// NewFSBlobStore constructs an FSBlobStore rooted at root, creating it
// (mode 0700) if necessary. serverKey must be crypto.KeySize bytes; it is
// used only to derive per-user path prefixes via HMAC-SHA256 and is never
// written to disk.
func NewFSBlobStore(root string, serverKey []byte) (*FSBlobStore, error) {
	if len(serverKey) != crypto.KeySize {
		return nil, fmt.Errorf("blob: new fs store: server key must be %d bytes, got %d", crypto.KeySize, len(serverKey))
	}
	if root == "" {
		return nil, errors.New("blob: new fs store: root must not be empty")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("blob: new fs store: resolve root: %w", err)
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, fmt.Errorf("blob: new fs store: create root: %w", err)
	}
	key := append([]byte(nil), serverKey...)
	return &FSBlobStore{root: abs, serverKey: key}, nil
}

// userPrefix derives the non-enumerable per-user path prefix:
// HMAC-SHA256(serverKey, userID), hex-encoded. It is never persisted —
// always recomputed from the server key.
func (s *FSBlobStore) userPrefix(userID ulid.ULID) string {
	mac := hmac.New(sha256.New, s.serverKey)
	mac.Write(userID[:])
	return hex.EncodeToString(mac.Sum(nil))
}

func (s *FSBlobStore) userDir(userID ulid.ULID) string {
	return filepath.Join(s.root, s.userPrefix(userID))
}

func (s *FSBlobStore) shardDir(userID ulid.ULID, h ContentHash) string {
	hexHash := h.String()
	return filepath.Join(s.userDir(userID), hexHash[:shardPrefixLen])
}

func (s *FSBlobStore) blobPath(userID ulid.ULID, h ContentHash) string {
	return filepath.Join(s.shardDir(userID, h), h.String())
}

// blobAAD is the AEAD associated data bound into every chunk of a blob's
// stream: the raw user ID. This binds ciphertext to its owning user without
// needing to know the content hash before the stream is fully read (see
// package doc, "Key binding").
func blobAAD(userID ulid.ULID) []byte {
	return userID[:]
}

// Put implements BlobStore.
func (s *FSBlobStore) Put(ctx context.Context, userID ulid.ULID, dek []byte, r io.Reader) (ContentHash, error) {
	if err := ctx.Err(); err != nil {
		return ContentHash{}, err
	}

	userDir := s.userDir(userID)
	if err := os.MkdirAll(userDir, 0o700); err != nil {
		return ContentHash{}, fmt.Errorf("blob: put: create user dir: %w", err)
	}

	tmp, err := os.CreateTemp(userDir, "put-*.tmp")
	if err != nil {
		return ContentHash{}, fmt.Errorf("blob: put: create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return ContentHash{}, fmt.Errorf("blob: put: chmod temp file: %w", err)
	}

	cleanup := true
	defer func() {
		if cleanup {
			_ = tmp.Close()
			_ = os.Remove(tmpPath)
		}
	}()

	hasher := sha256.New()
	tee := io.TeeReader(r, hasher)
	if err := crypto.SealStream(tmp, tee, dek, nil, blobAAD(userID), 0); err != nil {
		return ContentHash{}, fmt.Errorf("blob: put: seal stream: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return ContentHash{}, fmt.Errorf("blob: put: sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return ContentHash{}, fmt.Errorf("blob: put: close temp file: %w", err)
	}

	var h ContentHash
	copy(h[:], hasher.Sum(nil))

	destDir := s.shardDir(userID, h)
	if err := os.MkdirAll(destDir, 0o700); err != nil {
		return ContentHash{}, fmt.Errorf("blob: put: create shard dir: %w", err)
	}
	dest := s.blobPath(userID, h)

	// Content-addressed dedup: if this user already has this exact
	// plaintext stored, the temp file is redundant ciphertext for the same
	// content — discard it and report success.
	if _, statErr := os.Stat(dest); statErr == nil {
		return h, nil
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return ContentHash{}, fmt.Errorf("blob: put: stat destination: %w", statErr)
	}

	if err := os.Rename(tmpPath, dest); err != nil {
		// Lost a race with a concurrent Put of the same content (or, on
		// platforms where rename does not replace an existing file, simply
		// found it already there): the existing file is an equally valid
		// ciphertext for the same plaintext, so this is still success.
		if _, statErr := os.Stat(dest); statErr == nil {
			return h, nil
		}
		return ContentHash{}, fmt.Errorf("blob: put: rename into place: %w", err)
	}
	cleanup = false
	return h, nil
}

// Get implements BlobStore.
func (s *FSBlobStore) Get(ctx context.Context, userID ulid.ULID, dek []byte, h ContentHash) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	f, err := os.Open(s.blobPath(userID, h))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("blob: get: %w", ErrNotFound)
		}
		return nil, fmt.Errorf("blob: get: open: %w", err)
	}

	keyFor := func(keyID []byte) ([]byte, error) { return dek, nil }
	aad := blobAAD(userID)

	// OpenStream can write already-authenticated leading chunks before a
	// later chunk fails, and the content hash can only be confirmed once
	// every chunk has been read. Rather than buffer the whole plaintext,
	// pipe it: the reader's terminating Read surfaces any auth or
	// hash-mismatch failure via CloseWithError, so callers who only trust
	// output after a clean EOF get the all-or-nothing guarantee without an
	// in-memory copy of the blob.
	pr, pw := io.Pipe()
	go func() {
		defer func() { _ = f.Close() }()
		hasher := sha256.New()
		mw := io.MultiWriter(pw, hasher)
		if err := crypto.OpenStream(mw, f, keyFor, aad); err != nil {
			_ = pw.CloseWithError(fmt.Errorf("blob: get: open stream: %w", err))
			return
		}
		var got ContentHash
		copy(got[:], hasher.Sum(nil))
		if got != h {
			_ = pw.CloseWithError(fmt.Errorf("blob: get: content hash mismatch: want %s, got %s", h, got))
			return
		}
		_ = pw.Close()
	}()

	return pr, nil
}

// Exists implements BlobStore.
func (s *FSBlobStore) Exists(ctx context.Context, userID ulid.ULID, h ContentHash) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	_, err := os.Stat(s.blobPath(userID, h))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, fmt.Errorf("blob: exists: %w", err)
}

// StoredSize implements BlobStore.
func (s *FSBlobStore) StoredSize(ctx context.Context, userID ulid.ULID, h ContentHash) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	fi, err := os.Stat(s.blobPath(userID, h))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, fmt.Errorf("blob: stored size: %w", ErrNotFound)
		}
		return 0, fmt.Errorf("blob: stored size: %w", err)
	}
	return fi.Size(), nil
}

// Delete implements BlobStore. It is idempotent: deleting a blob that does
// not exist is not an error.
func (s *FSBlobStore) Delete(ctx context.Context, userID ulid.ULID, h ContentHash) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Remove(s.blobPath(userID, h)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("blob: delete: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// RECONCILIATION SURFACE (Forgejo #300).
//
// Everything above answers questions about a hash the caller already has. A
// reconciliation asks the opposite question — "what is on disk that nothing in
// the database knows about?" — and there was no way to ask it, which is why
// orphaned blobs accumulated unnoticed.
//
// These are deliberately NOT on the BlobStore interface. They are filesystem
// facts, and an object-store backend would answer them with a LIST call whose
// cost and consistency model are completely different. The report type-asserts
// for them and says so plainly when the backend cannot enumerate, rather than
// silently reporting zero orphans — which is the failure mode this whole issue
// is about.
// ---------------------------------------------------------------------------

// UserPrefix exposes the non-enumerable per-user directory name for userID, so
// a reconciliation can map the directories it finds under the root back to the
// users they belong to. It reveals nothing a caller could not already compute:
// deriving it needs the server key, which this process holds.
func (s *FSBlobStore) UserPrefix(userID ulid.ULID) string { return s.userPrefix(userID) }

// RootPrefixes lists every top-level directory name under the blob root. A name
// that no live user's UserPrefix reproduces is a directory belonging to a user
// who no longer exists — the residue a hard delete leaves behind, and the only
// evidence of it that survives the row being gone.
func (s *FSBlobStore) RootPrefixes(ctx context.Context) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(s.root)
	if errors.Is(err, fs.ErrNotExist) {
		// A root that was never created is not an error for a REPORT: it is the
		// honest answer "no blobs are stored". Distinguishing it from a failed
		// read matters, so only this one condition is folded away.
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("blob: read blob root: %w", err)
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

// UserHashes returns every content hash stored on disk for userID, by walking
// that user's shard directories. A file whose name is not a valid hash is
// returned in the second result rather than skipped: unreadable residue is a
// finding, and dropping it would let the report claim a clean reconciliation
// over files it declined to understand.
func (s *FSBlobStore) UserHashes(ctx context.Context, userID ulid.ULID) ([]ContentHash, []string, error) {
	dir := s.userDir(userID)
	var (
		hashes   []ContentHash
		unparsed []string
	)
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) && path == dir {
				return fs.SkipAll // this user has never stored a blob
			}
			return err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if d.IsDir() {
			return nil
		}
		h, perr := ParseContentHash(d.Name())
		if perr != nil {
			rel, _ := filepath.Rel(dir, path)
			unparsed = append(unparsed, filepath.ToSlash(rel))
			return nil
		}
		hashes = append(hashes, h)
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, nil, fmt.Errorf("blob: walk user blob dir: %w", err)
	}
	return hashes, unparsed, nil
}

// RemoveUserPrefix deletes a user's ENTIRE blob directory and returns how many
// files it removed (#54's hard delete).
//
// ⚠ IT WALKS THE DERIVED PREFIX, NOT app.blobs, and that is the whole point. A
// sweep driven by database rows is only as complete as every producer's
// discipline, and one producer forgot for the life of the project (#316) — a
// row-driven delete would have left every ML model artifact on disk after
// "delete everything about me" reported success. The prefix does not care who
// forgot to write a row.
//
// ⚠ THE COUNT IS TAKEN BEFORE THE REMOVAL AND THE ABSENCE IS CHECKED AFTER.
// os.RemoveAll returns nil for a path it never touched, so "no error" is not
// evidence that anything is gone; a caller told "removed 0 files" when a
// thousand remain is the exact silent-success failure this codebase keeps
// paying for.
func (s *FSBlobStore) RemoveUserPrefix(ctx context.Context, userID ulid.ULID) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	dir := s.userDir(userID)

	before := 0
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			before++
		}
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		// Nothing stored for this user. Not an error, and not a silent zero
		// either: the caller reports the number, and zero here is a true answer.
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("blob: count before removal: %w", err)
	}

	if err := os.RemoveAll(dir); err != nil {
		return 0, fmt.Errorf("blob: remove user prefix: %w", err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
		return 0, fmt.Errorf("blob: user prefix %s still exists after removal", dir)
	}
	return before, nil
}
