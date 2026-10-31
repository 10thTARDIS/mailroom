// SPDX-License-Identifier: AGPL-3.0-only

package pgpkeyd

// Fixtures for the decrypt tests, built IN-TEST with go-crypto (#47 6b).
//
// Most cases use openpgp.Encrypt, the same high-level path a correspondent's
// client library would. Three cannot, because the library will not produce
// them, and they are assembled here from go-crypto's own packet serializers:
//
//   - a message whose SESSION KEY the test knows, for the leak assertion
//     ("the session key never appears in a response or a log");
//   - a legacy SED (tag 9, no MDC) message — EFAIL's precondition. go-crypto
//     only writes SEIPD; the SED body is the library's own OCFB stream with
//     resync, exactly as RFC 4880 §5.7 describes it, and TestDecryptRefusesSED
//     proves the fixture is GENUINE by opening it with go-crypto's insecure
//     flag before asserting the daemon refuses it;
//   - deeply nested compression.
//
// None of this is cryptography of our own: every cipher operation is the
// library's. It is byte framing (a packet header) around library output.

import (
	"bytes"
	"crypto/aes"
	"crypto/rand"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

// pubRing parses an armored public key.
func pubRing(t *testing.T, armored string) openpgp.EntityList {
	t.Helper()
	ring, err := openpgp.ReadArmoredKeyRing(strings.NewReader(armored))
	if err != nil || len(ring) != 1 {
		t.Fatalf("read public key: %v (%d entities)", err, len(ring))
	}
	return ring
}

// encryptTo encrypts plain to the given keys through the library's normal
// path and returns an ASCII-armored PGP MESSAGE, as RFC 3156 transmits it.
func encryptTo(t *testing.T, to openpgp.EntityList, plain []byte, cfg *packet.Config) []byte {
	t.Helper()
	var buf bytes.Buffer
	aw, err := armor.Encode(&buf, "PGP MESSAGE", nil)
	if err != nil {
		t.Fatal(err)
	}
	w, err := openpgp.Encrypt(aw, to, nil, &openpgp.FileHints{IsBinary: true}, cfg)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if _, err := w.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := aw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

// craftedMessage builds a public-key-encrypted message to recipient with a
// session key the CALLER supplies, optionally as legacy SED, optionally with
// `layers` nested compressed packets around the literal. Returns binary packets.
func craftedMessage(t *testing.T, recipient *openpgp.Entity, sessionKey, plain []byte, sed bool, layers int) []byte {
	t.Helper()
	encKey, ok := recipient.EncryptionKey(time.Now())
	if !ok {
		t.Fatal("recipient has no usable encryption key")
	}
	cipherFunc := packet.CipherAES128
	if len(sessionKey) != cipherFunc.KeySize() {
		t.Fatalf("session key is %d bytes, want %d", len(sessionKey), cipherFunc.KeySize())
	}

	var out bytes.Buffer
	if err := packet.SerializeEncryptedKey(&out, encKey.PublicKey, cipherFunc, sessionKey, nil); err != nil {
		t.Fatalf("PKESK: %v", err)
	}

	// The inner packet stream: [compressed]^layers ( literal ).
	var inner bytes.Buffer
	// ⚠ ONE Close, on the innermost writer. go-crypto's packet writers close
	// what they wrap (a partial-length writer closes its underlying writer, and
	// a compressed writer closes its stream header), so closing the literal
	// cascades through every layer to w. Closing a layer again would write a
	// second trailer — a second MDC, for the SEIPD writer.
	writeInner := func(w io.WriteCloser) {
		cur := w
		for i := 0; i < layers; i++ {
			c, err := packet.SerializeCompressed(cur, packet.CompressionZLIB, &packet.CompressionConfig{Level: 9})
			if err != nil {
				t.Fatalf("compressed layer %d: %v", i, err)
			}
			cur = c
		}
		lit, err := packet.SerializeLiteral(cur, true, "", 0)
		if err != nil {
			t.Fatalf("literal: %v", err)
		}
		if _, err := lit.Write(plain); err != nil {
			t.Fatal(err)
		}
		if err := lit.Close(); err != nil {
			t.Fatal(err)
		}
	}

	if !sed {
		w, err := packet.SerializeSymmetricallyEncrypted(&out, cipherFunc, false, packet.CipherSuite{}, sessionKey, nil)
		if err != nil {
			t.Fatalf("SEIPD: %v", err)
		}
		writeInner(w) // closes w, through the cascade
		return out.Bytes()
	}

	// Legacy SED, tag 9: the library's OCFB stream WITH resync over the inner
	// packets, no MDC (RFC 4880 §5.7).
	writeInner(nopWriteCloser{&inner})
	block, err := aes.NewCipher(sessionKey)
	if err != nil {
		t.Fatal(err)
	}
	randData := make([]byte, block.BlockSize())
	if _, err := rand.Read(randData); err != nil {
		t.Fatal(err)
	}
	stream, prefix := packet.NewOCFBEncrypter(block, randData, packet.OCFBResync)
	body := make([]byte, inner.Len())
	stream.XORKeyStream(body, inner.Bytes())
	sedBody := append(prefix, body...)
	out.Write(newFormatHeader(9, len(sedBody)))
	out.Write(sedBody)
	return out.Bytes()
}

// newFormatHeader is an RFC 9580 §4.2.1 new-format packet header.
func newFormatHeader(tag byte, n int) []byte {
	h := []byte{0xC0 | tag}
	switch {
	case n < 192:
		h = append(h, byte(n))
	case n < 8384:
		n -= 192
		h = append(h, byte(n>>8)+192, byte(n))
	default:
		h = append(h, 0xFF, byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
	}
	return h
}

// armorMessage wraps binary packets as a PGP MESSAGE.
func armorMessage(t *testing.T, bin []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	aw, err := armor.Encode(&buf, "PGP MESSAGE", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := aw.Write(bin); err != nil {
		t.Fatal(err)
	}
	if err := aw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// dearmorMessage is the inverse.
func dearmorMessage(t *testing.T, armored []byte) []byte {
	t.Helper()
	blk, err := armor.Decode(bytes.NewReader(armored))
	if err != nil {
		t.Fatalf("dearmor: %v", err)
	}
	b, err := io.ReadAll(blk.Body)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// tamperTail flips one bit `fromEnd` bytes before the end of the binary
// message. For a SEIPD packet the last 22 bytes of plaintext are the MDC packet
// (RFC 4880 §5.14), so a flip there corrupts the integrity check itself — the
// failure the MDC exists to detect — without disturbing the session key.
func tamperTail(t *testing.T, armored []byte, fromEnd int) []byte {
	t.Helper()
	bin := dearmorMessage(t, armored)
	if fromEnd <= 0 || fromEnd > len(bin) {
		t.Fatalf("cannot tamper %d bytes from the end of %d", fromEnd, len(bin))
	}
	bin[len(bin)-fromEnd] ^= 0x01
	return armorMessage(t, bin)
}

// newLocalEntity makes a key the test holds outright, in the daemon's own key
// shape (Ed25519 primary, Curve25519 encryption subkey, v4).
func newLocalEntity(t *testing.T, email string) *openpgp.Entity {
	t.Helper()
	e, err := openpgp.NewEntity("Local", "", email, &packet.Config{
		Algorithm: packet.PubKeyAlgoEdDSA, Curve: packet.Curve25519, V6Keys: false,
	})
	if err != nil {
		t.Fatalf("NewEntity: %v", err)
	}
	return e
}

// armorPrivateEntity serialises an entity's private key, armored, for Import.
func armorPrivateEntity(t *testing.T, e *openpgp.Entity) string {
	t.Helper()
	var buf bytes.Buffer
	aw, err := armor.Encode(&buf, openpgp.PrivateKeyType, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.SerializePrivateWithoutSigning(aw, nil); err != nil {
		t.Fatal(err)
	}
	if err := aw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

// sessionKey returns a fresh random AES-128 key.
func sessionKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, 16)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return k
}
