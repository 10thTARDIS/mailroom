// SPDX-License-Identifier: AGPL-3.0-only

package compose

import (
	"time"

	"mailroom/internal/pgpkeys"
)

// ownKeyFitness reads the sending identity's bound key and says what it is still
// good for at now (#795, ADR-040):
//
//   - advertise: it can still be ENCRYPTED to (not revoked, an encryption
//     subkey, not past its usable-until), so putting it in an outgoing Autocrypt
//     header invites mail that can be sent. An expired key in the header would
//     tell every Autocrypt client "here is my key" for a key none of them can
//     use — and would keep refreshing that stale copy in their stores.
//   - sign: the primary key is neither revoked nor expired, so a signature made
//     with it verifies. go-crypto refuses to sign with an expired key anyway;
//     checking here turns "every send fails at the crypto boundary" into "the
//     send goes out unsigned", which is what an expired signing key means.
//
// ⚠ A KEY THIS CANNOT READ KEEPS ITS OLD TREATMENT (advertise and sign). Only
// the crypto process writes these rows, from go-crypto output, so an unreadable
// one is an anomaly the crypto process will report on its own; turning it into
// a silent "stop signing" here would hide it. Only a key whose OWN PACKETS say
// it has lapsed is held back.
//
// The signing predicate SigningKeyForIdentity applies (decrypt_only,
// revoked_at, private half present) has already run; this adds what only the
// packets know.
//
// The reading itself is pgpkeys.OwnKeyFitness, and the Identity projection's
// signingKeyId (store, "Will sign") reads the same function (#796, ADR-040
// amendment), so compose never sends unsigned under a "Will sign" badge.
func ownKeyFitness(pub []byte, now time.Time) (advertise, sign bool) {
	return pgpkeys.OwnKeyFitness(pub, now)
}
