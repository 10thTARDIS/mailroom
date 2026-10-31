// SPDX-License-Identifier: AGPL-3.0-only

package crypto

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"

	"golang.org/x/crypto/chacha20poly1305"
)

// KeySize is the size, in bytes, of every symmetric key in the hierarchy
// (master, KEK, DEK). 256-bit keys throughout.
const KeySize = 32

// Envelope format version. Bumping this is how we evolve the wire format
// without ambiguity; Open rejects versions it does not understand.
const Version1 = 0x01

// Algorithm identifiers carried in the envelope/stream header.
//
//   - algXChaCha20Poly1305 is used by the single-shot envelope (Seal/Open).
//     It uses XChaCha20-Poly1305 with a 192-bit (24-byte) random nonce. The
//     extended nonce is chosen deliberately: with random nonces, a 24-byte
//     nonce makes accidental collision cryptographically negligible for any
//     realistic volume of blobs sealed under a single DEK, which the standard
//     96-bit IETF nonce cannot guarantee. Poly1305 gives a 128-bit tag.
//   - algChaCha20Poly1305Stream is used by the chunked stream. There the
//     nonce is counter-derived (not random) and a fresh per-stream subkey is
//     derived via HKDF, so the 96-bit IETF nonce is safe and preferable
//     (12-byte nonces, no per-chunk nonce storage). See stream.go.
const (
	algXChaCha20Poly1305      = 0x01
	algChaCha20Poly1305Stream = 0x02
)

// maxKeyIDLen bounds the opaque, length-prefixed key-id (uint16 field).
const maxKeyIDLen = 1<<16 - 1

// ErrShortEnvelope indicates an envelope too small to contain a valid header
// or ciphertext (truncated or not an envelope at all).
var ErrShortEnvelope = errors.New("crypto: envelope too short")

// Envelope byte layout (Version1, algXChaCha20Poly1305)
//
//	offset  size   field
//	------  ----   -----
//	0       1      version            (0x01)
//	1       1      algorithm id       (0x01 = XChaCha20-Poly1305)
//	2       2      key-id length N    (uint16, big-endian)
//	4       N      key-id             (opaque, caller-supplied; may be empty)
//	4+N     24     nonce              (XChaCha20 192-bit nonce, crypto/rand)
//	4+N+24  ..     ciphertext || tag  (plaintext length + 16-byte Poly1305 tag)
//
// The header bytes [0 : 4+N] (version, algorithm, key-id length, key-id) are
// bound into the AEAD associated data together with the caller-supplied aad,
// so version/algorithm/key-id are authenticated and cannot be tampered. The
// nonce is not in the AAD but is covered implicitly: altering it changes the
// derived keystream and fails tag verification. Because the header is
// self-delimiting (length-prefixed key-id), header||aad is an unambiguous
// framing of the associated data.

// header is a parsed envelope header.
type header struct {
	alg   byte
	keyID []byte // slice into the source envelope; do not mutate
	raw   []byte // the exact header bytes [0 : 4+N], for use as AAD
}

// buildHeader assembles the fixed envelope header for the given algorithm.
func buildHeader(version, alg byte, keyID []byte) ([]byte, error) {
	if len(keyID) > maxKeyIDLen {
		return nil, fmt.Errorf("crypto: key-id too long: %d bytes (max %d)", len(keyID), maxKeyIDLen)
	}
	h := make([]byte, 4+len(keyID))
	h[0] = version
	h[1] = alg
	binary.BigEndian.PutUint16(h[2:4], uint16(len(keyID)))
	copy(h[4:], keyID)
	return h, nil
}

// parseHeader parses and validates an envelope header, returning the header
// and the trailing bytes (nonce || ciphertext).
func parseHeader(envelope []byte) (header, []byte, error) {
	if len(envelope) < 4 {
		return header{}, nil, ErrShortEnvelope
	}
	if envelope[0] != Version1 {
		return header{}, nil, fmt.Errorf("crypto: unsupported envelope version 0x%02x", envelope[0])
	}
	keyIDLen := int(binary.BigEndian.Uint16(envelope[2:4]))
	hlen := 4 + keyIDLen
	if len(envelope) < hlen {
		return header{}, nil, ErrShortEnvelope
	}
	h := header{
		alg:   envelope[1],
		keyID: envelope[4:hlen],
		raw:   envelope[:hlen],
	}
	return h, envelope[hlen:], nil
}

// Seal encrypts plaintext under key (32 bytes) into a versioned envelope.
// keyID is opaque, caller-supplied material (e.g. a DEK/KEK version id) that
// is authenticated and recoverable from the envelope via KeyID. aad is
// additional associated data bound into the tag but not stored; Open must be
// given the identical aad. A fresh random nonce is used for every call.
func Seal(key, keyID, plaintext, aad []byte) ([]byte, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("crypto: seal: key must be %d bytes, got %d", KeySize, len(key))
	}
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, fmt.Errorf("crypto: seal: init aead: %w", err)
	}
	hdr, err := buildHeader(Version1, algXChaCha20Poly1305, keyID)
	if err != nil {
		return nil, fmt.Errorf("crypto: seal: %w", err)
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("crypto: seal: read nonce: %w", err)
	}
	ad := make([]byte, 0, len(hdr)+len(aad))
	ad = append(ad, hdr...)
	ad = append(ad, aad...)

	out := make([]byte, 0, len(hdr)+len(nonce)+len(plaintext)+aead.Overhead())
	out = append(out, hdr...)
	out = append(out, nonce...)
	out = aead.Seal(out, nonce, plaintext, ad)
	return out, nil
}

// Open decrypts and authenticates an envelope produced by Seal, using key.
// aad must equal the aad passed to Seal. Any tampering (header, nonce,
// ciphertext or tag), a wrong key, or a mismatched aad yields an error and no
// plaintext.
func Open(key, envelope, aad []byte) ([]byte, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("crypto: open: key must be %d bytes, got %d", KeySize, len(key))
	}
	h, rest, err := parseHeader(envelope)
	if err != nil {
		return nil, fmt.Errorf("crypto: open: %w", err)
	}
	if h.alg != algXChaCha20Poly1305 {
		return nil, fmt.Errorf("crypto: open: unsupported algorithm 0x%02x", h.alg)
	}
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, fmt.Errorf("crypto: open: init aead: %w", err)
	}
	if len(rest) < aead.NonceSize()+aead.Overhead() {
		return nil, fmt.Errorf("crypto: open: %w", ErrShortEnvelope)
	}
	nonce := rest[:aead.NonceSize()]
	ciphertext := rest[aead.NonceSize():]

	ad := make([]byte, 0, len(h.raw)+len(aad))
	ad = append(ad, h.raw...)
	ad = append(ad, aad...)

	plaintext, err := aead.Open(nil, nonce, ciphertext, ad)
	if err != nil {
		return nil, fmt.Errorf("crypto: open: authentication failed: %w", err)
	}
	return plaintext, nil
}

// OpenWith decrypts an envelope, selecting the key by its recovered key-id.
// keyFor is called with a copy of the opaque key-id and must return the
// 32-byte key that sealed the envelope. This is the ergonomic path when a
// caller stores many versions/purposes and must pick the right key first.
func OpenWith(keyFor func(keyID []byte) ([]byte, error), envelope, aad []byte) ([]byte, error) {
	id, err := KeyID(envelope)
	if err != nil {
		return nil, err
	}
	key, err := keyFor(id)
	if err != nil {
		return nil, fmt.Errorf("crypto: open: select key: %w", err)
	}
	return Open(key, envelope, aad)
}

// KeyID returns a copy of the opaque key-id from an envelope header without
// decrypting, so callers can select the appropriate key.
func KeyID(envelope []byte) ([]byte, error) {
	h, _, err := parseHeader(envelope)
	if err != nil {
		return nil, fmt.Errorf("crypto: key-id: %w", err)
	}
	return append([]byte(nil), h.keyID...), nil
}

// Zero overwrites b with zeroes. Use on temporary copies of key material.
func Zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
