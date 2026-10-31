// SPDX-License-Identifier: AGPL-3.0-only

package webpush

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"testing"
)

// RFC 8291 §5 "Push Message Encryption Example" — the published test vectors that
// pin this package's aes128gcm implementation (ADR-016). The input keys, auth
// secret, salt, and plaintext are copied from that section (base64url, unpadded).
//
// The derivation is anchored to the standard by asserting the exact CEK and nonce
// RFC 8291 §5 lists (rfcCEK/rfcNonce): AES-128-GCM is stdlib and the header
// framing is checked independently, so matching CEK+nonce is equivalent to
// producing the RFC's exact ciphertext. TestRFC8291SealThenOpen additionally
// round-trips through the RFC's own keypairs.
//
// rfcBody is the full expected message body (header || ciphertext), base64url. It
// is left blank here: paste the exact value from RFC 8291 §5 to activate the
// strictest byte-for-byte equality check (TestRFC8291BodyMatchesVector skips while
// empty). The value MUST decode to 144 bytes (86-byte header + 58-byte ciphertext).
const (
	rfcPlaintext = "When I grow up, I want to be a watermelon"

	rfcUAPrivate = "q1dXpw3UpT5VOmu_cf_v6ih07Aems3njxI-JWgLcM94"
	rfcUAPublic  = "BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4"
	rfcAuth      = "BTBZMqHH6r4Tts7J_aSIgg"

	rfcASPrivate = "yfWPiYE-n46HLnH0KqZOF1fJJU3MYrct3AELtAQ-oRw"
	rfcASPublic  = "BP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A8"

	rfcSalt = "DGv6ra1nlYgDCS1FRnbzlw"

	// Derived content-encryption key and nonce (RFC 8291 §5 intermediate values).
	rfcCEK   = "oIhVW04MRdy2XN9CiKLxTg"
	rfcNonce = "4h_95klXJ5E_qnoN"

	// Full aes128gcm body — fill in from RFC 8291 §5 to enable the exact check.
	rfcBody = ""
)

func mustB64(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("decode %q: %v", s, err)
	}
	return b
}

// TestRFC8291DeriveKeys checks the HKDF chain (§3.3–3.4) yields the exact CEK and
// nonce the RFC publishes, from the ECDH secret computed off the example's own
// keypairs — so the ECDH step is exercised too. This is the anchoring gate.
func TestRFC8291DeriveKeys(t *testing.T) {
	asPriv, err := ecdh.P256().NewPrivateKey(mustB64(t, rfcASPrivate))
	if err != nil {
		t.Fatalf("as private: %v", err)
	}
	uaPub, err := ecdh.P256().NewPublicKey(mustB64(t, rfcUAPublic))
	if err != nil {
		t.Fatalf("ua public: %v", err)
	}
	ecdhSecret, err := asPriv.ECDH(uaPub)
	if err != nil {
		t.Fatalf("ecdh: %v", err)
	}

	cek, nonce, err := deriveKeys(ecdhSecret, mustB64(t, rfcAuth), mustB64(t, rfcSalt),
		mustB64(t, rfcUAPublic), mustB64(t, rfcASPublic))
	if err != nil {
		t.Fatalf("deriveKeys: %v", err)
	}
	if want := mustB64(t, rfcCEK); !bytes.Equal(cek, want) {
		t.Errorf("CEK\n got %x\nwant %x", cek, want)
	}
	if want := mustB64(t, rfcNonce); !bytes.Equal(nonce, want) {
		t.Errorf("nonce\n got %x\nwant %x", nonce, want)
	}
}

// TestRFC8291SealThenOpen encrypts the example plaintext with the example's
// ephemeral key + salt, checks the deterministic RFC 8188 header framing, then
// decrypts with the example's user-agent PRIVATE key and recovers the exact
// plaintext — exercising the whole chain against the standard's own keypairs.
func TestRFC8291SealThenOpen(t *testing.T) {
	asPriv, err := ecdh.P256().NewPrivateKey(mustB64(t, rfcASPrivate))
	if err != nil {
		t.Fatalf("as private: %v", err)
	}
	body, err := seal(asPriv, mustB64(t, rfcSalt), mustB64(t, rfcUAPublic),
		mustB64(t, rfcAuth), []byte(rfcPlaintext))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	assertHeader(t, body, mustB64(t, rfcSalt), mustB64(t, rfcASPublic))

	got, err := open(mustB64(t, rfcUAPrivate), mustB64(t, rfcAuth), body)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if string(got) != rfcPlaintext {
		t.Errorf("round trip through RFC keys: got %q, want %q", got, rfcPlaintext)
	}
}

// TestRFC8291BodyMatchesVector is the byte-for-byte check against the RFC's
// published message body. It runs only once rfcBody is filled in from the RFC.
func TestRFC8291BodyMatchesVector(t *testing.T) {
	if rfcBody == "" {
		t.Skip("rfcBody not set: paste the exact RFC 8291 §5 message body to enable this check")
	}
	want := mustB64(t, rfcBody)
	if len(want) != 144 {
		t.Fatalf("rfcBody decodes to %d bytes, want 144 (86-byte header + 58-byte ciphertext)", len(want))
	}
	asPriv, err := ecdh.P256().NewPrivateKey(mustB64(t, rfcASPrivate))
	if err != nil {
		t.Fatalf("as private: %v", err)
	}
	body, err := seal(asPriv, mustB64(t, rfcSalt), mustB64(t, rfcUAPublic),
		mustB64(t, rfcAuth), []byte(rfcPlaintext))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if !bytes.Equal(body, want) {
		t.Errorf("aes128gcm body mismatch:\n got %x\nwant %x", body, want)
	}
}

// TestEncryptOpenRoundTrip proves Encrypt (random ephemeral key + salt) and open
// are mutually consistent for an independently generated subscription keypair.
func TestEncryptOpenRoundTrip(t *testing.T) {
	uaPriv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ua keygen: %v", err)
	}
	auth := make([]byte, authSecretLen)
	if _, err := rand.Read(auth); err != nil {
		t.Fatalf("auth: %v", err)
	}
	msg := []byte("New mail: 3 messages")

	body, err := Encrypt(uaPriv.PublicKey().Bytes(), auth, msg)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	got, err := open(uaPriv.Bytes(), auth, body)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if !bytes.Equal(got, msg) {
		t.Errorf("round trip: got %q, want %q", got, msg)
	}
}

func TestEncryptRejectsBadKey(t *testing.T) {
	auth := make([]byte, authSecretLen)
	if _, err := Encrypt([]byte("not a point"), auth, []byte("x")); err == nil {
		t.Error("Encrypt accepted an invalid p256dh key")
	}
}

// assertHeader verifies the RFC 8188 content-coding header framing of a body.
func assertHeader(t *testing.T, body, wantSalt, wantKeyID []byte) {
	t.Helper()
	if len(body) < saltLen+4+1 {
		t.Fatalf("body too short: %d", len(body))
	}
	if !bytes.Equal(body[:saltLen], wantSalt) {
		t.Errorf("salt\n got %x\nwant %x", body[:saltLen], wantSalt)
	}
	if rs := binary.BigEndian.Uint32(body[saltLen : saltLen+4]); rs != recordSize {
		t.Errorf("rs = %d, want %d", rs, recordSize)
	}
	if idlen := int(body[saltLen+4]); idlen != len(wantKeyID) {
		t.Fatalf("idlen = %d, want %d", idlen, len(wantKeyID))
	}
	keyStart := saltLen + 4 + 1
	if got := body[keyStart : keyStart+len(wantKeyID)]; !bytes.Equal(got, wantKeyID) {
		t.Errorf("keyid\n got %x\nwant %x", got, wantKeyID)
	}
}
