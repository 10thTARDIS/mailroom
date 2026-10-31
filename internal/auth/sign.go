// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// statePurpose is the HMAC domain-separation string for the login-state
// cookie, the only signed cookie this package issues since sessions became
// server-side records (ADR-048). The purpose stays in the MAC so a value
// signed for some other use could never verify as login state.
const statePurpose = "mailroom.auth.state.v1"

// errMalformedCookie indicates a signed cookie could not be parsed (wrong
// shape, bad base64, invalid JSON) — never seen from a cookie this package
// issued, so any occurrence is either an old format, truncation, or a
// probe.
var errMalformedCookie = errors.New("auth: malformed signed cookie")

// errBadSignature indicates a signed cookie parsed correctly but its MAC
// did not verify — a tampered or forged cookie, or one signed under a
// different key/purpose.
var errBadSignature = errors.New("auth: signature verification failed")

// encodeSigned serializes v to JSON and returns "payload.signature", both
// base64url (no padding), where signature is HMAC-SHA256 over
// purpose||0x00||payload keyed by key.
func encodeSigned(key []byte, purpose string, v any) (string, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("auth: marshal cookie payload: %w", err)
	}
	payload := base64.RawURLEncoding.EncodeToString(data)
	sig := macFor(key, purpose, payload)
	return payload + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// decodeSigned verifies s against key/purpose and, on success, unmarshals
// the payload into v. Failures are errMalformedCookie or errBadSignature
// (checkable with errors.Is), distinguishing garbage input from a
// tampered/forged-but-well-formed cookie.
func decodeSigned(key []byte, purpose, s string, v any) error {
	payload, sigEnc, ok := strings.Cut(s, ".")
	if !ok || payload == "" || sigEnc == "" {
		return errMalformedCookie
	}
	sig, err := base64.RawURLEncoding.DecodeString(sigEnc)
	if err != nil {
		return errMalformedCookie
	}
	want := macFor(key, purpose, payload)
	if !hmac.Equal(sig, want) {
		return errBadSignature
	}
	data, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return errMalformedCookie
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("%w: %v", errMalformedCookie, err)
	}
	return nil
}

// macFor computes the HMAC-SHA256 tag over a purpose-separated payload.
func macFor(key []byte, purpose, payload string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(purpose))
	mac.Write([]byte{0})
	mac.Write([]byte(payload))
	return mac.Sum(nil)
}
