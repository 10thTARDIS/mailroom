// Isolated spike module (Forgejo #2, ADR-006 search spike).
//
// This module is DELIBERATELY separate from the root `mailroom` module: it
// carries the CGO tantivy-go dependency and its native static library so the
// experiment can never touch the root go.mod/go.sum or break the main build.
// A directory with its own go.mod is a module boundary, so the root module's
// `go build ./...` / `go vet ./...` / golangci-lint never descend here.
//
// Nothing in the main tree imports this. It exists to produce the evidence in
// docs/search-spike.md and is safe to delete once the ADR is landed.
module mailroom/internal/search/spike/tantivy

go 1.26

require github.com/anyproto/tantivy-go v1.0.6
