// SPDX-License-Identifier: AGPL-3.0-only

//go:build tantivy && cgo

package tantivy_test

import (
	"context"
	"io/fs"
	"path/filepath"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/search"
	"mailroom/internal/search/tantivy"
)

// The Indexer driven against the REAL engine, on disk.
//
// Every other Indexer test uses search.MemoryFactory, whose backend keeps no
// files — so none of them can see a conflict between the indexer's own files and
// the engine's. That blind spot shipped a bug that destroyed the index of every
// real instance on its first cursor write: the cursor file was named "meta.json",
// which is exactly what Tantivy calls its segment metadata, and writeMeta renames
// over it atomically. Search then failed forever with `missing field "segments"`,
// and a fresh instance never had working search at all.
//
// These cases exist to make that class of bug impossible to reintroduce: they
// assert that a cursor write, a close, and a reopen all leave a real on-disk
// index searchable.

// fakeSource is the minimum Source the Indexer needs: one message, one change.
type fakeSource struct {
	user ulid.ULID
	msg  search.Message
	raw  []byte
}

func (f *fakeSource) EmailState(context.Context, ulid.ULID) (int64, error) { return 1, nil }

func (f *fakeSource) EmailChanges(_ context.Context, _ ulid.ULID, since int64, _ int) (search.Changes, error) {
	if since >= 1 {
		return search.Changes{NewState: 1}, nil
	}
	return search.Changes{NewState: 1, Upsert: []ulid.ULID{f.msg.ID}}, nil
}

func (f *fakeSource) Messages(_ context.Context, _ ulid.ULID, ids []ulid.ULID) ([]search.Message, error) {
	for _, id := range ids {
		if id == f.msg.ID {
			return []search.Message{f.msg}, nil
		}
	}
	return nil, nil
}

func (f *fakeSource) MessageIDsAfter(_ context.Context, _ ulid.ULID, after ulid.ULID, _ int) ([]ulid.ULID, error) {
	if after.Compare(f.msg.ID) < 0 {
		return []ulid.ULID{f.msg.ID}, nil
	}
	return nil, nil
}

func (f *fakeSource) Raw(context.Context, ulid.ULID, []byte) ([]byte, error) { return f.raw, nil }

func newFakeSource() *fakeSource {
	user := ulid.Make()
	return &fakeSource{
		user: user,
		msg: search.Message{
			ID:          ulid.Make(),
			ThreadID:    ulid.Make(),
			ContentHash: []byte("hash"),
			Subject:     "Quarterly invoice",
			FromName:    "Alice Example",
			FromAddress: "alice@example.com",
			ReceivedAt:  time.Now().UTC(),
			SentAt:      time.Now().UTC(),
		},
		raw: []byte("From: alice@example.com\r\nSubject: Quarterly invoice\r\n\r\nthe body\r\n"),
	}
}

// findsInvoice reports whether the index can answer a free-text query for the one
// message the fake source holds.
func findsInvoice(t *testing.T, ix *search.Indexer, user ulid.ULID) bool {
	t.Helper()
	res, err := ix.Search(context.Background(), user, search.Parse("invoice", search.ParseOptions{}))
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	return res.Total > 0
}

// A cursor write must not disturb the engine's own files. Pre-fix this failed on
// the very first Sync: writeMeta clobbered Tantivy's meta.json, and the next
// commit died with "No such file or directory".
func TestIndexerCursorWriteDoesNotClobberEngineFiles(t *testing.T) {
	root := t.TempDir()
	src := newFakeSource()

	ix, err := search.NewIndexer(root, tantivy.Open, src, src)
	if err != nil {
		t.Fatalf("NewIndexer: %v", err)
	}
	defer func() { _ = ix.Close() }()

	// Sync indexes the message AND persists the cursor — the write that used to
	// destroy the index.
	if _, err := ix.Sync(context.Background(), src.user); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if !findsInvoice(t, ix, src.user) {
		t.Fatal("indexed message is not searchable after the cursor write")
	}

	// A second Sync commits again against the metadata the first one left behind.
	if _, err := ix.Sync(context.Background(), src.user); err != nil {
		t.Fatalf("second Sync (engine metadata was corrupted by the first): %v", err)
	}

	// The engine's own segment metadata must still be there, alongside — not
	// underneath — ours. (The engine's index lives in a subdirectory of the user's
	// index dir, so this walks rather than assuming the layout.)
	dir := filepath.Join(root, src.user.String())
	var sawEngineMeta bool
	if err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && d.Name() == "meta.json" {
			sawEngineMeta = true
		}
		return nil
	}); err != nil {
		t.Fatalf("walk index dir: %v", err)
	}
	if !sawEngineMeta {
		t.Errorf("Tantivy's meta.json is missing under %s — the indexer overwrote the engine's own metadata", dir)
	}
}

// Mail indexed on a LIVE handle must be searchable on that same handle, without
// a restart. Tantivy's reader serves the segment set it was opened with, so a
// commit is not enough — the reader has to be reloaded. The indexer is long-lived
// and shares its backend with the API, so if this regresses, search silently
// stops seeing new mail the moment the process has been up for one sync cycle,
// and looks perfectly healthy while doing it.
func TestIndexerNewMailIsVisibleWithoutRestart(t *testing.T) {
	root := t.TempDir()
	src := newFakeSource()
	ctx := context.Background()

	ix, err := search.NewIndexer(root, tantivy.Open, src, src)
	if err != nil {
		t.Fatalf("NewIndexer: %v", err)
	}
	defer func() { _ = ix.Close() }()

	if findsInvoice(t, ix, src.user) {
		t.Fatal("the index answered a hit before anything was indexed")
	}
	if _, err := ix.Sync(ctx, src.user); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if !findsInvoice(t, ix, src.user) {
		t.Error("mail indexed on a live handle is not searchable until a restart (reader was never reloaded)")
	}
}

// Reopening a real on-disk index must find the mail that is already in it — the
// whole point of persisting one. Pre-fix, the reopen failed to deserialize the
// (clobbered) metadata and search was permanently broken until the index was
// deleted by hand.
func TestIndexerReopensPersistedIndex(t *testing.T) {
	root := t.TempDir()
	src := newFakeSource()
	ctx := context.Background()

	ix, err := search.NewIndexer(root, tantivy.Open, src, src)
	if err != nil {
		t.Fatalf("NewIndexer: %v", err)
	}
	if _, err := ix.Sync(ctx, src.user); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if err := ix.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := search.NewIndexer(root, tantivy.Open, src, src)
	if err != nil {
		t.Fatalf("reopen NewIndexer: %v", err)
	}
	defer func() { _ = reopened.Close() }()

	if !findsInvoice(t, reopened, src.user) {
		t.Error("the persisted index came back empty or unreadable after a reopen")
	}
}
