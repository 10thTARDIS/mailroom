// SPDX-License-Identifier: AGPL-3.0-only

package onboarding

import (
	"encoding/json"
	"testing"

	"github.com/oklog/ulid/v2"
)

// A password account's sealed plaintext is byte-for-byte what it was before
// Credentials grew the OAuth field (ADR-046): omitempty keeps it out.
func TestPasswordCredentialsJSONUnchanged(t *testing.T) {
	b, err := json.Marshal(Credentials{IMAPUsername: "u@example.com", IMAPPassword: "pw"})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"imap_username":"u@example.com","imap_password":"pw"}`; string(b) != want {
		t.Errorf("password credentials JSON = %s, want %s", b, want)
	}
}

func TestOAuthCredentialsRoundTrip(t *testing.T) {
	master := make([]byte, 32)
	userID := ulid.Make()
	in := Credentials{IMAPUsername: "you@example.com",
		OAuth: &OAuthSecret{Provider: "microsoft", RefreshToken: "M.R3_refresh"}}
	ct, err := SealCredentials(master, userID, in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := OpenCredentials(master, userID, ct)
	if err != nil {
		t.Fatal(err)
	}
	if out.OAuth == nil || *out.OAuth != *in.OAuth || out.IMAPUsername != in.IMAPUsername || out.IMAPPassword != "" {
		t.Errorf("round trip = %+v", out)
	}
}
