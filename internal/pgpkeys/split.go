// SPDX-License-Identifier: AGPL-3.0-only

package pgpkeys

import (
	"bytes"

	"github.com/ProtonMail/go-crypto/openpgp"
)

// MaxSplitKeys bounds how many keys SplitKeys returns from one blob.
const MaxSplitKeys = 8

// SplitKeys returns each transferable public key in binary as its own binary
// blob (#818). A WKD response may carry several concatenated keys: the current
// one first, then revoked ones (ADR-040 §4), and the key a caller holds can be
// any of them, so reading only the first (as Parse does) would miss a renewal
// or a revocation served second.
//
// Each key is re-serialised by go-crypto, which keeps every user ID, signature
// and subkey binding it parsed; Merge then verifies what it keeps. A blob that
// carries secret key material, or that go-crypto cannot read, yields
// ErrHasPrivateKey or ErrMalformed rather than a partial list. At most
// MaxSplitKeys keys are returned.
func SplitKeys(binary []byte) ([][]byte, error) {
	if len(binary) == 0 {
		return nil, ErrMalformed
	}
	if len(binary) > MaxKeyBytes {
		return nil, ErrTooLarge
	}
	entities, err := openpgp.ReadKeyRing(bytes.NewReader(binary))
	if err != nil || len(entities) == 0 {
		return nil, ErrMalformed
	}
	var out [][]byte
	for _, e := range entities {
		if e.PrivateKey != nil {
			return nil, ErrHasPrivateKey
		}
		for _, sub := range e.Subkeys {
			if sub.PrivateKey != nil {
				return nil, ErrHasPrivateKey
			}
		}
		if len(out) == MaxSplitKeys {
			break
		}
		var buf bytes.Buffer
		if err := e.Serialize(&buf); err != nil {
			return nil, ErrMalformed
		}
		out = append(out, buf.Bytes())
	}
	return out, nil
}
