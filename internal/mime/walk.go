// SPDX-License-Identifier: AGPL-3.0-only

package mime

import (
	"bytes"
	"fmt"
	"io"
	"strings"

	message "github.com/emersion/go-message"
)

// walkState tracks the two cumulative limits (docs/mime-policy.md) that
// apply across a whole message tree, including its Nested descendants:
// total part count and total decoded byte size. Recursion depth is passed
// down the call stack instead, since it's naturally scoped per branch.
type walkState struct {
	parts int
	size  int64
}

// parseEntity turns a go-message entity into a Message, walking its full
// structure (multipart children flattened into the same Message; nested
// message/rfc822 parts recursively parsed into Message.Nested) and, in the
// same single pass, recording the shape-preserving tree in Message.Structure.
func parseEntity(ent *message.Entity) *Message {
	msg := &Message{}
	hdr, warnings := newHeader(ent.Header)
	msg.Header = hdr
	msg.Warnings = append(msg.Warnings, warnings...)

	st := &walkState{}
	msg.Structure = walkBody(ent, msg, 0, st)
	return msg
}

// walkBody dispatches on ent's media type: multipart descends into its
// children (merged into msg), message/rfc822 recurses into a new nested
// Message (appended to msg.Nested), anything else is a leaf part. It returns
// the tree node for ent, or nil when nothing could be extracted from it.
func walkBody(ent *message.Entity, msg *Message, depth int, st *walkState) *Structure {
	if depth > MaxDepth {
		msg.Truncated = true
		msg.Warnings = append(msg.Warnings, "mime: max nesting depth exceeded; remaining structure skipped")
		return nil
	}

	mediaType, params, ctErr := ent.Header.ContentType()
	if ctErr != nil {
		msg.Warnings = append(msg.Warnings, fmt.Sprintf("mime: content-type: %v", ctErr))
	}
	mediaType = strings.ToLower(strings.TrimSpace(mediaType))

	switch {
	case strings.HasPrefix(mediaType, "multipart/"):
		return walkMultipart(ent, mediaType, params, msg, depth, st)
	case mediaType == "message/rfc822" || mediaType == "message/global":
		return walkNestedMessage(ent, mediaType, params, msg, depth, st)
	default:
		return walkLeaf(ent, mediaType, params, msg, st)
	}
}

// walkMultipart iterates a multipart entity's children, merging each one's
// extracted content into the same msg (multipart is structural, not a new
// logical message) while assembling them as the SubParts of the container's
// tree node.
func walkMultipart(ent *message.Entity, mediaType string, params map[string]string, msg *Message, depth int, st *walkState) *Structure {
	mr := ent.MultipartReader()
	if mr == nil {
		// Content-Type claimed multipart/* but go-message couldn't build a
		// reader for it (e.g. missing boundary parameter). Fall back to
		// treating the whole thing as an opaque leaf rather than losing it.
		return walkLeaf(ent, "application/octet-stream", nil, msg, st)
	}
	defer func() { _ = mr.Close() }()

	node := &Structure{ContentType: mediaType, Params: params, SubParts: []*Structure{}}
	for {
		if st.parts >= MaxParts || st.size >= MaxTotalSize {
			msg.Truncated = true
			msg.Warnings = append(msg.Warnings, "mime: part/size limit reached; remaining parts skipped")
			return node
		}

		part, err := safeNextPart(mr)
		if err == io.EOF {
			return node
		}
		if part == nil {
			if err != nil {
				msg.Warnings = append(msg.Warnings, fmt.Sprintf("mime: multipart read: %v", err))
			}
			return node
		}
		if err != nil && !message.IsUnknownCharset(err) && !message.IsUnknownEncoding(err) {
			msg.Warnings = append(msg.Warnings, fmt.Sprintf("mime: multipart part: %v", err))
		}

		st.parts++
		if child := safeWalkBody(part, msg, depth+1, st); child != nil {
			node.SubParts = append(node.SubParts, child)
		}
	}
}

// walkNestedMessage parses ent's body as an embedded RFC 822 message,
// recursing into it as a new Message appended to msg.Nested, bounded by
// MaxDepth/MaxParts/MaxTotalSize.
//
// The nested message's raw bytes are materialized first so the tree node can
// report an honest size (a forwarded message is an attachment like any other,
// and an attachment of unknown size is not usable by a client). The nested
// leaves are still charged to the cumulative size budget as they are walked,
// exactly as before; the buffer itself is bounded by the remaining budget.
func walkNestedMessage(ent *message.Entity, mediaType string, params map[string]string, msg *Message, depth int, st *walkState) *Structure {
	if depth+1 > MaxDepth {
		msg.Truncated = true
		msg.Warnings = append(msg.Warnings, "mime: max nesting depth exceeded; nested message skipped")
		return nil
	}
	if st.parts >= MaxParts || st.size >= MaxTotalSize {
		msg.Truncated = true
		msg.Warnings = append(msg.Warnings, "mime: part/size limit reached; nested message skipped")
		return nil
	}
	st.parts++

	budget := MaxTotalSize - st.size
	raw, readErr := safeReadAll(ent.Body, budget+1)
	if int64(len(raw)) > budget {
		raw = raw[:budget]
		msg.Truncated = true
		msg.Warnings = append(msg.Warnings, "mime: nested message truncated at cumulative size limit")
	}
	if readErr != nil {
		msg.Warnings = append(msg.Warnings, fmt.Sprintf("mime: reading nested message/rfc822: %v", readErr))
	}

	disp, dispParams, _ := ent.Header.ContentDisposition()
	node := &Structure{
		ContentType: mediaType,
		Params:      params,
		Disposition: strings.ToLower(strings.TrimSpace(disp)),
		Filename:    attachmentFilename(params, dispParams),
		ContentID:   strings.Trim(strings.TrimSpace(ent.Header.Get("Content-Id")), "<>"),
		Data:        raw,
		Size:        len(raw),
		OctetSize:   len(raw),
	}

	nested, err := safeReadEntity(bytes.NewReader(raw))
	if nested == nil {
		msg.Warnings = append(msg.Warnings, fmt.Sprintf("mime: nested message/rfc822: %v", err))
		return node
	}

	child := &Message{}
	hdr, warnings := newHeader(nested.Header)
	child.Header = hdr
	child.Warnings = append(child.Warnings, warnings...)
	if err != nil {
		child.Warnings = append(child.Warnings, fmt.Sprintf("mime: nested message header: %v", err))
	}

	child.Structure = safeWalkBody(nested, child, depth+1, st)

	if child.Truncated {
		msg.Truncated = true
	}
	msg.Nested = append(msg.Nested, child)
	return node
}

// walkLeaf extracts a single non-multipart, non-message/rfc822 part:
// inline text/plain and text/html are appended to msg.Text/msg.HTML,
// everything else (and any text part explicitly marked as an attachment)
// is recorded in msg.Attachments. Every part is also recorded in msg.Parts,
// and returned as a leaf node of the tree.
func walkLeaf(ent *message.Entity, mediaType string, ctParams map[string]string, msg *Message, st *walkState) *Structure {
	disp, dispParams, _ := ent.Header.ContentDisposition()
	disp = strings.ToLower(strings.TrimSpace(disp))
	filename := attachmentFilename(ctParams, dispParams)
	contentID := strings.Trim(strings.TrimSpace(ent.Header.Get("Content-Id")), "<>")

	budget := MaxTotalSize - st.size
	if budget <= 0 {
		msg.Truncated = true
		msg.Warnings = append(msg.Warnings, "mime: cumulative size limit reached; part skipped")
		return nil
	}

	data, readErr := safeReadAll(ent.Body, budget+1)
	if int64(len(data)) > budget {
		data = data[:budget]
		msg.Truncated = true
		msg.Warnings = append(msg.Warnings, "mime: part truncated at cumulative size limit")
	}
	st.size += int64(len(data))
	if readErr != nil {
		msg.Warnings = append(msg.Warnings, fmt.Sprintf("mime: reading part body (%s): %v", mediaType, readErr))
	}

	part := Part{
		ContentType: mediaType,
		Params:      ctParams,
		Disposition: disp,
		Filename:    filename,
		ContentID:   contentID,
	}
	node := &Structure{
		ContentType: mediaType,
		Params:      ctParams,
		Disposition: disp,
		Filename:    filename,
		ContentID:   contentID,
		// The octet count is taken BEFORE any charset transcoding: it is what
		// the part weighs on the wire, which is what a download reports.
		OctetSize: len(data),
	}

	// ⚠ ONE predicate, shared with AttachmentParts (partpath.go), so the
	// flattened Attachments slice and the path-carrying walk cannot disagree
	// about what an attachment is.
	if !IsAttachmentLeaf(mediaType, disp) {
		text, problem := decodeLeafText(data, ctParams, msg)
		part.Data = []byte(text)
		part.Size = len(text)
		node.Data = part.Data
		node.Size = part.Size
		node.EncodingProblem = problem
		if mediaType == "text/html" {
			msg.HTML += text
		} else {
			msg.Text += text
		}
	} else {
		part.Data = data
		part.Size = len(data)
		node.Data = data
		node.Size = len(data)
		msg.Attachments = append(msg.Attachments, Attachment{
			Filename:    filename,
			ContentType: mediaType,
			ContentID:   contentID,
			Data:        data,
			Size:        len(data),
		})
	}

	msg.Parts = append(msg.Parts, part)
	return node
}

// attachmentFilename picks a decoded filename from Content-Disposition's
// filename parameter, falling back to Content-Type's name parameter.
// Both are already RFC 2231 continuation/percent-decoded by the underlying
// mime.ParseMediaType call; sanitizeText covers any residual 8-bit bytes.
func attachmentFilename(ctParams, dispParams map[string]string) string {
	if fn := dispParams["filename"]; fn != "" {
		return sanitizeText(fn)
	}
	if fn := ctParams["name"]; fn != "" {
		return sanitizeText(fn)
	}
	return ""
}
