// SPDX-License-Identifier: AGPL-3.0-only

package compose

// Encrypting an outgoing message to the recipients' keys (#47, ADR-038).
//
// WHERE IT HAPPENS. At Submit, in prepareOutbound, immediately after signing: the
// stored draft is rewritten into the signed-then-encrypted message BEFORE the
// submission row exists, so the Sent copy appended to IMAP, the blob, the content
// hash and the bytes the recipients receive are one object — the reasoning signing
// already documents (compose.go, SubmitPlanned). Drafts are NOT encrypted: a draft
// is the user's own working copy and is stored exactly as before.
//
// WHAT THIS FILE OWNS. Which stored key an address gets (ResolveRecipientKey), the
// refusals a send can run into (Bcc, no key, no sender key) and the interface to
// the crypto process. It does no cryptography; the daemon's encrypt verb does.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/mime"
	"mailroom/internal/pgpkeys"
	"mailroom/internal/store"
)

// EncryptTo is one recipient and the public key chosen for it.
type EncryptTo struct {
	// Address is the recipient, normalised.
	Address string
	// Fingerprint is the primary key fingerprint of the key chosen (uppercase
	// hex), recorded so "which key did this go to" has an answer.
	Fingerprint string
	// PublicKey is the key as stored: binary OpenPGP packets.
	PublicKey []byte
	// Source is how the key reached us (store.ContactKeyWKD, ...Autocrypt, ...).
	Source string
}

// Encrypter encrypts a MIME entity to public keys and to the sender's own, with
// the crypto process (ADR-024). An interface so this package never links the
// crypto client; cmd/mailroom supplies the real one, and a test a function.
type Encrypter interface {
	// Encrypt returns the ASCII-armored OpenPGP message. keyID is the sender's own
	// key (its public half is added as a recipient). A recipient the crypto
	// process cannot encrypt to is reported as *MissingKeysError.
	Encrypt(ctx context.Context, userID, keyID ulid.ULID, to []EncryptTo, entity []byte) (ciphertext string, err error)
}

// WithEncrypter attaches an outgoing-mail encrypter. Encryption additionally needs
// a Signer (the message is signed, then encrypted), so CanEncrypt is false
// without one.
func (c *Composer) WithEncrypter(e Encrypter) *Composer {
	c.encrypter = e
	return c
}

// CanEncrypt reports whether this server can encrypt outbound mail AT ALL: a
// crypto process is wired for BOTH signing and encryption. It is the field
// SubmitPlanned consults, and what the JMAP session advertises as
// urn:mailroom:pgp {encrypt}, so the two cannot disagree. Nil-safe like CanSign.
func (c *Composer) CanEncrypt() bool { return c != nil && c.encrypter != nil && c.signer != nil }

// The refusals an encrypted send can run into. Each is returned from PlanSubmit,
// BEFORE anything is written, so a refused send leaves the draft untouched.
var (
	// ErrEncryptUnavailable — the server has no crypto process wired for encrypting.
	ErrEncryptUnavailable = errors.New("compose: this server cannot encrypt outgoing mail (no crypto process is configured)")

	// ErrEncryptNoSenderKey — the sending identity has no usable PGP key bound.
	// Encryption always adds the sender's own key as a recipient (so the Sent copy
	// stays readable) and signs with it, so an identity without one cannot encrypt.
	ErrEncryptNoSenderKey = errors.New("compose: the sending identity has no PGP key bound, so its mail cannot be encrypted")

	// ErrEncryptBcc — a Bcc recipient with encryption.
	//
	// ⚠ ONE CIPHERTEXT, EVERY RECIPIENT'S KEY ID. An OpenPGP message carries one
	// session-key packet per recipient, each naming the key it is for; the same
	// bytes go to every recipient, so a Bcc'd person's key id would be readable by
	// everyone who can read a key id. Separate ciphertexts per Bcc recipient would
	// fix that and is deliberately not built in this version (use "hidden
	// recipients" / throw-key-ids, RFC 9580 wildcard key ids, or one message per
	// Bcc, later).
	ErrEncryptBcc = errors.New("compose: a message with Bcc recipients cannot be encrypted: " +
		"one ciphertext would reveal every recipient's key, including the Bcc ones")
)

// MissingKeysError lists recipients that have no usable encryption key.
type MissingKeysError struct {
	// Addresses are normalised and sorted.
	Addresses []string
}

func (e *MissingKeysError) Error() string {
	return "compose: no usable encryption key for: " + strings.Join(e.Addresses, ", ")
}

// Trust ranks of a key's source. A higher rank is never displaced by a lower one,
// whatever the dates (see ResolveRecipientKey).
const (
	rankIneligible = 0
	rankAutocrypt  = 1 // authenticated Autocrypt only
	rankWKD        = 2
	rankManual     = 3 // imported by the user: an explicit act
)

// sourceRank is how much a stored key may be trusted for ENCRYPTION, from its
// source and whether its carrying message was authenticated.
//
//   - manual (the user imported it): the user's own explicit act.
//   - wkd: served by the recipient's own domain over HTTPS (internal/wkd,
//     through the SSRF-guarded egress client).
//   - autocrypt: a header on received mail, whose From can be forged. Only when
//     the carrying message was AUTHENTICATED for its From domain (store column
//     authenticated, decided at ingest from a DMARC pass under the topmost trusted
//     Authentication-Results or an aligned local DKIM pass).
//   - keyserver and anything unknown: not eligible; nothing writes them today.
func sourceRank(k store.ContactKey) int {
	if k.Refused {
		return rankIneligible // the user chose to keep the old key (#47, ADR-039)
	}
	switch k.Source {
	case store.ContactKeyManual:
		return rankManual
	case store.ContactKeyWKD:
		return rankWKD
	case store.ContactKeyAutocrypt:
		if k.Authenticated {
			return rankAutocrypt
		}
	}
	return rankIneligible
}

// EligibleSource reports whether k's SOURCE makes it a key the user may rely on
// for this correspondent (ADR-038 section 4, rule 1, and not refused): imported,
// WKD, or Autocrypt from an authenticated message. The signature display uses the
// same rule, so "verified" and "may encrypt to" cannot drift apart.
func EligibleSource(k store.ContactKey) bool { return sourceRank(k) > rankIneligible }

// SourceRank exposes the source rank for ordering keys (higher is more trusted).
func SourceRank(k store.ContactKey) int { return sourceRank(k) }

// ResolveRecipientKey picks the key to encrypt to for one address, from the keys
// held for its contact. ok is false when none is eligible.
//
// # The rules (ADR-038)
//
// A stored key is ELIGIBLE when all of these hold at `now`:
//  1. its source ranks above ineligible (sourceRank): imported, WKD, or
//     AUTHENTICATED Autocrypt. An unauthenticated Autocrypt key is ignored: its
//     header rode on mail whose From could have been forged;
//  2. it can be encrypted to: readable, no private material, and an unexpired,
//     unrevoked encryption subkey on an unexpired, unrevoked primary key
//     (pgpkeys.EncryptionFingerprint);
//  3. it NAMES THE ADDRESS: a valid, non-revoked user ID whose email is the
//     address (pgpkeys.EncryptionFingerprintFor).
//
// Among eligible keys the HIGHEST source rank wins (imported > WKD > Autocrypt),
// then the most recently fetched. So a newer Autocrypt key never displaces an
// older imported or WKD key.
//
// This answers "which key", not "is it the key we used before": that is the pin
// (ResolveRecipient).
func ResolveRecipientKey(ctx context.Context, tx *store.UserTx, address string, now time.Time) (EncryptTo, bool, error) {
	addr := store.NormalizeAddress(address)
	if addr == "" {
		return EncryptTo{}, false, nil
	}
	cid, found, err := tx.ContactIDForAddress(ctx, addr)
	if err != nil {
		return EncryptTo{}, false, err
	}
	if !found {
		return EncryptTo{}, false, nil
	}
	keys, err := tx.ContactKeysFor(ctx, cid) // newest fetch first
	if err != nil {
		return EncryptTo{}, false, err
	}
	best, bestRank := EncryptTo{}, rankIneligible
	for _, k := range keys {
		if k.Kind != store.KeyPGP {
			continue
		}
		rank := sourceRank(k)
		if rank <= bestRank { // ineligible, or not better than what we have (ties keep the newest)
			continue
		}
		fpr, err := pgpkeys.EncryptionFingerprintFor(k.PublicKey, addr, now)
		if err != nil {
			continue // unreadable, expired, revoked, no encryption subkey, names another address
		}
		best, bestRank = EncryptTo{Address: addr, Fingerprint: fpr, PublicKey: k.PublicKey, Source: k.Source}, rank
	}
	return best, bestRank > rankIneligible, nil
}

// KeyResolution is ResolveRecipientKey plus trust on first use.
type KeyResolution struct {
	Key EncryptTo
	// Found: an eligible key exists (Key is set).
	Found bool
	// KeyChanged: a fingerprint is pinned for this address and the eligible key is
	// a different one. Encryption to this recipient is REFUSED until the user
	// confirms (ContactKey/decide, ADR-039).
	KeyChanged        bool
	PinnedFingerprint string
	// ExpiresAt (Found only): when the chosen key stops being usable
	// (pgpkeys.Validity.UsableUntil); nil means it never expires (#795).
	ExpiresAt *time.Time
	// Unusable (not Found only) says WHY no key could be chosen, when the
	// answer is more useful than "none" (#795): UnusableExpired (ExpiredAt is
	// the latest moment one of the held keys stopped working) or
	// UnusableRevoked. "" means nothing usable was ever held for the address.
	Unusable  string
	ExpiredAt *time.Time
}

// Why an address has no usable key, when it has held one (KeyResolution.Unusable).
const (
	UnusableExpired = "expired"
	UnusableRevoked = "revoked"
)

// ResolveRecipient resolves the key for one address and checks it against the pin.
//
// # Trust on first use
//
// The fingerprint actually encrypted to is pinned per address at send time. If
// the key resolved later differs, the recipient's key CHANGED, which is exactly
// what a planted or rotated key looks like, and the send is refused rather than
// quietly following the newer key. The one exception is a key the user IMPORTED
// (source manual): importing is an explicit act, so it may replace the pin.
func ResolveRecipient(ctx context.Context, tx *store.UserTx, address string, now time.Time) (KeyResolution, error) {
	key, ok, err := ResolveRecipientKey(ctx, tx, address, now)
	if err != nil {
		return KeyResolution{}, err
	}
	if !ok {
		return explainNoKey(ctx, tx, address, now)
	}
	res := KeyResolution{Key: key, Found: true}
	if v, verr := pgpkeys.Assess(key.PublicKey, now); verr == nil {
		if until, never := v.UsableUntil(); !never {
			u := until.UTC()
			res.ExpiresAt = &u
		}
	}
	pinned, hasPin, err := tx.ContactKeyPin(ctx, key.Address)
	if err != nil {
		return KeyResolution{}, err
	}
	if hasPin && pinned != key.Fingerprint && key.Source != store.ContactKeyManual {
		res.KeyChanged, res.PinnedFingerprint = true, pinned
	}
	return res, nil
}

// explainNoKey says why an address with no eligible key has none, when a key
// that WOULD have been eligible is held and has lapsed (#795): the same source
// rule (ADR-038 rule 1, refused keys excluded) and the same address rule (rule
// 3, read just before the key's end, since a user ID's self-signature may be
// what expired), but the clock is what failed it. Expired outranks revoked: a
// correspondent whose old key was revoked and whose new key expired needs to be
// told about the date. It never changes what Submit does; it only names the
// reason RecipientKey/lookup reports.
func explainNoKey(ctx context.Context, tx *store.UserTx, address string, now time.Time) (KeyResolution, error) {
	addr := store.NormalizeAddress(address)
	if addr == "" {
		return KeyResolution{}, nil
	}
	cid, found, err := tx.ContactIDForAddress(ctx, addr)
	if err != nil || !found {
		return KeyResolution{}, err
	}
	keys, err := tx.ContactKeysFor(ctx, cid)
	if err != nil {
		return KeyResolution{}, err
	}
	var res KeyResolution
	for _, k := range keys {
		if k.Kind != store.KeyPGP || sourceRank(k) <= rankIneligible {
			continue
		}
		v, verr := pgpkeys.Assess(k.PublicKey, now)
		if verr != nil || !v.HasEncryptionKey {
			continue
		}
		switch {
		case v.Revoked:
			if res.Unusable == "" && pgpkeys.NamesAddress(k.PublicKey, addr, now) {
				res.Unusable = UnusableRevoked
			}
		case v.Expired(now):
			until, _ := v.UsableUntil()
			if !pgpkeys.NamesAddress(k.PublicKey, addr, until.Add(-time.Second)) {
				continue
			}
			u := until.UTC()
			if res.ExpiredAt == nil || u.After(*res.ExpiredAt) {
				res.Unusable, res.ExpiredAt = UnusableExpired, &u
			}
		}
	}
	return res, nil
}

// KeyChange names one recipient whose key changed since it was pinned.
type KeyChange struct {
	Address     string
	Pinned, Now string
}

// KeyChangedError refuses an encrypted send because recipients' keys changed.
type KeyChangedError struct{ Changes []KeyChange }

func (e *KeyChangedError) Error() string {
	parts := make([]string, 0, len(e.Changes))
	for _, c := range e.Changes {
		parts = append(parts, fmt.Sprintf("%s (was %s, now %s)", c.Address, c.Pinned, c.Now))
	}
	return "compose: the encryption key changed and must be confirmed before you can encrypt to: " +
		strings.Join(parts, "; ")
}

// encryptPlan is everything PlanSubmit resolved for an encrypted send, carried to
// SubmitPlanned so the keys that were checked are the keys that are used.
type encryptPlan struct {
	to []EncryptTo
}

// planEncrypt validates an encrypted send and resolves its recipient keys. It
// reads (SELECTs) and writes nothing.
func (c *Composer) planEncrypt(ctx context.Context, tx *store.UserTx, si store.SendIdentity, raw []byte, rcpt []string) (*encryptPlan, error) {
	if !c.CanEncrypt() {
		return nil, ErrEncryptUnavailable
	}

	key, ok, err := tx.SigningKeyForIdentity(ctx, si.IdentityID)
	if err != nil {
		return nil, err
	}
	if !ok || key.Kind != store.KeyPGP || len(key.PublicKey) == 0 {
		return nil, ErrEncryptNoSenderKey
	}
	// A sender key that has lapsed (#795) cannot be encrypted to, and the crypto
	// process always encrypts to the sender too; refuse here, before any
	// recipient is resolved, with the error that names the actual problem.
	if advertise, _ := ownKeyFitness(key.PublicKey, time.Now()); !advertise {
		return nil, ErrEncryptNoSenderKey
	}

	parsed, _ := mime.ParseBytes(raw)
	if parsed == nil {
		return nil, errors.New("compose: the message could not be read")
	}

	// ─── BCC IS REFUSED, BY TWO INDEPENDENT TESTS ───────────────────────────
	// A Bcc header on the draft, and any envelope recipient that the To/Cc headers
	// do not name (an envelope override is a Bcc by another route). Either alone
	// would let one through.
	if len(parsed.Header.Bcc()) > 0 {
		return nil, ErrEncryptBcc
	}
	disclosed := map[string]bool{}
	for _, list := range [][]mime.Address{parsed.Header.To(), parsed.Header.Cc()} {
		for _, a := range list {
			if n := store.NormalizeAddress(a.Address); n != "" {
				disclosed[n] = true
			}
		}
	}
	for _, r := range rcpt {
		if !disclosed[store.NormalizeAddress(r)] {
			return nil, ErrEncryptBcc
		}
	}

	// Every disclosed (To/Cc) address needs a key, or the send is refused naming
	// all of those that lack one.
	addrs := make([]string, 0, len(disclosed))
	for a := range disclosed {
		addrs = append(addrs, a)
	}
	sort.Strings(addrs)

	now := time.Now().UTC()
	var (
		to      []EncryptTo
		missing []string
		changed []KeyChange
	)
	for _, a := range addrs {
		if a == store.NormalizeAddress(si.FromAddress) {
			// Writing to yourself: the sender's own key is the recipient's key.
			fpr, ferr := pgpkeys.EncryptionFingerprint(key.PublicKey, now)
			if ferr != nil {
				return nil, ErrEncryptNoSenderKey
			}
			to = append(to, EncryptTo{Address: a, Fingerprint: fpr, PublicKey: key.PublicKey, Source: "own"})
			continue
		}
		r, err := ResolveRecipient(ctx, tx, a, now)
		if err != nil {
			return nil, err
		}
		switch {
		case !r.Found:
			missing = append(missing, a)
		case r.KeyChanged:
			changed = append(changed, KeyChange{Address: a, Pinned: r.PinnedFingerprint, Now: r.Key.Fingerprint})
		default:
			to = append(to, r.Key)
		}
	}
	if len(missing) > 0 {
		return nil, &MissingKeysError{Addresses: missing}
	}
	if len(changed) > 0 {
		return nil, &KeyChangedError{Changes: changed}
	}
	if len(to) == 0 {
		return nil, fmt.Errorf("%w: the message has no To or Cc recipient", ErrNoRecipients)
	}
	return &encryptPlan{to: to}, nil
}
