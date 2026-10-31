// SPDX-License-Identifier: AGPL-3.0-only

package crypto

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"strconv"
	"testing"
)

// staticKey returns a keyFor callback that always yields key.
func staticKey(key []byte) func([]byte) ([]byte, error) {
	return func([]byte) ([]byte, error) { return key, nil }
}

func sealToBytes(t *testing.T, key, keyID, aad, pt []byte, chunkSize int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := SealStream(&buf, bytes.NewReader(pt), key, keyID, aad, chunkSize); err != nil {
		t.Fatalf("SealStream: %v", err)
	}
	return buf.Bytes()
}

func openFromBytes(t *testing.T, key, aad, stream []byte) ([]byte, error) {
	t.Helper()
	var out bytes.Buffer
	err := OpenStream(&out, bytes.NewReader(stream), staticKey(key), aad)
	return out.Bytes(), err
}

func TestStreamRoundTrip(t *testing.T) {
	key := mustKey(t)
	keyID := []byte("dek-blob-1")
	aad := []byte("blob-aad")
	const chunk = 64

	sizes := []int{0, 1, 63, 64, 65, 128, 200, 64*3 + 7}
	for _, sz := range sizes {
		t.Run(sizeName(sz), func(t *testing.T) {
			pt := make([]byte, sz)
			if _, err := rand.Read(pt); err != nil {
				t.Fatalf("rand: %v", err)
			}
			stream := sealToBytes(t, key, keyID, aad, pt, chunk)
			got, err := openFromBytes(t, key, aad, stream)
			if err != nil {
				t.Fatalf("OpenStream: %v", err)
			}
			if !bytes.Equal(got, pt) {
				t.Fatalf("round-trip mismatch at size %d: got %d bytes", sz, len(got))
			}
		})
	}
}

func TestStreamKeyIDInHeader(t *testing.T) {
	key := mustKey(t)
	keyID := []byte("dek-blob-9")
	stream := sealToBytes(t, key, keyID, nil, []byte("hi"), 64)

	var gotID []byte
	var out bytes.Buffer
	err := OpenStream(&out, bytes.NewReader(stream), func(id []byte) ([]byte, error) {
		gotID = append([]byte(nil), id...)
		return key, nil
	}, nil)
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	if !bytes.Equal(gotID, keyID) {
		t.Fatalf("key-id in header = %q, want %q", gotID, keyID)
	}
}

func TestStreamWrongKey(t *testing.T) {
	key := mustKey(t)
	stream := sealToBytes(t, key, []byte("id"), nil, bytes.Repeat([]byte("x"), 200), 64)
	if _, err := openFromBytes(t, mustKey(t), nil, stream); err == nil {
		t.Fatal("expected error with wrong key")
	}
}

func TestStreamWrongAAD(t *testing.T) {
	key := mustKey(t)
	stream := sealToBytes(t, key, []byte("id"), []byte("aad-a"), bytes.Repeat([]byte("x"), 200), 64)
	if _, err := openFromBytes(t, key, []byte("aad-b"), stream); err == nil {
		t.Fatal("expected error with wrong aad")
	}
}

func TestStreamTamper(t *testing.T) {
	key := mustKey(t)
	stream := sealToBytes(t, key, []byte("id"), nil, bytes.Repeat([]byte("x"), 200), 64)
	// Flip a byte inside the first frame's ciphertext (past the header).
	hdrLen := streamHeaderLen(t, []byte("id"))
	bad := append([]byte(nil), stream...)
	bad[hdrLen+4+1] ^= 0x01 // skip header + 4-byte frame length, into ciphertext
	if _, err := openFromBytes(t, key, nil, bad); err == nil {
		t.Fatal("expected error on tampered chunk")
	}
}

func TestStreamTruncationRejected(t *testing.T) {
	key := mustKey(t)
	keyID := []byte("id")
	// 200 bytes @ 64 => 4 chunks (64,64,64,8). Drop the final frame.
	pt := bytes.Repeat([]byte("y"), 200)
	stream := sealToBytes(t, key, keyID, nil, pt, 64)
	frames := splitFrames(t, stream, keyID)
	if len(frames.frames) < 2 {
		t.Fatalf("expected multiple frames, got %d", len(frames.frames))
	}

	t.Run("drop final frame", func(t *testing.T) {
		truncated := frames.rebuild(frames.frames[:len(frames.frames)-1])
		if _, err := openFromBytes(t, key, nil, truncated); err == nil {
			t.Fatal("expected error: final chunk dropped")
		}
	})
	t.Run("drop trailing bytes mid-frame", func(t *testing.T) {
		if _, err := openFromBytes(t, key, nil, stream[:len(stream)-3]); err == nil {
			t.Fatal("expected error: mid-frame truncation")
		}
	})
}

func TestStreamReorderRejected(t *testing.T) {
	key := mustKey(t)
	keyID := []byte("id")
	pt := bytes.Repeat([]byte("z"), 200)
	stream := sealToBytes(t, key, keyID, nil, pt, 64)
	f := splitFrames(t, stream, keyID)
	if len(f.frames) < 3 {
		t.Fatalf("need >=3 frames, got %d", len(f.frames))
	}
	// Swap the first two (both full, non-final) frames.
	swapped := append([][]byte(nil), f.frames...)
	swapped[0], swapped[1] = swapped[1], swapped[0]
	if _, err := openFromBytes(t, key, nil, f.rebuild(swapped)); err == nil {
		t.Fatal("expected error: reordered chunks")
	}
}

// --- test helpers for framing ---

func streamHeaderLen(t *testing.T, keyID []byte) int {
	t.Helper()
	return 4 + len(keyID) + streamSaltSize + 4
}

type splitStream struct {
	header []byte
	frames [][]byte // each element is the raw ciphertext (without the length prefix)
}

func (s splitStream) rebuild(frames [][]byte) []byte {
	out := append([]byte(nil), s.header...)
	for _, f := range frames {
		var lp [4]byte
		binary.BigEndian.PutUint32(lp[:], uint32(len(f)))
		out = append(out, lp[:]...)
		out = append(out, f...)
	}
	return out
}

func splitFrames(t *testing.T, stream, keyID []byte) splitStream {
	t.Helper()
	hlen := streamHeaderLen(t, keyID)
	if len(stream) < hlen {
		t.Fatalf("stream shorter than header")
	}
	s := splitStream{header: stream[:hlen]}
	rest := stream[hlen:]
	for len(rest) > 0 {
		if len(rest) < 4 {
			t.Fatalf("dangling bytes at frame boundary")
		}
		l := int(binary.BigEndian.Uint32(rest[:4]))
		rest = rest[4:]
		if len(rest) < l {
			t.Fatalf("frame length %d exceeds remaining %d", l, len(rest))
		}
		s.frames = append(s.frames, rest[:l])
		rest = rest[l:]
	}
	return s
}

func sizeName(n int) string {
	return "size_" + strconv.Itoa(n)
}
