// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"fmt"

	"github.com/oklog/ulid/v2"
)

// reclaimBlobFiles deletes the on-disk bytes of blobs whose app.blobs metadata
// row a store call just removed (#300). It is the second half of a reclaim: the
// store deletes the row and hands back the hashes, this deletes the files.
//
// ⚠ IT MUST RUN INSIDE THE TRANSACTION THAT REMOVED THE ROWS, before commit.
// That ordering is the race proof spelled out in store.HardDeleteMessage: the
// row delete is invisible to a concurrent ingest until this transaction commits,
// so the "not found by content hash" window a byte-identical re-ingest needs in
// order to re-Put these bytes opens only AFTER the file is already gone, never
// during. An error aborts the caller's transaction, so the failure mode is
// "nothing was destroyed, try again", never a canonical row that outlives its
// bytes.
//
// A nil blob store is not a silent leak, and the reason is worth stating rather
// than assuming: an app.blobs row exists only because some path wrote a FILE
// first (ingest, compose, the upload endpoint and — since #316 — the ML model
// registry; each Puts before it inserts, and all of them refuse outright when
// their blob store is nil). A server with no blob store therefore has no blob
// rows, so hashes is empty and this loop does not run. The guard is here so the
// invariant is asserted rather than relied on.
func (a *API) reclaimBlobFiles(ctx context.Context, userID ulid.ULID, hashes [][]byte) error {
	if len(hashes) == 0 {
		return nil
	}
	if a.blobs == nil {
		return fmt.Errorf("api: the store reclaimed %d blob row(s) but this server has no blob "+
			"store to delete the files from", len(hashes))
	}
	for _, h := range hashes {
		if err := a.blobs.DeleteRaw(ctx, userID, h); err != nil {
			return fmt.Errorf("api: reclaim blob file: %w", err)
		}
	}
	return nil
}
