// SPDX-License-Identifier: AGPL-3.0-only

//go:build tantivy && cgo

package tantivy_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"mailroom/internal/search"
	"mailroom/internal/search/tantivy"
)

// BenchmarkSearchLimit measures what a candidate search costs at the hit limit
// Email/query uses, over a corpus whose bodies are a few KB (#769). Run:
//
//	GOTESTFLAGS='-run XXX -bench BenchmarkSearchLimit -benchtime 20x' sh scripts/tantivy-test.sh
//
// Run it once on the code before the hit-field change (every stored field is
// serialized per hit) and once after (id and received only) to compare.
func BenchmarkSearchLimit(b *testing.B) {
	const n = 20000
	bk, err := tantivy.Open(context.Background(), b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = bk.Close() })
	para := strings.Repeat("The quarterly statement is attached; please review the figures and reply before Friday. ", 40)
	docs := make([]search.Document, 0, 1000)
	for i := 0; i < n; i++ {
		docs = append(docs, search.Document{
			ID:         mkID(int64(i+1), fmt.Sprintf("B%07d", i)),
			From:       search.Address{Name: "Bulk Sender", Address: "bulk@example.com"},
			Subject:    fmt.Sprintf("Statement %d", i),
			Body:       fmt.Sprintf("%s reference %d", para, i),
			ReceivedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Minute),
		})
		if len(docs) == cap(docs) {
			if err := bk.Upsert(context.Background(), docs); err != nil {
				b.Fatal(err)
			}
			docs = docs[:0]
		}
	}
	q := search.Parse("from:bulk@example.com", search.ParseOptions{})
	for _, c := range []struct {
		name  string
		limit int
	}{{"limit500", 500}, {"limit5000", 5000}} {
		b.Run(c.name, func(b *testing.B) {
			qq := q
			qq.Limit = c.limit
			for i := 0; i < b.N; i++ {
				res, err := bk.Search(context.Background(), qq)
				if err != nil {
					b.Fatal(err)
				}
				if len(res.Hits) != c.limit {
					b.Fatalf("got %d hits, want %d", len(res.Hits), c.limit)
				}
			}
		})
	}
}
