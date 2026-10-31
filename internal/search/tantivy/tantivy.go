// SPDX-License-Identifier: AGPL-3.0-only

//go:build tantivy && cgo

package tantivy

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"

	tantivy "github.com/anyproto/tantivy-go"

	"mailroom/internal/search"
)

// The embedded-Tantivy SearchBackend (ADR-006). This file is the ONLY place in
// Mailroom where an engine type appears; everything it exports is in Mailroom's
// own vocabulary.

// Tokenizer names registered on every index.
const (
	tokText  = tantivy.TokenizerSimple // lowercase + English stemming + positions
	tokRaw   = tantivy.TokenizerRaw    // the whole value as ONE token (exact match)
	tokNgram = tantivy.TokenizerNgram  // character n-grams: the typo-tolerance field
)

// n-gram bounds for the fuzzy field. 3–4 character grams over subject +
// participants: long enough to be selective, short enough that a single-character
// typo still leaves most grams intact (which is what makes the match survive).
const (
	ngramMin = 3
	ngramMax = 4
)

// textLimit caps how many bytes of a text field the analyzer will tokenize.
// Generous: the caller already caps the body at search.MaxBodyBytes, and a
// silently dropped tail would be a silently unsearchable message.
const textLimit = search.MaxBodyBytes

// noneToken is the value a "match nothing" clause looks for in the id field. No
// document can carry it: ids are 26-character Crockford base32 (ADR-007).
const noneToken = "__mailroom_matches_nothing__"

// storedFields are the fields serialized back with a hit. Tantivy computes
// highlights over the STORED fields of a document, so subject and body are stored
// precisely so they can be highlighted; nothing else is stored, which keeps the
// highlighter from producing nonsense fragments over id/flag/label values.
var storedFields = []string{
	string(search.FieldID),
	fieldReceived,
	string(search.FieldSubject),
	string(search.FieldBody),
}

// hitFields are the fields serialized when no highlighting is asked for. A hit
// then needs only its id, date and score (Score is the engine's, not a stored
// field). Skipping the subject and body avoids reading and serializing a
// message's whole text for every one of up to search.MaxLimit hits (#769).
var hitFields = []string{
	string(search.FieldID),
	fieldReceived,
}

// fieldReceived stores received_at as RFC 3339 so a hit carries its date without
// a second database round trip. (It is not range-queryable — the binding exposes
// no numeric fields — which is why date filters go through the bucket tokens in
// search.FieldDate.)
const fieldReceived = "received"

// libInit runs tantivy-go's one-time global initialization exactly once per
// process, no matter how many user indexes are opened.
var libInit sync.Once
var libInitErr error

// Available reports whether this build carries the Tantivy engine.
func Available() bool { return true }

// Open opens (creating if absent) the Tantivy index in dir. The caller
// (search.Indexer) has already created dir 0700; this must not widen it, and
// Tantivy only creates files inside it.
func Open(_ context.Context, dir string) (search.SearchBackend, error) {
	libInit.Do(func() {
		// cleanOnPanic: unwind native state rather than leaving the process in a
		// half-dead state. utf8Lenient: mail is full of mislabelled charsets, and
		// a hard failure on one bad byte would cost us the whole message.
		libInitErr = tantivy.LibInit(true, true, "error")
	})
	if libInitErr != nil {
		return nil, fmt.Errorf("search/tantivy: init: %w", libInitErr)
	}

	// Open with the current schema; failing that, with each previous one in
	// turn. Tantivy refuses to open an index with a schema other than the one
	// it was written with, which would wedge the indexer forever — the
	// schema-version bump that is supposed to trigger the background rebuild
	// could never be reached (#712). An old index opened with its own schema
	// keeps serving everything it can (Search refuses, with
	// ErrIndexUpgrading, what it cannot) while Indexer.Sync rebuilds it, and the
	// rebuild swaps in an index built with the current schema. When the schema
	// changes again, bump schemaCurrent and teach buildSchema the new version.
	var (
		tc      *tantivy.TantivyContext
		version int
		err     error
	)
	for version = schemaCurrent; version >= 1; version-- {
		schema, serr := buildSchema(version)
		if serr != nil {
			return nil, serr
		}
		tc, err = tantivy.NewTantivyContextWithSchema(dir, schema)
		if err == nil || !strings.Contains(err.Error(), "schema does not match") {
			break
		}
	}
	if err != nil {
		return nil, fmt.Errorf("search/tantivy: open index at %s: %w", dir, err)
	}
	if err := tc.RegisterTextAnalyzerSimple(tokText, textLimit, tantivy.English); err != nil {
		_ = tc.Close()
		return nil, fmt.Errorf("search/tantivy: register text analyzer: %w", err)
	}
	if err := tc.RegisterTextAnalyzerRaw(tokRaw); err != nil {
		_ = tc.Close()
		return nil, fmt.Errorf("search/tantivy: register raw analyzer: %w", err)
	}
	// prefixOnly=false: grams from anywhere in the token, so a typo in the middle
	// of a word is still recoverable (edge n-grams would only survive typos in the
	// tail).
	if err := tc.RegisterTextAnalyzerNgram(tokNgram, ngramMin, ngramMax, false); err != nil {
		_ = tc.Close()
		return nil, fmt.Errorf("search/tantivy: register ngram analyzer: %w", err)
	}
	return &backend{tc: tc, version: version}, nil
}

// schemaCurrent is the schema version buildSchema builds for a new index. It
// tracks search's own index schema version (search.FieldSchemaVersion names the
// version each field arrived in).
const schemaCurrent = 3

// buildSchema declares the index schema at a version. Bump search's
// schemaVersion whenever it changes: every user's index is then rebuilt in the
// background, old one serving until the swap (spec §7).
//
// Each version is the previous one plus the fields search.FieldSchemaVersion
// assigns to it: v2 (#712) the from_domain / to_domain fields, v3 (#722/#723)
// list, size, deliveredto, deliveredto_domain and filename. Versions below
// schemaCurrent are kept only to open an index written before a bump.
func buildSchema(version int) (*tantivy.Schema, error) {
	sb, err := tantivy.NewSchemaBuilder()
	if err != nil {
		return nil, fmt.Errorf("search/tantivy: schema builder: %w", err)
	}

	// exact(name): raw, unanalyzed, one token. The value indexed and the value
	// queried must be byte-identical — search.ExactValue guarantees that at both
	// ends.
	exact := func(name string, stored bool) error {
		return sb.AddTextField(name, stored, false, false, tantivy.IndexRecordOptionBasic, tokRaw)
	}
	// text(name): analyzed with positions, so phrase queries work.
	text := func(name string, stored bool) error {
		return sb.AddTextField(name, stored, true, false, tantivy.IndexRecordOptionWithFreqsAndPositions, tokText)
	}

	for _, f := range []struct {
		name   string
		stored bool
	}{
		{string(search.FieldID), true},
		{fieldReceived, true},
		{string(search.FieldThread), false},
		{string(search.FieldFrom), false},
		{string(search.FieldTo), false},
		{string(search.FieldLabel), false},
		{string(search.FieldMailbox), false},
		{string(search.FieldAccount), false},
		{string(search.FieldFlag), false},
		{string(search.FieldKeyword), false},
		{string(search.FieldDate), false},
	} {
		if err := exact(f.name, f.stored); err != nil {
			return nil, fmt.Errorf("search/tantivy: add field %s: %w", f.name, err)
		}
	}
	if version >= 2 {
		for _, f := range []search.Field{search.FieldFromDomain, search.FieldToDomain} {
			if err := exact(string(f), false); err != nil {
				return nil, fmt.Errorf("search/tantivy: add field %s: %w", f, err)
			}
		}
	}
	if version >= 3 {
		for _, f := range []search.Field{search.FieldList, search.FieldSize,
			search.FieldDeliveredTo, search.FieldDeliveredToDomain} {
			if err := exact(string(f), false); err != nil {
				return nil, fmt.Errorf("search/tantivy: add field %s: %w", f, err)
			}
		}
		// Analyzed, so filename:invoice finds "Invoice 2026-03.pdf" and
		// filename:pdf its extension; not stored, so it is never highlighted.
		if err := text(string(search.FieldFilename), false); err != nil {
			return nil, fmt.Errorf("search/tantivy: add field filename: %w", err)
		}
	}
	if err := text(string(search.FieldSubject), true); err != nil {
		return nil, fmt.Errorf("search/tantivy: add field subject: %w", err)
	}
	if err := text(string(search.FieldBody), true); err != nil {
		return nil, fmt.Errorf("search/tantivy: add field body: %w", err)
	}
	if err := text(string(search.FieldPeople), false); err != nil {
		return nil, fmt.Errorf("search/tantivy: add field people: %w", err)
	}
	// The typo-tolerance field: n-grammed, frequencies only (no positions — an
	// n-gram phrase means nothing), not stored (it is a search structure, not
	// content).
	if err := sb.AddTextField(string(search.FieldFuzzy), false, true,
		false, tantivy.IndexRecordOptionWithFreqs, tokNgram); err != nil {
		return nil, fmt.Errorf("search/tantivy: add field fuzzy: %w", err)
	}

	schema, err := sb.BuildSchema()
	if err != nil {
		return nil, fmt.Errorf("search/tantivy: build schema: %w", err)
	}
	return schema, nil
}

// backend is one user's Tantivy index.
type backend struct {
	mu sync.Mutex // serializes writes; TantivyContext is not documented as write-safe
	tc *tantivy.TantivyContext

	// version is the schema the index was opened with (see Open). Below
	// schemaCurrent the index is an old one being rebuilt: fields newer than it
	// are neither written nor queryable. It is transient — the indexer rebuilds
	// it into a current-schema index straight away.
	version int
}

// indexedFields are the fields a Document contributes values to, in schema order.
var indexedFields = []search.Field{
	search.FieldID, search.FieldThread, search.FieldFrom, search.FieldTo,
	search.FieldFromDomain, search.FieldToDomain,
	search.FieldLabel, search.FieldMailbox, search.FieldAccount,
	search.FieldFlag, search.FieldKeyword, search.FieldDate,
	search.FieldList, search.FieldSize, search.FieldDeliveredTo, search.FieldDeliveredToDomain,
	search.FieldSubject, search.FieldBody, search.FieldPeople, search.FieldFuzzy, search.FieldFilename,
}

// Upsert indexes documents idempotently: BatchAddAndDeleteDocumentsWithOpstamp
// deletes by id first and adds second, in ONE commit. That is what makes the
// change-log consumer's at-least-once delivery safe — replaying a change
// re-writes the same document instead of duplicating it.
func (b *backend) Upsert(_ context.Context, docs []search.Document) error {
	if len(docs) == 0 {
		return nil
	}
	tdocs := make([]*tantivy.Document, 0, len(docs))
	ids := make([]string, 0, len(docs))

	for _, d := range docs {
		td := tantivy.NewDocument()
		if td == nil {
			return fmt.Errorf("search/tantivy: allocate document")
		}
		for _, f := range indexedFields {
			if search.FieldSchemaVersion(f) > b.version {
				continue // an old index being rebuilt has no such field
			}
			for _, v := range d.Values(f) {
				// Multi-valued fields are just repeated AddField calls: a term
				// query on any one value matches (verified against the binding).
				if err := td.AddField(v, b.tc, string(f)); err != nil {
					td.Free()
					return fmt.Errorf("search/tantivy: add field %s: %w", f, err)
				}
			}
		}
		if !d.ReceivedAt.IsZero() {
			if err := td.AddField(d.ReceivedAt.UTC().Format(rfc3339), b.tc, fieldReceived); err != nil {
				td.Free()
				return fmt.Errorf("search/tantivy: add field %s: %w", fieldReceived, err)
			}
		}
		tdocs = append(tdocs, td)
		ids = append(ids, d.ID.String())
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if _, err := b.tc.BatchAddAndDeleteDocumentsWithOpstamp(tdocs, string(search.FieldID), ids); err != nil {
		return fmt.Errorf("search/tantivy: upsert %d documents: %w", len(tdocs), err)
	}
	// A commit alone does NOT make the new segments visible: the reader holds the
	// segment set it was opened with, so without this the freshly indexed mail is
	// invisible to every Search on this handle until the process restarts and the
	// reader is rebuilt. The indexer is long-lived and shares this handle with the
	// API, so skipping it means search silently never sees new mail.
	if err := b.tc.ReloadReader(); err != nil {
		return fmt.Errorf("search/tantivy: reload reader after upsert: %w", err)
	}
	return nil
}

// rfc3339 is the stored received_at format (UTC, second precision — a mail's
// received time is not a benchmark timestamp).
const rfc3339 = "2006-01-02T15:04:05Z"

// Delete removes documents by id.
func (b *backend) Delete(_ context.Context, ids []ulid.ULID) error {
	if len(ids) == 0 {
		return nil
	}
	vals := make([]string, 0, len(ids))
	for _, id := range ids {
		vals = append(vals, id.String())
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.tc.DeleteDocuments(string(search.FieldID), vals...); err != nil {
		return fmt.Errorf("search/tantivy: delete %d documents: %w", len(vals), err)
	}
	// Same as Upsert: without the reload the reader keeps serving the segments the
	// deleted documents still live in, so a destroyed message goes on appearing in
	// search results for the life of the process.
	if err := b.tc.ReloadReader(); err != nil {
		return fmt.Errorf("search/tantivy: reload reader after delete: %w", err)
	}
	return nil
}

// NumDocs reports the live document count.
func (b *backend) NumDocs(_ context.Context) (uint64, error) {
	n, err := b.tc.NumDocs()
	if err != nil {
		return 0, fmt.Errorf("search/tantivy: num docs: %w", err)
	}
	return n, nil
}

// Close flushes and releases the native index.
// PathBound: Tantivy writes each new segment under the directory the index was
// opened on, so the indexer must reopen a rebuilt index at its live path.
func (b *backend) PathBound() bool { return true }

func (b *backend) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.tc == nil {
		return nil
	}
	err := b.tc.Close()
	b.tc = nil
	if err != nil {
		return fmt.Errorf("search/tantivy: close: %w", err)
	}
	return nil
}

// Search compiles Mailroom's AST into Tantivy's query JSON and runs it.
func (b *backend) Search(_ context.Context, q search.Query) (search.Result, error) {
	if b.version < schemaCurrent && search.RequiredSchemaVersion(search.Expand(q)) > b.version {
		// The old index has no such field — or, for a newer flag atom, has
		// the field but never wrote the atom. Either way its answer would be
		// "nothing matched", which is not the truth (#712).
		return search.Result{}, search.ErrIndexUpgrading
	}
	qb := tantivy.NewQueryBuilder()
	// The top level goes through addGroup, not addNode: a query whose whole root
	// is a negation (`-is:trashed`) would otherwise compile to a boolean with a
	// single MustNot clause and no positive one — which matches NOTHING in a
	// Lucene-family engine, turning "everything except trash" into "nothing".
	// addGroup injects the AllQuery to subtract from.
	addGroup(qb, []search.Node{search.Expand(q)}, tantivy.Must)
	final := qb.Build()

	sctx := tantivy.NewSearchContextBuilder().
		SetQueryFromJson(&final).
		SetDocsLimit(uintptr(q.EffectiveLimit())).
		SetWithHighlights(q.Highlight).
		Build()

	res, err := b.tc.SearchJson(sctx)
	if err != nil {
		return search.Result{}, fmt.Errorf("search/tantivy: search: %w", err)
	}
	defer res.Free()

	size, err := res.GetSize()
	if err != nil {
		return search.Result{}, fmt.Errorf("search/tantivy: result size: %w", err)
	}

	fields := hitFields
	if q.Highlight {
		fields = storedFields
	}
	hits := make([]search.Hit, 0, size)
	for i := uint64(0); i < size; i++ {
		doc, err := res.Get(i)
		if err != nil {
			return search.Result{}, fmt.Errorf("search/tantivy: result %d: %w", i, err)
		}
		js, err := doc.ToJson(b.tc, fields...)
		doc.Free()
		if err != nil {
			return search.Result{}, fmt.Errorf("search/tantivy: result %d to json: %w", i, err)
		}
		hit, err := decodeHit(js)
		if err != nil {
			return search.Result{}, err
		}
		hits = append(hits, hit)
	}
	return search.Result{Hits: hits, Total: len(hits)}, nil
}

// rawHit is the shape SearchJson serializes a result document as.
type rawHit struct {
	ID         string  `json:"id"`
	Received   string  `json:"received"`
	Score      float64 `json:"score"`
	Highlights []struct {
		FieldName string `json:"field_name"`
		Fragment  struct {
			R [][2]int `json:"r"`
			T string   `json:"t"`
		} `json:"fragment"`
	} `json:"highlights"`
}

// decodeHit converts one engine result document into a Mailroom Hit. Anything the
// engine reports that we do not understand is dropped, not propagated: a snippet
// on an id field, an out-of-range offset, or an unparseable date must never reach
// a client.
func decodeHit(js string) (search.Hit, error) {
	var rh rawHit
	if err := json.Unmarshal([]byte(js), &rh); err != nil {
		return search.Hit{}, fmt.Errorf("search/tantivy: decode hit %q: %w", js, err)
	}
	id, err := ulid.Parse(rh.ID)
	if err != nil {
		return search.Hit{}, fmt.Errorf("search/tantivy: hit id %q: %w", rh.ID, err)
	}
	hit := search.Hit{ID: id, Score: rh.Score}
	if rh.Received != "" {
		if t, err := parseTime(rh.Received); err == nil {
			hit.ReceivedAt = t
		}
	}
	for _, h := range rh.Highlights {
		f := search.Field(h.FieldName)
		if f.IsExact() {
			continue // no snippets over ids/flags/addresses
		}
		hit.Snippets = append(hit.Snippets, search.Snippet{
			Field:  f,
			Text:   h.Fragment.T,
			Ranges: h.Fragment.R,
		})
	}
	return hit, nil
}

// addNode compiles one Mailroom AST node into the engine's query builder under
// the given modifier.
//
// The one non-obvious rule: a boolean clause made ONLY of negations matches
// nothing in Tantivy (as in Lucene), so an all-negative group gets an explicit
// AllQuery to subtract from. `-is:read` must mean "everything that is not read",
// not "nothing".
func addNode(qb *tantivy.QueryBuilder, n search.Node, mod tantivy.QueryModifier) {
	switch v := n.(type) {
	case search.And:
		nb := qb.NestedBuilder()
		addGroup(nb, v.Nodes, tantivy.Must)
		qb.BooleanQuery(mod, nb, 1.0)
	case search.Or:
		if len(v.Nodes) == 0 {
			addNone(qb, mod)
			return
		}
		nb := qb.NestedBuilder()
		addGroup(nb, v.Nodes, tantivy.Should)
		qb.BooleanQuery(mod, nb, 1.0)
	case search.Not:
		if mod == tantivy.Must {
			// The common, cheap case: subtract directly from the parent clause.
			addNode(qb, v.Node, tantivy.MustNot)
			return
		}
		nb := qb.NestedBuilder()
		nb.AllQuery(tantivy.Must, 1.0)
		addNode(nb, v.Node, tantivy.MustNot)
		qb.BooleanQuery(mod, nb, 1.0)
	case search.All:
		qb.AllQuery(mod, 1.0)
	case search.None:
		addNone(qb, mod)
	case search.Term:
		addTerm(qb, v, mod)
	default:
		// A node the engine cannot evaluate (a store-side predicate that
		// slipped past Service.Run's guard) must not vanish from its group —
		// that would widen the query. Match nothing instead.
		addNone(qb, mod)
	}
}

// addGroup compiles the children of an And (or of the top level), injecting an
// AllQuery when every child is a negation — see addNode.
//
// The injection applies to MUST groups only. In a SHOULD group each negated
// child is already compiled as a self-contained "everything except X" subquery
// (addNode's non-Must branch), so injecting a second AllQuery beside it would
// widen the group to "everything" instead.
func addGroup(nb *tantivy.QueryBuilder, nodes []search.Node, mod tantivy.QueryModifier) {
	if mod == tantivy.Must {
		positive := false
		for _, c := range nodes {
			if _, isNot := c.(search.Not); !isNot {
				positive = true
				break
			}
		}
		if !positive {
			nb.AllQuery(tantivy.Must, 1.0)
		}
	}
	for _, c := range nodes {
		addNode(nb, c, mod)
	}
}

// addNone adds a clause no document can satisfy.
func addNone(qb *tantivy.QueryBuilder, mod tantivy.QueryModifier) {
	qb.Query(mod, string(search.FieldID), noneToken, tantivy.TermQuery, 1.0)
}

// addTerm compiles a leaf.
//
// Match semantics, and why each maps the way it does:
//
//	MatchExact  → TermQuery on a raw field. The raw analyzer passes the query
//	              text through as a single token, so the value must already be
//	              normalized the way the index normalized it — search.ExactValue
//	              is that single source of truth.
//	MatchToken  → EveryTermQuery: the field's analyzer tokenizes the query text
//	              (lowercasing and stemming it, so `subject:INVOICES` finds
//	              "invoice"), and ALL resulting tokens are required. A plain
//	              TermQuery would silently use only the first token, so a
//	              two-word value would quietly match on one word.
//	MatchPhrase → PhraseQuery: analyzed, position-aware, ordered.
//	MatchPrefix → TermPrefixQuery: as-you-type.
//	MatchFuzzy  → OneOfTermQuery over the n-gram field: the query word is
//	              n-grammed by the field's analyzer and the grams are OR-ed with
//	              decaying weights, so a misspelling that preserves most grams
//	              still scores. This is Mailroom's typo tolerance — the binding
//	              exposes no FuzzyTermQuery at all (docs/search-spike.md §6).
func addTerm(qb *tantivy.QueryBuilder, t search.Term, mod tantivy.QueryModifier) {
	boost := t.Boost
	if boost == 0 {
		boost = 1
	}
	value := t.Value
	if t.Field.IsExact() {
		value = search.ExactValue(t.Field, value)
	}
	if value == "" {
		// An empty clause must not silently widen a Must group.
		if mod == tantivy.Must {
			addNone(qb, mod)
		}
		return
	}

	var qt tantivy.QueryType
	switch t.Match {
	case search.MatchPhrase:
		qt = tantivy.PhraseQuery
	case search.MatchPrefix:
		qt = tantivy.TermPrefixQuery
	case search.MatchFuzzy:
		qt = tantivy.OneOfTermQuery
	case search.MatchExact:
		qt = tantivy.TermQuery
	default:
		qt = tantivy.EveryTermQuery
	}
	qb.Query(mod, string(t.Field), value, qt, boost)
}

// parseTime parses the stored received_at.
func parseTime(s string) (time.Time, error) { return time.Parse(rfc3339, s) }
