// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
)

// randomTokenBytes is the entropy used for the OAuth2 "state" and OIDC
// "nonce" values: 32 random bytes -> 43 base64url characters, comfortably
// unguessable and consistent with the PKCE verifier length produced by
// golang.org/x/oauth2's GenerateVerifier (which this package uses directly
// for PKCE — see login.go/callback.go).
const randomTokenBytes = 32

// randomURLSafeString returns a base64url-encoded (no padding), CSPRNG
// random string derived from n random bytes.
func randomURLSafeString(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("auth: read random bytes: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
