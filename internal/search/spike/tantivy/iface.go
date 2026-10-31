// SPDX-License-Identifier: AGPL-3.0-only

package main

// This file defines the ONLY surface the rest of a hypothetical Mailroom would
// see. No tantivy_go type appears here: it is the anti-corruption boundary the
// spec (§7) and ADR-006 require ("Mailroom owns its query AST; Tantivy syntax
// never leaks into the API or clients"). The tantivy-go backend in
// backend_tantivy.go implements these interfaces; a Bleve backend could
// implement the same ones unchanged.

// Doc is an email-like document to index. Deliberately minimal for the spike.
type Doc struct {
	ID      string
	From    string
	To      string
	Subject string
	Body    string
}

// Hit is one search result. Score is backend-relative and only used to sanity
// check ranking; callers rely on ID.
type Hit struct {
	ID    string
	Score float64
}

// Index is the write side of the backend.
type Index interface {
	// Add indexes (or upserts) documents and makes them searchable.
	Add(docs ...Doc) error
	// Delete removes documents by ID (incremental delete).
	Delete(ids ...string) error
	// NumDocs reports the live document count.
	NumDocs() (uint64, error)
	// Close flushes and releases native resources.
	Close() error
}

// Searcher is the read side. Every query is expressed in Mailroom's own terms;
// no backend query syntax crosses this boundary.
type Searcher interface {
	// Term matches documents whose tokenized field contains the exact term.
	Term(field, term string) ([]Hit, error)
	// Prefix matches documents whose field contains a token starting with prefix
	// (as-you-type / autocomplete).
	Prefix(field, prefix string) ([]Hit, error)
	// Phrase matches the ordered sequence of words in the field.
	Phrase(field string, words ...string) ([]Hit, error)
	// Bool requires every term in must and excludes every term in mustNot,
	// all scoped to one field.
	Bool(field string, must, mustNot []string) ([]Hit, error)
	// FieldEquals matches an exact untokenized field value (e.g. from:addr).
	FieldEquals(field, value string) ([]Hit, error)
}

// Backend is the union used by the harness.
type Backend interface {
	Index
	Searcher
}
