// SPDX-License-Identifier: AGPL-3.0-only

package attachproj_test

// The cross-package invariant the re-scan predicate now rests on (#409).
//
// ⚠ store.missingDigestSQL EXCLUDES ROWS WITH `size_bytes = 0`, on the grounds
// that a part which decoded to nothing has no content identity to compute —
// which is exactly what attachscan.HasNoBytes says and why attachscan.PartHash
// returns nil for it. That is one claim spanning three packages:
//
//	mime.walkLeaf      sets OctetSize = len(data), from the same read
//	attachproj.Rows    copies OctetSize into AttachmentRow.SizeBytes
//	attachscan         calls a part with no Data "HasNoBytes"
//
// If any of those stops being true — a declared size substituted for the decoded
// one, say — `size_bytes = 0` stops meaning "HasNoBytes" and the predicate
// silently readmits the fixed point it was written to close: a row that can
// never acquire a digest, due on every pass forever, at the head of an
// `ORDER BY m.id LIMIT 50` that starves everything behind it.
//
// Nothing else would notice. There is no error, no failing scan, and the row
// looks entirely ordinary. So the equivalence is asserted here rather than left
// as a reading of two files in a comment.

import (
	"testing"

	"mailroom/internal/attachproj"
	"mailroom/internal/attachscan"
	"mailroom/internal/mime"
)

// A message with one attachment that has bytes and one that decodes to nothing.
// The empty part is not exotic: old mail is full of `ATT00001.txt` artefacts,
// and internal/mime reaches this state without hostile input — walkLeaf records
// a body read error as a WARNING and leaves Data as whatever it got.
const oneFullOneEmpty = "From: a@example.com\r\n" +
	"Subject: two files\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: multipart/mixed; boundary=bnd\r\n" +
	"\r\n" +
	"--bnd\r\n" +
	"Content-Type: text/plain\r\n" +
	"\r\n" +
	"body\r\n" +
	"--bnd\r\n" +
	"Content-Type: application/octet-stream\r\n" +
	"Content-Disposition: attachment; filename=\"full.bin\"\r\n" +
	"\r\n" +
	"PAYLOAD\r\n" +
	"--bnd\r\n" +
	"Content-Type: application/octet-stream\r\n" +
	"Content-Disposition: attachment; filename=\"empty.bin\"\r\n" +
	"\r\n" +
	"--bnd--\r\n"

func TestSizeBytesIsZeroExactlyWhenThePartHasNoBytes(t *testing.T) {
	m, err := mime.ParseBytes([]byte(oneFullOneEmpty))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	parts := m.AttachmentParts()
	rows := attachproj.Rows(m)
	if len(parts) != len(rows) {
		t.Fatalf("AttachmentParts gave %d part(s) and Rows gave %d row(s) — they must describe the "+
			"same list or the row a verdict names is not the part that was scanned", len(parts), len(rows))
	}

	// ⚠ THE CONTROL FIRST. Both directions of an equivalence are vacuous if the
	// fixture only exercises one side: with no empty part, "size 0 iff no bytes"
	// holds trivially and this test would pass against any implementation.
	var full, empty int
	for _, ap := range parts {
		if attachscan.HasNoBytes(ap) {
			empty++
		} else {
			full++
		}
	}
	if full == 0 || empty == 0 {
		t.Fatalf("CONTROL FAILED: the fixture produced %d part(s) with bytes and %d without; both "+
			"populations are needed or the equivalence below is asserted against one side only",
			full, empty)
	}

	for i, ap := range parts {
		noBytes := attachscan.HasNoBytes(ap)
		zeroSize := rows[i].SizeBytes == 0
		if noBytes != zeroSize {
			t.Errorf("part %q: HasNoBytes=%v but size_bytes=%d. store.missingDigestSQL uses "+
				"`size_bytes > 0` to mean 'this part could have a content digest'; when those two "+
				"disagree, a row that can NEVER be digested is due for re-scan on every pass "+
				"forever, and fifty of them starve the whole queue (#409)",
				ap.Path, noBytes, rows[i].SizeBytes)
		}
	}
}
