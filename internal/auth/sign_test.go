// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"errors"
	"testing"
)

type signTestPayload struct {
	Foo string `json:"foo"`
	Bar int    `json:"bar"`
}

func TestEncodeDecodeSigned_RoundTrip(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	want := signTestPayload{Foo: "hello", Bar: 42}

	s, err := encodeSigned(key, statePurpose, want)
	if err != nil {
		t.Fatalf("encodeSigned: %v", err)
	}

	var got signTestPayload
	if err := decodeSigned(key, statePurpose, s, &got); err != nil {
		t.Fatalf("decodeSigned: %v", err)
	}
	if got != want {
		t.Fatalf("round trip mismatch: got %+v, want %+v", got, want)
	}
}

func TestDecodeSigned_TamperedPayloadRejected(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	s, err := encodeSigned(key, statePurpose, signTestPayload{Foo: "hello", Bar: 42})
	if err != nil {
		t.Fatalf("encodeSigned: %v", err)
	}

	// Flip the last character of the payload segment (before the '.').
	tampered := flipLastChar(t, s, true)

	var got signTestPayload
	err = decodeSigned(key, statePurpose, tampered, &got)
	if err == nil {
		t.Fatal("decodeSigned accepted a tampered payload")
	}
	if !errors.Is(err, errBadSignature) && !errors.Is(err, errMalformedCookie) {
		t.Fatalf("unexpected error type: %v", err)
	}
}

func TestDecodeSigned_TamperedSignatureRejected(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	s, err := encodeSigned(key, statePurpose, signTestPayload{Foo: "hello", Bar: 42})
	if err != nil {
		t.Fatalf("encodeSigned: %v", err)
	}

	tampered := flipLastChar(t, s, false)

	var got signTestPayload
	err = decodeSigned(key, statePurpose, tampered, &got)
	if !errors.Is(err, errBadSignature) {
		t.Fatalf("decodeSigned err = %v, want errBadSignature", err)
	}
}

func TestDecodeSigned_WrongKeyRejected(t *testing.T) {
	key1 := []byte("0123456789abcdef0123456789abcdef")
	key2 := []byte("fedcba9876543210fedcba9876543210")
	s, err := encodeSigned(key1, statePurpose, signTestPayload{Foo: "hello", Bar: 42})
	if err != nil {
		t.Fatalf("encodeSigned: %v", err)
	}

	var got signTestPayload
	err = decodeSigned(key2, statePurpose, s, &got)
	if !errors.Is(err, errBadSignature) {
		t.Fatalf("decodeSigned err = %v, want errBadSignature", err)
	}
}

func TestDecodeSigned_WrongPurposeRejected(t *testing.T) {
	// Domain separation: a value signed for another purpose must not verify
	// as login state even though both use the same signing key.
	key := []byte("0123456789abcdef0123456789abcdef")
	s, err := encodeSigned(key, statePurpose, signTestPayload{Foo: "hello", Bar: 42})
	if err != nil {
		t.Fatalf("encodeSigned: %v", err)
	}

	var got signTestPayload
	err = decodeSigned(key, "mailroom.auth.other.v1", s, &got)
	if !errors.Is(err, errBadSignature) {
		t.Fatalf("decodeSigned err = %v, want errBadSignature", err)
	}
}

func TestDecodeSigned_MalformedInputRejected(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	var got signTestPayload

	for _, s := range []string{"", "no-dot-here", ".", "a.", ".b"} {
		if err := decodeSigned(key, statePurpose, s, &got); err == nil {
			t.Fatalf("decodeSigned(%q) accepted malformed input", s)
		}
	}
}

// flipLastChar flips the last character of the payload segment (if
// payloadSide is true) or the signature segment (otherwise) of a
// "payload.signature" string, in a way guaranteed to change its value.
func flipLastChar(t *testing.T, s string, payloadSide bool) string {
	t.Helper()
	dot := -1
	for i, c := range s {
		if c == '.' {
			dot = i
			break
		}
	}
	if dot < 0 {
		t.Fatalf("no '.' in %q", s)
	}
	b := []byte(s)
	idx := dot - 1
	if !payloadSide {
		idx = len(b) - 1
	}
	if b[idx] == 'A' {
		b[idx] = 'B'
	} else {
		b[idx] = 'A'
	}
	return string(b)
}
