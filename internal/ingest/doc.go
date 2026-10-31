// SPDX-License-Identifier: AGPL-3.0-only

// Package ingest is canonical message ingestion (spec §4, ADR D1): it turns a
// raw RFC 5322 message plus its provenance (which account/mailbox/UID it came
// from) into Mailroom's canonical model.
//
// Identity is the content hash: a message is the SHA-256 of its raw bytes,
// scoped per user. The same bytes seen in a second mailbox or account do not
// create a second message — they add a provenance row to the existing one and
// merge flags. Every ingest runs inside a single store.UserTx, so RLS scopes
// every statement and the change_log invariant (CLAUDE.md, ADR-003) holds:
// creating a message appends an Email change; creating or growing its thread
// appends a Thread change; a flag or membership change on a deduplicated
// message appends an Email update.
//
// Threading here is incremental: attach to a related thread by Message-ID
// hints, else start a new one. That is store.FindThreadByHints — one SQL
// query, and the ONLY threader this application runs.
//
// ⚠ internal/thread IS NOT WIRED IN, and saying otherwise here has already
// cost real work. That package is a complete, tested JWZ implementation with
// zero importers (control: 186 tracked files
// import internal/store). #210 named a
// file in it as the site of a duplicate-Message-ID defect; the fix made there
// was correct, self-consistent, and would have changed nothing, because the
// code does not run. The real defect was a missing match arm in
// FindThreadByHints. Only checking importers caught it.
//
// The two are behaviourally aligned today, and whether JWZ should become the
// running threader is an open decision (spec §4 asks for it; the shipped
// behaviour is hint-matching with no cross-thread merge and no subject
// fallback). Until that is settled, believe this comment over the package.
//
// Attachment extraction and alias/identity resolution are deliberately out of
// scope for this first cut.
package ingest
