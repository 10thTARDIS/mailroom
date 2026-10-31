// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/clamd"
	"mailroom/internal/mime"
)

type cleanScanner struct{ calls int }

func (c *cleanScanner) Scan(context.Context, []byte) (clamd.Verdict, error) {
	c.calls++
	return clamd.Verdict{State: clamd.Clean}, nil
}

// A decrypted entity can itself carry an encrypted subtree (a second layer, or
// a forwarded encrypted message). Its parts are `unknown` and never scanned —
// the subtree-scoped rule ingest applies since #405 — while a plaintext sibling
// is scanned normally (#47 6b, ADR-037 §5).
func TestDecryptedPartVerdictAppliesTheSubtreeRule(t *testing.T) {
	synth := "From: a@example.com\r\nMIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=\"o\"\r\n\r\n" +
		"--o\r\nContent-Type: application/pdf\r\nContent-Disposition: attachment; filename=\"a.pdf\"\r\n\r\n%PDF-1.4 sibling\r\n" +
		"--o\r\nContent-Type: multipart/encrypted; boundary=\"e\"; protocol=\"application/pgp-encrypted\"\r\n\r\n" +
		"--e\r\nContent-Type: application/pgp-encrypted\r\n\r\nVersion: 1\r\n" +
		"--e\r\nContent-Type: application/octet-stream\r\n\r\n-----BEGIN PGP MESSAGE-----\r\nxx\r\n-----END PGP MESSAGE-----\r\n" +
		"--e--\r\n--o--\r\n"
	parsed, err := mime.ParseBytes([]byte(synth))
	if err != nil || parsed == nil {
		t.Fatal(err)
	}
	byPath := map[string]*mime.Structure{}
	for n, p := range mime.PartPaths(parsed.Structure) {
		if p != "" {
			byPath[p] = n
		}
	}
	sc := &cleanScanner{}
	a := &API{onDemandScan: sc}
	if got, _ := a.decryptedPartVerdict(context.Background(), ulid.ULID{}, ulid.ULID{}, parsed, byPath["2.2"], "2.2"); got != string(clamd.Unknown) {
		t.Errorf("a part inside a nested encrypted subtree = %q, want unknown", got)
	}
	if sc.calls != 0 {
		t.Errorf("the nested ciphertext was sent to the scanner")
	}
	if got, _ := a.decryptedPartVerdict(context.Background(), ulid.ULID{}, ulid.ULID{}, parsed, byPath["1"], "1"); got != string(clamd.Clean) {
		t.Errorf("the plaintext sibling = %q, want clean (scanned)", got)
	}
	if sc.calls != 1 {
		t.Errorf("scanner calls = %d, want 1 (the sibling)", sc.calls)
	}
}
