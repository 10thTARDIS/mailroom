// SPDX-License-Identifier: AGPL-3.0-only

package mime

import (
	"fmt"
	"testing"
)

// nested builds a multipart/mixed whose second part is a multipart/alternative,
// so paths have to nest: "1", "2.1", "2.2", "3".
const nestedMsg = "From: a@example.com\r\n" +
	"Subject: nested\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: multipart/mixed; boundary=OUT\r\n" +
	"\r\n" +
	"--OUT\r\n" +
	"Content-Type: text/plain\r\n\r\nhello\r\n" +
	"--OUT\r\n" +
	"Content-Type: multipart/alternative; boundary=IN\r\n" +
	"\r\n" +
	"--IN\r\n" +
	"Content-Type: text/plain\r\n\r\nalt text\r\n" +
	"--IN\r\n" +
	"Content-Type: text/html\r\n\r\n<p>alt html</p>\r\n" +
	"--IN--\r\n" +
	"--OUT\r\n" +
	"Content-Type: application/pdf\r\n" +
	"Content-Disposition: attachment; filename=\"r.pdf\"\r\n\r\n%PDF-1.4\r\n" +
	"--OUT--\r\n"

// The projection's part_path and JMAP's partId must name the same part, so the
// numbering rule is asserted directly rather than only through its callers.
func TestPartPathsNumbersTheTreeIMAPStyle(t *testing.T) {
	msg, err := ParseBytes([]byte(nestedMsg))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	paths := PartPaths(msg.Structure)

	got := map[string]string{} // path -> content type
	for node, p := range paths {
		if p == "" {
			// A container. RFC 8621: partId is null iff multipart/*.
			if !IsMultipart(node.ContentType) {
				t.Errorf("leaf %q got no path", node.ContentType)
			}
			continue
		}
		if _, dup := got[p]; dup {
			t.Errorf("path %q assigned twice — paths must be unique, they are a UNIQUE key", p)
		}
		got[p] = node.ContentType
	}

	want := map[string]string{
		"1":   "text/plain",
		"2.1": "text/plain",
		"2.2": "text/html",
		"3":   "application/pdf",
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("paths =\n  %v\nwant\n  %v", got, want)
	}
}

// A message that is a single part, not a multipart, is part "1" — the case that
// is easy to get wrong because there is no container to number from.
func TestPartPathsSinglePartMessageIsPartOne(t *testing.T) {
	msg, err := ParseBytes([]byte("From: a@example.com\r\nContent-Type: text/plain\r\n\r\njust text\r\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	paths := PartPaths(msg.Structure)
	if len(paths) != 1 {
		t.Fatalf("len(paths) = %d, want 1", len(paths))
	}
	for _, p := range paths {
		if p != "1" {
			t.Errorf("single-part path = %q, want %q", p, "1")
		}
	}
}

// ⚠ THE INVARIANT THE PROJECTION RESTS ON. AttachmentParts walks the tree to
// get paths; Message.Attachments is the flattened list ingest counts to set
// has_attachments. If they ever disagreed, the attachment-report's coverage
// comparison — rows against has_attachments — would be measuring two different
// populations and its PARTIAL branch would fire on a correct projection.
func TestAttachmentPartsAgreesWithTheFlattenedList(t *testing.T) {
	for _, raw := range []string{nestedMsg, singleAttachmentMsg, inlineImageMsg} {
		msg, err := ParseBytes([]byte(raw))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		parts := msg.AttachmentParts()
		if len(parts) != len(msg.Attachments) {
			t.Errorf("AttachmentParts = %d, Attachments = %d — the two definitions have drifted",
				len(parts), len(msg.Attachments))
			continue
		}
		// Same membership, not merely the same count.
		for i, p := range parts {
			if p.Node.ContentType != msg.Attachments[i].ContentType {
				t.Errorf("part %d: walk says %q, flattened list says %q",
					i, p.Node.ContentType, msg.Attachments[i].ContentType)
			}
			if p.Path == "" {
				t.Errorf("attachment %d (%s) has no part path — part_path is NOT NULL",
					i, p.Node.ContentType)
			}
		}
	}
}

const singleAttachmentMsg = "From: a@example.com\r\n" +
	"Content-Type: multipart/mixed; boundary=B\r\n\r\n" +
	"--B\r\nContent-Type: text/plain\r\n\r\nbody\r\n" +
	"--B\r\nContent-Type: image/png\r\nContent-Disposition: attachment; filename=\"x.png\"\r\n\r\n\x89PNG\r\n" +
	"--B--\r\n"

// An inline image referenced by cid: is an attachment by THIS system's rule
// (non-text leaf), which is what has_attachments counts. Pinned deliberately —
// ADR-027 increment 5 has to reconcile it with RFC 8621's different rule, and
// this test is what will go red if someone changes it silently in the meantime.
const inlineImageMsg = "From: a@example.com\r\n" +
	"Content-Type: multipart/related; boundary=R\r\n\r\n" +
	"--R\r\nContent-Type: text/html\r\n\r\n<img src=\"cid:i\">\r\n" +
	"--R\r\nContent-Type: image/gif\r\nContent-ID: <i>\r\nContent-Disposition: inline\r\n\r\nGIF89a\r\n" +
	"--R--\r\n"

// A text/plain part that carries a filename is an attachment, not body — the
// disposition wins over the media type. The reverse case (a non-text part with
// no disposition) is covered by inlineImageMsg above.
func TestIsAttachmentLeafDispositionBeatsMediaType(t *testing.T) {
	for _, tc := range []struct {
		mediaType, disposition string
		want                   bool
	}{
		{"text/plain", "", false},
		{"text/html", "inline", false},
		{"text/plain", "attachment", true}, // disposition wins
		{"application/pdf", "", true},      // non-text with no disposition
		{"image/png", "inline", true},      // inline image is still an attachment here
	} {
		if got := IsAttachmentLeaf(tc.mediaType, tc.disposition); got != tc.want {
			t.Errorf("IsAttachmentLeaf(%q, %q) = %v, want %v",
				tc.mediaType, tc.disposition, got, tc.want)
		}
	}
}

// ⚠ A multipart WHOSE BOUNDARY IS MISSING is not a container at all: go-message
// cannot build a reader, and walkMultipart deliberately falls back to treating
// the whole thing as one opaque leaf rather than losing it. That fallback leaf
// still needs a path, because app.attachments.part_path is NOT NULL and this is
// exactly the malformed shape a hostile message takes.
func TestPartPathsGiveTheMalformedMultipartFallbackAPath(t *testing.T) {
	// multipart/mixed with NO boundary parameter.
	raw := "From: a@example.com\r\nContent-Type: multipart/mixed\r\n\r\nnot really multipart\r\n"
	msg, err := ParseBytes([]byte(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if msg.Structure == nil {
		t.Fatal("no structure — the fallback lost the part entirely")
	}
	paths := PartPaths(msg.Structure)
	for node, p := range paths {
		if IsMultipart(node.ContentType) {
			continue
		}
		if p == "" {
			t.Errorf("fallback leaf %q got no path", node.ContentType)
		}
	}
	// And it must be projectable: a path, and classified as an attachment
	// (application/octet-stream is not text), so a scanner would see it.
	for _, ap := range msg.AttachmentParts() {
		if ap.Path == "" {
			t.Errorf("attachment %q has no path", ap.Node.ContentType)
		}
	}
}
