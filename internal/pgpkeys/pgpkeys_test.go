// SPDX-License-Identifier: AGPL-3.0-only

package pgpkeys

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
)

// newEntity makes a real OpenPGP key. Real key material, not a hand-rolled
// fixture: the whole job of this package is deciding what real keys are, and a
// fake would prove nothing about the library's behaviour.
func newEntity(t *testing.T, name, email string) *openpgp.Entity {
	t.Helper()
	e, err := openpgp.NewEntity(name, "", email, nil)
	if err != nil {
		t.Fatalf("NewEntity: %v", err)
	}
	return e
}

func publicBytes(t *testing.T, e *openpgp.Entity) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := e.Serialize(&buf); err != nil {
		t.Fatalf("Serialize public: %v", err)
	}
	return buf.Bytes()
}

func TestNamesAddress(t *testing.T) {
	now := time.Now()
	e := newEntity(t, "Alice Example", "Alice@Example.com")
	pub := publicBytes(t, e)
	if !NamesAddress(pub, "alice@example.com", now) {
		t.Error("a key with a UID for the address (mixed case) does not name it")
	}
	if NamesAddress(pub, "mallory@example.com", now) {
		t.Error("a key names an address it has no UID for")
	}
	if NamesAddress(pub, "", now) || NamesAddress([]byte("junk"), "alice@example.com", now) {
		t.Error("an empty address or unreadable key names an address")
	}
	// Signing-only is enough: unlike EncryptionFingerprintFor this needs no
	// encryption subkey.
	e.Subkeys = nil
	if !NamesAddress(publicBytes(t, e), "alice@example.com", now) {
		t.Error("a key with no encryption subkey was refused for naming an address")
	}
}

func TestParseReadsFingerprintAndAddress(t *testing.T) {
	e := newEntity(t, "Alice Example", "alice@example.com")
	k, err := Parse(publicBytes(t, e))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if len(k.Fingerprint) != 40 {
		t.Errorf("fingerprint %q is %d chars, want 40 hex", k.Fingerprint, len(k.Fingerprint))
	}
	if k.Fingerprint != strings.ToUpper(k.Fingerprint) {
		t.Errorf("fingerprint %q is not uppercase — it would not match app.user_keys", k.Fingerprint)
	}
	if !k.CoversAddress("ALICE@EXAMPLE.COM") {
		t.Errorf("CoversAddress is case-sensitive; addresses = %v", k.Addresses)
	}
	if k.CoversAddress("mallory@example.com") {
		t.Error("CoversAddress matched an address the key does not carry")
	}
	if len(k.UserIDs) == 0 {
		t.Error("no UserIDs extracted")
	}
}

// TestParseRefusesPrivateKeyMaterial is the security assertion of this package.
//
// ⚠ `gpg --export-secret-keys` output is a VALID transferable key, so a
// correspondent can put one in an Autocrypt header or serve it over WKD by
// accident. Without this check it would be stored in a column called
// public_key, and nothing would ever look for it there again.
func TestParseRefusesPrivateKeyMaterial(t *testing.T) {
	e := newEntity(t, "Alice Example", "alice@example.com")
	var buf bytes.Buffer
	if err := e.SerializePrivateWithoutSigning(&buf, nil); err != nil {
		t.Fatalf("SerializePrivate: %v", err)
	}

	// The control: the SAME key, public half only, must be accepted. Without it
	// this test would pass against a Parse that refused everything.
	if _, err := Parse(publicBytes(t, e)); err != nil {
		t.Fatalf("control failed — the public half of the same key must parse: %v", err)
	}

	if _, err := Parse(buf.Bytes()); !errors.Is(err, ErrHasPrivateKey) {
		t.Fatalf("Parse(secret key) error = %v, want ErrHasPrivateKey", err)
	}
}

// TestParseRefusesPrivateKeyHidingBehindAPublicOne covers the reason the check
// walks every entity rather than only the first: a ring can lead with an
// innocuous public key.
func TestParseRefusesPrivateKeyHidingBehindAPublicOne(t *testing.T) {
	pub := newEntity(t, "Alice", "alice@example.com")
	sec := newEntity(t, "Mallory", "mallory@example.com")

	var buf bytes.Buffer
	buf.Write(publicBytes(t, pub))
	if err := sec.SerializePrivateWithoutSigning(&buf, nil); err != nil {
		t.Fatalf("SerializePrivate: %v", err)
	}

	if _, err := Parse(buf.Bytes()); !errors.Is(err, ErrHasPrivateKey) {
		t.Fatalf("error = %v, want ErrHasPrivateKey — the check must see past the first entity", err)
	}
}

func TestParseRejectsJunkAndArmor(t *testing.T) {
	if _, err := Parse(nil); !errors.Is(err, ErrMalformed) {
		t.Errorf("Parse(nil) = %v, want ErrMalformed", err)
	}
	if _, err := Parse([]byte("not a key at all")); !errors.Is(err, ErrMalformed) {
		t.Errorf("Parse(junk) = %v, want ErrMalformed", err)
	}

	// ⚠ ARMOR IS REJECTED ON PURPOSE. Every producer here deals in binary, and
	// accepting armor would let armored bytes reach a column documented as
	// binary — the defect keypub already guards against on the serving side.
	e := newEntity(t, "Alice", "alice@example.com")
	var armored bytes.Buffer
	armored.WriteString("-----BEGIN PGP PUBLIC KEY BLOCK-----\n\n")
	armored.WriteString("mDMEY\n-----END PGP PUBLIC KEY BLOCK-----\n")
	if _, err := Parse(armored.Bytes()); !errors.Is(err, ErrMalformed) {
		t.Errorf("Parse(armor) = %v, want ErrMalformed", err)
	}
	_ = e
}

func TestParseRejectsOversize(t *testing.T) {
	if _, err := Parse(make([]byte, MaxKeyBytes+1)); !errors.Is(err, ErrTooLarge) {
		t.Errorf("error = %v, want ErrTooLarge", err)
	}
}
