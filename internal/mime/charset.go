// SPDX-License-Identifier: AGPL-3.0-only

package mime

import (
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding/charmap"
)

// This file implements the charset/8-bit fallback rules from
// docs/mime-policy.md. The underlying parsing library (go-message, with
// github.com/emersion/go-message/charset imported for its side effect in
// parse.go) already decodes any *recognized* declared charset to UTF-8 at
// entity-construction time. What's left for us to handle explicitly is:
//
//   - A declared charset that go-message/charset could not decode (unknown
//     or unsupported label): the bytes we see are still in the original,
//     undecoded charset. We can't know what they mean, so we pass ASCII
//     bytes through and substitute U+FFFD for anything non-ASCII.
//   - No charset declared, or charset declared as us-ascii/utf-8 (which
//     go-message passes through untouched): the bytes might still contain
//     unencoded 8-bit content. We try strict UTF-8 first (mislabeled UTF-8
//     is common), and only if that fails do we permissively reinterpret the
//     bytes as Windows-1252 — a full 256-byte charmap that can never itself
//     fail to produce *some* rune per byte.
//
// The same "valid UTF-8 first, else Windows-1252" rule also applies to
// already-materialized strings (e.g. raw header values untouched by RFC
// 2047 decoding), via sanitizeText.

// decodeWindows1252 permissively decodes data as Windows-1252. Windows-1252
// maps every byte 0x00-0xFF to a rune (a handful of unassigned code points
// are mapped to U+FFFD by the charmap table itself), so this can never
// fail — it is the policy's fallback for unencoded 8-bit bytes that aren't
// valid UTF-8.
func decodeWindows1252(data []byte) string {
	var b strings.Builder
	b.Grow(len(data))
	for _, c := range data {
		b.WriteRune(charmap.Windows1252.DecodeByte(c))
	}
	return b.String()
}

// replaceNonASCII substitutes the Unicode replacement character for every
// byte >= 0x80, leaving ASCII bytes untouched. Used when a declared
// charset is unknown/unsupported and go-message/charset could not decode
// it ("for the rest, replacement" — docs/mime-policy.md).
func replaceNonASCII(data []byte) string {
	var b strings.Builder
	b.Grow(len(data))
	for _, c := range data {
		if c < utf8.RuneSelf {
			b.WriteByte(c)
		} else {
			b.WriteRune(utf8.RuneError)
		}
	}
	return b.String()
}

// sanitizeText enforces the "unencoded 8-bit bytes" policy on an
// already-materialized string: valid UTF-8 passes through unchanged,
// anything else is permissively reinterpreted as Windows-1252.
func sanitizeText(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	return decodeWindows1252([]byte(s))
}

// decodeLeafText decodes a leaf text/* part's transfer-decoded body bytes
// to a Mailroom-owned UTF-8 string, applying the charset/8-bit policy
// above. msg collects a Warnings entry whenever a fallback path is taken, and
// problem reports (per part) whether one was — the signal JMAP surfaces as
// EmailBodyValue.isEncodingProblem (RFC 8621 §4.1.4).
func decodeLeafText(data []byte, ctParams map[string]string, msg *Message) (text string, problem bool) {
	charsetLabel := strings.ToLower(strings.TrimSpace(ctParams["charset"]))
	switch charsetLabel {
	case "", "utf-8", "utf8", "us-ascii", "ascii":
		if utf8.Valid(data) {
			return string(data), false
		}
		msg.Warnings = append(msg.Warnings,
			"mime: invalid UTF-8 in body declared as us-ascii/utf-8 (or undeclared); falling back to windows-1252")
		return decodeWindows1252(data), true
	default:
		// go-message/charset already tried to decode this charset when the
		// entity was constructed. If it succeeded, data is already valid
		// UTF-8 and the first branch below returns it unchanged. If it
		// failed (unknown/unsupported charset), data is still the raw,
		// undecoded original bytes.
		if utf8.Valid(data) {
			return string(data), false
		}
		msg.Warnings = append(msg.Warnings,
			"mime: charset \""+charsetLabel+"\" could not be decoded; substituting replacement characters")
		return replaceNonASCII(data), true
	}
}
