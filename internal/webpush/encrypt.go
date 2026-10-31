// SPDX-License-Identifier: AGPL-3.0-only

package webpush

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
)

// RFC 8291 aes128gcm content encryption for Web Push, built on stdlib crypto
// (crypto/ecdh, crypto/hkdf, crypto/aes) so the project owns its crypto and the
// chain is checked against the RFC 8291 §5 published test vectors, not assumed
// (ADR-016). The wire format is RFC 8188's aes128gcm content coding:
//
//	body = salt(16) || rs(4, big-endian) || idlen(1) || keyid(idlen) || ciphertext
//
// where keyid is the application server's ephemeral public key and there is a
// single record (Web Push payloads are small).

const (
	authSecretLen = 16 // subscription auth secret, RFC 8291 §2
	saltLen       = 16 // per-message salt (RFC 8188 header)
	p256PointLen  = 65 // uncompressed P-256 point: 0x04 || X || Y
	cekLen        = 16 // AES-128 content-encryption key
	nonceLen      = 12 // AES-GCM nonce
	gcmTagLen     = 16 // AES-GCM authentication tag

	// recordSize is the RFC 8188 `rs` header field. RFC 8291 §5 uses 4096; a Web
	// Push notification is a single, small record well under this bound.
	recordSize = 4096

	// lastRecordPadDelimiter terminates the padding-delimited plaintext of the
	// final (here, only) record: 0x02 for the last record, 0x01 otherwise
	// (RFC 8188 §2).
	lastRecordPadDelimiter = 0x02
)

// The HKDF info strings (RFC 8291 §3.3, RFC 8188 §2.2). Each carries an embedded
// NUL, and the key-combining prefix is followed by the two raw public keys.
var (
	webPushKeyInfoPrefix = []byte("WebPush: info\x00")
	cekInfo              = "Content-Encoding: aes128gcm\x00"
	nonceInfo            = "Content-Encoding: nonce\x00"
)

// deriveKeys computes the content-encryption key and nonce for one push message
// from the shared ECDH secret, the subscription auth secret, the per-message
// salt, and both public keys, following RFC 8291 §3.3–3.4:
//
//	IKM   = HKDF(salt=auth_secret, IKM=ecdh_secret, info="WebPush: info"||0||ua||as, L=32)
//	CEK   = HKDF(salt=message_salt, IKM=IKM, info="Content-Encoding: aes128gcm"||0, L=16)
//	NONCE = HKDF(salt=message_salt, IKM=IKM, info="Content-Encoding: nonce"||0,     L=12)
//
// (crypto/hkdf.Key is HKDF-Expand(HKDF-Extract(secret, salt), info): its `secret`
// argument is the IKM and `salt` is the salt.)
func deriveKeys(ecdhSecret, authSecret, salt, uaPublic, asPublic []byte) (cek, nonce []byte, err error) {
	keyInfo := make([]byte, 0, len(webPushKeyInfoPrefix)+len(uaPublic)+len(asPublic))
	keyInfo = append(keyInfo, webPushKeyInfoPrefix...)
	keyInfo = append(keyInfo, uaPublic...)
	keyInfo = append(keyInfo, asPublic...)

	ikm, err := hkdf.Key(sha256.New, ecdhSecret, authSecret, string(keyInfo), sha256.Size)
	if err != nil {
		return nil, nil, fmt.Errorf("webpush: derive ikm: %w", err)
	}
	cek, err = hkdf.Key(sha256.New, ikm, salt, cekInfo, cekLen)
	if err != nil {
		return nil, nil, fmt.Errorf("webpush: derive cek: %w", err)
	}
	nonce, err = hkdf.Key(sha256.New, ikm, salt, nonceInfo, nonceLen)
	if err != nil {
		return nil, nil, fmt.Errorf("webpush: derive nonce: %w", err)
	}
	return cek, nonce, nil
}

// Encrypt produces the aes128gcm request body for one push message to a
// subscription whose user-agent public key is uaPublic (65-byte uncompressed
// P-256 point) and auth secret is authSecret (16 bytes). It generates a fresh
// ephemeral application-server keypair and a random salt per message, as RFC 8291
// requires.
func Encrypt(uaPublic, authSecret, plaintext []byte) ([]byte, error) {
	asPriv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("webpush: generate ephemeral key: %w", err)
	}
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("webpush: generate salt: %w", err)
	}
	return seal(asPriv, salt, uaPublic, authSecret, plaintext)
}

// seal is the deterministic core of Encrypt with the ephemeral key and salt
// supplied by the caller, so the RFC 8291 §5 vector test can pin them.
func seal(asPriv *ecdh.PrivateKey, salt, uaPublic, authSecret, plaintext []byte) ([]byte, error) {
	if len(authSecret) != authSecretLen {
		return nil, fmt.Errorf("webpush: auth secret must be %d bytes, got %d", authSecretLen, len(authSecret))
	}
	if len(salt) != saltLen {
		return nil, fmt.Errorf("webpush: salt must be %d bytes, got %d", saltLen, len(salt))
	}
	uaPub, err := ecdh.P256().NewPublicKey(uaPublic) // validates the 65-byte point
	if err != nil {
		return nil, fmt.Errorf("webpush: invalid subscription public key (p256dh): %w", err)
	}
	ecdhSecret, err := asPriv.ECDH(uaPub)
	if err != nil {
		return nil, fmt.Errorf("webpush: ecdh: %w", err)
	}
	asPublic := asPriv.PublicKey().Bytes() // 65-byte uncompressed point = keyid

	cek, nonce, err := deriveKeys(ecdhSecret, authSecret, salt, uaPublic, asPublic)
	if err != nil {
		return nil, err
	}
	aead, err := newGCM(cek)
	if err != nil {
		return nil, err
	}

	// Single record: plaintext followed by the last-record padding delimiter.
	record := make([]byte, 0, len(plaintext)+1)
	record = append(record, plaintext...)
	record = append(record, lastRecordPadDelimiter)
	ciphertext := aead.Seal(nil, nonce, record, nil) // no additional authenticated data

	header := make([]byte, 0, saltLen+4+1+len(asPublic))
	header = append(header, salt...)
	var rs [4]byte
	binary.BigEndian.PutUint32(rs[:], recordSize)
	header = append(header, rs[:]...)
	header = append(header, byte(len(asPublic))) // idlen
	header = append(header, asPublic...)         // keyid
	return append(header, ciphertext...), nil
}

// open decrypts an aes128gcm Web Push body given the subscription's PRIVATE key
// and auth secret. It exists so the RFC 8291 §5 test can round-trip the published
// ciphertext (the app server never decrypts in production), and so encrypt/decrypt
// are proven mutually consistent.
func open(uaPrivate, authSecret, body []byte) ([]byte, error) {
	if len(body) < saltLen+4+1 {
		return nil, errors.New("webpush: body too short for aes128gcm header")
	}
	salt := body[:saltLen]
	idlen := int(body[saltLen+4])
	keyStart := saltLen + 4 + 1
	if len(body) < keyStart+idlen {
		return nil, errors.New("webpush: body too short for keyid")
	}
	asPublic := body[keyStart : keyStart+idlen]
	ciphertext := body[keyStart+idlen:]

	uaPriv, err := ecdh.P256().NewPrivateKey(uaPrivate)
	if err != nil {
		return nil, fmt.Errorf("webpush: invalid subscription private key: %w", err)
	}
	asPub, err := ecdh.P256().NewPublicKey(asPublic)
	if err != nil {
		return nil, fmt.Errorf("webpush: invalid keyid public key: %w", err)
	}
	ecdhSecret, err := uaPriv.ECDH(asPub)
	if err != nil {
		return nil, fmt.Errorf("webpush: ecdh: %w", err)
	}
	uaPublic := uaPriv.PublicKey().Bytes()

	cek, nonce, err := deriveKeys(ecdhSecret, authSecret, salt, uaPublic, asPublic)
	if err != nil {
		return nil, err
	}
	aead, err := newGCM(cek)
	if err != nil {
		return nil, err
	}
	record, err := aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("webpush: gcm open: %w", err)
	}
	return stripPadding(record)
}

// newGCM builds an AES-128-GCM AEAD over a 16-byte content-encryption key.
func newGCM(cek []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, fmt.Errorf("webpush: aes cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("webpush: gcm: %w", err)
	}
	return aead, nil
}

// stripPadding removes RFC 8188 record padding: trailing 0x00 octets and the
// single delimiter (0x01/0x02) that precedes them.
func stripPadding(record []byte) ([]byte, error) {
	i := len(record) - 1
	for i >= 0 && record[i] == 0x00 {
		i--
	}
	if i < 0 {
		return nil, errors.New("webpush: record has no padding delimiter")
	}
	// record[i] is the delimiter (0x01 or 0x02); the plaintext precedes it.
	return record[:i], nil
}
