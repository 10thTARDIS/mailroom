// SPDX-License-Identifier: AGPL-3.0-only

package pgpmime

import "bytes"

// Canonicalize renders b in the form RFC 3156 §5 requires a signature to be
// computed over: CRLF line endings, and no trailing space or tab on any line.
//
// Both rules exist because mail transport rewrites text. An MTA may convert
// line endings, and gateways have long been known to strip or add trailing
// whitespace; a signature over the bytes as they left here would then fail at
// the recipient, for a reason nobody on this side can observe. Canonicalising
// means both ends hash the same thing even when the bytes on the wire differ.
//
// Two properties verification depends on, neither of which is optional:
//
//   - IDEMPOTENCE. The recipient canonicalises what it received, which has
//     already been through this transformation once. Canonicalize(Canonicalize(x))
//     must equal Canonicalize(x) or the two sides hash different bytes.
//   - THE FINAL LINE ENDING IS PRESERVED, PRESENT OR ABSENT. "a\n" and "a" are
//     different content and must hash differently. The CRLF that terminates the
//     last line of a MIME part belongs to the part; the CRLF before the next
//     boundary delimiter belongs to the delimiter (RFC 2046 §5.1.1). Conflating
//     those two is the classic PGP/MIME off-by-one, and it produces a message
//     that verifies in whatever built it and nowhere else.
//
// A lone CR (old-Mac line endings) is deliberately left alone. It is not a line
// ending in any mail format in use, and treating it as one would rewrite bytes
// inside content the signature is supposed to protect exactly.
func Canonicalize(b []byte) []byte {
	lines := bytes.Split(b, []byte("\n"))
	for i, line := range lines {
		lines[i] = trimTrailingWhite(bytes.TrimSuffix(line, []byte("\r")))
	}
	return bytes.Join(lines, []byte("\r\n"))
}

// trimTrailingWhite removes trailing SP and HT. Only those two: RFC 3156 §5
// says "trailing white space", and anything else at the end of a line is
// content.
func trimTrailingWhite(line []byte) []byte {
	end := len(line)
	for end > 0 && (line[end-1] == ' ' || line[end-1] == '\t') {
		end--
	}
	return line[:end]
}
