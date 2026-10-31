// SPDX-License-Identifier: AGPL-3.0-only

package search

import (
	"context"
	"sort"
	"strings"
	"sync"
	"unicode"

	"github.com/oklog/ulid/v2"
)

// MemoryBackend is a small, honest, pure-Go SearchBackend.
//
// It exists for two reasons, and neither of them is "be a search engine":
//
//  1. It is the ORACLE the engine-neutral layers are tested against. The AST,
//     the parser, the date cover, the change-log indexer, the cursor, and the
//     reindex path all get exercised at full fidelity with no CGO, no native
//     library, and no database — which is what makes `go test ./...` pass on a
//     bare toolchain (CLAUDE.md: unit tests must pass with no database present).
//  2. It keeps a CGO-less build FUNCTIONAL rather than broken: a binary built
//     without the `tantivy` tag still answers text queries (in RAM, rebuilt from
//     Postgres on start) instead of failing every search. That is the correct
//     degradation for a disposable cache.
//
// It is NOT production search: everything lives in RAM, scoring is a crude sum of
// matched-clause boosts rather than BM25, and there is no stemming (Tantivy's
// analyzed fields stem with a real English stemmer, so "invoices" finds
// "invoice" there and not here). Nothing chooses it in production — the
// Dockerfile builds with the tantivy tag — and Indexer logs loudly when it is in
// use.
type MemoryBackend struct {
	mu   sync.RWMutex
	docs map[ulid.ULID]Document
}

// NewMemoryBackend returns an empty in-memory backend.
func NewMemoryBackend() *MemoryBackend {
	return &MemoryBackend{docs: make(map[ulid.ULID]Document)}
}

// MemoryFactory is a BackendFactory over MemoryBackend. The directory is
// ignored: nothing is persisted, so the cursor written next to it always reads
// back as "empty index, reindex from scratch" — exactly the behavior a volatile
// cache should have.
func MemoryFactory(_ context.Context, _ string) (SearchBackend, error) {
	return NewMemoryBackend(), nil
}

// Upsert indexes docs, replacing any document with the same id (idempotent).
func (m *MemoryBackend) Upsert(_ context.Context, docs []Document) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, d := range docs {
		m.docs[d.ID] = d
	}
	return nil
}

// Delete removes documents by id; unknown ids are ignored.
func (m *MemoryBackend) Delete(_ context.Context, ids []ulid.ULID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, id := range ids {
		delete(m.docs, id)
	}
	return nil
}

// NumDocs reports the live document count.
func (m *MemoryBackend) NumDocs(_ context.Context) (uint64, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return uint64(len(m.docs)), nil
}

// Close releases nothing (there is nothing to release).
func (m *MemoryBackend) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.docs = make(map[ulid.ULID]Document)
	return nil
}

// Search evaluates the query tree against every document. O(corpus) per query —
// fine for tests and a single-user fallback, hopeless at archive scale, which is
// why Tantivy exists.
func (m *MemoryBackend) Search(_ context.Context, q Query) (Result, error) {
	root := expand(q.Root, q.Fuzzy)

	m.mu.RLock()
	defer m.mu.RUnlock()

	hits := make([]Hit, 0, len(m.docs))
	for _, d := range m.docs {
		score, ok := evalNode(root, d)
		if !ok {
			continue
		}
		h := Hit{ID: d.ID, Score: score, ReceivedAt: d.ReceivedAt}
		if q.Highlight {
			h.Snippets = memHighlight(root, d, q.HighlightFields)
		}
		hits = append(hits, h)
	}
	// Deterministic order: score desc, then newest first, then id — so a test
	// asserting on ranking is not asserting on map iteration order.
	sort.Slice(hits, func(i, j int) bool {
		switch {
		case hits[i].Score != hits[j].Score:
			return hits[i].Score > hits[j].Score
		case !hits[i].ReceivedAt.Equal(hits[j].ReceivedAt):
			return hits[i].ReceivedAt.After(hits[j].ReceivedAt)
		default:
			return hits[i].ID.Compare(hits[j].ID) < 0
		}
	})
	if lim := q.limit(); len(hits) > lim {
		hits = hits[:lim]
	}
	return Result{Hits: hits, Total: len(hits)}, nil
}

// evalNode reports whether d matches n, and the score it earns.
func evalNode(n Node, d Document) (float64, bool) {
	switch v := n.(type) {
	case All:
		return 1, true
	case None:
		return 0, false
	case And:
		total := 0.0
		for _, c := range v.Nodes {
			s, ok := evalNode(c, d)
			if !ok {
				return 0, false
			}
			total += s
		}
		return total, true
	case Or:
		total, any := 0.0, false
		for _, c := range v.Nodes {
			if s, ok := evalNode(c, d); ok {
				any = true
				total += s
			}
		}
		return total, any
	case Not:
		if _, ok := evalNode(v.Node, d); ok {
			return 0, false
		}
		return 0, true
	case Term:
		return evalTerm(v, d)
	default:
		return 0, false
	}
}

func evalTerm(t Term, d Document) (float64, bool) {
	boost := t.Boost
	if boost == 0 {
		boost = 1
	}
	if t.Field.IsExact() {
		want := t.Value
		if !isCaseSensitiveField(t.Field) {
			want = normalizeExact(want)
		}
		for _, v := range d.Values(t.Field) {
			if v == want {
				return boost, true
			}
		}
		return 0, false
	}

	text := strings.Join(d.Values(t.Field), " ")
	switch t.Match {
	case MatchPhrase:
		if containsPhrase(text, t.Value) {
			return boost, true
		}
	case MatchPrefix:
		// Tantivy's TermPrefixQuery analyzes the query text and prefix-matches its
		// FIRST token; do the same, or `body:invo` and `body:Invo` would behave
		// differently across backends.
		want := analyze(t.Value)
		if len(want) == 0 {
			return 0, false
		}
		for _, tok := range analyze(text) {
			if strings.HasPrefix(tok.text, want[0].text) {
				return boost, true
			}
		}
	case MatchFuzzy:
		if fuzzyMatch(text, t.Value) {
			return boost, true
		}
	default: // MatchToken
		// The query VALUE is analyzed too (as Tantivy does), and every token it
		// yields must be present — the EveryTermQuery semantics the Tantivy
		// backend compiles MatchToken to. Without analyzing the value, `body:
		// tomorrow?` would look for the literal token "tomorrow?" and find
		// nothing, while Tantivy would find "tomorrow".
		want := analyze(t.Value)
		if len(want) == 0 {
			return 0, false
		}
		have := make(map[string]bool)
		for _, tok := range analyze(text) {
			have[tok.text] = true
		}
		for _, w := range want {
			if !have[w.text] {
				return 0, false
			}
		}
		return boost, true
	}
	return 0, false
}

// containsPhrase reports whether the analyzed text contains the analyzed phrase
// as a consecutive token run.
func containsPhrase(text, phrase string) bool {
	want := analyze(phrase)
	if len(want) == 0 {
		return false
	}
	have := analyze(text)
	for i := 0; i+len(want) <= len(have); i++ {
		match := true
		for j := range want {
			if have[i+j].text != want[j].text {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// fuzzyMatch approximates the n-gram typo tolerance the Tantivy backend gets
// from an n-gram-tokenized field: a query term matches when a token in the text
// shares enough trigrams with it. Same intent, same shape, deliberately not the
// same scoring.
func fuzzyMatch(text, term string) bool {
	q := trigrams(strings.ToLower(term))
	if len(q) == 0 {
		return strings.Contains(strings.ToLower(text), strings.ToLower(term))
	}
	for _, tok := range analyze(text) {
		t := trigrams(tok.text)
		if len(t) == 0 {
			continue
		}
		common := 0
		for g := range q {
			if t[g] {
				common++
			}
		}
		// Half the query's trigrams present is enough to survive one typo in a
		// word of ordinary length.
		if float64(common) >= 0.5*float64(len(q)) {
			return true
		}
	}
	return false
}

func trigrams(s string) map[string]bool {
	out := map[string]bool{}
	r := []rune(s)
	for i := 0; i+3 <= len(r); i++ {
		out[string(r[i:i+3])] = true
	}
	return out
}

// memToken is one analyzed token with its byte offsets in the source text (the
// offsets are what highlighting needs).
type memToken struct {
	text     string
	lo, hi   int
	position int
}

// analyze is the memory backend's tokenizer: lowercase, split on non-letter/
// non-digit. No stemming (see the type comment).
func analyze(s string) []memToken {
	var out []memToken
	start := -1
	for i, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if start < 0 {
				start = i
			}
			continue
		}
		if start >= 0 {
			out = append(out, memToken{text: strings.ToLower(s[start:i]), lo: start, hi: i, position: len(out)})
			start = -1
		}
	}
	if start >= 0 {
		out = append(out, memToken{text: strings.ToLower(s[start:]), lo: start, hi: len(s), position: len(out)})
	}
	return out
}

// memHighlight produces snippets for the analyzed fields a query touched.
func memHighlight(n Node, d Document, fields []Field) []Snippet {
	if len(fields) == 0 {
		fields = TextFields()
	}
	want := map[Field]bool{}
	for _, f := range fields {
		want[f] = true
	}
	terms := collectTerms(n)

	var out []Snippet
	for _, f := range fields {
		if f.IsExact() {
			continue
		}
		text := strings.Join(d.Values(f), " ")
		if text == "" {
			continue
		}
		var ranges [][2]int
		for _, tok := range analyze(text) {
			for _, t := range terms {
				if t.Field != f && t.Field != FieldAny {
					continue
				}
				if matchesToken(t, tok.text) {
					ranges = append(ranges, [2]int{tok.lo, tok.hi})
					break
				}
			}
		}
		if len(ranges) == 0 {
			continue
		}
		sort.Slice(ranges, func(i, j int) bool { return ranges[i][0] < ranges[j][0] })
		out = append(out, Snippet{Field: f, Text: text, Ranges: ranges})
	}
	return out
}

func matchesToken(t Term, tok string) bool {
	want := analyze(t.Value)
	switch t.Match {
	case MatchPrefix:
		return len(want) > 0 && strings.HasPrefix(tok, want[0].text)
	case MatchFuzzy:
		return fuzzyMatch(tok, t.Value)
	default: // MatchToken, MatchPhrase — any analyzed token of the value
		for _, w := range want {
			if w.text == tok {
				return true
			}
		}
		return false
	}
}

// collectTerms flattens the positive text terms of a tree (negations contribute
// nothing to highlight).
func collectTerms(n Node) []Term {
	switch v := n.(type) {
	case And:
		var out []Term
		for _, c := range v.Nodes {
			out = append(out, collectTerms(c)...)
		}
		return out
	case Or:
		var out []Term
		for _, c := range v.Nodes {
			out = append(out, collectTerms(c)...)
		}
		return out
	case Term:
		if v.Field.IsExact() {
			return nil
		}
		return []Term{v}
	default:
		return nil
	}
}

var _ SearchBackend = (*MemoryBackend)(nil)
