// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"
)

// The session tokens are sealed under a key derived from the master key per
// user, bound to the user and session ids. Open must reverse Seal, must refuse a
// ciphertext moved to another session or user, and the ciphertext must not
// contain the token.
func TestSessionTokenSealing(t *testing.T) {
	master := bytes.Repeat([]byte{0x42}, 32)
	user, sess := ulid.Make(), ulid.Make()
	const token = "refresh-token-value-123"

	ct, err := sealSessionToken(master, user, sess, sessionPurposeRefresh, token)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if bytes.Contains(ct, []byte(token)) {
		t.Fatal("the sealed form contains the plaintext token")
	}
	got, err := openSessionToken(master, user, sess, sessionPurposeRefresh, ct)
	if err != nil || got != token {
		t.Fatalf("open = %q, %v; want the token back", got, err)
	}

	if _, err := openSessionToken(master, user, ulid.Make(), sessionPurposeRefresh, ct); err == nil {
		t.Error("a ciphertext opened under a different session id")
	}
	if _, err := openSessionToken(master, ulid.Make(), sess, sessionPurposeRefresh, ct); err == nil {
		t.Error("a ciphertext opened under a different user id")
	}
	// A refresh ciphertext moved into the ID-token column must not open as one:
	// logout would hand the refresh token to the browser as id_token_hint.
	if _, err := openSessionToken(master, user, sess, sessionPurposeIDToken, ct); err == nil {
		t.Error("a refresh-token ciphertext opened as an ID token")
	}
	other := bytes.Repeat([]byte{0x43}, 32)
	if _, err := openSessionToken(other, user, sess, sessionPurposeRefresh, ct); err == nil {
		t.Error("a ciphertext opened under a different master key")
	}

	// Errors never carry the token.
	if _, err := openSessionToken(other, user, sess, sessionPurposeRefresh, ct); err != nil && strings.Contains(err.Error(), token) {
		t.Error("the open error contains the token")
	}
}
