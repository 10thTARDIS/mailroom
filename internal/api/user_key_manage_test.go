// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"strings"
	"testing"
)

// The second line of the passphrase rule: whatever the crypto process says, a
// refusal that would quote a supplied secret or private key armor is replaced.
func TestScrubCustodyMessage(t *testing.T) {
	const secret = "correct-horse-battery"
	cases := map[string]bool{ // message -> must be replaced
		"days must be between 1 and 3650":                   false,
		"that passphrase does not open the key":             false,
		"bad passphrase " + secret:                          true,
		"-----BEGIN PGP PRIVATE KEY BLOCK----- ...":         true,
		"input looked like a private key, not a public one": true,
	}
	for msg, replace := range cases {
		got := scrubCustodyMessage(msg, secret, "")
		if replaced := got != msg; replaced != replace {
			t.Errorf("scrub(%q) = %q, replaced=%v want %v", msg, got, replaced, replace)
		}
		if strings.Contains(got, secret) {
			t.Errorf("scrub(%q) kept the secret", msg)
		}
	}
}

func TestPrivateKeyArmor(t *testing.T) {
	if _, mErr := privateKeyArmor("-----BEGIN PGP PUBLIC KEY BLOCK-----\n..."); mErr == nil || mErr.Type != errPublicKeyRefused {
		t.Errorf("public key: %+v", mErr)
	}
	if _, mErr := privateKeyArmor("not base64 !!"); mErr == nil || mErr.Type != errInvalidKey {
		t.Errorf("garbage: %+v", mErr)
	}
	out, mErr := privateKeyArmor("AAEC")
	if mErr != nil || !strings.HasPrefix(out, "-----BEGIN PGP PRIVATE KEY BLOCK-----") {
		t.Errorf("base64 binary was not armored: %q %+v", out, mErr)
	}
}
