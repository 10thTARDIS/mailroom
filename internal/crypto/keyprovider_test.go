// SPDX-License-Identifier: AGPL-3.0-only

package crypto

import (
	"bytes"
	"context"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func writeSecret(t *testing.T, content []byte) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "master.key")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	return path
}

func TestFileKeyProviderRaw(t *testing.T) {
	key := mustKey(t)
	p := NewFileKeyProvider(writeSecret(t, key))
	got, err := p.MasterKey(context.Background())
	if err != nil {
		t.Fatalf("MasterKey: %v", err)
	}
	if !bytes.Equal(got, key) {
		t.Fatal("raw master key mismatch")
	}
}

func TestFileKeyProviderHex(t *testing.T) {
	key := mustKey(t)
	enc := make([]byte, hex.EncodedLen(KeySize))
	hex.Encode(enc, key)
	cases := map[string][]byte{
		"no newline":      enc,
		"trailing lf":     append(append([]byte(nil), enc...), '\n'),
		"trailing crlf":   append(append([]byte(nil), enc...), '\r', '\n'),
		"leading spaces":  append([]byte("  "), enc...),
		"uppercase input": bytes.ToUpper(enc),
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			p := NewFileKeyProvider(writeSecret(t, content))
			got, err := p.MasterKey(context.Background())
			if err != nil {
				t.Fatalf("MasterKey: %v", err)
			}
			if !bytes.Equal(got, key) {
				t.Fatal("hex master key mismatch")
			}
		})
	}
}

func TestFileKeyProviderBadInput(t *testing.T) {
	cases := map[string][]byte{
		"too short":    []byte("deadbeef"),
		"wrong length": make([]byte, 31),
		"bad hex":      bytes.Repeat([]byte("zz"), 32), // 64 chars, not hex
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			p := NewFileKeyProvider(writeSecret(t, content))
			if _, err := p.MasterKey(context.Background()); err == nil {
				t.Fatal("expected error for invalid master key file")
			}
		})
	}
}

func TestFileKeyProviderMissingFile(t *testing.T) {
	p := NewFileKeyProvider(filepath.Join(t.TempDir(), "nope"))
	if _, err := p.MasterKey(context.Background()); err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestFileKeyProviderContextCanceled(t *testing.T) {
	key := mustKey(t)
	p := NewFileKeyProvider(writeSecret(t, key))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.MasterKey(ctx); err == nil {
		t.Fatal("expected error for canceled context")
	}
}

// FileKeyProvider satisfies KeyProvider.
var _ KeyProvider = (*FileKeyProvider)(nil)
