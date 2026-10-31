// SPDX-License-Identifier: AGPL-3.0-only

// Package pgpmime assembles RFC 3156 multipart/signed messages.
//
// It holds NO key material and does no cryptography: signing arrives as a
// callback, which in Mailroom is a call across the crypto-process boundary
// (ADR-024). That split is the point — this package is pure byte handling and
// can be tested exhaustively without a key, and the key holder never has to
// know what MIME is.
//
// # What is hard here
//
// Nothing about multipart/signed is hard to produce. It is hard to produce so
// that somebody ELSE accepts it, and the failure mode is silent: a message that
// verifies against the code that made it and shows "bad signature" in
// Thunderbird. Three details decide it, and all three are off-by-ones:
//
//  1. The signed entity is the whole original body entity INCLUDING its own
//     Content-Type and other content headers — not the payload alone. The
//     message's own headers (From, To, Subject, Date) are not signed, which is
//     an RFC 3156 property worth knowing rather than a limitation to fix here:
//     PGP/MIME does not authenticate the Subject line.
//  2. The bytes signed are the CANONICAL form (see Canonicalize), while the
//     bytes transmitted are the original. That asymmetry is deliberate and is
//     what makes the signature survive transport; see the note on Sign.
//  3. The CRLF before a boundary delimiter belongs to the delimiter, not to the
//     part. Sign therefore emits an extra CRLF of its own, so that a part whose
//     content genuinely ends in a newline keeps it.
package pgpmime

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// SignFunc produces an ASCII-armored DETACHED signature over canonical, and
// reports the micalg parameter value naming the hash it used.
//
// micalg comes back from the signer rather than being chosen by the caller
// because the signer is the only party that knows: a key's own algorithm
// preferences can override the hash that was asked for, and a micalg that
// disagrees with the signature makes some verifiers reject the message.
type SignFunc func(canonical []byte) (signature string, micalg string, err error)

// SignatureProtocol is RFC 3156's protocol parameter value. It appears in the
// top-level Content-Type and MUST match part two's own type, or a verifier is
// entitled to treat the message as unsigned.
const SignatureProtocol = "application/pgp-signature"

const (
	sigArmorBegin = "-----BEGIN PGP SIGNATURE-----"
	sigArmorEnd   = "-----END PGP SIGNATURE-----"
)

// preamble sits before the first boundary, where MIME-aware readers ignore it
// and everything else shows it. It is the only text in the output a human might
// see if their client cannot read the message at all.
const preamble = "This is an OpenPGP/MIME signed message (RFC 3156).\r\n"

// ErrNoBody means the input was not a complete RFC 5322 message: there was no
// blank line separating headers from body.
var ErrNoBody = errors.New("pgpmime: message has no header/body separator")

// Sign wraps an assembled RFC 5322 message in an RFC 3156 multipart/signed.
//
// The original message's content headers travel WITH the body into part one and
// are covered by the signature; its message headers stay at the top level and
// are not. MIME-Version and Content-Type at the top level are replaced; every
// other original header is preserved in its original order.
//
// # Part one is transmitted verbatim, and signed canonically
//
// The signature is computed over Canonicalize(part one); part one itself goes
// out as the caller wrote it. This is what RFC 3156 §5 describes, and it is
// what makes the signature survive an MTA that rewrites line endings — the
// recipient canonicalises what arrived and gets the same bytes that were
// hashed, whatever happened in between.
//
// It does assume the recipient canonicalises. A verifier that hashes the raw
// received part instead will reject a message whose body had bare LFs or
// trailing whitespace. Mail clients do canonicalise (it is the only way PGP/MIME
// works at all); `gpg --verify` on a hand-extracted part does not, which is
// worth knowing before debugging a report of a "bad signature" that way.
//
// # What is NOT done
//
// The body is not re-encoded. RFC 3156 §3 asks that the signed part be in a
// transfer encoding that survives a 7-bit hop, and Sign will happily wrap 8-bit
// content if it is handed some — because the alternative, quietly re-encoding,
// would sign bytes the caller never agreed to send. Producing a transport-safe
// body is the composer's job, upstream of here.
func Sign(raw []byte, sign SignFunc) ([]byte, error) {
	if sign == nil {
		return nil, errors.New("pgpmime: nil signer")
	}

	head, body, err := splitMessage(raw)
	if err != nil {
		return nil, err
	}
	fields, err := parseFields(head)
	if err != nil {
		return nil, err
	}

	var top, content []field
	for _, f := range fields {
		switch {
		case entityHeaders[f.name]:
			content = append(content, f)
		case f.name == "mime-version":
			// Dropped, then re-emitted below. The only legal value is 1.0, and
			// the one on the new outer message is the one that must be right.
		default:
			top = append(top, f)
		}
	}

	partOne := renderEntity(content, body)

	sigArmor, micalg, err := sign(Canonicalize(partOne))
	if err != nil {
		return nil, fmt.Errorf("pgpmime: sign: %w", err)
	}
	if !strings.Contains(sigArmor, sigArmorBegin) || !strings.Contains(sigArmor, sigArmorEnd) {
		// A signer that returned something else — an empty string, an error page,
		// a public key — would otherwise produce a message that looks signed and
		// verifies nowhere.
		return nil, errors.New("pgpmime: the signer did not return an armored PGP signature")
	}
	micalg, err = checkMicAlg(micalg)
	if err != nil {
		return nil, err
	}

	// Trailing newlines come off because the CRLF before the closing delimiter
	// supplies the last one. Leaving them adds a blank line inside part two,
	// which is legal and looks like a bug forever after.
	partTwo := renderSignatureEntity(bytes.TrimRight(Canonicalize([]byte(sigArmor)), "\r\n"))

	boundary, err := chooseBoundary(partOne, partTwo, []byte(preamble))
	if err != nil {
		return nil, err
	}

	var out bytes.Buffer
	for _, f := range top {
		out.Write(f.raw)
		out.WriteString("\r\n")
	}
	out.WriteString("MIME-Version: 1.0\r\n")
	out.WriteString("Content-Type: multipart/signed; boundary=\"" + boundary + "\"; " +
		"protocol=\"" + SignatureProtocol + "\"; micalg=\"" + micalg + "\"\r\n")
	out.WriteString("\r\n")
	out.WriteString(preamble)
	out.WriteString("--" + boundary + "\r\n")
	out.Write(partOne)
	// ⚠ THIS CRLF IS THE DELIMITER'S, NOT THE PART'S (RFC 2046 §5.1.1). Writing
	// the delimiter without it would eat the final newline of any body that had
	// one, and the recipient would then hash one byte less than was signed.
	out.WriteString("\r\n--" + boundary + "\r\n")
	out.Write(partTwo)
	out.WriteString("\r\n--" + boundary + "--\r\n")
	return out.Bytes(), nil
}

// field is one header field with its continuation lines already joined.
type field struct {
	// name is lowercased, for comparison only.
	name string
	// raw is the field exactly as it will be written, continuation lines
	// included, line endings normalised to CRLF, with no trailing CRLF.
	raw []byte
}

// entityHeaders are the header fields that describe the CONTENT rather than the
// MESSAGE, and therefore move into part one where the signature covers them.
//
// Exactly RFC 2045 §3's MIME entity headers plus Content-Disposition (RFC 2183),
// and deliberately not a "Content-*" prefix rule: a prefix match would drag
// along anything a future extension names that way, silently changing what is
// signed. MIME-Version is not here — RFC 2045 §4 puts it on the message, not on
// each entity.
var entityHeaders = map[string]bool{
	"content-type":              true,
	"content-transfer-encoding": true,
	"content-id":                true,
	"content-description":       true,
	"content-disposition":       true,
}

// splitMessage divides raw at the first blank line. The blank line itself
// belongs to neither half.
func splitMessage(raw []byte) (head, body []byte, err error) {
	for i := 0; i < len(raw); {
		j := bytes.IndexByte(raw[i:], '\n')
		if j < 0 {
			break
		}
		line := raw[i : i+j]
		if len(line) == 0 || (len(line) == 1 && line[0] == '\r') {
			return raw[:i], raw[i+j+1:], nil
		}
		i += j + 1
	}
	return nil, nil, ErrNoBody
}

// parseFields splits a header block into fields, joining continuation lines.
func parseFields(head []byte) ([]field, error) {
	var fields []field
	for _, line := range bytes.Split(head, []byte("\n")) {
		line = bytes.TrimSuffix(line, []byte("\r"))
		if len(line) == 0 {
			continue
		}
		if line[0] == ' ' || line[0] == '\t' {
			if len(fields) == 0 {
				return nil, errors.New("pgpmime: message begins with a header continuation line")
			}
			f := &fields[len(fields)-1]
			f.raw = append(f.raw, '\r', '\n')
			f.raw = append(f.raw, line...)
			continue
		}
		colon := bytes.IndexByte(line, ':')
		if colon <= 0 {
			return nil, fmt.Errorf("pgpmime: not a header field: %.40q", line)
		}
		fields = append(fields, field{
			name: strings.ToLower(strings.TrimSpace(string(line[:colon]))),
			raw:  append([]byte(nil), line...),
		})
	}
	return fields, nil
}

// renderEntity puts the content headers back in front of the body, which is
// what "the entity" means in RFC 3156 and is the thing the signature covers.
// The body is copied byte for byte; only the headers are re-emitted.
func renderEntity(content []field, body []byte) []byte {
	var b bytes.Buffer
	for _, f := range content {
		b.Write(f.raw)
		b.WriteString("\r\n")
	}
	b.WriteString("\r\n")
	b.Write(body)
	return b.Bytes()
}

func renderSignatureEntity(sig []byte) []byte {
	var b bytes.Buffer
	b.WriteString("Content-Type: " + SignatureProtocol + "; name=\"signature.asc\"\r\n")
	b.WriteString("Content-Description: OpenPGP digital signature\r\n")
	b.WriteString("Content-Disposition: attachment; filename=\"signature.asc\"\r\n")
	b.WriteString("\r\n")
	b.Write(sig)
	return b.Bytes()
}

// checkMicAlg refuses anything that is not a MIME token.
//
// The value goes straight into a header, so the check that matters is the one
// against CR and LF: a signer that returned "pgp-sha256\r\nBcc: someone" would
// otherwise inject a header into every signed message. The signer here is our
// own crypto process, which makes this cheap insurance rather than a threat
// model — and cheap insurance on the header-injection class is worth having.
func checkMicAlg(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", errors.New("pgpmime: the signer reported no micalg")
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '.', r == '_', r == '+':
		default:
			return "", fmt.Errorf("pgpmime: micalg %q is not a token", s)
		}
	}
	return s, nil
}

// boundaryAttempts bounds the regeneration loop. Reaching it means the content
// contains every boundary a CSPRNG produced eight times running, which is not a
// thing that happens — so the error exists to be a loud impossibility rather
// than an infinite loop.
const boundaryAttempts = 8

// boundarySource yields a candidate boundary. It is a variable ONLY so the
// collision test can force a candidate that appears in the content: with
// 144 bits of randomness the regeneration path below is otherwise unreachable
// code that has never once been executed, and unreachable code that guards
// against silent message corruption is not something to take on faith.
var boundarySource = randomBoundary

func randomBoundary() (string, error) {
	var b [18]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("pgpmime: generate boundary: %w", err)
	}
	return "mailroom-" + hex.EncodeToString(b[:]), nil
}

// chooseBoundary returns a delimiter that appears nowhere in the content.
//
// A boundary that collides does not fail loudly: the recipient's parser splits
// the message at the wrong place, and part one is truncated to something the
// signature does not cover. The message still "arrives".
func chooseBoundary(content ...[]byte) (string, error) {
	for range boundaryAttempts {
		cand, err := boundarySource()
		if err != nil {
			return "", err
		}
		if !containsAny(content, cand) {
			return cand, nil
		}
	}
	return "", errors.New("pgpmime: could not find a boundary absent from the content")
}

func containsAny(content [][]byte, s string) bool {
	for _, c := range content {
		if bytes.Contains(c, []byte(s)) {
			return true
		}
	}
	return false
}
