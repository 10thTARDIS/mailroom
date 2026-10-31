// SPDX-License-Identifier: AGPL-3.0-only

package pgpmime

import (
	"bytes"
	"errors"
	"fmt"
	"mime"
	"strings"
)

// Splitting a received multipart/signed back into the bytes that were signed
// (#47 increment 6a). This is Sign read backwards, and it has to agree with it
// byte for byte or every verification fails.
//
// # Why this cannot go through a MIME parser
//
// internal/mime decodes: it strips Content-Transfer-Encoding and charset-decodes
// text parts. Those are the right rules for displaying a message and exactly the
// wrong ones here — the signature covers the bytes AS TRANSMITTED, so a part
// that arrived quoted-printable must be hashed quoted-printable. Handing a
// decoded part to a verifier produces "bad signature" on mail that is perfectly
// valid, and the failure looks like a crypto problem rather than a parsing one.
//
// So this walks bytes, and takes the boundary from the header rather than
// guessing.

// ErrNotSigned means the message is not an RFC 3156 multipart/signed with the
// OpenPGP protocol. It is not a failure — most mail is not signed — and callers
// report it as "nothing to verify" rather than as an error.
var ErrNotSigned = errors.New("pgpmime: not a multipart/signed OpenPGP message")

// SignedParts is a received multipart/signed, taken apart.
type SignedParts struct {
	// Entity is part one EXACTLY as it arrived, its content headers included.
	// Canonicalize this to get the bytes the signature covers.
	Entity []byte
	// Signature is the ASCII-armored detached signature from part two.
	Signature []byte
	// MicAlg is the container's declared micalg, lowercased. It is a HINT, not
	// a fact: the signature packet carries the real hash, and a mismatch
	// between the two is a malformed message rather than a verification
	// failure. Reported so a caller can say so.
	MicAlg string
}

// SplitSigned takes a received message apart into the signed entity and the
// signature.
//
// ⚠ THE CRLF BEFORE A BOUNDARY DELIMITER BELONGS TO THE DELIMITER (RFC 2046
// §5.1.1), not to the part before it. Including it in the entity hashes one byte
// more than was signed; excluding a genuine trailing newline hashes one byte
// less. Both produce a "bad signature" on a message that is completely valid,
// and neither is visible by looking at the output. Sign emits that CRLF
// deliberately; this removes exactly the same one.
func SplitSigned(raw []byte) (SignedParts, error) {
	head, body, err := splitMessage(raw)
	if err != nil {
		return SignedParts{}, err
	}
	fields, err := parseFields(head)
	if err != nil {
		return SignedParts{}, err
	}

	var ctype string
	for _, f := range fields {
		if f.name == "content-type" {
			ctype = unfold(f.raw)
		}
	}
	if ctype == "" {
		return SignedParts{}, ErrNotSigned
	}

	mediaType, params, err := mime.ParseMediaType(valueOf(ctype))
	if err != nil {
		return SignedParts{}, fmt.Errorf("pgpmime: unparsable Content-Type: %w", err)
	}
	if !strings.EqualFold(mediaType, "multipart/signed") {
		return SignedParts{}, ErrNotSigned
	}
	// The protocol parameter is what separates a PGP signature from an S/MIME
	// one on an otherwise identical container. Refusing here rather than
	// discovering it later means an S/MIME message is reported as "not PGP"
	// instead of as a broken PGP signature.
	if !strings.EqualFold(strings.TrimSpace(params["protocol"]), SignatureProtocol) {
		return SignedParts{}, ErrNotSigned
	}
	boundary := params["boundary"]
	if boundary == "" {
		return SignedParts{}, errors.New("pgpmime: multipart/signed with no boundary")
	}

	parts, err := splitOnBoundary(body, boundary)
	if err != nil {
		return SignedParts{}, err
	}
	// RFC 3156 §5: exactly two parts. More is not a signed message we can
	// reason about, and picking the first two would be guessing at which the
	// signature covers.
	if len(parts) != 2 {
		return SignedParts{}, fmt.Errorf("pgpmime: multipart/signed has %d parts, want 2", len(parts))
	}

	sigEntity := parts[1]
	_, sigBody, err := splitMessage(sigEntity)
	if err != nil {
		return SignedParts{}, fmt.Errorf("pgpmime: signature part has no body: %w", err)
	}
	if !bytes.Contains(sigBody, []byte(sigArmorBegin)) || !bytes.Contains(sigBody, []byte(sigArmorEnd)) {
		return SignedParts{}, errors.New("pgpmime: part two is not an armored PGP signature")
	}

	return SignedParts{
		Entity:    parts[0],
		Signature: sigBody,
		MicAlg:    strings.ToLower(strings.TrimSpace(params["micalg"])),
	}, nil
}

// splitOnBoundary returns the bodies between delimiters, with the CRLF that
// precedes each delimiter removed (it belongs to the delimiter).
//
// Line endings are accepted as CRLF or bare LF. A transport that rewrote them
// is exactly the case RFC 3156 canonicalisation exists to survive, so refusing
// such a message here would defeat the mechanism one step before it worked.
func splitOnBoundary(body []byte, boundary string) ([][]byte, error) {
	delim := "--" + boundary
	var parts [][]byte

	lines := splitKeepEnds(body)
	var cur []byte
	started := false
	for _, line := range lines {
		trimmed := strings.TrimRight(string(line), "\r\n")
		if trimmed == delim || trimmed == delim+"--" {
			if started {
				parts = append(parts, trimTrailingEOL(cur))
			}
			cur = nil
			started = trimmed == delim
			if trimmed == delim+"--" {
				break
			}
			continue
		}
		if started {
			cur = append(cur, line...)
		}
		// Anything before the first delimiter is the preamble, and is not part
		// of any entity.
	}
	if len(parts) == 0 {
		return nil, errors.New("pgpmime: no parts found for the declared boundary")
	}
	return parts, nil
}

// trimTrailingEOL removes ONE trailing line ending — the delimiter's, not the
// part's. Removing more would eat a blank line that is genuinely content.
func trimTrailingEOL(b []byte) []byte {
	switch {
	case bytes.HasSuffix(b, []byte("\r\n")):
		return b[:len(b)-2]
	case bytes.HasSuffix(b, []byte("\n")):
		return b[:len(b)-1]
	}
	return b
}

func splitKeepEnds(b []byte) [][]byte {
	var out [][]byte
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			out = append(out, b)
			break
		}
		out = append(out, b[:i+1])
		b = b[i+1:]
	}
	return out
}

// unfold joins a header field's continuation lines into one line.
func unfold(raw []byte) string {
	s := string(raw)
	s = strings.ReplaceAll(s, "\r\n", "\n")
	var b strings.Builder
	for i, line := range strings.Split(s, "\n") {
		if i > 0 {
			b.WriteString(" ")
			line = strings.TrimLeft(line, " \t")
		}
		b.WriteString(line)
	}
	return b.String()
}

// valueOf returns the part of a header field after the colon.
func valueOf(field string) string {
	if i := strings.IndexByte(field, ':'); i >= 0 {
		return strings.TrimSpace(field[i+1:])
	}
	return field
}
