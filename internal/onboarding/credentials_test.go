// SPDX-License-Identifier: AGPL-3.0-only

package onboarding

import (
	"bytes"
	"testing"

	"github.com/oklog/ulid/v2"
)

// testMaster returns a deterministic 32-byte master key for unit tests.
func testMaster() []byte {
	m := make([]byte, 32)
	for i := range m {
		m[i] = byte(i + 1)
	}
	return m
}

func TestSealOpenRoundTrip(t *testing.T) {
	master := testMaster()
	userID := ulid.Make()
	creds := Credentials{
		IMAPUsername: "you@example.com",
		IMAPPassword: "hunter2",
		SMTPUsername: "you@example.com",
		SMTPPassword: "hunter2",
	}

	ct, err := SealCredentials(master, userID, creds)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if bytes.Contains(ct, []byte("hunter2")) {
		t.Fatal("ciphertext leaks the plaintext password")
	}

	got, err := OpenCredentials(master, userID, ct)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if got != creds {
		t.Errorf("round-trip mismatch: got %+v, want %+v", got, creds)
	}
}

func TestOpenWrongUserFails(t *testing.T) {
	master := testMaster()
	ct, err := SealCredentials(master, ulid.Make(), Credentials{IMAPPassword: "x"})
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	// A different user id changes both the derived key (salt) and the AAD, so
	// authentication must fail.
	if _, err := OpenCredentials(master, ulid.Make(), ct); err == nil {
		t.Fatal("expected authentication failure opening with a different user id")
	}
}

func TestDeriveCredentialsKeyDeterministicPerUser(t *testing.T) {
	master := testMaster()
	u := ulid.Make()

	k1, err := DeriveCredentialsKey(master, u)
	if err != nil {
		t.Fatal(err)
	}
	k2, err := DeriveCredentialsKey(master, u)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(k1, k2) {
		t.Error("derivation must be deterministic for the same (master, user)")
	}
	if len(k1) != 32 {
		t.Errorf("derived key length = %d, want 32", len(k1))
	}

	k3, err := DeriveCredentialsKey(master, ulid.Make())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(k1, k3) {
		t.Error("different users must derive different keys")
	}
}

func TestCredentialsSchemeSentinelIsValidULID(t *testing.T) {
	// The sentinel must render to a parseable ULID string (app.ulid domain).
	if _, err := ulid.Parse(CredentialsSchemeV1.String()); err != nil {
		t.Errorf("credentials scheme sentinel is not a valid ULID string: %v", err)
	}
}
