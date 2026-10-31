// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"encoding/json"
	"fmt"
	"os"

	tantivy "github.com/anyproto/tantivy-go"
)

// Field names and tokenizer names used by the spike schema.
const (
	fID      = "id"
	fFrom    = "from"
	fTo      = "to"
	fSubject = "subject"
	fBody    = "body"

	tokText = tantivy.TokenizerSimple // stemming + lowercasing, positions for phrase
	tokRaw  = tantivy.TokenizerRaw    // whole value as one token (exact match)
)

// tantivyBackend is the tantivy-go implementation of Backend.
type tantivyBackend struct {
	ctx *tantivy.TantivyContext
}

// newTantivyBackend builds the schema, opens a fresh index at dir, and
// registers the tokenizers. dir is wiped first so runs are reproducible.
func newTantivyBackend(dir string) (*tantivyBackend, error) {
	if err := tantivy.LibInit(true, true, "error"); err != nil {
		return nil, fmt.Errorf("LibInit: %w", err)
	}

	sb, err := tantivy.NewSchemaBuilder()
	if err != nil {
		return nil, fmt.Errorf("NewSchemaBuilder: %w", err)
	}

	// id: exact, stored, used for delete-by-id.
	if err := sb.AddTextField(fID, true, false, false, tantivy.IndexRecordOptionBasic, tokRaw); err != nil {
		return nil, err
	}
	// from/to: exact address match for field filters (from:/to:).
	if err := sb.AddTextField(fFrom, true, false, false, tantivy.IndexRecordOptionBasic, tokRaw); err != nil {
		return nil, err
	}
	if err := sb.AddTextField(fTo, true, false, false, tantivy.IndexRecordOptionBasic, tokRaw); err != nil {
		return nil, err
	}
	// subject/body: full text with positions so phrase queries work.
	if err := sb.AddTextField(fSubject, true, true, false, tantivy.IndexRecordOptionWithFreqsAndPositions, tokText); err != nil {
		return nil, err
	}
	if err := sb.AddTextField(fBody, true, true, false, tantivy.IndexRecordOptionWithFreqsAndPositions, tokText); err != nil {
		return nil, err
	}

	schema, err := sb.BuildSchema()
	if err != nil {
		return nil, fmt.Errorf("BuildSchema: %w", err)
	}

	_ = os.RemoveAll(dir)
	ctx, err := tantivy.NewTantivyContextWithSchema(dir, schema)
	if err != nil {
		return nil, fmt.Errorf("NewTantivyContextWithSchema: %w", err)
	}

	// text_limit is generous so we never silently drop long tokens.
	if err := ctx.RegisterTextAnalyzerSimple(tokText, 1024, tantivy.English); err != nil {
		return nil, fmt.Errorf("RegisterTextAnalyzerSimple: %w", err)
	}
	if err := ctx.RegisterTextAnalyzerRaw(tokRaw); err != nil {
		return nil, fmt.Errorf("RegisterTextAnalyzerRaw: %w", err)
	}

	return &tantivyBackend{ctx: ctx}, nil
}

func (b *tantivyBackend) Add(docs ...Doc) error {
	tdocs := make([]*tantivy.Document, 0, len(docs))
	for _, d := range docs {
		td := tantivy.NewDocument()
		if td == nil {
			return fmt.Errorf("NewDocument returned nil")
		}
		if err := addField(td, b.ctx, fID, d.ID); err != nil {
			return err
		}
		if err := addField(td, b.ctx, fFrom, d.From); err != nil {
			return err
		}
		if err := addField(td, b.ctx, fTo, d.To); err != nil {
			return err
		}
		if err := addField(td, b.ctx, fSubject, d.Subject); err != nil {
			return err
		}
		if err := addField(td, b.ctx, fBody, d.Body); err != nil {
			return err
		}
		tdocs = append(tdocs, td)
	}
	// AddAndConsumeDocuments commits and reloads the reader (the native side
	// consumes and frees the documents).
	return b.ctx.AddAndConsumeDocuments(tdocs...)
}

func addField(td *tantivy.Document, ctx *tantivy.TantivyContext, name, val string) error {
	if err := td.AddField(val, ctx, name); err != nil {
		return fmt.Errorf("AddField(%s): %w", name, err)
	}
	return nil
}

func (b *tantivyBackend) Delete(ids ...string) error {
	return b.ctx.DeleteDocuments(fID, ids...)
}

func (b *tantivyBackend) NumDocs() (uint64, error) {
	return b.ctx.NumDocs()
}

func (b *tantivyBackend) Close() error {
	return b.ctx.Close()
}

// --- Searcher: build Mailroom-side queries, translate to the tantivy AST. ---

// fullLimit materializes every match — used for exact correctness counts.
// topK mirrors production, where a search fetches only the first page.
const (
	fullLimit = 100000
	topK      = 50
)

func (b *tantivyBackend) run(qb *tantivy.QueryBuilder) ([]Hit, error) {
	return b.runLimited(qb, fullLimit)
}

func (b *tantivyBackend) runLimited(qb *tantivy.QueryBuilder, limit uintptr) ([]Hit, error) {
	fq := qb.Build()
	sctx := tantivy.NewSearchContextBuilder().
		SetQueryFromJson(&fq).
		SetDocsLimit(limit).
		SetWithHighlights(false).
		Build()
	res, err := b.ctx.SearchJson(sctx)
	if err != nil {
		return nil, err
	}
	defer res.Free()

	size, err := res.GetSize()
	if err != nil {
		return nil, err
	}
	hits := make([]Hit, 0, size)
	for i := uint64(0); i < size; i++ {
		doc, err := res.Get(i)
		if err != nil {
			return nil, err
		}
		js, err := doc.ToJson(b.ctx, fID)
		doc.Free()
		if err != nil {
			return nil, err
		}
		// SearchJson results serialize as {"highlights":[],"id":"...","score":N}.
		var rh struct {
			ID    string  `json:"id"`
			Score float64 `json:"score"`
		}
		if err := json.Unmarshal([]byte(js), &rh); err != nil {
			return nil, fmt.Errorf("parse doc json %q: %w", js, err)
		}
		hits = append(hits, Hit{ID: rh.ID, Score: rh.Score})
	}
	return hits, nil
}

func (b *tantivyBackend) Term(field, term string) ([]Hit, error) {
	qb := tantivy.NewQueryBuilder()
	qb.Query(tantivy.Must, field, term, tantivy.TermQuery, 1.0)
	return b.run(qb)
}

func (b *tantivyBackend) Prefix(field, prefix string) ([]Hit, error) {
	qb := tantivy.NewQueryBuilder()
	qb.Query(tantivy.Must, field, prefix, tantivy.TermPrefixQuery, 1.0)
	return b.run(qb)
}

func (b *tantivyBackend) Phrase(field string, words ...string) ([]Hit, error) {
	phrase := ""
	for i, w := range words {
		if i > 0 {
			phrase += " "
		}
		phrase += w
	}
	qb := tantivy.NewQueryBuilder()
	qb.Query(tantivy.Must, field, phrase, tantivy.PhraseQuery, 1.0)
	return b.run(qb)
}

func (b *tantivyBackend) Bool(field string, must, mustNot []string) ([]Hit, error) {
	qb := tantivy.NewQueryBuilder()
	for _, t := range must {
		qb.Query(tantivy.Must, field, t, tantivy.TermQuery, 1.0)
	}
	for _, t := range mustNot {
		qb.Query(tantivy.MustNot, field, t, tantivy.TermQuery, 1.0)
	}
	return b.run(qb)
}

func (b *tantivyBackend) FieldEquals(field, value string) ([]Hit, error) {
	qb := tantivy.NewQueryBuilder()
	qb.Query(tantivy.Must, field, value, tantivy.TermQuery, 1.0)
	return b.run(qb)
}

// benchTerm / benchPhrase / benchBool / benchPrefix mirror a production
// top-K search (fetch first page only), isolating engine+query latency from
// the cost of materializing an unbounded result set.
func (b *tantivyBackend) benchTerm(field, term string) error {
	qb := tantivy.NewQueryBuilder()
	qb.Query(tantivy.Must, field, term, tantivy.TermQuery, 1.0)
	_, err := b.runLimited(qb, topK)
	return err
}

func (b *tantivyBackend) benchPhrase(field string, words ...string) error {
	phrase := ""
	for i, w := range words {
		if i > 0 {
			phrase += " "
		}
		phrase += w
	}
	qb := tantivy.NewQueryBuilder()
	qb.Query(tantivy.Must, field, phrase, tantivy.PhraseQuery, 1.0)
	_, err := b.runLimited(qb, topK)
	return err
}

func (b *tantivyBackend) benchBool(field string, must, mustNot []string) error {
	qb := tantivy.NewQueryBuilder()
	for _, t := range must {
		qb.Query(tantivy.Must, field, t, tantivy.TermQuery, 1.0)
	}
	for _, t := range mustNot {
		qb.Query(tantivy.MustNot, field, t, tantivy.TermQuery, 1.0)
	}
	_, err := b.runLimited(qb, topK)
	return err
}

func (b *tantivyBackend) benchPrefix(field, prefix string) error {
	qb := tantivy.NewQueryBuilder()
	qb.Query(tantivy.Must, field, prefix, tantivy.TermPrefixQuery, 1.0)
	_, err := b.runLimited(qb, topK)
	return err
}

var _ Backend = (*tantivyBackend)(nil)
