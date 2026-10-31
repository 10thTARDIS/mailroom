// SPDX-License-Identifier: AGPL-3.0-only

// Package tantivy is the embedded-Tantivy SearchBackend (ADR-006), and the ONLY
// place in Mailroom that may import github.com/anyproto/tantivy-go. Everything
// else talks to search.SearchBackend and Mailroom's own query AST, so replacing
// this package with a Bleve one is a wiring change and nothing more (spec §3
// anti-corruption layers, spec §7).
//
// # Build tag
//
// The engine is CGO (a prebuilt Rust static library) and is compiled in ONLY
// under `-tags tantivy`. That is a deliberate confinement, not a hedge:
//
//   - CGO is reserved for the search engine (ADR-006/CLAUDE.md). Every other
//     binary — the migrator, the admin CLI — and every other CI job (vet, unit
//     tests, lint, govulncheck) stays pure Go and needs no C toolchain and no
//     58 MB native artifact to build.
//   - The prebuilt Linux library ships musl-only, so the link needs a musl-native
//     image (golang:1.26-alpine + gcc musl-dev). Making every developer and every
//     CI job carry that to run `go test ./...` would be a large, permanent tax for
//     a dependency only one package has.
//
// Without the tag (and without CGO), Open returns search.ErrBackendUnavailable
// and the caller falls back to search.MemoryFactory: the server still runs and
// still answers searches, from an in-RAM index rebuilt from Postgres. A
// disposable cache degrading is not an outage.
//
// The production image builds WITH the tag (see Dockerfile), and the tantivy CI
// job builds, vets, and tests this package with it — so the tagged path is never
// untested.
//
// # Getting the native library
//
//	curl -fsSL https://github.com/anyproto/tantivy-go/releases/download/v1.0.6/linux-amd64-musl.tar.gz \
//	  | tar -xz -C /usr/local/lib
//	CGO_ENABLED=1 go build -tags tantivy ./...
//
// No Rust toolchain is needed: the release ships a versioned static `.a`, and the
// C header rides along inside the Go module.
package tantivy
