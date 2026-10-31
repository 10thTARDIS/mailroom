// SPDX-License-Identifier: AGPL-3.0-only

package compose

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"errors"

	"github.com/oklog/ulid/v2"
)

// draftFingerprintInfo separates the fingerprint key from every other use of
// the mail-blob DEK. Changing it orphans every stored fingerprint, which is
// harmless: an unmatched fingerprint only means a repeat writes a new draft.
const draftFingerprintInfo = "mailroom-api-key-draft-fingerprint-v1"

// DraftFingerprint is a KEYED digest of canonical, the normalised content of
// an API key's draft create (#872, ADR-045 §2): HMAC-SHA256 under a key
// derived from the user's mail-blob DEK.
//
// WHY KEYED. A draft's recipients and body are not in Postgres; they live
// only in the encrypted blob store. A plain SHA-256 of them stored beside the
// draft would let anyone holding a database copy (a backup, a dump) confirm a
// guess — "is there a draft to alice@ saying yes?" — without the key. Under a
// key derived from the DEK, the database alone confirms nothing, and whoever
// holds the DEK can read the blobs anyway, so the fingerprint discloses
// nothing they could not already read.
func (c *Composer) DraftFingerprint(ctx context.Context, userID ulid.ULID, canonical []byte) ([]byte, error) {
	if c == nil || c.keys == nil {
		return nil, errors.New("compose: no keys to fingerprint a draft with")
	}
	dek, _, err := c.keys.MailDEK(ctx, userID)
	if err != nil {
		return nil, err
	}
	defer zero(dek)
	derive := hmac.New(sha256.New, dek)
	derive.Write([]byte(draftFingerprintInfo))
	key := derive.Sum(nil)
	defer zero(key)
	mac := hmac.New(sha256.New, key)
	mac.Write(canonical)
	return mac.Sum(nil), nil
}
