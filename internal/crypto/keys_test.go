// SPDX-License-Identifier: AGPL-3.0-only

package crypto

import (
	"bytes"
	"testing"
)

func TestGenerateKeysDistinctAndSized(t *testing.T) {
	kek1, err := GenerateKEK()
	if err != nil {
		t.Fatalf("GenerateKEK: %v", err)
	}
	kek2, _ := GenerateKEK()
	dek, err := GenerateDEK()
	if err != nil {
		t.Fatalf("GenerateDEK: %v", err)
	}
	for _, k := range [][]byte{kek1, kek2, dek} {
		if len(k) != KeySize {
			t.Fatalf("key length = %d, want %d", len(k), KeySize)
		}
	}
	if bytes.Equal(kek1, kek2) {
		t.Fatal("two generated KEKs are identical")
	}
}

func TestKEKWrapRoundTrip(t *testing.T) {
	master := mustKey(t)
	kek, _ := GenerateKEK()

	wrapped, err := WrapKEK(master, kek)
	if err != nil {
		t.Fatalf("WrapKEK: %v", err)
	}
	got, err := UnwrapKEK(master, wrapped)
	if err != nil {
		t.Fatalf("UnwrapKEK: %v", err)
	}
	if !bytes.Equal(got, kek) {
		t.Fatal("KEK round-trip mismatch")
	}

	// Wrong master fails.
	if _, err := UnwrapKEK(mustKey(t), wrapped); err == nil {
		t.Fatal("expected error unwrapping KEK with wrong master")
	}
	// Tampered ciphertext fails.
	bad := append([]byte(nil), wrapped...)
	bad[len(bad)-1] ^= 0x01
	if _, err := UnwrapKEK(master, bad); err == nil {
		t.Fatal("expected error unwrapping tampered KEK")
	}
}

func TestDEKWrapRoundTrip(t *testing.T) {
	kek, _ := GenerateKEK()
	purposes := []string{"mail-blob", "credentials", "private-keys"}
	for _, p := range purposes {
		t.Run(p, func(t *testing.T) {
			dek, _ := GenerateDEK()
			wrapped, err := WrapDEK(kek, dek, p)
			if err != nil {
				t.Fatalf("WrapDEK: %v", err)
			}
			got, err := UnwrapDEK(kek, wrapped, p)
			if err != nil {
				t.Fatalf("UnwrapDEK: %v", err)
			}
			if !bytes.Equal(got, dek) {
				t.Fatal("DEK round-trip mismatch")
			}
		})
	}
}

func TestDEKWrongPurposeRejected(t *testing.T) {
	kek, _ := GenerateKEK()
	dek, _ := GenerateDEK()
	wrapped, err := WrapDEK(kek, dek, "mail-blob")
	if err != nil {
		t.Fatalf("WrapDEK: %v", err)
	}
	if _, err := UnwrapDEK(kek, wrapped, "credentials"); err == nil {
		t.Fatal("expected error: DEK unwrapped under wrong purpose")
	}
	// A KEK-wrapped DEK must not open as a master-wrapped KEK (domain sep).
	if _, err := UnwrapKEK(kek, wrapped); err == nil {
		t.Fatal("expected error: DEK opened as KEK")
	}
	// Empty purpose is rejected on both wrap and unwrap.
	if _, err := WrapDEK(kek, dek, ""); err == nil {
		t.Fatal("expected error wrapping with empty purpose")
	}
	if _, err := UnwrapDEK(kek, wrapped, ""); err == nil {
		t.Fatal("expected error unwrapping with empty purpose")
	}
}

// TestMasterRotation proves ADR-005's cheap rotation: after the master key
// rotates (KEK re-wrapped only), data sealed under a DEK still opens. The DEK
// and the sealed ciphertext are never touched.
func TestMasterRotation(t *testing.T) {
	oldMaster := mustKey(t)
	newMaster := mustKey(t)

	// Build the hierarchy and seal some data.
	kek, _ := GenerateKEK()
	dek, _ := GenerateDEK()
	wrappedKEK, err := WrapKEK(oldMaster, kek)
	if err != nil {
		t.Fatalf("WrapKEK: %v", err)
	}
	wrappedDEK, err := WrapDEK(kek, dek, "mail-blob")
	if err != nil {
		t.Fatalf("WrapDEK: %v", err)
	}
	plaintext := []byte("the quick brown fox")
	sealed, err := Seal(dek, []byte("blob-1"), plaintext, []byte("aad"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	// Rotate the master: re-wrap the KEK only.
	rewrappedKEK, err := RewrapKEK(oldMaster, newMaster, wrappedKEK)
	if err != nil {
		t.Fatalf("RewrapKEK: %v", err)
	}
	// Old master must no longer unwrap the re-wrapped KEK.
	if _, err := UnwrapKEK(oldMaster, rewrappedKEK); err == nil {
		t.Fatal("expected old master to fail on re-wrapped KEK")
	}

	// Recover the whole chain under the NEW master; wrappedDEK and sealed
	// are the exact same bytes as before rotation.
	kek2, err := UnwrapKEK(newMaster, rewrappedKEK)
	if err != nil {
		t.Fatalf("UnwrapKEK(new): %v", err)
	}
	if !bytes.Equal(kek2, kek) {
		t.Fatal("KEK changed across rotation")
	}
	dek2, err := UnwrapDEK(kek2, wrappedDEK, "mail-blob")
	if err != nil {
		t.Fatalf("UnwrapDEK after rotation: %v", err)
	}
	got, err := Open(dek2, sealed, []byte("aad"))
	if err != nil {
		t.Fatalf("Open after rotation: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatal("data unreadable after master rotation")
	}
}
