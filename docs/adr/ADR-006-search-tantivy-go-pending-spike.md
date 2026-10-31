# ADR-006: Search = embedded Tantivy via tantivy-go, pending spike; Bleve fallback; Mailroom owns the query AST

**Status:** Accepted — spike PASSED (`docs/search-spike.md`).
`anyproto/tantivy-go` v1.0.6 builds reproducibly with CGO in a musl-native
container (no Rust toolchain in CI; prebuilt static `.a` fetched per release),
and passed correctness (term/prefix/phrase/boolean/field, incremental
delete+upsert, highlighting) with production top-K p95 in the single-digit-ms
range at 20 k docs. Two follow-ups carried, neither blocking: (1) the binding
does **not** expose Tantivy's `FuzzyTermQuery`, so typo tolerance (spec §7)
needs either a small upstream/fork FFI patch or ngram-based approximation
behind Mailroom's query AST; (2) the p95 < 100 ms / ~25 GB scale criterion
still needs a large-corpus measurement before search GA. Bleve remains the
documented fallback (spec §18.2) but is not adopted. (Original: Accepted,
pending spike — spec v0.4 §3/§7; design review P0-4/D6.)

## Context

Typo-tolerant sub-100ms search over tens of GB per user, in-process, no extra
service. The viable binding is `anyproto/tantivy-go` (CGO; prebuilt Rust
static libs; production-proven in Anytype, MIT) — but its exposed API surface
is not verified to cover everything we need, and CGO costs a second toolchain
in CI. "Static Go binary" is therefore corrected to "single deployable
container".

## Decision

Embedded Tantivy via tantivy-go behind a `SearchBackend` interface, **gated
on a week-one spike** with pass/fail criteria: per-user index directories;
fuzzy + prefix + phrase + field queries; highlighting; incremental
delete/update; p95 < 100 ms over a ~25 GB synthetic corpus. Fallback: Bleve
(pure Go, slower) behind the same interface. Mailroom owns its query AST —
Tantivy syntax never leaks into the API or clients.

The index is a **disposable cache** of the canonical store: never backed up,
always rebuildable, updated from the change log (ADR-003). Index directories
and ML model files require volume-level at-rest encryption as a stated
deployment requirement — they contain the plaintext of encrypted mail.

## Consequences

- The spike gates final search design, not the data model.
- Versioned index schema; rebuild-and-swap in the background on bump.
- The index is honestly the weakest isolation layer (per-user directories +
  FS permissions in one process); compensating controls documented in §10.

## References

spec §3, §7, §11 · design review P0-4, deep-dive C · D6
