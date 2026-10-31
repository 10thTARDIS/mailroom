// SPDX-License-Identifier: AGPL-3.0-only

package importer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// This file is the durable cursor.
//
// WHY IT LIVES ON DISK, NEXT TO THE ARCHIVE, AND NOT IN POSTGRES
//
// Correctness does not depend on it. Idempotence comes from the content hash
// (ADR-001): re-presenting bytes Mailroom already has deduplicates, so an import
// with NO cursor at all is still safe to re-run — it is merely slow, because it
// re-reads and re-hashes 25 GB to rediscover what it already knows. The cursor
// exists to make a resume fast, and nothing else.
//
// What it describes is a BYTE OFFSET INTO A FILE ON THIS MACHINE — a fact about
// the operator's disk, not about the user's mailbox. It is not JMAP-visible, has
// no RLS story, and would need a schema migration to store; putting a local
// file-scan optimization into the canonical, row-level-secured store would be
// paying a permanent structural cost for a temporary convenience.
//
// The one thing a database-side cursor WOULD buy is consistency with a database
// rollback: restore Postgres to last night's backup and a file cursor still claims
// this morning's messages were imported, so a resume would skip them. That is a
// real hazard, and it is why Validate() PROVES the cursor against the database
// before trusting it: it re-checks that the last message the checkpoint claims to
// have committed is actually in app.messages. A rolled-back or swapped database
// fails that check and the import starts from zero — correct, just slower.
//
// Ordering is what makes it safe: the checkpoint is written strictly AFTER the
// ingest transaction commits, so it can only ever LAG reality. A crash therefore
// re-reads a handful of already-imported messages, which dedup absorbs. It can
// never run AHEAD and skip a message that was never stored.

// checkpointVersion is bumped when the on-disk shape changes; an unknown version
// is ignored rather than misread.
const checkpointVersion = 1

// Checkpoint is the persisted state of an interrupted import.
type Checkpoint struct {
	Version int    `json:"version"`
	UserID  string `json:"userID"`
	Account string `json:"account"`

	// Archive fingerprints the SOURCE, so a checkpoint can never be applied to a
	// different archive that happens to sit at the same path.
	Archive Fingerprint `json:"archive"`

	// Position is where to resume.
	Position Position `json:"position"`

	// LastContentHash is the content hash of the last message COMMITTED before
	// this checkpoint was written. It is the probe Validate uses to prove the
	// database still contains what the cursor assumes.
	LastContentHash string `json:"lastContentHash,omitempty"`

	Stats     Stats     `json:"stats"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Fingerprint identifies an archive well enough to detect that it changed.
// The head hash catches a file that was replaced in place with one of the same
// size and mtime (a re-export, a restore); size and mtime catch the ordinary case
// without reading anything.
type Fingerprint struct {
	Path    string    `json:"path"`
	Kind    string    `json:"kind"`
	Size    int64     `json:"size,omitempty"`
	ModTime time.Time `json:"modTime,omitempty"`
	Head    string    `json:"head,omitempty"`
}

// headSampleBytes is how much of an archive file is hashed for the fingerprint.
const headSampleBytes = 64 << 10

// FingerprintArchive fingerprints the archive at path. A directory (a maildir, a
// Takeout tree) has no meaningful size or head, so it is identified by path and
// kind alone; the per-folder cursor is name-based and re-validates itself against
// the listing anyway.
func FingerprintArchive(path string, kind Kind) (Fingerprint, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return Fingerprint{}, fmt.Errorf("importer: resolve archive path: %w", err)
	}
	fp := Fingerprint{Path: filepath.ToSlash(abs), Kind: string(kind)}
	fi, err := os.Stat(abs)
	if err != nil {
		return Fingerprint{}, fmt.Errorf("importer: stat archive: %w", err)
	}
	if fi.IsDir() {
		return fp, nil
	}
	fp.Size = fi.Size()
	fp.ModTime = fi.ModTime().UTC()

	f, err := os.Open(abs) //nolint:gosec // the archive path is an operator argument
	if err != nil {
		return Fingerprint{}, fmt.Errorf("importer: open archive: %w", err)
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.CopyN(h, f, headSampleBytes); err != nil && !errors.Is(err, io.EOF) {
		return Fingerprint{}, fmt.Errorf("importer: fingerprint archive: %w", err)
	}
	fp.Head = hex.EncodeToString(h.Sum(nil))
	return fp, nil
}

// Matches reports whether two fingerprints describe the same archive.
func (f Fingerprint) Matches(o Fingerprint) bool {
	return f.Path == o.Path && f.Kind == o.Kind &&
		f.Size == o.Size && f.ModTime.Equal(o.ModTime) && f.Head == o.Head
}

// LoadCheckpoint reads a checkpoint. A missing file is not an error (nil, nil):
// there is simply nothing to resume from.
func LoadCheckpoint(path string) (*Checkpoint, error) {
	b, err := os.ReadFile(path) //nolint:gosec // the state path is an operator argument
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil //nolint:nilnil // "no checkpoint" is not a failure
		}
		return nil, fmt.Errorf("importer: read checkpoint: %w", err)
	}
	var cp Checkpoint
	if err := json.Unmarshal(b, &cp); err != nil {
		return nil, fmt.Errorf("importer: parse checkpoint %s: %w", path, err)
	}
	if cp.Version != checkpointVersion {
		return nil, fmt.Errorf("importer: checkpoint %s has version %d, want %d",
			path, cp.Version, checkpointVersion)
	}
	return &cp, nil
}

// Save writes the checkpoint durably: a temp file, fsync, atomic rename. A
// half-written cursor that survived a crash and pointed into the middle of a
// message would be worse than no cursor at all.
func (cp *Checkpoint) Save(path string) error {
	cp.Version = checkpointVersion
	cp.UpdatedAt = time.Now().UTC()
	b, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return fmt.Errorf("importer: encode checkpoint: %w", err)
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".import-state-*.tmp")
	if err != nil {
		return fmt.Errorf("importer: create checkpoint temp: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }() // no-op once the rename succeeds

	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("importer: write checkpoint: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("importer: sync checkpoint: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("importer: close checkpoint: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("importer: install checkpoint: %w", err)
	}
	return nil
}

// Validate decides whether a checkpoint may be trusted to skip work.
//
// Three questions, all of which must be yes: is it for THIS user and THIS
// archive, and — the one that matters — does the DATABASE still contain the last
// message it claims to have committed? That last probe is what makes a file-side
// cursor safe against a database that was restored, swapped or wiped underneath
// it: on a mismatch the cursor is discarded and the import starts over, which
// costs time and cannot lose mail.
func (cp *Checkpoint) Validate(ctx context.Context, st *store.Store, userID ulid.ULID, fp Fingerprint) (bool, string) {
	if cp.UserID != userID.String() {
		return false, "it belongs to a different user"
	}
	if !cp.Archive.Matches(fp) {
		return false, "the archive has changed since it was written"
	}
	if cp.LastContentHash == "" {
		return false, "it records no committed message"
	}
	sum, err := hex.DecodeString(cp.LastContentHash)
	if err != nil || len(sum) != sha256.Size {
		return false, "its last-message hash is malformed"
	}
	var found bool
	if err := st.UserTx(ctx, userID, func(tx *store.UserTx) error {
		_, ok, e := tx.MessageIDByContentHash(ctx, sum)
		found = ok
		return e
	}); err != nil {
		return false, "the database could not be probed: " + err.Error()
	}
	if !found {
		return false, "the last message it committed is NOT in the database " +
			"(restored or replaced?) — restarting the import from the beginning"
	}
	return true, ""
}
