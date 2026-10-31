// SPDX-License-Identifier: AGPL-3.0-only

// Package search is Mailroom's full-text search layer (spec §7, ADR-006).
//
// # The doctrine
//
// The index is a DISPOSABLE CACHE of the canonical store. It is never backed
// up, never trusted, and always rebuildable: losing every index directory costs
// nothing but the time to rebuild them from Postgres (Indexer.Reindex, a
// first-class operation). Nothing in here is a system of record.
//
// # The layers
//
//	SearchBackend  the engine seam (backend.go). Tantivy via tantivy-go lives
//	               behind it in internal/search/tantivy; a Bleve (or any other)
//	               backend implements the SAME interface. NOTHING outside
//	               internal/search may import an engine binding — the ONLY
//	               engine types that exist anywhere are inside that subpackage.
//	Query          Mailroom's OWN query AST (query.go): And/Or/Not over typed
//	               Terms. Engine query syntax never leaks into the API, into a
//	               client, or into this package's callers (spec §7). Parse
//	               (parse.go) builds it from the operator syntax users type
//	               (from:/to:/subject:/label:/is:/has:/account:/before:/after:).
//	Indexer        the change-log consumer (index.go): one ordered stream, one
//	               persisted cursor per user, at-least-once delivery, idempotent
//	               upsert-by-message-id. A crash between the DB commit and the
//	               index write heals by cursor replay.
//	Service        the query-time facade (service.go) internal/api talks to:
//	               parse → resolve names to ids → search → highlight.
//
// # Isolation
//
// Search is per-user, and that is a SECURITY control, not organization
// (spec §7). Each user's index lives in its own directory under the index root,
// created 0700, and a query is always executed against exactly one user's
// backend — there is no cross-user index and no query path that can reach one.
// This mirrors the fail-closed posture RLS gives the database: no user id, no
// index, no rows. It is also honestly the weakest isolation layer we have
// (filesystem permissions inside one process), which is why the spec requires
// volume-level at-rest encryption for the index root.
//
// # At-rest encryption (a DEPLOYMENT requirement, spec §7/§11)
//
// Mailroom holds the keys, so encrypted mail is decrypted server-side and
// indexed like anything else: THE INDEX CONTAINS THE PLAINTEXT OF ENCRYPTED
// MAIL. Tantivy has no native encryption. The index root MUST therefore sit on
// an at-rest-encrypted volume (LUKS/dm-crypt or equivalent). Nothing in this
// package can compensate for that, so nothing in this package pretends to.
//
// What is deliberately NOT indexed: credentials of any kind. The indexer reads
// canonical message rows and the decrypted RFC 5322 blob; it never touches
// app.accounts.credentials_ct, and it never logs message content — log lines
// carry ids and counts only.
package search
