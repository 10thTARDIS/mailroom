// SPDX-License-Identifier: AGPL-3.0-only

// Package mime is Mailroom's anti-corruption layer around RFC 822/2822/5322
// message parsing (CLAUDE.md: "wrap third-party protocol libraries behind
// our own interfaces"). It parses raw messages into a normalized,
// Mailroom-owned structure (Message, Header, Part, Attachment, Address);
// callers never see types from the underlying parsing library.
//
// The parsing engine is github.com/emersion/go-message (plus its mail and
// charset subpackages), but that dependency is confined to this package.
// Parsing is deliberately tolerant of hostile/malformed input: Parse and
// ParseBytes never panic (they recover internally and degrade to an error
// or a best-effort partial Message), and a set of explicit, documented
// fallback rules governs malformed encoded-words, unknown charsets,
// unencoded 8-bit bytes, and pathologically nested messages. See
// docs/mime-policy.md for the full policy that this package implements,
// and testdata/corpus for the golden corpus of hostile fixtures that
// exercises it.
package mime
