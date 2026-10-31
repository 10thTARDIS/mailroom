// SPDX-License-Identifier: AGPL-3.0-only

package mime

import (
	"fmt"
	"io"

	message "github.com/emersion/go-message"
)

// This file collects every point where we call into the underlying parsing
// library on a hostile byte stream and wraps it with recover(), converting
// any panic into an error or a Warnings entry instead — the "never panic on
// hostile input" requirement (docs/mime-policy.md). None of go-message's
// documented behavior is known to panic, but the library was not written
// with an adversarial-input threat model in mind, so we don't rely on that.

// safeReadEntity parses a message header (and returns a lazily-decoded
// body) from r, recovering from any panic.
func safeReadEntity(r io.Reader) (ent *message.Entity, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("mime: recovered reading message: %v", rec)
		}
	}()
	return message.Read(r)
}

// safeNextPart reads the next part of a multipart entity, recovering from
// any panic.
func safeNextPart(mr message.MultipartReader) (part *message.Entity, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("mime: recovered reading multipart part: %v", rec)
		}
	}()
	return mr.NextPart()
}

// safeWalkBody runs walkBody with a recover boundary, so a panic while
// processing one part (or one nested message) can't take down the whole
// parse — it degrades to Truncated + a Warnings entry on msg instead (and a
// nil tree node for the part that blew up).
func safeWalkBody(ent *message.Entity, msg *Message, depth int, st *walkState) (node *Structure) {
	defer func() {
		if rec := recover(); rec != nil {
			node = nil
			msg.Truncated = true
			msg.Warnings = append(msg.Warnings, fmt.Sprintf("mime: recovered walking part: %v", rec))
		}
	}()
	return walkBody(ent, msg, depth, st)
}

// safeReadAll reads up to limit bytes from r, recovering from any panic in
// r's Read (e.g. a malformed base64/quoted-printable decoder). A non-nil
// err from a partial read is not fatal to the caller — whatever bytes were
// read are still returned and used.
func safeReadAll(r io.Reader, limit int64) (data []byte, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("mime: recovered reading part body: %v", rec)
		}
	}()
	lr := &io.LimitedReader{R: r, N: limit}
	return io.ReadAll(lr)
}
