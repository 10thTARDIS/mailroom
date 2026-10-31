// SPDX-License-Identifier: AGPL-3.0-only

package census

import (
	"context"
	"time"
)

// Reader is the census's entire view of an IMAP server. It deliberately has
// no method that can change server state: no STORE, EXPUNGE, APPEND, COPY,
// MOVE, CREATE, DELETE, RENAME, SUBSCRIBE. That absence is the first of two
// read-only guarantees; the second is that Examine uses EXAMINE, so the
// server itself refuses flag changes (see imap.go's Examine doc for exactly
// how far that guarantee extends against this client library).
//
// TestReaderMethodSetIsExactlyReadOnly pins the method set with reflect: a
// pull request that adds a method here is a design change to the read-only
// guarantee, and must be reviewed as one, not merged as an incidental part
// of some other feature.
type Reader interface {
	// Capabilities returns the server's advertised capability names, raw
	// (uppercase, as the server sent them; see census.go's Supports for the
	// normalized subset the report actually reasons about).
	Capabilities(ctx context.Context) ([]string, error)

	// ListMailboxes lists every mailbox the account can see, with its raw
	// IMAP attributes (LIST, RFC 9051 §6.3.9, including RFC 6154 SPECIAL-USE
	// when the server advertises it).
	ListMailboxes(ctx context.Context) ([]Mailbox, error)

	// Examine opens name read-only (EXAMINE, never SELECT) and streams one
	// Meta per message in the mailbox to fn, in the order the server
	// returns them. It never buffers the whole mailbox: a fn that returns
	// promptly keeps peak memory to a small, bounded number of in-flight
	// messages, which is what makes this safe to run against a five-figure
	// mailbox. An error from fn aborts the fetch and is returned.
	Examine(ctx context.Context, name string, fn func(Meta) error) (Examined, error)

	// Close releases the connection. Safe to call once; see the
	// implementation's own doc for whether it also logs out.
	Close() error
}

// Mailbox is one mailbox as returned by ListMailboxes.
type Mailbox struct {
	Name string
	// Attributes are the mailbox's IMAP attributes, LOWERCASED (e.g.
	// "\\sent", "\\noselect") so callers can compare without worrying about
	// a server's casing choices. Includes RFC 6154 SPECIAL-USE roles when
	// the server advertises SPECIAL-USE.
	Attributes []string
}

// Examined is what EXAMINE (RFC 9051 §6.3.2) reported about a mailbox,
// before any messages were streamed.
type Examined struct {
	Messages    uint32
	UIDValidity uint32
	UIDNext     uint32
	// ReadOnly is true iff the server's response confirmed the mailbox was
	// opened READ-ONLY.
	//
	// ⚠ ALWAYS false with this client library. go-imap v2 beta.8's tagged
	// SELECT/EXAMINE response reader (imapclient's readResponseTagged, in
	// the module source) recognizes exactly three resp-text-codes —
	// CAPABILITY, APPENDUID, COPYUID — and silently discards any other
	// resp-text-code, including READ-ONLY and READ-WRITE. So whether the
	// server actually confirmed read-only access is NOT OBSERVABLE through
	// this library version, and this field cannot be populated from it.
	// This is a client-library limitation, not a design choice: the census's
	// read-only guarantee does not depend on this field. It rests instead on
	// (1) every call here being EXAMINE, never SELECT, so a compliant server
	// refuses write commands on this connection regardless of what it
	// reports back, and (2) Reader exposing no write method at all. Treat
	// ReadOnly as documentation of a gap, not as a safety signal to branch
	// on.
	ReadOnly bool
}

// Meta is one message's metadata, as reported by EXAMINE + FETCH. It never
// carries body bytes: the census counts and classifies, it does not read
// mail.
type Meta struct {
	UID uint32
	// Flags are exactly as the server sent them (e.g. "\Seen", "\Draft"),
	// not case-normalized; callers should compare case-insensitively.
	Flags        []string
	InternalDate time.Time
	// Size is RFC822.SIZE: the message's size on the wire, in octets.
	Size int64
	// MessageID is the Message-ID header value from ENVELOPE, angle
	// brackets stripped by the underlying library. "" if the message had
	// none.
	MessageID string
	// Structure is the message's MIME body structure (BODYSTRUCTURE,
	// extended form), or nil if the server returned none. See HasAttachment
	// for the one thing the census does with it.
	Structure *Part
}

// Part is one node of a message's MIME structure, as reported by
// BODYSTRUCTURE. Mailroom's own type: no go-imap type is exported from this
// package (anti-corruption layer, CLAUDE.md).
type Part struct {
	// MediaType is "type/subtype", lowercased (e.g. "text/plain",
	// "multipart/mixed", "message/rfc822").
	MediaType string
	// Disposition is the lowercased Content-Disposition value ("inline",
	// "attachment"), or "" if absent. Only meaningful on a leaf part.
	Disposition string
	// Children are a multipart container's parts, in document order. Always
	// nil for a leaf, including message/rfc822: the nested message's own
	// structure is never descended into here (see imap.go's mapping and
	// HasAttachment's doc for why).
	Children []*Part
}
