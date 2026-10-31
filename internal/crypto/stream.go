// SPDX-License-Identifier: AGPL-3.0-only

package crypto

import (
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"

	"golang.org/x/crypto/chacha20poly1305"
)

// DefaultChunkSize is the plaintext chunk size for SealStream (64 KiB).
const DefaultChunkSize = 64 * 1024

const (
	streamSaltSize  = 32 // HKDF salt, random per stream
	streamNonceSize = 12 // IETF ChaCha20-Poly1305 nonce size

	streamKDFInfo = "mailroom-stream-aead-v1"

	// maxChunkSize bounds allocation from an attacker-controlled header
	// (256 MiB). DefaultChunkSize is far below this.
	maxChunkSize = 1 << 28
)

// Chunked stream format (Version1, algChaCha20Poly1305Stream)
//
// Stream header:
//
//	offset  size   field
//	------  ----   -----
//	0       1      version         (0x01)
//	1       1      algorithm id    (0x02 = ChaCha20-Poly1305, IETF)
//	2       2      key-id length N  (uint16, big-endian)
//	4       N      key-id           (opaque, caller-supplied)
//	4+N     32     salt             (HKDF-SHA256 salt, crypto/rand)
//	36+N    4      chunk size       (uint32, big-endian, plaintext bytes/chunk)
//
// Then one or more frames, each:
//
//	4      ciphertext length L (uint32, big-endian)
//	L      ciphertext || tag   (plaintext chunk + 16-byte Poly1305 tag)
//
// Construction and defenses:
//
//   - Per-stream subkey: streamKey = HKDF-SHA256(secret=DEK, salt=salt,
//     info="mailroom-stream-aead-v1" || key-id). The random per-stream salt
//     makes the subkey unique per stream, so the same DEK can seal unlimited
//     streams while a counter-based nonce stays collision-free WITHIN a stream.
//   - Per-chunk nonce = uint64 big-endian chunk index (bytes 0..7) || final
//     flag (byte 8, 0x01 iff this is the last chunk) || 0x00 0x00 0x00.
//   - Per-chunk AAD = the exact stream-header bytes || caller aad, binding
//     every chunk to its stream (key-id, salt, chunk size, version, alg).
//
// This resists:
//
//   - Truncation: the final chunk is sealed with the final flag set. On read,
//     only the last frame present is opened with the final nonce; if the real
//     final chunk was dropped, the new last frame (sealed final=0) fails to
//     open under the final nonce. Mid-frame truncation fails the length read.
//   - Reordering / dropping middle chunks: a chunk's index is bound into its
//     nonce, so moving it to another position fails authentication.
//   - Chunk-swapping across streams: different salt -> different subkey (and
//     different key-id in the AAD), so a chunk from another stream fails.
//
// An empty input produces exactly one empty final chunk (16-byte tag frame),
// which round-trips to empty output.

func streamNonce(index uint64, final bool) []byte {
	var n [streamNonceSize]byte
	binary.BigEndian.PutUint64(n[0:8], index)
	if final {
		n[8] = 1
	}
	return n[:]
}

func deriveStreamKey(key, salt, keyID []byte) ([]byte, error) {
	// info binds the opaque key-id into the KDF so subkeys are additionally
	// separated per key-id; salt (random per stream) makes each subkey unique.
	info := streamKDFInfo + string(keyID)
	out, err := hkdf.Key(sha256.New, key, salt, info, KeySize)
	if err != nil {
		return nil, fmt.Errorf("crypto: derive stream key: %w", err)
	}
	return out, nil
}

func buildStreamHeader(keyID, salt []byte, chunkSize int) ([]byte, error) {
	if len(keyID) > maxKeyIDLen {
		return nil, fmt.Errorf("crypto: key-id too long: %d bytes (max %d)", len(keyID), maxKeyIDLen)
	}
	h := make([]byte, 0, 4+len(keyID)+streamSaltSize+4)
	h = append(h, Version1, algChaCha20Poly1305Stream)
	var kl [2]byte
	binary.BigEndian.PutUint16(kl[:], uint16(len(keyID)))
	h = append(h, kl[:]...)
	h = append(h, keyID...)
	h = append(h, salt...)
	var cs [4]byte
	binary.BigEndian.PutUint32(cs[:], uint32(chunkSize))
	h = append(h, cs[:]...)
	return h, nil
}

type streamHeader struct {
	keyID     []byte
	salt      []byte
	chunkSize int
	raw       []byte // exact header bytes, used as AAD
}

func readStreamHeader(r io.Reader) (streamHeader, error) {
	var fixed [4]byte
	if _, err := io.ReadFull(r, fixed[:]); err != nil {
		return streamHeader{}, fmt.Errorf("crypto: open stream: read header: %w", err)
	}
	if fixed[0] != Version1 {
		return streamHeader{}, fmt.Errorf("crypto: open stream: unsupported version 0x%02x", fixed[0])
	}
	if fixed[1] != algChaCha20Poly1305Stream {
		return streamHeader{}, fmt.Errorf("crypto: open stream: unsupported algorithm 0x%02x", fixed[1])
	}
	keyIDLen := int(binary.BigEndian.Uint16(fixed[2:4]))
	keyID := make([]byte, keyIDLen)
	if _, err := io.ReadFull(r, keyID); err != nil {
		return streamHeader{}, fmt.Errorf("crypto: open stream: read key-id: %w", err)
	}
	salt := make([]byte, streamSaltSize)
	if _, err := io.ReadFull(r, salt); err != nil {
		return streamHeader{}, fmt.Errorf("crypto: open stream: read salt: %w", err)
	}
	var cs [4]byte
	if _, err := io.ReadFull(r, cs[:]); err != nil {
		return streamHeader{}, fmt.Errorf("crypto: open stream: read chunk size: %w", err)
	}
	chunkSize := int(binary.BigEndian.Uint32(cs[:]))
	if chunkSize <= 0 || chunkSize > maxChunkSize {
		return streamHeader{}, fmt.Errorf("crypto: open stream: invalid chunk size %d", chunkSize)
	}
	raw := make([]byte, 0, 4+keyIDLen+streamSaltSize+4)
	raw = append(raw, fixed[:]...)
	raw = append(raw, keyID...)
	raw = append(raw, salt...)
	raw = append(raw, cs[:]...)
	return streamHeader{keyID: keyID, salt: salt, chunkSize: chunkSize, raw: raw}, nil
}

// SealStream reads plaintext from src and writes the chunked, authenticated
// stream to dst. key is the 32-byte DEK; keyID is opaque, caller-supplied and
// recoverable from the stream header; aad is bound into every chunk. If
// chunkSize <= 0, DefaultChunkSize is used. See the format comment above for
// the wire layout and threat model.
func SealStream(dst io.Writer, src io.Reader, key, keyID, aad []byte, chunkSize int) error {
	if len(key) != KeySize {
		return fmt.Errorf("crypto: seal stream: key must be %d bytes, got %d", KeySize, len(key))
	}
	if chunkSize <= 0 {
		chunkSize = DefaultChunkSize
	}
	if chunkSize > maxChunkSize {
		return fmt.Errorf("crypto: seal stream: chunk size %d exceeds max %d", chunkSize, maxChunkSize)
	}
	salt := make([]byte, streamSaltSize)
	if _, err := rand.Read(salt); err != nil {
		return fmt.Errorf("crypto: seal stream: read salt: %w", err)
	}
	streamKey, err := deriveStreamKey(key, salt, keyID)
	if err != nil {
		return err
	}
	defer Zero(streamKey)
	aead, err := chacha20poly1305.New(streamKey)
	if err != nil {
		return fmt.Errorf("crypto: seal stream: init aead: %w", err)
	}
	hdr, err := buildStreamHeader(keyID, salt, chunkSize)
	if err != nil {
		return fmt.Errorf("crypto: seal stream: %w", err)
	}
	if _, err := dst.Write(hdr); err != nil {
		return fmt.Errorf("crypto: seal stream: write header: %w", err)
	}
	chunkAAD := make([]byte, 0, len(hdr)+len(aad))
	chunkAAD = append(chunkAAD, hdr...)
	chunkAAD = append(chunkAAD, aad...)

	var index uint64
	sealChunk := func(pt []byte, final bool) error {
		ct := aead.Seal(nil, streamNonce(index, final), pt, chunkAAD)
		var lp [4]byte
		binary.BigEndian.PutUint32(lp[:], uint32(len(ct)))
		if _, err := dst.Write(lp[:]); err != nil {
			return fmt.Errorf("crypto: seal stream: write frame length: %w", err)
		}
		if _, err := dst.Write(ct); err != nil {
			return fmt.Errorf("crypto: seal stream: write frame: %w", err)
		}
		return nil
	}

	// One-chunk look-ahead so the last chunk with data is sealed as final.
	var prev []byte
	havePrev := false
	for {
		buf := make([]byte, chunkSize)
		n, rerr := io.ReadFull(src, buf)
		switch rerr {
		case nil:
			if havePrev {
				if err := sealChunk(prev, false); err != nil {
					return err
				}
				index++
			}
			prev, havePrev = buf[:n], true
		case io.ErrUnexpectedEOF:
			// Short final read (n > 0): this is the last chunk with data.
			if havePrev {
				if err := sealChunk(prev, false); err != nil {
					return err
				}
				index++
			}
			return sealChunk(buf[:n], true)
		case io.EOF:
			// No more data. If we buffered a full final chunk, seal it now;
			// otherwise (empty input) emit a single empty final chunk.
			if havePrev {
				return sealChunk(prev, true)
			}
			return sealChunk(nil, true)
		default:
			return fmt.Errorf("crypto: seal stream: read: %w", rerr)
		}
	}
}

// OpenStream reads a chunked stream from src, authenticates and decrypts it,
// and writes the recovered plaintext to dst. keyFor is called with the opaque
// key-id recovered from the header and must return the 32-byte DEK. aad must
// equal the aad passed to SealStream. Truncation, reordering, chunk-swapping
// and any tampering fail with an error and no (further) plaintext.
//
// Note: dst may receive already-authenticated leading chunks before a later
// chunk fails; callers that need all-or-nothing output should buffer dst and
// commit only on a nil return.
func OpenStream(dst io.Writer, src io.Reader, keyFor func(keyID []byte) ([]byte, error), aad []byte) error {
	h, err := readStreamHeader(src)
	if err != nil {
		return err
	}
	key, err := keyFor(h.keyID)
	if err != nil {
		return fmt.Errorf("crypto: open stream: select key: %w", err)
	}
	if len(key) != KeySize {
		return fmt.Errorf("crypto: open stream: key must be %d bytes, got %d", KeySize, len(key))
	}
	streamKey, err := deriveStreamKey(key, h.salt, h.keyID)
	if err != nil {
		return err
	}
	defer Zero(streamKey)
	aead, err := chacha20poly1305.New(streamKey)
	if err != nil {
		return fmt.Errorf("crypto: open stream: init aead: %w", err)
	}
	chunkAAD := make([]byte, 0, len(h.raw)+len(aad))
	chunkAAD = append(chunkAAD, h.raw...)
	chunkAAD = append(chunkAAD, aad...)
	maxCT := h.chunkSize + aead.Overhead()

	cur, err := readFrame(src, maxCT, aead.Overhead())
	if err == io.EOF {
		// A valid stream always has at least one frame (the final chunk).
		return fmt.Errorf("crypto: open stream: no chunks")
	}
	if err != nil {
		return err
	}
	var index uint64
	for {
		next, nerr := readFrame(src, maxCT, aead.Overhead())
		if nerr == io.EOF {
			// cur is the final chunk.
			pt, err := aead.Open(nil, streamNonce(index, true), cur, chunkAAD)
			if err != nil {
				return fmt.Errorf("crypto: open stream: final chunk %d authentication failed: %w", index, err)
			}
			if _, err := dst.Write(pt); err != nil {
				return fmt.Errorf("crypto: open stream: write plaintext: %w", err)
			}
			return nil
		}
		if nerr != nil {
			return nerr
		}
		// cur is a non-final chunk.
		pt, err := aead.Open(nil, streamNonce(index, false), cur, chunkAAD)
		if err != nil {
			return fmt.Errorf("crypto: open stream: chunk %d authentication failed: %w", index, err)
		}
		if _, err := dst.Write(pt); err != nil {
			return fmt.Errorf("crypto: open stream: write plaintext: %w", err)
		}
		index++
		cur = next
	}
}

// readFrame reads one length-prefixed ciphertext frame. It returns io.EOF
// only for a clean end (no bytes at the frame boundary). A partial frame,
// an out-of-range length, or an unexpected EOF is a truncation/corruption
// error.
func readFrame(r io.Reader, maxCT, overhead int) ([]byte, error) {
	var lp [4]byte
	if _, err := io.ReadFull(r, lp[:]); err != nil {
		if err == io.EOF {
			return nil, io.EOF
		}
		return nil, fmt.Errorf("crypto: open stream: truncated frame length: %w", err)
	}
	l := int(binary.BigEndian.Uint32(lp[:]))
	if l < overhead || l > maxCT {
		return nil, fmt.Errorf("crypto: open stream: invalid frame length %d", l)
	}
	ct := make([]byte, l)
	if _, err := io.ReadFull(r, ct); err != nil {
		return nil, fmt.Errorf("crypto: open stream: truncated frame: %w", err)
	}
	return ct, nil
}
