// SPDX-License-Identifier: AGPL-3.0-only

package mime

import (
	"bytes"
	"fmt"
	"io"

	// Registers message.CharsetReader so declared charsets beyond
	// UTF-8/US-ASCII (Windows-1252, ISO-8859-*, Shift-JIS, GBK, ...) are
	// recognized and decoded to UTF-8 (docs/mime-policy.md). Charsets this
	// package doesn't recognize still don't crash anything — see charset.go
	// for the replacement-character fallback that covers "the rest".
	_ "github.com/emersion/go-message/charset"
)

// Parse reads a raw RFC 822/2822/5322 message from r and parses it into a
// normalized Message. It never panics: hostile/malformed input degrades to
// a Message with Warnings/Truncated set, or — only when the input can't
// even be split into a header block, e.g. empty input or a header line
// with no colon — a non-nil error alongside a best-effort (typically
// near-empty) Message. See docs/mime-policy.md for the full policy.
func Parse(r io.Reader) (*Message, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("mime: read input: %w", err)
	}
	return ParseBytes(raw)
}

// ParseBytes parses a raw RFC 822/2822/5322 message already held in
// memory. See Parse for the panic/error contract.
func ParseBytes(raw []byte) (msg *Message, err error) {
	msg = &Message{}
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("mime: recovered parsing message: %v", rec)
			msg.Truncated = true
			msg.Warnings = append(msg.Warnings, fmt.Sprintf("mime: recovered: %v", rec))
		}
	}()

	ent, readErr := safeReadEntity(bytes.NewReader(raw))
	if ent == nil {
		// The header block itself couldn't be parsed at all (e.g. empty
		// input, or a header line with no colon in it). Still return a
		// usable, non-nil, best-effort Message rather than forcing every
		// caller to nil-check.
		msg.Truncated = true
		msg.Warnings = append(msg.Warnings, fmt.Sprintf("mime: could not parse message header: %v", readErr))
		return msg, fmt.Errorf("mime: parse message header: %w", readErr)
	}

	msg = parseEntity(ent)

	if readErr != nil {
		// go-message returns a non-nil, readable entity alongside
		// UnknownCharsetError/UnknownEncodingError when the *top-level*
		// Content-Type/Content-Transfer-Encoding is unsupported. Not
		// fatal — decodeLeafText's fallback already handled the bytes.
		msg.Warnings = append(msg.Warnings, fmt.Sprintf("mime: top-level body: %v", readErr))
	}

	return msg, nil
}
