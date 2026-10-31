// SPDX-License-Identifier: AGPL-3.0-only

package mime

import "strconv"

// Part paths: the ONE place a MIME part's identity within its message is
// derived (#393, ADR-027 §3).
//
// # Why this is here and not next to its callers
//
// A part path is IMAP-style and hierarchical — "1", "2", "2.1" — and three
// separate things need to agree on it exactly:
//
//	internal/api    JMAP's Email.bodyStructure partId, and the blobId that
//	                extends the message blob id ("<hash>.2.1") so a download
//	                URL resolves without a server-side lookup.
//	internal/ingest app.attachments.part_path, the projection a scan verdict
//	                will hang off (ADR-027). Its UNIQUE key is
//	                (message_id, part_path).
//	the operator    reading either of the above in a log and expecting them
//	                to name the same bytes.
//
// If those drifted, a quarantined attachment would be recorded against a path
// that no download URL resolves to — the verdict would be attached to nothing.
// `internal/api` cannot be imported by `internal/ingest` (and should not be),
// so the derivation lives here, below both. This is the defect #350 names,
// avoided rather than fixed later.
//
// ⚠ THE PATH IS DERIVED FROM THE FINISHED TREE, NOT THREADED THROUGH THE PARSE,
// AND THAT IS LOAD-BEARING. walkMultipart appends a child to SubParts only when
// it walks successfully, so SubParts indices are the SURVIVING positions, not
// the ordinals on the wire. Numbering during the walk would count parts that
// were dropped; numbering the finished tree counts what a client can actually
// see. The two differ exactly on malformed messages — the population least
// likely to be tested and most likely to be hostile.

// PartPaths assigns every node in a parsed MIME tree its IMAP-style path.
//
// A multipart container gets NO path (the empty string): RFC 8621 is explicit
// that partId is null if and only if the part is multipart/*, because a
// container names no octets a client could fetch. Callers should treat "" as
// "this node has no identity", not as a path.
//
// A non-multipart message is part "1"; a multipart root numbers its children
// 1..n and has no id of its own.
func PartPaths(root *Structure) map[*Structure]string {
	out := map[*Structure]string{}
	if root == nil {
		return out
	}
	var walk func(n *Structure, path string)
	walk = func(n *Structure, path string) {
		if n == nil {
			return
		}
		if !IsMultipart(n.ContentType) {
			out[n] = path
		} else {
			out[n] = ""
		}
		for i, sub := range n.SubParts {
			child := strconv.Itoa(i + 1)
			if IsMultipart(n.ContentType) && path != "" {
				child = path + "." + child
			}
			walk(sub, child)
		}
	}
	if IsMultipart(root.ContentType) {
		walk(root, "")
	} else {
		walk(root, "1")
	}
	return out
}

// IsMultipart reports whether a media type is a multipart container.
func IsMultipart(contentType string) bool {
	return len(contentType) >= 10 && contentType[:10] == "multipart/"
}

// IsAttachmentLeaf reports whether a leaf part is an attachment rather than
// body text, given its media type and Content-Disposition.
//
// ⚠ THIS IS THE SYSTEM'S DEFINITION AND IT HAS EXACTLY ONE IMPLEMENTATION.
// walkLeaf classifies with it when building Message.Attachments, and
// AttachmentParts re-walks the tree with it — so "how many attachments does
// this message have" cannot get two answers depending on who asked. Ingest sets
// app.messages.has_attachments from the same count, which is why the
// attachment-report can compare the two and mean something.
//
// ⚠ It is NOT the same rule as RFC 8621's attachments property, which is
// "parts not used as body". An inline image referenced by cid: is an attachment
// here and may be presented differently by JMAP. That divergence is deliberate
// and currently harmless — has_attachments and the projection agree with each
// other — but it becomes a decision the moment a scan verdict is JMAP-visible
// (ADR-027 increment 5), because then the two lists must be reconciled or a
// user sees a warning against a part their client does not list.
func IsAttachmentLeaf(mediaType, disposition string) bool {
	isText := len(mediaType) >= 5 && mediaType[:5] == "text/"
	// Equivalent to !(isText && disposition != "attachment"), which is how
	// walkLeaf's body/attachment branch reads; De Morgan applied for the linter.
	return !isText || disposition == "attachment"
}

// AttachmentPart is one attachment leaf together with its part path.
type AttachmentPart struct {
	// Path is the IMAP-style part path ("2.1"). Never empty for a leaf.
	Path string
	// Node is the tree node, carrying ContentType, Filename, Disposition,
	// OctetSize and the decoded Data.
	Node *Structure
}

// AttachmentParts returns every attachment leaf in the message, with its path.
//
// It walks Structure rather than reading the flattened Attachments slice,
// because that slice carries no path — but it classifies with the same
// IsAttachmentLeaf predicate, so the two agree in count and membership by
// construction rather than by coincidence.
//
// ⚠ Nested messages are NOT descended into. Message.Attachments excludes parts
// inside an embedded message/rfc822 (they belong to that nested Message), and
// this matches it deliberately: a projection that disagreed with
// has_attachments would make the coverage report meaningless. Attachments
// inside forwarded mail are a known blind spot, counted and reported as such by
// `mailroom-admin attachment-report`.
func (m *Message) AttachmentParts() []AttachmentPart {
	if m == nil || m.Structure == nil {
		return nil
	}
	paths := PartPaths(m.Structure)
	var out []AttachmentPart
	var walk func(n *Structure)
	walk = func(n *Structure) {
		if n == nil {
			return
		}
		if !IsMultipart(n.ContentType) {
			if IsAttachmentLeaf(n.ContentType, n.Disposition) {
				out = append(out, AttachmentPart{Path: paths[n], Node: n})
			}
			return
		}
		for _, sub := range n.SubParts {
			walk(sub)
		}
	}
	walk(m.Structure)
	return out
}
