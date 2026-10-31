// SPDX-License-Identifier: AGPL-3.0-only

package pgpmime

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/quotedprintable"
	"strings"
)

// Taking a received RFC 3156 §4 multipart/encrypted apart (#47 increment 6b),
// and putting the decrypted entity back under the message's own headers.
//
// Like SplitSigned, this walks bytes rather than going through internal/mime.
// Here the reason is weaker — nothing is hashed, so decoding would not break a
// signature — but the boundary rules are the same, and one byte-level reading
// of RFC 2046 delimiters is easier to keep right than two.

// EncryptedProtocol is RFC 3156 §4's protocol parameter value, and the media
// type of the control part (part one).
const EncryptedProtocol = "application/pgp-encrypted"

// ErrNotEncrypted means the message is not an RFC 3156 multipart/encrypted with
// the OpenPGP protocol. Not a failure: most mail is not encrypted.
var ErrNotEncrypted = errors.New("pgpmime: not a multipart/encrypted OpenPGP message")

// maxCiphertext bounds what SplitEncrypted hands back. Anything larger cannot be
// sent to the crypto process anyway (pgpwire.MaxDecryptRequestBytes is 64 MiB
// AFTER JSON's base64, so about 48 MiB of ciphertext), so refusing it here keeps
// a hopeless request from being built.
const maxCiphertext = 46 << 20

// SplitEncrypted returns the ciphertext carried by a received multipart/encrypted:
// the body of part two, transfer-decoded, as the OpenPGP message (normally
// armored) the crypto process expects.
//
// RFC 3156 §4 requires exactly two parts: an application/pgp-encrypted control
// part containing "Version: 1", and an application/octet-stream part containing
// the OpenPGP message. Anything else is reported as malformed rather than
// guessed at — a guess about which part is the ciphertext is a guess about
// which bytes a crypto process is asked to open.
func SplitEncrypted(raw []byte) ([]byte, error) {
	head, body, err := splitMessage(raw)
	if err != nil {
		return nil, err
	}
	fields, err := parseFields(head)
	if err != nil {
		return nil, err
	}
	params, ok := encryptedParams(fields)
	if !ok {
		return nil, ErrNotEncrypted
	}
	boundary := params["boundary"]
	if boundary == "" {
		return nil, errors.New("pgpmime: multipart/encrypted with no boundary")
	}
	parts, err := splitOnBoundary(body, boundary)
	if err != nil {
		return nil, err
	}
	if len(parts) != 2 {
		return nil, fmt.Errorf("pgpmime: multipart/encrypted has %d parts, want 2", len(parts))
	}

	// Part one: the control part. Its type is the protocol; its body says
	// "Version: 1". A different version is not a message we know how to read.
	ctlHead, ctlBody, err := splitMessage(parts[0])
	if err != nil {
		return nil, fmt.Errorf("pgpmime: control part has no body: %w", err)
	}
	ctlType, _, _ := partType(ctlHead)
	if ctlType != EncryptedProtocol {
		return nil, fmt.Errorf("pgpmime: part one is %q, want %s", ctlType, EncryptedProtocol)
	}
	if !bytes.Contains(ctlBody, []byte("Version: 1")) {
		return nil, errors.New("pgpmime: control part does not say Version: 1")
	}

	// Part two: the OpenPGP message. RFC 3156 says application/octet-stream;
	// the transfer encoding is normally 7bit (it is armored), but a gateway may
	// have re-encoded it, so base64 and quoted-printable are undone here.
	ctHead, ctBody, err := splitMessage(parts[1])
	if err != nil {
		return nil, fmt.Errorf("pgpmime: ciphertext part has no body: %w", err)
	}
	dataType, cte, err := partType(ctHead)
	if err != nil {
		return nil, err
	}
	if dataType != "application/octet-stream" {
		return nil, fmt.Errorf("pgpmime: part two is %q, want application/octet-stream", dataType)
	}
	ct, err := decodeTransfer(ctBody, cte)
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(ct)) == 0 {
		return nil, errors.New("pgpmime: ciphertext part is empty")
	}
	if len(ct) > maxCiphertext {
		return nil, fmt.Errorf("pgpmime: ciphertext is %d bytes, over the %d this server will send for decryption", len(ct), maxCiphertext)
	}
	return ct, nil
}

// IsEncrypted reports whether raw's top-level Content-Type is an RFC 3156
// multipart/encrypted with the OpenPGP protocol. It does not check the parts.
func IsEncrypted(raw []byte) bool {
	head, _, err := splitMessage(raw)
	if err != nil {
		return false
	}
	fields, err := parseFields(head)
	if err != nil {
		return false
	}
	_, ok := encryptedParams(fields)
	return ok
}

// Rewrap replaces raw's content with a decrypted entity: the message headers of
// raw (From, To, Subject, Date, …) stay, its content headers go, and the
// entity's MIME CONTENT fields and body follow. The result is an RFC 5322
// message the ordinary body pipeline can render.
//
// ⚠ ONLY CONTENT FIELDS CROSS FROM THE ENTITY (rewrapContentFields). Encryption
// authenticates nothing — anyone can encrypt to a published key — so a header
// inside the ciphertext is exactly as trustworthy as the stranger who wrote it.
// Appending the entity verbatim would let `From: ceo@yourbank.example`, a
// second Subject, a Reply-To, or a forged Authentication-Results or
// DKIM-Signature hidden in the ciphertext land in the rebuilt header block:
// spoofing the visible sender, defeating the one-From rule (#754), and feeding
// the auth notice (#625, #776) input the provider never saw. Identity, trace
// and authentication headers always come from the OUTER, provider-delivered
// message. Every other inner field is dropped.
//
// ⚠ Protected headers (RFC 9788 / "memory hole": an inner Subject meant to
// replace the outer one) are NOT honoured — out of scope, and a later feature
// with its own security review (ADR-037). The Subject shown is the one that
// travelled in the clear.
//
// ⚠ THE RESULT IS PLAINTEXT AND MUST NEVER BE PERSISTED (ADR-037). It exists
// for the duration of one request: Email/get renders it, a part download
// extracts one part from it, and both drop it. Both go through this function.
func Rewrap(raw, entity []byte) ([]byte, error) {
	head, _, err := splitMessage(raw)
	if err != nil {
		return nil, err
	}
	fields, err := parseFields(head)
	if err != nil {
		return nil, err
	}
	innerHead, innerBody, err := splitMessage(entity)
	if err != nil {
		return nil, fmt.Errorf("pgpmime: decrypted entity: %w", err)
	}
	innerFields, err := parseFields(innerHead)
	if err != nil {
		return nil, fmt.Errorf("pgpmime: decrypted entity: %w", err)
	}
	var out bytes.Buffer
	for _, f := range fields {
		if entityHeaders[f.name] || f.name == "mime-version" || f.name == "content-language" {
			continue
		}
		out.Write(f.raw)
		out.WriteString("\r\n")
	}
	out.WriteString("MIME-Version: 1.0\r\n")
	for _, f := range innerFields {
		if !rewrapContentFields[f.name] {
			continue
		}
		out.Write(f.raw)
		out.WriteString("\r\n")
	}
	out.WriteString("\r\n")
	out.Write(innerBody)
	return out.Bytes(), nil
}

// rewrapContentFields are the ONLY fields Rewrap copies out of a decrypted
// entity: RFC 2045 §3's entity headers, Content-Disposition (RFC 2183) and
// Content-Language (RFC 3282). An allowlist, not a denylist, for the reason
// Rewrap gives: anything else in the ciphertext was written by whoever
// encrypted it, and must not reach the header block.
var rewrapContentFields = map[string]bool{
	"content-type":              true,
	"content-transfer-encoding": true,
	"content-disposition":       true,
	"content-id":                true,
	"content-description":       true,
	"content-language":          true,
}

func encryptedParams(fields []field) (map[string]string, bool) {
	var ctype string
	for _, f := range fields {
		if f.name == "content-type" {
			ctype = unfold(f.raw)
		}
	}
	if ctype == "" {
		return nil, false
	}
	mediaType, params, err := mime.ParseMediaType(valueOf(ctype))
	if err != nil || !strings.EqualFold(mediaType, "multipart/encrypted") {
		return nil, false
	}
	if !strings.EqualFold(strings.TrimSpace(params["protocol"]), EncryptedProtocol) {
		return nil, false
	}
	return params, true
}

// partType returns a part's lowercased media type and transfer encoding.
func partType(head []byte) (mediaType, cte string, err error) {
	fields, err := parseFields(head)
	if err != nil {
		return "", "", err
	}
	mediaType = "text/plain" // RFC 2045 §5.2's default
	for _, f := range fields {
		switch f.name {
		case "content-type":
			mt, _, perr := mime.ParseMediaType(valueOf(unfold(f.raw)))
			if perr != nil {
				return "", "", fmt.Errorf("pgpmime: unparsable part Content-Type: %w", perr)
			}
			mediaType = strings.ToLower(mt)
		case "content-transfer-encoding":
			cte = strings.ToLower(strings.TrimSpace(valueOf(unfold(f.raw))))
		}
	}
	return mediaType, cte, nil
}

func decodeTransfer(body []byte, cte string) ([]byte, error) {
	switch cte {
	case "", "7bit", "8bit", "binary":
		return body, nil
	case "base64":
		// Line breaks are legal inside base64 bodies; the decoder wants them gone.
		clean := bytes.Map(func(r rune) rune {
			if r == '\r' || r == '\n' || r == ' ' || r == '\t' {
				return -1
			}
			return r
		}, body)
		out := make([]byte, base64.StdEncoding.DecodedLen(len(clean)))
		n, err := base64.StdEncoding.Decode(out, clean)
		if err != nil {
			return nil, fmt.Errorf("pgpmime: ciphertext part is not valid base64: %w", err)
		}
		return out[:n], nil
	case "quoted-printable":
		out, err := io.ReadAll(quotedprintable.NewReader(bytes.NewReader(body)))
		if err != nil {
			return nil, fmt.Errorf("pgpmime: ciphertext part is not valid quoted-printable: %w", err)
		}
		return out, nil
	}
	return nil, fmt.Errorf("pgpmime: unknown transfer encoding %q on the ciphertext part", cte)
}
