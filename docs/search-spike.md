# Search spike — tantivy-go (ADR-006)

**Verdict: PASS — adopt `anyproto/tantivy-go` behind the `SearchBackend`
interface. Keep Bleve as the documented fallback but do not switch to it.**
Two follow-ups are carried, neither a blocker (see §6).

Reproduce: `sh internal/search/spike/tantivy/run-spike.sh` (throwaway
Docker container; no host toolchain, mirrors `scripts/integration-test.sh`).
The spike lives in an isolated nested Go module
(`internal/search/spike/tantivy/go.mod`) so its CGO dependency never touches
the root `go.mod`/`go.sum`.

## 1. Pass/fail criteria (from ADR-006 + spec §7/§15)

| Criterion | Result |
|---|---|
| Builds reproducibly with CGO in a container | PASS |
| Per-user index directories | PASS (one context per directory) |
| Phrase queries | PASS (positions verified) |
| Prefix / as-you-type queries | PASS |
| Field-scoped queries (`from:` etc.) | PASS |
| Boolean must/should/must-not | PASS |
| Highlighting | PASS (exposed; not perf-critical) |
| Incremental delete + update (upsert) | PASS |
| Ranked relevance | PASS (BM25 scores returned) |
| **Fuzzy / typo tolerance (edit distance)** | **NOT EXPOSED by the binding** — see §6 |
| p95 < 100 ms | PASS at moderate scale; 25 GB test still owed — see §5 |

## 2. Binding evaluated

- **`github.com/anyproto/tantivy-go` v1.0.6** (HEAD `09b8796`, 2026-01-06). MIT.
  Actively maintained; production-proven in Anytype across Linux/macOS/Windows,
  x86-64/ARM64, iOS/Android. This is the only viable, maintained Go/Tantivy CGO
  binding; no serious competitor exists (the pure-Go alternative is Bleve, a
  different engine, not a Tantivy binding).

Native library acquisition:
- Prebuilt **static** libs are published as per-platform GitHub **release
  assets** (`linux-amd64-musl.tar.gz` → `libtantivy_go.a`, ~58 MB uncompressed).
  **No Rust toolchain is required** for supported platforms — you fetch a
  versioned `.a`. The C header (`bindings.h`) ships inside the Go module.
- Two link modes via build tag: default (`.a` on the linker's search path) or
  `-tags tantivylocal` (links a bundled `libs/<platform>/` copy). The spike uses
  the default mode with the `.a` dropped in `/usr/local/lib`.

## 3. Build reproducibility & CGO/linker friction

**The Linux release ships musl-only.** The one real friction point. The
prebuilt Linux `.a` is built against musl (`-Wl,--allow-multiple-definition`
is already baked into the binding's `LDFLAGS`). The clean, reproducible fix:
build on a **musl-native image (`golang:1.26-alpine` + `gcc musl-dev`)**, where
the `.a` links with zero cross-toolchain. On a glibc image (`golang:1.26`,
Debian) you would additionally need `x86_64-linux-musl-gcc`. Result on Alpine is
an effectively **fully static** binary.

Reproducibility is good and pinned three ways: the Go module + `go.sum` pin the
binding; the release URL pins the `.a` to `TANTIVY_VERSION`; the base image pins
the toolchain. Cold `go build` ≈ 15–20 s. CI cost added by this choice: one
`apk add gcc musl-dev`, one `curl | tar` of a 58 MB asset, and CGO enabled — but
**no Rust toolchain and no second language build in CI**. This is materially
cheaper than ADR-006 feared ("CGO costs a second toolchain in CI"): the second
toolchain is just a C compiler, and the Rust output is a pinned binary artifact.

## 4. Correctness

Harness indexes a self-contained synthetic email corpus (example.com only) with
rare marker tokens planted on fixed index strides, so every query has an exactly
known expected hit count independent of corpus size. All checks exact at
n = 500 / 2 000 / 20 000. Representative run (n = 20 000):

```
[ok] Term(body,zorbex)                        got=400 want=400
[ok] Prefix(body,zorb)                        got=400 want=400
[ok] Phrase(subject,quarterly revenue report) got=200 want=200
[ok] Bool(body, +invoice -spam)               got=500 want=500
[ok] FieldEquals(from,alerts@example.com)     got=313 want=313
[info] Term(subject,revenue)=400 vs Phrase=200   (phrase narrows → positions work)
--- incremental delete ---
deleted 400 zorbex docs; Term(body,zorbex) now=0; NumDocs 20000 -> 19600
```

Phrase strictly narrows vs the bag-of-words term match (200 vs 400), proving
position-aware phrase matching, not mere term presence. Delete-by-id is exact.
Atomic upsert is available via `BatchAddAndDeleteDocumentsWithOpstamp`, which
maps directly onto ADR-006's change-log-driven idempotent upsert-by-message-id.

## 5. Latency

The p95 < 100 ms target applies to a **production top-K** search (fetch first
page). Measured at n = 20 000, limit = 50, 500 iterations:

| Query | p50 | p95 | p99 | max |
|---|---|---|---|---|
| Term (`body:invoice`) | 1.6 ms | 2.4 ms | 3.2 ms | 3.4 ms |
| Phrase (`subject:"quarterly revenue report"`) | 0.34 ms | 0.52 ms | 0.68 ms | 1.0 ms |
| Bool (`+invoice -spam`) | 1.8 ms | 2.4 ms | 2.7 ms | 3.0 ms |
| Prefix (`body:dep`) | 0.64 ms | 0.95 ms | 1.2 ms | 1.5 ms |

All ~1–2 orders of magnitude under the 100 ms budget.

Diagnostic note: an early run showed Prefix p95 ≈ 200 ms — that was **fetching
and JSON-marshalling _every_ match** (`limit = 100 000`), i.e. a harness
artifact, not engine cost. Bounding to top-K (as real search does) collapses it
to < 1 ms; the "full materialization" tier in the harness output documents the
difference.

**Honest scope limit:** 20 000 synthetic docs is far from ADR-006's ~25 GB /
tens-of-GB-per-user target. This spike validates the *binding path, correctness,
API coverage, and that the engine is very fast at moderate scale*. It does **not**
prove p95 < 100 ms at 25 GB. Tantivy is disk-backed/memory-mapped and explicitly
chosen for that scale (spec §7 scale note); nothing observed suggests it won't
hold, but the large-corpus measurement is the remaining verification (§6).

## 6. Findings that shape the decision

1. **Typo tolerance is not exposed by the binding.** Spec §7 lists "typo
   tolerance" and ADR-006's criteria list "fuzzy". Tantivy-the-library has
   `FuzzyTermQuery`, but `anyproto/tantivy-go` surfaces **no** fuzzy /
   Levenshtein / edit-distance query anywhere — not in Go, not in its Rust FFI
   layer, not in `bindings.h`. The query AST it exposes is: term, term-prefix,
   phrase, phrase-prefix, boolean, all-query, plus `ngram`/`edge_ngram`
   tokenizers. Options, in preference order:
   - **(a) Small upstream/fork patch** exposing `FuzzyTermQuery` through the FFI
     (add a `FuzzyQuery` variant to the query-type enum + a few lines of Rust).
     Bounded, mechanical; upstream may take the PR.
   - **(b) App-side approximation** — `ngram`/`edge_ngram` tokenizers (present)
     for substring/as-you-type, plus query expansion in Mailroom's own AST
     layer. Works without touching the binding but inflates index size and isn't
     true edit distance.
   This is the one spec requirement not natively covered. It is **not a reason to
   prefer Bleve**: Bleve's fuzzy support is also limited and slow. Since Mailroom
   owns its query AST (spec §7), either path stays behind the interface.

2. **25 GB scale test still owed** before final sign-off (§5).

Neither blocks adoption: they are bounded follow-ups, and the risk ADR-006
actually flagged (CGO/toolchain friction, unverified API surface) came back
**favorable** — the API covers everything except fuzzy, and the toolchain cost is
a pinned C-lib + Alpine gcc, not a Rust build.

## 7. Recommendation

**PASS. Proceed with `anyproto/tantivy-go` (v1.0.6, pinned) as the
`SearchBackend` implementation.** Keep Bleve as the documented fallback but do
not adopt it: the CGO friction that motivated the fallback proved modest
(Alpine/musl solves the link cleanly), correctness is exact, API coverage is
complete except fuzzy, and latency headroom is large. Carry the two follow-ups
above into the search milestone.

Bleve remains the escape hatch if either (a) the 25 GB test regresses badly, or
(b) the fuzzy patch proves unexpectedly costly *and* ngram approximation is
judged inadequate — both low-probability given the evidence.

## 8. Proposed ADR-006 status update

Replace the current status line:

> **Status:** Accepted, pending spike (spec v0.4 §3/§7; design review P0-4/D6)

with:

> **Status:** Accepted — spike PASSED (`docs/search-spike.md`).
> `anyproto/tantivy-go` v1.0.6 builds reproducibly with CGO in a musl-native
> container (no Rust toolchain in CI; prebuilt static `.a` fetched per release),
> and passed correctness (term/prefix/phrase/boolean/field, incremental
> delete+upsert, highlighting) with production top-K p95 in the single-digit-ms
> range at 20 k docs. Two follow-ups carried, neither blocking: (1) the binding
> does **not** expose Tantivy's `FuzzyTermQuery`, so typo tolerance (spec §7)
> needs either a small upstream/fork FFI patch or ngram-based approximation
> behind Mailroom's query AST; (2) the p95 < 100 ms / ~25 GB scale criterion
> still needs a large-corpus measurement before search GA. Bleve remains the
> documented fallback (spec §18.2) but is not adopted.
