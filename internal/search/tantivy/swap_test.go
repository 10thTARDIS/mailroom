// SPDX-License-Identifier: AGPL-3.0-only

//go:build tantivy && cgo

package tantivy_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/search"
	"mailroom/internal/search/tantivy"
)

// Mail that arrives AFTER a rebuild must be indexed by the same process.
//
// Reindex builds into build-<id>/ and swaps it in by renaming the directory to
// the live path. The engine opened on build-<id>/ keeps that path, so if the
// backend is not reopened at the live path its next commit writes into a
// directory that no longer exists, and every catch-up fails with "No such file
// or directory" until a restart. Every rebuild hits this: a user's first index
// and the schema-version bump (#712). No earlier test committed after a swap,
// because each fake source went quiet once the rebuild had run. This one keeps
// delivering mail.

// streamSource is a Source whose change stream grows: each added message is one
// more change.
type streamSource struct {
	mu   sync.Mutex
	user ulid.ULID
	msgs []search.Message
}

func (s *streamSource) add(subject string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.msgs = append(s.msgs, search.Message{
		ID: ulid.Make(), ThreadID: ulid.Make(), ContentHash: []byte("h"),
		Subject: subject, FromAddress: "alice@example.com",
		ReceivedAt: time.Now().UTC(), SentAt: time.Now().UTC(),
	})
}

func (s *streamSource) EmailState(context.Context, ulid.ULID) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return int64(len(s.msgs)), nil
}

func (s *streamSource) EmailChanges(_ context.Context, _ ulid.ULID, since int64, _ int) (search.Changes, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch := search.Changes{NewState: int64(len(s.msgs))}
	for i := since; i < int64(len(s.msgs)); i++ {
		ch.Upsert = append(ch.Upsert, s.msgs[i].ID)
	}
	return ch, nil
}

func (s *streamSource) Messages(_ context.Context, _ ulid.ULID, ids []ulid.ULID) ([]search.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []search.Message
	for _, id := range ids {
		for _, m := range s.msgs {
			if m.ID == id {
				out = append(out, m)
			}
		}
	}
	return out, nil
}

func (s *streamSource) MessageIDsAfter(_ context.Context, _ ulid.ULID, after ulid.ULID, _ int) ([]ulid.ULID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []ulid.ULID
	for _, m := range s.msgs {
		if after.Compare(m.ID) < 0 {
			out = append(out, m.ID)
		}
	}
	return out, nil
}

func (s *streamSource) Raw(context.Context, ulid.ULID, []byte) ([]byte, error) {
	return []byte("Subject: x\r\n\r\nbody\r\n"), nil
}

func TestIndexerIndexesNewMailAfterARebuild(t *testing.T) {
	ctx := context.Background()
	src := &streamSource{user: ulid.Make()}
	src.add("Quarterly invoice")

	ix, err := search.NewIndexer(t.TempDir(), tantivy.Open, src, src)
	if err != nil {
		t.Fatalf("NewIndexer: %v", err)
	}
	defer func() { _ = ix.Close() }()

	// A new index has no metadata, so this first Sync is a rebuild and swap.
	prog, err := ix.Sync(ctx, src.user)
	if err != nil {
		t.Fatalf("first Sync: %v", err)
	}
	if !prog.Rebuilt {
		t.Fatal("a new index was not built through Reindex; this test no longer covers the swap")
	}

	// Mail arrives after the swap, in the same process.
	src.add("Shipping notice")
	if _, err := ix.Sync(ctx, src.user); err != nil {
		t.Fatalf("Sync after the swap (the backend still points at the build directory): %v", err)
	}
	res, err := ix.Search(ctx, src.user, search.Parse("shipping", search.ParseOptions{}))
	if err != nil || res.Total != 1 {
		t.Fatalf("mail indexed after the swap: total=%d err=%v, want 1", res.Total, err)
	}

	// An explicit reindex-user swaps again; the stream must keep landing.
	if _, err := ix.Reindex(ctx, src.user); err != nil {
		t.Fatalf("Reindex: %v", err)
	}
	src.add("Delivery receipt")
	if _, err := ix.Sync(ctx, src.user); err != nil {
		t.Fatalf("Sync after a second swap: %v", err)
	}
	res, err = ix.Search(ctx, src.user, search.Parse("delivery", search.ParseOptions{}))
	if err != nil || res.Total != 1 {
		t.Fatalf("mail indexed after the second swap: total=%d err=%v, want 1", res.Total, err)
	}
}
