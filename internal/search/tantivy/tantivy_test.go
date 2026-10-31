// SPDX-License-Identifier: AGPL-3.0-only

//go:build tantivy && cgo

package tantivy_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/search"
	"mailroom/internal/search/tantivy"
)

// The REAL engine, exercised through Mailroom's interface only. Run with:
//
//	CGO_ENABLED=1 go test -tags tantivy ./internal/search/...
//
// (after dropping libtantivy_go.a on the linker path — see doc.go and
// scripts/tantivy-test.sh).
//
// These cases deliberately mirror the ones the in-memory backend answers in
// internal/search/search_test.go. That is the point: "the backend is swappable"
// is only true if two backends give the same answers to the same Mailroom query,
// and this is where that claim is checked against the engine we actually ship.

func mkID(ms int64, seed string) ulid.ULID {
	return ulid.MustNew(uint64(ms), strings.NewReader(seed+"0123456789abcdefghijklmnop"))
}

func open(t *testing.T) search.SearchBackend {
	t.Helper()
	b, err := tantivy.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b
}

type doc struct {
	key string
	d   search.Document
}

func corpus(t *testing.T) (search.SearchBackend, map[string]search.Document) {
	t.Helper()
	labelWork := mkID(10, "LABELWORK")
	acctPersonal := mkID(11, "ACCTPERSON")

	docs := []doc{
		{"invoice", search.Document{
			ID:         mkID(1, "AAA"),
			From:       search.Address{Name: "Acme Billing", Address: "Billing@Example.com"},
			To:         []search.Address{{Address: "you@example.com"}, {Address: "carol@example.com"}},
			Subject:    "Invoice 42 for quarterly revenue",
			Body:       "Please find invoice 42 attached. Payment is due on receipt.",
			Flags:      search.Flags(false, false, false, false, false, false, true),
			LabelIDs:   []ulid.ULID{labelWork},
			AccountIDs: []ulid.ULID{acctPersonal},
			ReceivedAt: time.Date(2026, 7, 12, 9, 0, 0, 0, time.UTC),
		}},
		{"lunch", search.Document{
			ID:         mkID(2, "BBB"),
			From:       search.Address{Name: "Bob Example", Address: "bob@example.com"},
			To:         []search.Address{{Address: "you@example.com"}},
			Subject:    "Lunch tomorrow?",
			Body:       "Are we still on for lunch tomorrow at the usual place?",
			Flags:      search.Flags(true, true, false, false, false, false, false),
			ReceivedAt: time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC),
		}},
		{"trashed", search.Document{
			ID:         mkID(3, "CCC"),
			From:       search.Address{Address: "spam@example.com"},
			Subject:    "Invoice you did not ask for",
			Body:       "Unsolicited invoice.",
			Flags:      search.Flags(true, false, false, false, false, true, false),
			ReceivedAt: time.Date(2025, 12, 1, 0, 0, 0, 0, time.UTC),
		}},
	}

	b := open(t)
	list := make([]search.Document, 0, len(docs))
	byKey := make(map[string]search.Document, len(docs))
	for _, d := range docs {
		list = append(list, d.d)
		byKey[d.key] = d.d
	}
	if err := b.Upsert(context.Background(), list); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	return b, byKey
}

func hits(t *testing.T, b search.SearchBackend, q string) map[ulid.ULID]bool {
	t.Helper()
	res, err := b.Search(context.Background(), search.Parse(q, search.ParseOptions{}))
	if err != nil {
		t.Fatalf("search %q: %v", q, err)
	}
	out := map[ulid.ULID]bool{}
	for _, h := range res.Hits {
		out[h.ID] = true
	}
	return out
}

func TestTantivyQuerySemantics(t *testing.T) {
	b, docs := corpus(t)

	tests := []struct {
		name string
		q    string
		want []string
	}{
		{"free text", "invoice", []string{"invoice", "trashed"}},
		{"case-insensitive", "INVOICE", []string{"invoice", "trashed"}},
		{"all terms required", "invoice lunch", nil},
		{"stemming (the engine's analyzer)", "invoices", []string{"invoice", "trashed"}},
		{"phrase is ordered", `"quarterly revenue"`, []string{"invoice"}},
		{"phrase is not a bag of words", `"revenue quarterly"`, nil},
		{"prefix", "invo*", []string{"invoice", "trashed"}},
		{"from: exact", "from:billing@example.com", []string{"invoice"}},
		{"from: normalizes case at BOTH ends", "from:BILLING@EXAMPLE.COM", []string{"invoice"}},
		{"to: matches any recipient (multi-valued field)", "to:carol@example.com", []string{"invoice"}},
		{"subject: is scoped", "subject:lunch", []string{"lunch"}},
		{"body: is scoped", "body:receipt", []string{"invoice"}},
		{"body: does not see the subject", "body:tomorrow", []string{"lunch"}},
		{"is:unread", "is:unread", []string{"invoice"}},
		{"is:starred", "is:starred", []string{"lunch"}},
		{"has:attachment", "has:attachment", []string{"invoice"}},
		{"is:trashed", "is:trashed", []string{"trashed"}},
		{"negation", "invoice -is:trashed", []string{"invoice"}},
		{"negation-only means everything-except", "-is:trashed", []string{"invoice", "lunch"}},
		{"participant by display name", "acme", []string{"invoice"}},
		{"date bucket: month", "date:2026-07", []string{"invoice"}},
		{"date range: after", "after:2026-06-01", []string{"invoice"}},
		{"date range: before", "before:2026-01-01", []string{"trashed"}},
		{"date range: on", "on:2026-03-02", []string{"lunch"}},
		{"date range spanning months", "after:2026-01-01 before:2026-08-01", []string{"invoice", "lunch"}},
		{"combined", "invoice from:billing@example.com is:unread", []string{"invoice"}},
		{"an unresolved label name matches nothing", "label:Work", nil},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := hits(t, b, tc.q)
			if len(got) != len(tc.want) {
				t.Fatalf("%q: got %d hits, want %d (%v)", tc.q, len(got), len(tc.want), tc.want)
			}
			for _, key := range tc.want {
				if !got[docs[key].ID] {
					t.Errorf("%q: missing %s", tc.q, key)
				}
			}
		})
	}
}

// Id-valued fields match on the id, which is how label:/account: work once the
// Service has resolved the name.
func TestTantivyIDFields(t *testing.T) {
	b, docs := corpus(t)
	ctx := context.Background()

	q := search.Query{Root: search.IDTerm(search.FieldLabel, docs["invoice"].LabelIDs[0])}
	res, err := b.Search(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 1 || res.Hits[0].ID != docs["invoice"].ID {
		t.Fatalf("label id query: got %d hits", len(res.Hits))
	}

	q = search.Query{Root: search.IDTerm(search.FieldAccount, docs["invoice"].AccountIDs[0])}
	res, err = b.Search(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 1 {
		t.Fatalf("account id query: got %d hits", len(res.Hits))
	}
}

// Typo tolerance (spec §7). The binding exposes NO FuzzyTermQuery
// (docs/search-spike.md §6); this is the n-gram field standing in for it, and
// this test is the proof that the requirement is actually met.
func TestTantivyTypoTolerance(t *testing.T) {
	b, docs := corpus(t)

	if !hits(t, b, "quartelry")[docs["invoice"].ID] { // "quarterly", transposed
		t.Error("a transposition typo should still find the message")
	}

	res, err := b.Search(context.Background(), search.Parse("quartelry", search.ParseOptions{NoFuzzy: true}))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 0 {
		t.Errorf("with typo tolerance off, a misspelling must not match: %d hits", len(res.Hits))
	}
}

// Exact spelling must outrank the fuzzy match, or typo tolerance ruins ranking.
func TestTantivyExactOutranksFuzzy(t *testing.T) {
	b := open(t)
	ctx := context.Background()

	exact := search.Document{ID: mkID(1, "EXACT"), Subject: "quarterly report",
		ReceivedAt: time.Now().UTC(), Flags: search.Flags(true, false, false, false, false, false, false)}
	near := search.Document{ID: mkID(2, "NEAR"), Subject: "quarterlies reporting quartelry",
		ReceivedAt: time.Now().UTC(), Flags: search.Flags(true, false, false, false, false, false, false)}
	if err := b.Upsert(ctx, []search.Document{exact, near}); err != nil {
		t.Fatal(err)
	}

	res, err := b.Search(ctx, search.Parse("quarterly", search.ParseOptions{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) == 0 {
		t.Fatal("no hits")
	}
	if res.Hits[0].ID != exact.ID {
		t.Error("the exactly spelled match must rank first")
	}
}

// Idempotent upsert — the property the change-log cursor's at-least-once delivery
// depends on. Replaying a change must not duplicate a document.
func TestTantivyUpsertIsIdempotent(t *testing.T) {
	ctx := context.Background()
	b, docs := corpus(t)

	before, err := b.NumDocs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	d := docs["invoice"]
	for i := 0; i < 3; i++ {
		if err := b.Upsert(ctx, []search.Document{d}); err != nil {
			t.Fatal(err)
		}
	}
	after, err := b.NumDocs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("replay duplicated documents: %d -> %d", before, after)
	}

	// And it truly REPLACES.
	d.Subject = "Completely different heading"
	if err := b.Upsert(ctx, []search.Document{d}); err != nil {
		t.Fatal(err)
	}
	if hits(t, b, "subject:quarterly")[d.ID] {
		t.Error("the old document survived an upsert")
	}
	if !hits(t, b, "subject:heading")[d.ID] {
		t.Error("the replacement is not searchable")
	}
}

func TestTantivyDelete(t *testing.T) {
	ctx := context.Background()
	b, docs := corpus(t)
	id := docs["lunch"].ID

	// Deleting twice is a no-op, not an error (the indexer replays destroys).
	for i := 0; i < 2; i++ {
		if err := b.Delete(ctx, []ulid.ULID{id}); err != nil {
			t.Fatalf("delete %d: %v", i, err)
		}
	}
	if hits(t, b, "lunch")[id] {
		t.Error("a deleted document is still searchable")
	}
}

// Highlighting (spec §7), and the Mailroom-side markup that hides the engine's.
func TestTantivyHighlighting(t *testing.T) {
	b, docs := corpus(t)

	q := search.Parse("invoice", search.ParseOptions{})
	q.Highlight = true
	res, err := b.Search(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}

	var found bool
	for _, h := range res.Hits {
		if h.ID != docs["invoice"].ID {
			continue
		}
		found = true
		if len(h.Snippets) == 0 {
			t.Fatal("a matching hit came back with no snippets")
		}
		var marked string
		for _, s := range h.Snippets {
			if s.Field.IsExact() {
				t.Errorf("an exact field (%s) must never produce a snippet", s.Field)
			}
			marked += s.Mark()
		}
		if !strings.Contains(strings.ToLower(marked), "<mark>invoice</mark>") {
			t.Errorf("nothing was marked: %q", marked)
		}
	}
	if !found {
		t.Fatal("the matching document was not returned")
	}
}

// The stored received_at comes back on the hit, so a caller can order or window
// without a second database round trip.
func TestTantivyHitCarriesReceivedAt(t *testing.T) {
	b, docs := corpus(t)
	res, err := b.Search(context.Background(), search.Query{Root: search.IDTerm(search.FieldID, docs["invoice"].ID)})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 1 {
		t.Fatalf("got %d hits", len(res.Hits))
	}
	if !res.Hits[0].ReceivedAt.Equal(docs["invoice"].ReceivedAt) {
		t.Errorf("receivedAt: got %s, want %s", res.Hits[0].ReceivedAt, docs["invoice"].ReceivedAt)
	}
}

// A match-everything query must work (a filter-only search has no text clause to
// hang the query on; the engine needs an explicit AllQuery).
func TestTantivyMatchAll(t *testing.T) {
	b, _ := corpus(t)
	res, err := b.Search(context.Background(), search.Query{Root: search.All{}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 3 {
		t.Fatalf("match-all returned %d of 3", len(res.Hits))
	}
}

// None must match nothing — the compiled form of an unresolvable name.
func TestTantivyMatchNone(t *testing.T) {
	b, _ := corpus(t)
	res, err := b.Search(context.Background(), search.Query{Root: search.None{}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 0 {
		t.Fatalf("None matched %d documents", len(res.Hits))
	}
}

// The limit is honored (and clamped).
func TestTantivyLimit(t *testing.T) {
	b, _ := corpus(t)
	q := search.Query{Root: search.All{}, Limit: 1}
	res, err := b.Search(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 1 {
		t.Fatalf("limit=1 returned %d hits", len(res.Hits))
	}
}

// An index reopened from disk still serves what was written — the index survives
// a restart, which is what makes the cursor worth persisting.
func TestTantivySurvivesReopen(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	b, err := tantivy.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	d := search.Document{ID: mkID(1, "PERSIST"), Subject: "Invoice 42",
		Body: "zorbex", ReceivedAt: time.Now().UTC(),
		Flags: search.Flags(true, false, false, false, false, false, false)}
	if err := b.Upsert(ctx, []search.Document{d}); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}

	b2, err := tantivy.Open(ctx, dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = b2.Close() }()

	res, err := b2.Search(ctx, search.Parse("zorbex", search.ParseOptions{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 1 || res.Hits[0].ID != d.ID {
		t.Fatalf("the reopened index lost the document: %d hits", len(res.Hits))
	}
}

func TestTantivyAvailable(t *testing.T) {
	if !tantivy.Available() {
		t.Fatal("built with -tags tantivy but Available() is false")
	}
}
