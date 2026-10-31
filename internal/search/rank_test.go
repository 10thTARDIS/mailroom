// SPDX-License-Identifier: AGPL-3.0-only

package search

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
)

// The relevance order Email/query pages through (#724) and the search-latency
// timing hook. Both are contracts of the Indexer/Service, not of a backend.

// reversingBackend wraps a backend and hands its hits back in REVERSE — an
// engine whose tie order (or whole order) differs from Mailroom's. Indexer.Search
// must still return RankHits order.
type reversingBackend struct{ SearchBackend }

func (r reversingBackend) Search(ctx context.Context, q Query) (Result, error) {
	res, err := r.SearchBackend.Search(ctx, q)
	slices.Reverse(res.Hits)
	return res, err
}

func TestRankHitsIsATotalOrder(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	id := func(n uint64) ulid.ULID { return ulid.MustNew(n, strings.NewReader(strings.Repeat("r", 16))) }
	want := []Hit{
		{ID: id(5), Score: 9, ReceivedAt: t0},
		{ID: id(4), Score: 3, ReceivedAt: t0.Add(2 * time.Hour)}, // tie on score: newer first
		{ID: id(1), Score: 3, ReceivedAt: t0},                    // tie on score and date: id
		{ID: id(2), Score: 3, ReceivedAt: t0},
		{ID: id(3), Score: 1, ReceivedAt: t0.Add(9 * time.Hour)},
	}
	// Every rotation of the input lands in the same order.
	for r := range want {
		in := append(slices.Clone(want[r:]), want[:r]...)
		slices.Reverse(in)
		RankHits(in)
		for i := range want {
			if in[i].ID != want[i].ID {
				t.Fatalf("rotation %d: position %d = %v, want %v", r, i, in[i].ID, want[i].ID)
			}
		}
	}
}

func TestIndexerSearchReturnsRankOrderWhateverTheEngineDoes(t *testing.T) {
	ctx := context.Background()
	user := testUser(t)
	fs := newFakeStore()
	// Equal scores for all three (one subject word each), different ages.
	a := msg(t, 1, "invoice", "")
	a.ReceivedAt = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	b := msg(t, 2, "invoice", "")
	b.ReceivedAt = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	c := msg(t, 3, "invoice", "")
	c.ReceivedAt = time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	for _, m := range []Message{a, b, c} {
		fs.put(user, m, "created")
	}
	factory := func(ctx context.Context, dir string) (SearchBackend, error) {
		be, err := MemoryFactory(ctx, dir)
		return reversingBackend{be}, err
	}
	ix, err := NewIndexer(t.TempDir(), factory, fs, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ix.Close() })
	if _, err := ix.Sync(ctx, user); err != nil {
		t.Fatal(err)
	}
	res, err := ix.Search(ctx, user, Parse("invoice", ParseOptions{}))
	if err != nil {
		t.Fatal(err)
	}
	got := make([]ulid.ULID, 0, len(res.Hits))
	for _, h := range res.Hits {
		got = append(got, h.ID)
	}
	want := []ulid.ULID{b.ID, c.ID, a.ID}
	if !slices.Equal(got, want) {
		t.Fatalf("hits = %v, want newest-first among equal scores %v", got, want)
	}
}

// The timing hook sees every index read, labelled by kind, and nothing else.
func TestServiceTimingObservesSearchAndSnippet(t *testing.T) {
	ctx := context.Background()
	base, user, ids := serviceFixture(t)
	var kinds []TimingKind
	svc := NewService(base.ix, base.resolver, WithSearchTiming(func(k TimingKind, d time.Duration) {
		if d < 0 {
			t.Errorf("negative duration %v", d)
		}
		kinds = append(kinds, k)
	}))

	if _, err := svc.Search(ctx, user, "invoice", SearchOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Run(ctx, user, Parse("invoice", ParseOptions{})); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Snippets(ctx, user, "invoice", []ulid.ULID{ids["work"]}, SearchOptions{}); err != nil {
		t.Fatal(err)
	}
	want := []TimingKind{TimingSearch, TimingSearch, TimingSnippet}
	if !slices.Equal(kinds, want) {
		t.Fatalf("observed %v, want %v", kinds, want)
	}

	// A query refused before the engine runs (a store-side predicate) costs no
	// engine time and is not observed as a search.
	kinds = nil
	if _, err := svc.Run(ctx, user, Query{Root: StoreTerm{Kind: StoreArea, Value: "Work"}}); err == nil {
		t.Fatal("a store predicate must be refused")
	}
	if len(kinds) != 0 {
		t.Fatalf("a refused query was timed: %v", kinds)
	}
}
