// SPDX-License-Identifier: AGPL-3.0-only

package pgpkeys

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

func armoredPublic(t *testing.T, e *openpgp.Entity) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := armor.Encode(&buf, openpgp.PublicKeyType, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Serialize(w); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestImportAcceptsArmoredAndBinary(t *testing.T) {
	now := time.Now().Add(time.Minute) // keys below are created after this line; their signatures must not post-date it
	e := newEntity(t, "Alice Example", "Alice@Example.com")
	for name, in := range map[string][]byte{"binary": publicBytes(t, e), "armored": armoredPublic(t, e)} {
		got, err := Import(in, "alice@example.com", now)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		want, _ := Parse(publicBytes(t, e))
		if got.Fingerprint != want.Fingerprint || !strings.Contains(got.UserID, "Alice") {
			t.Errorf("%s: got %+v", name, got)
		}
		if _, err := Parse(got.Binary); err != nil {
			t.Errorf("%s: stored form is not binary-parseable: %v", name, err)
		}
	}
}

func TestImportRefusals(t *testing.T) {
	now := time.Now().Add(time.Minute) // keys below are created after this line; their signatures must not post-date it
	const addr = "alice@example.com"

	secret := newEntity(t, "Alice", addr)
	var sb bytes.Buffer
	if err := secret.SerializePrivate(&sb, nil); err != nil {
		t.Fatal(err)
	}
	var sa bytes.Buffer
	w, _ := armor.Encode(&sa, openpgp.PrivateKeyType, nil)
	if err := secret.SerializePrivate(w, nil); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()

	signOnly := newEntity(t, "Alice", addr)
	signOnly.Subkeys = nil

	expired, err := openpgp.NewEntity("Alice", "", addr, &packet.Config{
		KeyLifetimeSecs: 3600, Time: func() time.Time { return now.Add(-72 * time.Hour) },
	})
	if err != nil {
		t.Fatal(err)
	}
	revoked := newEntity(t, "Alice", addr)
	if err := revoked.RevokeKey(packet.KeyCompromised, "x", nil); err != nil {
		t.Fatal(err)
	}
	two := append(publicBytes(t, newEntity(t, "Alice", addr)), publicBytes(t, newEntity(t, "Alice", addr))...)

	cases := []struct {
		name string
		in   []byte
		addr string
		want error
	}{
		{"binary secret key", sb.Bytes(), addr, ErrHasPrivateKey},
		{"armored secret key", sa.Bytes(), addr, ErrHasPrivateKey},
		{"other address", publicBytes(t, newEntity(t, "Eve", "eve@example.com")), addr, ErrNamesOtherAddress},
		{"no encryption subkey", publicBytes(t, signOnly), addr, ErrNoEncryptionSubkey},
		{"expired", publicBytes(t, expired), addr, ErrExpiredOrRevoked},
		{"revoked", publicBytes(t, revoked), addr, ErrExpiredOrRevoked},
		{"two primary keys", two, addr, ErrMultipleKeys},
		{"oversized", bytes.Repeat([]byte{0x99}, MaxKeyBytes*2+1), addr, ErrTooLarge},
		{"junk", []byte("not a key"), addr, ErrMalformed},
		{"empty", nil, addr, ErrMalformed},
	}
	for _, c := range cases {
		if _, err := Import(c.in, c.addr, now); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", c.name, err, c.want)
		}
	}
	// CONTROL: the same address with a good key is accepted, so the refusals above
	// are the rules and not a broken harness.
	if _, err := Import(publicBytes(t, newEntity(t, "Alice", addr)), addr, now); err != nil {
		t.Errorf("control: %v", err)
	}
}

func TestImportReportsTheUserIDThatNamesTheAddress(t *testing.T) {
	now := time.Now().Add(time.Minute)
	e := newEntity(t, "Alice Work", "alice@work.example.com")
	if err := e.AddUserId("Alice Home", "", "alice@example.com", nil); err != nil {
		t.Fatal(err)
	}
	got, err := Import(publicBytes(t, e), "ALICE@example.com", now)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.UserID, "alice@example.com") || strings.Contains(got.UserID, "work") {
		t.Errorf("UserID = %q, want the Home user ID that names the address", got.UserID)
	}
}

// A binary key whose last byte is whitespace must come through byte for byte: the
// armor sniffing trims, the binary path must not.
func TestImportKeepsBinaryBytesThatLookLikeWhitespace(t *testing.T) {
	now := time.Now().Add(time.Minute)
	cfg := &packet.Config{Algorithm: packet.PubKeyAlgoEdDSA, Curve: packet.Curve25519}
	for i := 0; i < 5000; i++ {
		e, err := openpgp.NewEntity("Alice", "", "alice@example.com", cfg)
		if err != nil {
			t.Fatal(err)
		}
		bin := publicBytes(t, e)
		if !bytes.ContainsRune([]byte(" \t\n\r\v\f"), rune(bin[len(bin)-1])) {
			continue
		}
		got, err := Import(bin, "alice@example.com", now)
		if err != nil {
			t.Fatalf("a binary key ending in %#x was refused: %v", bin[len(bin)-1], err)
		}
		if !bytes.Equal(got.Binary, bin) {
			t.Fatal("the stored bytes differ from the key that was given")
		}
		return
	}
	t.Fatal("no generated key ended in a whitespace byte; the test proved nothing")
}
