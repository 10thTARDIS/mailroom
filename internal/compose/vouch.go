// SPDX-License-Identifier: AGPL-3.0-only

package compose

// A vouched-for key rotation (ADR-054 section 3), the part that needs the user's
// stored keys: rule 1 (the voucher is the PIN for the address and is held) and the
// user's own refusal (rule 7). The cryptographic rules live in pgpkeys.CheckVouch.
//
// ⚠ THIS DECIDES WHAT A PROMPT SAYS, NEVER WHAT THE PIN IS. Nothing here, and
// nothing that reads its result, moves a pin: the user does, by deciding
// (ContactKey/decide). That is the operator's decision on ADR-054 section 4
// ("always ask, gently"), and TestAVouchNeverMovesAPin pins it.

import (
	"context"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/pgpkeys"
	"mailroom/internal/store"
)

// Vouch is what a key-changed prompt about one address may add.
type Vouch struct {
	// VouchedBy is the fingerprint of the pinned key that vouches for the new key;
	// "" when any rule failed.
	VouchedBy string
	// PreviousCompromised: the pinned key carries a revocation for reason
	// "compromised". It hardens the ordinary prompt and is independent of
	// VouchedBy.
	PreviousCompromised bool
	// Refusal is the rule that failed, for logs and tests ("" when vouched, or
	// when there was no pin to vouch).
	Refusal string
}

// VouchFor judges key n, which differs from the pin, for address. keys are the
// keys held (not removed) for the contact; firstSeen maps a held key's row id to
// its first_seen time (store.ContactKeyFirstSeen).
func VouchFor(keys []store.ContactKey, firstSeen map[ulid.ULID]time.Time, address, pin string, n store.ContactKey, now time.Time) Vouch {
	// Rule 1: the voucher is the pin for this address, and a held key.
	var prev *store.ContactKey
	for i := range keys {
		if keys[i].Kind == store.KeyPGP && pin != "" && keys[i].Fingerprint == pin {
			prev = &keys[i]
			break
		}
	}
	if prev == nil || prev.Fingerprint == n.Fingerprint {
		return Vouch{}
	}
	out := Vouch{PreviousCompromised: pgpkeys.RevokedAsCompromised(prev.PublicKey)}
	if n.Refused { // rule 7: a key the user already refused is not softened
		out.Refusal = "refused"
		return out
	}
	v := pgpkeys.CheckVouch(pgpkeys.VouchInput{
		Previous: prev.PublicKey, New: n.PublicKey, Address: address,
		NewFirstSeen: firstSeen[n.ID], Now: now,
	})
	out.Refusal = v.Refusal
	if v.Vouched {
		out.VouchedBy = prev.Fingerprint
	}
	return out
}

// VouchForKey is VouchFor reading the contact's keys and first-seen times itself:
// for callers that have only the address and the key (compose, ContactKey/decide).
func VouchForKey(ctx context.Context, tx *store.UserTx, address, pin string, n store.ContactKey, now time.Time) (Vouch, error) {
	addr := store.NormalizeAddress(address)
	cid, found, err := tx.ContactIDForAddress(ctx, addr)
	if err != nil || !found {
		return Vouch{}, err
	}
	keys, err := tx.ContactKeysFor(ctx, cid)
	if err != nil {
		return Vouch{}, err
	}
	ids := make([]ulid.ULID, 0, len(keys))
	for _, k := range keys {
		ids = append(ids, k.ID)
	}
	seen, err := tx.ContactKeyFirstSeen(ctx, ids)
	if err != nil {
		return Vouch{}, err
	}
	return VouchFor(keys, seen, addr, pin, n, now), nil
}

// PinVoucher is ADR-054 rule 8, the compensating notice: the pin was made through
// a vouch, and the key that vouched has since been seen revoked.
type PinVoucher struct {
	// Revoked: the current pin's latest `pinned` event is "vouched:<P>", P is a
	// key still held for the contact, and P carries a revocation.
	Revoked bool
	// Compromised: Revoked, and the reason is "compromised".
	Compromised bool
}

// PinVoucherFor reads rule 8 for the key pinned for a contact. keys are the
// contact's held keys. It only reads: nothing here, and nothing that shows the
// result, changes the pin. A failure to read means no notice is shown, which is
// today's behaviour.
func PinVoucherFor(ctx context.Context, tx *store.UserTx, contactID ulid.ULID, keys []store.ContactKey, pin string, now time.Time) (PinVoucher, error) {
	if pin == "" || len(keys) < 2 {
		return PinVoucher{}, nil
	}
	voucher, err := tx.ContactKeyPinVoucher(ctx, contactID, pin)
	if err != nil || voucher == "" || voucher == pin {
		return PinVoucher{}, err
	}
	for _, k := range keys {
		if k.Kind != store.KeyPGP || k.Fingerprint != voucher {
			continue
		}
		if v, verr := pgpkeys.Assess(k.PublicKey, now); verr == nil && v.Revoked {
			return PinVoucher{Revoked: true, Compromised: pgpkeys.RevokedAsCompromised(k.PublicKey)}, nil
		}
	}
	return PinVoucher{}, nil
}
