// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"testing"

	"golang.org/x/oauth2"
)

func TestRandomURLSafeString_LengthAndUniqueness(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 1000; i++ {
		s, err := randomURLSafeString(randomTokenBytes)
		if err != nil {
			t.Fatalf("randomURLSafeString: %v", err)
		}
		if len(s) == 0 {
			t.Fatal("randomURLSafeString returned empty string")
		}
		if seen[s] {
			t.Fatalf("randomURLSafeString produced a duplicate: %q", s)
		}
		seen[s] = true
	}
}

// TestPKCE_S256ChallengeDerivation pins golang.org/x/oauth2's S256 code
// challenge derivation (which LoginHandler/CallbackHandler rely on for
// PKCE) against the worked example from RFC 7636 Appendix B.
func TestPKCE_S256ChallengeDerivation(t *testing.T) {
	const verifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	const wantChallenge = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"

	got := oauth2.S256ChallengeFromVerifier(verifier)
	if got != wantChallenge {
		t.Fatalf("S256ChallengeFromVerifier(%q) = %q, want %q", verifier, got, wantChallenge)
	}
}

func TestPKCE_GenerateVerifier_Uniqueness(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 200; i++ {
		v := oauth2.GenerateVerifier()
		if len(v) < 43 || len(v) > 128 {
			t.Fatalf("GenerateVerifier produced %d chars, want 43-128 (RFC 7636)", len(v))
		}
		if seen[v] {
			t.Fatalf("GenerateVerifier produced a duplicate: %q", v)
		}
		seen[v] = true
	}
}
