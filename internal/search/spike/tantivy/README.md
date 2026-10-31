# tantivy-go search spike (ADR-006 / Forgejo #2)

Throwaway spike proving out embedded Tantivy via `anyproto/tantivy-go` behind a
Mailroom-owned `Index`/`Searcher` interface. **Findings and the PASS/FAIL
decision live in [`docs/search-spike.md`](../../../../docs/search-spike.md).**

This is an **isolated nested Go module** (its own `go.mod`/`go.sum`) carrying the
CGO dependency and its native static lib, so it can never touch the root
`mailroom` module or break the main build. Nothing in the main tree imports it;
it is safe to delete once ADR-006 is landed.

Run (throwaway Docker container, no host toolchain):

```sh
sh run-spike.sh              # default corpus (N=2000)
N=20000 ITERS=500 sh run-spike.sh
```

Files: `iface.go` (leak-free interface) · `backend_tantivy.go` (binding impl)
· `corpus.go` (synthetic example.com email corpus with planted counts) ·
`main.go` (index → correctness → delete → latency harness) · `run-spike.sh`
(Docker build/run, mirrors `scripts/integration-test.sh`).
