// SPDX-License-Identifier: AGPL-3.0-only

package pgpmime

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
)

// Building an RFC 3156 §4 multipart/encrypted (#47, ADR-038): Sign's sibling, and
// SplitEncrypted/Rewrap read backwards.
//
// Like Sign this package holds no key and does no cryptography. Encryption
// arrives as a callback, which in Mailroom is a call across the crypto-process
// boundary.

// EncryptFunc turns the MIME entity to protect into an ASCII-armored OpenPGP
// message (a PGP MESSAGE block).
type EncryptFunc func(entity []byte) (armored string, err error)

const (
	msgArmorBegin = "-----BEGIN PGP MESSAGE-----"
	msgArmorEnd   = "-----END PGP MESSAGE-----"
)

// encryptedPreamble is the text a client that cannot read the message shows. It
// is the wording RFC 3156 §4 itself uses.
const encryptedPreamble = "This is an OpenPGP/MIME encrypted message (RFC 4880 and 3156)\r\n"

// Encrypt wraps an assembled RFC 5322 message in an RFC 3156 multipart/encrypted.
//
// The message's content headers and body become the ENTITY handed to enc (and so
// travel inside the ciphertext); every other header — From, To, Subject, Date,
// Message-ID, Autocrypt — stays on the outer message, in its original order,
// because a transport has to route on it. MIME-Version and Content-Type at the top
// level are replaced.
//
// ⚠ THE SUBJECT AND THE RECIPIENTS TRAVEL IN THE CLEAR. PGP/MIME without protected
// headers (RFC 9788) encrypts the body only, and ADR-037 declines to honour them on
// the read side; writing them here without a reader would hide the Subject from the
// recipient's client. Out of scope, and a documented property of the product.
//
// Pass a message that has already been signed (Sign) to produce RFC 3156 §6.1's
// sign-then-encrypt: the entity is then the multipart/signed container.
func Encrypt(raw []byte, enc EncryptFunc) ([]byte, error) {
	if enc == nil {
		return nil, errors.New("pgpmime: nil encrypter")
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
			// Re-emitted below; the only legal value is 1.0.
		default:
			top = append(top, f)
		}
	}
	entity := renderEntity(content, body)

	armored, err := enc(entity)
	if err != nil {
		return nil, fmt.Errorf("pgpmime: encrypt: %w", err)
	}
	if !strings.Contains(armored, msgArmorBegin) || !strings.Contains(armored, msgArmorEnd) {
		// An encrypter that returned something else — an empty string, an error
		// page, a public key — would otherwise produce a message that looks
		// encrypted and opens nowhere (or, worse, carries plaintext).
		return nil, errors.New("pgpmime: the encrypter did not return an armored PGP message")
	}
	ciphertext := bytes.TrimRight(Canonicalize([]byte(armored)), "\r\n")

	boundary, err := chooseBoundary(ciphertext, []byte(encryptedPreamble))
	if err != nil {
		return nil, err
	}

	var out bytes.Buffer
	for _, f := range top {
		out.Write(f.raw)
		out.WriteString("\r\n")
	}
	out.WriteString("MIME-Version: 1.0\r\n")
	out.WriteString("Content-Type: multipart/encrypted; boundary=\"" + boundary + "\"; " +
		"protocol=\"" + EncryptedProtocol + "\"\r\n")
	out.WriteString("\r\n")
	out.WriteString(encryptedPreamble)
	out.WriteString("--" + boundary + "\r\n")
	out.WriteString("Content-Type: " + EncryptedProtocol + "\r\n")
	out.WriteString("Content-Description: PGP/MIME version identification\r\n")
	out.WriteString("\r\n")
	out.WriteString("Version: 1\r\n")
	// The CRLF before a delimiter belongs to the delimiter (RFC 2046 §5.1.1), as
	// in Sign.
	out.WriteString("\r\n--" + boundary + "\r\n")
	out.WriteString("Content-Type: application/octet-stream; name=\"encrypted.asc\"\r\n")
	out.WriteString("Content-Description: OpenPGP encrypted message\r\n")
	out.WriteString("Content-Disposition: inline; filename=\"encrypted.asc\"\r\n")
	out.WriteString("\r\n")
	out.Write(ciphertext)
	out.WriteString("\r\n--" + boundary + "--\r\n")
	return out.Bytes(), nil
}
