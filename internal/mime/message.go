// SPDX-License-Identifier: AGPL-3.0-only

package mime

// Message is Mailroom's normalized representation of a parsed RFC
// 822/2822/5322 message, produced by Parse/ParseBytes. It never exposes
// types from the underlying parsing library (CLAUDE.md anti-corruption-layer
// rule).
type Message struct {
	// Header holds decoded, RFC 2047-tolerant access to the top-level
	// message headers.
	Header Header

	// Text is the concatenation (in document order) of every text/plain
	// body part's decoded text, using the charset/8-bit fallback rules in
	// docs/mime-policy.md. It does not include text from Nested messages.
	Text string

	// HTML is the concatenation (in document order) of every text/html
	// body part's decoded text. It does not include HTML from Nested
	// messages.
	HTML string

	// Parts lists every leaf MIME part encountered directly under this
	// message, in document order — both inline text/HTML parts and
	// attachments — for callers that want the raw structure rather than
	// the flattened Text/HTML/Attachments views. Parts belonging to a
	// Nested message are recorded on that nested Message, not here.
	Parts []Part

	// Attachments lists every leaf part classified as an attachment
	// (Content-Disposition: attachment, or any non-text part). A subset of
	// Parts, provided for convenience.
	Attachments []Attachment

	// Nested holds recursively-parsed embedded message/rfc822 (and
	// message/global) parts, bounded by MaxDepth, MaxParts and
	// MaxTotalSize (docs/mime-policy.md).
	Nested []*Message

	// Structure is the message's MIME TREE: the root node, with SubParts for
	// every multipart container. Text/HTML/Parts/Attachments above are the
	// FLATTENED views of the same data (what ingest and search want); this is
	// the shape-preserving one, for callers that must reproduce the structure
	// itself — JMAP's Email.bodyStructure and the RFC 8621 §4.1.4 algorithm
	// that picks textBody/htmlBody out of a multipart/alternative both need to
	// know which parts were siblings under which container, which the flat
	// views cannot say.
	//
	// nil only when the message could not be parsed at all (see ParseBytes).
	Structure *Structure

	// Truncated is true if parsing this message — or any part of its
	// Nested tree — stopped early because a defensive limit was hit
	// (recursion depth, part count, cumulative size) or because a panic
	// was recovered from partway through parsing. The Message is always
	// safe to use even when Truncated is true; it may just be incomplete.
	Truncated bool

	// Warnings collects non-fatal issues encountered while parsing:
	// malformed encoded-words, unsupported charsets, decode fallbacks,
	// recovered panics, and truncation reasons. Never fatal by itself —
	// see docs/mime-policy.md for what each class of warning means.
	Warnings []string
}

// Part is one leaf MIME part: a part with no further multipart or nested
// message/rfc822 structure of its own.
type Part struct {
	// ContentType is the part's media type, e.g. "text/plain". Defaults to
	// "text/plain" if the part had no Content-Type header (RFC 2045).
	ContentType string
	// Params are the Content-Type parameters (e.g. "charset", "name"),
	// RFC 2231 extended-parameter continuations already joined and
	// percent-decoded by the underlying library.
	Params map[string]string
	// Disposition is the lowercased Content-Disposition value ("inline",
	// "attachment"), or "" if the header was absent or unparsable.
	Disposition string
	// Filename is the best-effort decoded attachment/part filename, from
	// Content-Disposition's filename parameter or, failing that,
	// Content-Type's name parameter. "" if neither is present.
	Filename string
	// ContentID is the part's Content-ID header value with angle brackets
	// stripped, or "" if absent.
	ContentID string
	// Data is the part's decoded content: transfer-encoding (base64/
	// quoted-printable) is always removed; for text/* parts the bytes are
	// additionally charset-decoded to UTF-8 per docs/mime-policy.md, so
	// Data for a text part is valid UTF-8 text.
	Data []byte
	// Size is len(Data), provided for convenience.
	Size int
}

// Structure is one node of a message's MIME tree: either a multipart
// container (SubParts non-nil, Data nil) or a leaf part (Data holds the
// decoded content). A message/rfc822 part is a LEAF here — its own inner
// structure is reachable through Message.Nested — which is also how RFC 8621
// models it (only multipart/* parts have subParts).
type Structure struct {
	// ContentType is the node's media type, lowercased, defaulting to
	// "text/plain" per RFC 2045 when the part carried no Content-Type.
	ContentType string
	// Params are the Content-Type parameters (notably "charset").
	Params map[string]string
	// Disposition is the lowercased Content-Disposition value ("inline",
	// "attachment"), or "" if absent/unparsable.
	Filename    string
	Disposition string
	// ContentID is the Content-ID header with angle brackets stripped.
	ContentID string

	// Data is a LEAF's decoded content, with exactly the decoding rules of
	// Part.Data: transfer-encoding always removed, and for text/* parts the
	// bytes are additionally charset-decoded to UTF-8. nil for multipart
	// containers.
	Data []byte
	// Size is len(Data).
	Size int
	// OctetSize is the size of the part's content in octets after removing
	// the Content-Transfer-Encoding but BEFORE any charset decoding — i.e.
	// the number of octets a client would download. It differs from Size only
	// for a text part that was transcoded to UTF-8 from another charset. This
	// is what RFC 8621's EmailBodyPart.size means.
	OctetSize int
	// EncodingProblem is true when the charset/8-bit fallback policy had to
	// be applied to this part (undeclared/unknown charset, or invalid UTF-8):
	// the text is still usable, but it is a best-effort reinterpretation.
	// RFC 8621's EmailBodyValue.isEncodingProblem is exactly this signal.
	EncodingProblem bool

	// SubParts are a multipart container's children, in document order. nil
	// for every leaf (including message/rfc822).
	SubParts []*Structure
}

// Attachment is a Part classified as an attachment (as opposed to inline
// text/HTML body content). A convenience view over the same data recorded
// in Message.Parts.
type Attachment struct {
	Filename    string
	ContentType string
	ContentID   string
	Data        []byte
	Size        int
}

// Address is a single RFC 5322 mailbox address (a display name plus an
// addr-spec).
type Address struct {
	// Name is the decoded display name, e.g. "Alice Example". "" if the
	// address has no display name.
	Name string
	// Address is the addr-spec, e.g. "alice@example.com".
	Address string
	// EAI is true if Address contains non-ASCII bytes in the local-part or
	// domain, meaning it would require SMTPUTF8 (RFC 6531) to send. This is
	// a syntactic detection flag only — see eai.go and spec §18.1
	// (Forgejo issue #17): the compose-time behavior (reject vs. pass
	// through) is PENDING the provider's SMTPUTF8 support answer.
	EAI bool
}
