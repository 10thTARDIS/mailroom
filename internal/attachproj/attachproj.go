// SPDX-License-Identifier: AGPL-3.0-only

// Package attachproj projects a parsed message's attachment parts into the
// store rows that back app.attachments (#393, ADR-027 §3).
//
// # Why a package for one function
//
// It exists because of a boundary neither side should cross. internal/store
// deliberately does not import internal/mime — a store row is a database
// concern and must not depend on a parser — and internal/mime must not import
// internal/store, which would invert the layering entirely. So the mapping
// between them has no home in either.
//
// It had three call sites before this package existed (ingest, compose, and the
// backfill command), each with its own copy of the same six field assignments.
// The JUDGEMENT was never duplicated — what counts as an attachment and what
// its part path is live once, in mime.AttachmentParts — but three copies of a
// mapping is how a fourth gets written, and #350 is an open issue about exactly
// that pattern in this repo.
package attachproj

import (
	"mailroom/internal/mime"
	"mailroom/internal/store"
)

// Rows projects every attachment part of m into an app.attachments row.
//
// ⚠ ContentHash is deliberately left nil. Attachments are NOT stored
// separately: a part's bytes are a slice of the message blob, and its JMAP
// blobId is "<messageHash>.<partPath>", resolved by re-parsing at read time.
// There is no per-attachment blob to name, so nothing here arms
// blobreclaim.go's attachments clause — which remains inert, as it always has
// been. ADR-027 §3 says that clause goes live "the moment the projection
// populates content_hash"; this projection does not populate it.
//
// Returns nil for a message with no attachments, which ReplaceAttachments
// treats as "prune everything" — correct for a message whose parts have gone.
func Rows(m *mime.Message) []store.AttachmentRow {
	if m == nil {
		return nil
	}
	parts := m.AttachmentParts()
	if len(parts) == 0 {
		return nil
	}
	out := make([]store.AttachmentRow, 0, len(parts))
	for _, ap := range parts {
		out = append(out, store.AttachmentRow{
			PartPath: ap.Path,
			Filename: ap.Node.Filename,
			// The DECLARED media type, not a sniffed one. The gap between what
			// a part claims to be and what its bytes look like is itself a
			// signal a scanner wants, so it must not be normalised away here.
			ContentType: ap.Node.ContentType,
			// OctetSize, not Size: the number of octets a client would
			// download, before any charset transcoding.
			SizeBytes: int64(ap.Node.OctetSize),
		})
	}
	return out
}
