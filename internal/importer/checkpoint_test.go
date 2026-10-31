// SPDX-License-Identifier: AGPL-3.0-only

package importer

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
)

func newTestULID() ulid.ULID { return ulid.Make() }

func TestCheckpointRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	if cp, err := LoadCheckpoint(path); err != nil || cp != nil {
		t.Fatalf("LoadCheckpoint(missing) = %v, %v; want nil, nil", cp, err)
	}

	cp := &Checkpoint{
		UserID:          newTestULID().String(),
		Account:         "Archive (imported)",
		Archive:         Fingerprint{Path: "/a/b.mbox", Kind: "mbox", Size: 10, ModTime: time.Unix(5, 0).UTC(), Head: "ff"},
		Position:        Position{Source: "b.mbox", Offset: 4096},
		LastContentHash: "aa",
		Stats:           Stats{Imported: 7},
	}
	if err := cp.Save(path); err != nil {
		t.Fatal(err)
	}
	got, err := LoadCheckpoint(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Position != cp.Position || got.Stats.Imported != 7 || got.UserID != cp.UserID {
		t.Errorf("round trip lost data: %+v", got)
	}
	if !got.Archive.Matches(cp.Archive) {
		t.Errorf("fingerprint did not survive the round trip: %+v", got.Archive)
	}
	if got.Version != checkpointVersion {
		t.Errorf("version = %d, want %d", got.Version, checkpointVersion)
	}
}

func TestCheckpointRejectsAnUnknownVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte(`{"version":999}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCheckpoint(path); err == nil {
		t.Fatal("a checkpoint from the future must be rejected, not misread")
	}
}

func TestFingerprintDetectsAChangedArchive(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "a.mbox", "From x@example.com Thu Jan  1 00:00:00 1970\nSubject: a\n\nx\n")

	fp1, err := FingerprintArchive(path, KindMbox)
	if err != nil {
		t.Fatal(err)
	}
	if !fp1.Matches(fp1) {
		t.Fatal("a fingerprint must match itself")
	}

	// Rewrite the file with DIFFERENT content but the same length, and restore the
	// mtime — the case size+mtime alone would miss, and the reason the head of the
	// file is hashed.
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("From y@example.com Thu Jan  1 00:00:00 1970\nSubject: b\n\nx\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, fi.ModTime(), fi.ModTime()); err != nil {
		t.Fatal(err)
	}
	fp2, err := FingerprintArchive(path, KindMbox)
	if err != nil {
		t.Fatal(err)
	}
	if fp1.Matches(fp2) {
		t.Error("a replaced archive of the same size and mtime must NOT match — " +
			"resuming into it would skip messages that were never imported")
	}
}

func TestSaveIsAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	cp := &Checkpoint{UserID: newTestULID().String(), Position: Position{Source: "a", Offset: 1}}
	if err := cp.Save(path); err != nil {
		t.Fatal(err)
	}
	cp.Position.Offset = 2
	if err := cp.Save(path); err != nil {
		t.Fatal(err)
	}
	// No temp files left behind: a directory that fills with .import-state-*.tmp
	// over a three-hour import is its own kind of outage.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("directory holds %d files, want 1 (the checkpoint)", len(entries))
	}
	got, err := LoadCheckpoint(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Position.Offset != 2 {
		t.Errorf("offset = %d, want the newer 2", got.Position.Offset)
	}
}
