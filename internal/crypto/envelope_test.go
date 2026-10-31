// SPDX-License-Identifier: AGPL-3.0-only

package crypto

import (
	"bytes"
	"crypto/rand"
	"errors"
	"testing"
)

func mustKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, KeySize)
	if _, err := rand.Read(k); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return k
}

func TestSealOpenRoundTrip(t *testing.T) {
	key := mustKey(t)
	cases := []struct {
		name      string
		keyID     []byte
		plaintext []byte
		aad       []byte
	}{
		{"empty plaintext", []byte("dek-v1"), nil, nil},
		{"small", []byte("dek-v1"), []byte("hello mailroom"), nil},
		{"with aad", []byte("dek-v2"), []byte("payload"), []byte("blob:0123")},
		{"empty keyid", nil, []byte("no key id"), []byte("aad")},
		{"binary keyid", []byte{0x00, 0xff, 0x10}, []byte("body"), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env, err := Seal(key, tc.keyID, tc.plaintext, tc.aad)
			if err != nil {
				t.Fatalf("Seal: %v", err)
			}
			got, err := Open(key, env, tc.aad)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			if !bytes.Equal(got, tc.plaintext) {
				t.Fatalf("round-trip mismatch: got %q want %q", got, tc.plaintext)
			}
			id, err := KeyID(env)
			if err != nil {
				t.Fatalf("KeyID: %v", err)
			}
			if !bytes.Equal(id, tc.keyID) {
				t.Fatalf("KeyID mismatch: got %x want %x", id, tc.keyID)
			}
		})
	}
}

func TestOpenRejectsTampering(t *testing.T) {
	key := mustKey(t)
	aad := []byte("aad")
	env, err := Seal(key, []byte("dek-v1"), []byte("secret payload"), aad)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	t.Run("flip ciphertext byte", func(t *testing.T) {
		bad := append([]byte(nil), env...)
		bad[len(bad)-1] ^= 0x01
		if _, err := Open(key, bad, aad); err == nil {
			t.Fatal("expected error on tampered ciphertext")
		}
	})
	t.Run("flip header byte (key-id)", func(t *testing.T) {
		bad := append([]byte(nil), env...)
		bad[4] ^= 0x01 // first key-id byte, bound into AAD
		if _, err := Open(key, bad, aad); err == nil {
			t.Fatal("expected error on tampered header")
		}
	})
	t.Run("flip nonce byte", func(t *testing.T) {
		bad := append([]byte(nil), env...)
		// nonce starts right after the 4+len("dek-v1")=10 byte header
		bad[10] ^= 0x01
		if _, err := Open(key, bad, aad); err == nil {
			t.Fatal("expected error on tampered nonce")
		}
	})
	t.Run("wrong key", func(t *testing.T) {
		if _, err := Open(mustKey(t), env, aad); err == nil {
			t.Fatal("expected error with wrong key")
		}
	})
	t.Run("wrong aad", func(t *testing.T) {
		if _, err := Open(key, env, []byte("other")); err == nil {
			t.Fatal("expected error with wrong aad")
		}
	})
}

func TestSealKeyValidation(t *testing.T) {
	if _, err := Seal(make([]byte, 16), nil, []byte("x"), nil); err == nil {
		t.Fatal("expected error for short key")
	}
	if _, err := Open(make([]byte, 16), []byte("xxxx"), nil); err == nil {
		t.Fatal("expected error for short key")
	}
}

func TestOpenShortEnvelope(t *testing.T) {
	if _, err := Open(mustKey(t), []byte{0x01}, nil); err == nil {
		t.Fatal("expected error for short envelope")
	}
	if _, err := Open(mustKey(t), nil, nil); !errors.Is(err, ErrShortEnvelope) {
		t.Fatalf("expected ErrShortEnvelope, got %v", err)
	}
}

func TestOpenWith(t *testing.T) {
	key := mustKey(t)
	env, err := Seal(key, []byte("dek-7"), []byte("via keyFor"), nil)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	got, err := OpenWith(func(id []byte) ([]byte, error) {
		if !bytes.Equal(id, []byte("dek-7")) {
			return nil, errors.New("unknown key id")
		}
		return key, nil
	}, env, nil)
	if err != nil {
		t.Fatalf("OpenWith: %v", err)
	}
	if !bytes.Equal(got, []byte("via keyFor")) {
		t.Fatalf("OpenWith mismatch: %q", got)
	}
}
