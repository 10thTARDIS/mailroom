// SPDX-License-Identifier: AGPL-3.0-only

//go:build !tantivy || !cgo

package tantivy

import (
	"context"

	"mailroom/internal/search"
)

// This is the build WITHOUT the engine: no CGO, no native library, no
// tantivy-go import anywhere in the binary. See doc.go for why the engine is
// behind a build tag.

// Available reports whether this build carries the Tantivy engine. It is false
// here, and callers use it to log honestly about which backend is in play.
func Available() bool { return false }

// Open reports that no engine was compiled in. Callers fall back to
// search.MemoryFactory: a disposable cache degrading to an in-RAM one is not an
// outage, and a server that refuses to boot because search is unavailable would
// be strictly worse than one that serves mail with slower search.
func Open(_ context.Context, _ string) (search.SearchBackend, error) {
	return nil, search.ErrBackendUnavailable
}
