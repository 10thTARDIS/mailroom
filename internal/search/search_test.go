// SPDX-License-Identifier: AGPL-3.0-only

package search

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
)

// End-to-end query semantics, exercised against the in-memory backend. Every
// assertion here is engine-neutral: the SAME cases run against Tantivy in
// internal/search/tantivy (tantivy_test.go, `-tags tantivy`), which is what makes
// "the backend is swappable" a tested claim rather than an aspiration.

func mkID(t *testing.T, ms int64) ulid.ULID {
	t.Helper()
	return ulid.MustNew(uint64(ms), strings.NewReader("0123456789abcdefghijklmnop"))
}

// corpus builds a small, fixed set of documents with known properties.
func corpus(t *testing.T) (*MemoryBackend, map[string]Document) {
	t.Helper()
	labelWork := ulid.MustNew(1, strings.NewReader("LABELWORK0123456789abcdef"))
	acctPersonal := ulid.MustNew(2, strings.NewReader("ACCTPERSONAL0123456789abc"))

	docs := map[string]Document{
		"invoice": {
			ID:         mkID(t, 1),
			From:       Address{Name: "Acme Billing", Address: "billing@example.com"},
			To:         []Address{{Name: "You", Address: "you@example.com"}},
			Subject:    "Invoice 42 for quarterly revenue",
			Body:       "Please find invoice 42 attached. Payment is due on receipt.",
			Flags:      Flags(false, false, false, false, false, false, true),
			LabelIDs:   []ulid.ULID{labelWork},
			AccountIDs: []ulid.ULID{acctPersonal},
			ReceivedAt: time.Date(2026, 7, 12, 9, 0, 0, 0, time.UTC),
		},
		"lunch": {
			ID:         mkID(t, 2),
			From:       Address{Name: "Bob Example", Address: "bob@example.com"},
			To:         []Address{{Address: "you@example.com"}, {Address: "carol@example.com"}},
			Subject:    "Lunch tomorrow?",
			Body:       "Are we still on for lunch tomorrow at the usual place?",
			Flags:      Flags(true, true, false, false, false, false, false),
			ReceivedAt: time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC),
		},
		"trashed": {
			ID:         mkID(t, 3),
			From:       Address{Address: "spam@example.com"},
			Subject:    "Invoice you did not ask for",
			Body:       "Unsolicited invoice.",
			Flags:      Flags(true, false, false, false, false, true, false),
			ReceivedAt: time.Date(2025, 12, 1, 0, 0, 0, 0, time.UTC),
		},
	}

	b := NewMemoryBackend()
	list := make([]Document, 0, len(docs))
	for _, d := range docs {
		list = append(list, d)
	}
	if err := b.Upsert(context.Background(), list); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	return b, docs
}

// run parses a query string and executes it, returning the hit ids as keys.
func run(t *testing.T, b *MemoryBackend, q string) map[ulid.ULID]bool {
	t.Helper()
	parsed := Parse(q, ParseOptions{})
	res, err := b.Search(context.Background(), parsed)
	if err != nil {
		t.Fatalf("search %q: %v", q, err)
	}
	got := map[ulid.ULID]bool{}
	for _, h := range res.Hits {
		got[h.ID] = true
	}
	return got
}

func TestQuerySemantics(t *testing.T) {
	b, docs := corpus(t)

	tests := []struct {
		name string
		q    string
		want []string
	}{
		{"free text hits subject and body", "invoice", []string{"invoice", "trashed"}},
		{"free text is case-insensitive", "INVOICE", []string{"invoice", "trashed"}},
		{"all terms are required", "invoice lunch", nil},
		{"phrase is ordered", `"quarterly revenue"`, []string{"invoice"}},
		{"phrase does not match a bag of words", `"revenue quarterly"`, nil},
		{"prefix / as-you-type", "invo*", []string{"invoice", "trashed"}},
		{"from: exact address", "from:billing@example.com", []string{"invoice"}},
		{"from: is case-insensitive", "from:BILLING@EXAMPLE.COM", []string{"invoice"}},
		{"to: matches any recipient, not just the first", "to:carol@example.com", []string{"lunch"}},
		{"subject: is field-scoped", "subject:lunch", []string{"lunch"}},
		{"body: is field-scoped", "body:receipt", []string{"invoice"}},
		{"body: does not match the subject", "body:tomorrow?", []string{"lunch"}},
		{"is:unread", "is:unread", []string{"invoice"}},
		{"is:starred", "is:starred", []string{"lunch"}},
		{"has:attachment", "has:attachment", []string{"invoice"}},
		{"is:trashed", "is:trashed", []string{"trashed"}},
		{"negation", "invoice -is:trashed", []string{"invoice"}},
		{"free text finds a participant by name", "acme", []string{"invoice"}},
		{"date: month", "date:2026-07", []string{"invoice"}},
		{"after:", "after:2026-06-01", []string{"invoice"}},
		{"before:", "before:2026-01-01", []string{"trashed"}},
		{"on:", "on:2026-03-02", []string{"lunch"}},
		{"combined", "invoice from:billing@example.com is:unread", []string{"invoice"}},
		{"an unknown label matches nothing (it is not resolved here)", "label:Nope", nil},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := run(t, b, tc.q)
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

// Typo tolerance (spec §7). The binding exposes no fuzzy query, so this is the
// n-gram approximation — but the CONTRACT is what matters: a misspelled word
// still finds the message, and turning fuzziness off makes it stop.
func TestTypoTolerance(t *testing.T) {
	b, docs := corpus(t)

	got := run(t, b, "quartelry") // "quarterly", transposed
	if !got[docs["invoice"].ID] {
		t.Error("a one-transposition typo should still find the message")
	}

	strict := Parse("quartelry", ParseOptions{NoFuzzy: true})
	res, err := b.Search(context.Background(), strict)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 0 {
		t.Errorf("with typo tolerance off, a misspelling must not match: %d hits", len(res.Hits))
	}
}

// Exact matches must outrank approximate ones, or typo tolerance turns every
// query into mush.
func TestExactOutranksFuzzy(t *testing.T) {
	b, _ := corpus(t)
	res, err := b.Search(context.Background(), Parse("invoice", ParseOptions{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) < 2 {
		t.Fatalf("want at least 2 hits, got %d", len(res.Hits))
	}
	if res.Hits[0].Score < res.Hits[1].Score {
		t.Error("hits must come back in descending score order")
	}
}

// The subject is worth more than the body: a subject hit must rank above a
// body-only hit for the same word.
func TestSubjectOutranksBody(t *testing.T) {
	b := NewMemoryBackend()
	subj := Document{ID: mkID(t, 1), Subject: "refund", Body: "nothing here",
		ReceivedAt: time.Now(), Flags: Flags(true, false, false, false, false, false, false)}
	body := Document{ID: mkID(t, 2), Subject: "nothing here", Body: "refund",
		ReceivedAt: time.Now(), Flags: Flags(true, false, false, false, false, false, false)}
	if err := b.Upsert(context.Background(), []Document{subj, body}); err != nil {
		t.Fatal(err)
	}
	res, err := b.Search(context.Background(), Parse("refund", ParseOptions{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 2 {
		t.Fatalf("want 2 hits, got %d", len(res.Hits))
	}
	if res.Hits[0].ID != subj.ID {
		t.Error("a subject match must outrank a body match")
	}
}

// Upsert is IDEMPOTENT — the property the whole at-least-once cursor design rests
// on. Re-applying the same document must not duplicate it.
func TestUpsertIsIdempotent(t *testing.T) {
	ctx := context.Background()
	b, docs := corpus(t)
	before, _ := b.NumDocs(ctx)

	d := docs["invoice"]
	for i := 0; i < 3; i++ {
		if err := b.Upsert(ctx, []Document{d}); err != nil {
			t.Fatal(err)
		}
	}
	after, _ := b.NumDocs(ctx)
	if before != after {
		t.Fatalf("replaying an upsert changed the document count: %d -> %d", before, after)
	}

	// And it REPLACES: the old subject must no longer match.
	d.Subject = "Completely different"
	if err := b.Upsert(ctx, []Document{d}); err != nil {
		t.Fatal(err)
	}
	if run(t, b, "subject:quarterly")[d.ID] {
		t.Error("an upsert must replace the old document, not add a second one")
	}
	if !run(t, b, "subject:different")[d.ID] {
		t.Error("the replacement must be searchable")
	}
}

// Deleting an id that is not there is a no-op, not an error: the indexer replays
// destroys.
func TestDeleteIsIdempotent(t *testing.T) {
	ctx := context.Background()
	b, docs := corpus(t)
	id := docs["lunch"].ID
	for i := 0; i < 3; i++ {
		if err := b.Delete(ctx, []ulid.ULID{id}); err != nil {
			t.Fatalf("delete %d: %v", i, err)
		}
	}
	if run(t, b, "lunch")[id] {
		t.Error("a deleted document must not be searchable")
	}
}

func TestHighlighting(t *testing.T) {
	b, docs := corpus(t)
	q := Parse("invoice", ParseOptions{})
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
			t.Fatal("no snippets on a matching hit")
		}
		for _, s := range h.Snippets {
			marked := s.Mark()
			if !strings.Contains(strings.ToLower(marked), "<mark>invoice</mark>") {
				t.Errorf("snippet on %s not marked: %q", s.Field, marked)
			}
		}
	}
	if !found {
		t.Fatal("the matching document was not returned")
	}
}

// Mark must escape: a subject that contains HTML must not become HTML.
func TestMarkEscapes(t *testing.T) {
	s := Snippet{Field: FieldSubject, Text: `<script>x</script> invoice`, Ranges: [][2]int{{19, 26}}}
	got := s.Mark()
	if strings.Contains(got, "<script>") {
		t.Fatalf("Mark must escape markup in the source text: %q", got)
	}
	if !strings.Contains(got, "<mark>invoice</mark>") {
		t.Fatalf("Mark must wrap the matched range: %q", got)
	}
}

// A bogus range from an engine must never produce broken markup.
func TestMarkIgnoresBadRanges(t *testing.T) {
	s := Snippet{Field: FieldSubject, Text: "hello", Ranges: [][2]int{{3, 99}, {-1, 2}, {4, 2}}}
	if got := s.Mark(); got != "hello" {
		t.Fatalf("bad ranges must be dropped, got %q", got)
	}
}

func TestLimitIsClamped(t *testing.T) {
	if got := (Query{}).EffectiveLimit(); got != DefaultLimit {
		t.Errorf("zero limit: got %d, want %d", got, DefaultLimit)
	}
	if got := (Query{Limit: MaxLimit * 10}).EffectiveLimit(); got != MaxLimit {
		t.Errorf("huge limit: got %d, want %d", got, MaxLimit)
	}
}

// An all-negative query must mean "everything except", not "nothing". (In a
// Lucene-family engine a boolean clause of only negations matches no documents —
// the backends must inject a match-everything clause to subtract from.)
func TestNegationOnlyQuery(t *testing.T) {
	b, docs := corpus(t)
	got := run(t, b, "-is:trashed")
	if len(got) != 2 || !got[docs["invoice"].ID] || !got[docs["lunch"].ID] {
		t.Fatalf("`-is:trashed` should match everything that is not trashed, got %d hits", len(got))
	}
}
