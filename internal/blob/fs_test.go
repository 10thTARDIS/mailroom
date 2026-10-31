// SPDX-License-Identifier: AGPL-3.0-only

package blob

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/crypto"
)

func testServerKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, crypto.KeySize)
	if _, err := rand.Read(k); err != nil {
		t.Fatalf("generate server key: %v", err)
	}
	return k
}

func testDEK(t *testing.T) []byte {
	t.Helper()
	k, err := crypto.GenerateDEK()
	if err != nil {
		t.Fatalf("generate dek: %v", err)
	}
	return k
}

func testULID(t *testing.T) ulid.ULID {
	t.Helper()
	id, err := ulid.New(ulid.Now(), rand.Reader)
	if err != nil {
		t.Fatalf("generate ulid: %v", err)
	}
	return id
}

func newTestStore(t *testing.T) (*FSBlobStore, []byte) {
	t.Helper()
	key := testServerKey(t)
	s, err := NewFSBlobStore(t.TempDir(), key)
	if err != nil {
		t.Fatalf("NewFSBlobStore: %v", err)
	}
	return s, key
}

func mustGetAll(t *testing.T, s *FSBlobStore, userID ulid.ULID, dek []byte, h ContentHash) []byte {
	t.Helper()
	rc, err := s.Get(context.Background(), userID, dek, h)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer func() { _ = rc.Close() }()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read plaintext: %v", err)
	}
	return got
}

func TestPutGetRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		size int
	}{
		{"empty", 0},
		{"small", 128},
		{"exactly-one-chunk", crypto.DefaultChunkSize},
		{"multi-chunk", crypto.DefaultChunkSize*2 + 12345},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newTestStore(t)
			userID := testULID(t)
			dek := testDEK(t)

			plaintext := make([]byte, tc.size)
			if _, err := rand.Read(plaintext); err != nil {
				t.Fatalf("generate plaintext: %v", err)
			}

			h, err := s.Put(context.Background(), userID, dek, bytes.NewReader(plaintext))
			if err != nil {
				t.Fatalf("Put: %v", err)
			}

			wantHash := sha256Sum(plaintext)
			if h != wantHash {
				t.Fatalf("Put returned hash %s, want %s", h, wantHash)
			}

			got := mustGetAll(t, s, userID, dek, h)
			if !bytes.Equal(got, plaintext) {
				t.Fatalf("round-tripped plaintext mismatch: got %d bytes, want %d bytes", len(got), len(plaintext))
			}
		})
	}
}

func TestContentAddressingDedup(t *testing.T) {
	s, _ := newTestStore(t)
	userID := testULID(t)
	dek := testDEK(t)

	plaintext := []byte("the same content, written twice")

	h1, err := s.Put(context.Background(), userID, dek, bytes.NewReader(plaintext))
	if err != nil {
		t.Fatalf("first Put: %v", err)
	}
	h2, err := s.Put(context.Background(), userID, dek, bytes.NewReader(plaintext))
	if err != nil {
		t.Fatalf("second Put: %v", err)
	}
	if h1 != h2 {
		t.Fatalf("hashes differ across identical Puts: %s vs %s", h1, h2)
	}

	path := s.blobPath(userID, h1)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("stat blob path: %v", err)
	}

	// Exactly one file should exist under the shard directory.
	entries, err := os.ReadDir(s.shardDir(userID, h1))
	if err != nil {
		t.Fatalf("read shard dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("shard dir has %d entries, want 1 (dedup failed)", len(entries))
	}
}

func TestDifferentUsersDifferentPaths(t *testing.T) {
	s, _ := newTestStore(t)
	userA := testULID(t)
	userB := testULID(t)
	dekA := testDEK(t)
	dekB := testDEK(t)

	plaintext := []byte("shared plaintext, different owners")

	hA, err := s.Put(context.Background(), userA, dekA, bytes.NewReader(plaintext))
	if err != nil {
		t.Fatalf("Put userA: %v", err)
	}
	hB, err := s.Put(context.Background(), userB, dekB, bytes.NewReader(plaintext))
	if err != nil {
		t.Fatalf("Put userB: %v", err)
	}

	if hA != hB {
		t.Fatalf("content hashes should match across users: %s vs %s", hA, hB)
	}

	pathA := s.blobPath(userA, hA)
	pathB := s.blobPath(userB, hB)
	if pathA == pathB {
		t.Fatalf("expected different paths for different users, got the same: %s", pathA)
	}
	if filepath.Dir(filepath.Dir(pathA)) == filepath.Dir(filepath.Dir(pathB)) {
		t.Fatalf("expected different user prefixes, got the same directory: %s", filepath.Dir(filepath.Dir(pathA)))
	}

	// userB must not be able to read userA's blob under its own hash lookup
	// using userA's path semantics; Exists must reflect per-user isolation.
	existsForA, err := s.Exists(context.Background(), userA, hA)
	if err != nil {
		t.Fatalf("Exists userA: %v", err)
	}
	if !existsForA {
		t.Fatalf("expected userA's blob to exist")
	}

	existsForBUnderA, err := s.Exists(context.Background(), userB, hA)
	if err != nil {
		t.Fatalf("Exists userB under hA: %v", err)
	}
	// userB happens to also have stored the same hash under its own prefix,
	// so this should be true (it's userB's own copy, not userA's).
	if !existsForBUnderA {
		t.Fatalf("expected userB's own copy under the same hash to exist")
	}

	// A user who never wrote this content should not see it.
	userC := testULID(t)
	existsForC, err := s.Exists(context.Background(), userC, hA)
	if err != nil {
		t.Fatalf("Exists userC: %v", err)
	}
	if existsForC {
		t.Fatalf("userC should not see a blob it never wrote")
	}
}

func TestGetTamperedCiphertextFails(t *testing.T) {
	s, _ := newTestStore(t)
	userID := testULID(t)
	dek := testDEK(t)

	plaintext := make([]byte, crypto.DefaultChunkSize+1000)
	if _, err := rand.Read(plaintext); err != nil {
		t.Fatalf("generate plaintext: %v", err)
	}

	h, err := s.Put(context.Background(), userID, dek, bytes.NewReader(plaintext))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	path := s.blobPath(userID, h)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read ciphertext: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("ciphertext file is empty")
	}
	// Flip a byte well past the header, inside frame data.
	tampered := append([]byte(nil), data...)
	tampered[len(tampered)-1] ^= 0xFF
	if err := os.WriteFile(path, tampered, 0o600); err != nil {
		t.Fatalf("write tampered ciphertext: %v", err)
	}

	rc, err := s.Get(context.Background(), userID, dek, h)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer func() { _ = rc.Close() }()
	if _, err := io.ReadAll(rc); err == nil {
		t.Fatalf("expected error reading tampered ciphertext, got nil")
	}
}

func TestGetWrongDEKFails(t *testing.T) {
	s, _ := newTestStore(t)
	userID := testULID(t)
	dek := testDEK(t)
	wrongDEK := testDEK(t)

	plaintext := []byte("secret content")
	h, err := s.Put(context.Background(), userID, dek, bytes.NewReader(plaintext))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	rc, err := s.Get(context.Background(), userID, wrongDEK, h)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer func() { _ = rc.Close() }()
	if _, err := io.ReadAll(rc); err == nil {
		t.Fatalf("expected error reading with wrong dek, got nil")
	}
}

func TestDeleteIdempotent(t *testing.T) {
	s, _ := newTestStore(t)
	userID := testULID(t)
	dek := testDEK(t)

	plaintext := []byte("to be deleted")
	h, err := s.Put(context.Background(), userID, dek, bytes.NewReader(plaintext))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	if err := s.Delete(context.Background(), userID, h); err != nil {
		t.Fatalf("first Delete: %v", err)
	}

	exists, err := s.Exists(context.Background(), userID, h)
	if err != nil {
		t.Fatalf("Exists after delete: %v", err)
	}
	if exists {
		t.Fatalf("blob still exists after Delete")
	}

	// Deleting again (already gone) must be a no-op, not an error.
	if err := s.Delete(context.Background(), userID, h); err != nil {
		t.Fatalf("second (idempotent) Delete: %v", err)
	}

	// Deleting something that never existed must also be a no-op.
	otherHash, err := ParseContentHash(strings.Repeat("00", 32))
	if err != nil {
		t.Fatalf("ParseContentHash: %v", err)
	}
	if err := s.Delete(context.Background(), userID, otherHash); err != nil {
		t.Fatalf("Delete of never-existing blob: %v", err)
	}
}

func TestGetNotFound(t *testing.T) {
	s, _ := newTestStore(t)
	userID := testULID(t)
	dek := testDEK(t)

	h, err := ParseContentHash(strings.Repeat("00", 32))
	if err != nil {
		t.Fatalf("ParseContentHash: %v", err)
	}

	_, err = s.Get(context.Background(), userID, dek, h)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get on missing blob: got err %v, want ErrNotFound", err)
	}
}

func TestParseContentHashRoundTrip(t *testing.T) {
	plaintext := []byte("round trip me")
	h := sha256Sum(plaintext)

	parsed, err := ParseContentHash(h.String())
	if err != nil {
		t.Fatalf("ParseContentHash: %v", err)
	}
	if parsed != h {
		t.Fatalf("parsed hash mismatch: got %s, want %s", parsed, h)
	}

	if _, err := ParseContentHash("not-hex"); err == nil {
		t.Fatalf("expected error for invalid hex")
	}
	if _, err := ParseContentHash("ab"); err == nil {
		t.Fatalf("expected error for short hash")
	}
}

func TestNewFSBlobStoreValidatesServerKey(t *testing.T) {
	if _, err := NewFSBlobStore(t.TempDir(), make([]byte, 16)); err == nil {
		t.Fatalf("expected error for short server key")
	}
	if _, err := NewFSBlobStore("", testServerKey(t)); err == nil {
		t.Fatalf("expected error for empty root")
	}
}

// sha256Sum is a small test helper mirroring the hash Put computes.
func sha256Sum(b []byte) ContentHash {
	return ContentHash(sha256.Sum256(b))
}
