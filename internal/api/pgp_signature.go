// SPDX-License-Identifier: AGPL-3.0-only

package api

// The `pgpSignature` Email property (urn:mailroom:pgp, #47, #626 section E,
// ADR-039): what a PGP signature on a message means, said honestly.
//
// ONE PROPERTY FOR BOTH SHAPES. A PGP/MIME multipart/signed message verified at
// Email/get, and the multipart/signed INSIDE a decrypted message (#786's
// `encryptedSignature`, which this replaces), answer the same question for the
// same reader with the same four outcomes. Two properties would make every client
// implement the table below twice.
//
// THREE SHAPES, ONE DECISION. A signature embedded IN the encryption (RFC 3156
// section 6.2, "combined", what Thunderbird and GnuPG send) is checked by the
// crypto process during the decrypt, against keys chosen here
// (sigKeys.embeddedCandidates); its {status, fingerprint} is mapped onto a
// pgpverify.Result (embeddedVerdict) and goes through the same judgeSignature and
// classifySignature as the other two. PRECEDENCE, when a decrypted entity
// carries both: the inner multipart/signed wins (see signatureOfDecrypted).
//
// THE STATES
//
//	verified    the signature checks out, by a key we hold for the From address,
//	            that NAMES that address (a valid user ID with that email) and is
//	            one the user may rely on (compose.EligibleSource: imported, WKD,
//	            authenticated Autocrypt; not refused), or by the user's own key.
//	bad         the signature does not check out (or is malformed). With
//	            reason "keyExpired" it DID check out and was refused only because
//	            the key that made it has expired (#795/#796): a stale key, not a
//	            forgery.
//	unknownKey  a well-formed signature we cannot attribute to the sender: no key
//	            held, or the key that made it does not name the From address, or it
//	            is a held key nobody has vouched for and there is no earlier key to
//	            compare it with.
//	keyChanged  a good signature by a key that DIFFERS from the one already pinned
//	            for this address (or, when none is pinned, from the best eligible
//	            key held). previousFingerprint is that earlier key.
//	null        unsigned.
//
// ⚠ A HELD KEY IS NOT A TRUSTED KEY. Autocrypt puts a key in a header of the very
// message it signs, and the From of that message can be forged, so "the signature
// verifies against a key we hold for this sender" proves nothing when the holding
// came from the same message. That is why `verified` needs an ELIGIBLE source and
// why an ineligible key that verifies is reported as a change (when something to
// change from exists) or as unknown, never as verified.
//
// WHAT IT WRITES. Nothing is stored about the message. The one write is TOFU by
// verification: the first time an eligible key verifies for an address with no
// pin, that key is pinned (store.PinContactKeyIfAbsent), so a later different key
// is reported as changed even for a correspondent the user has never encrypted to.

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/compose"
	"mailroom/internal/mime"
	"mailroom/internal/pgpkeys"
	"mailroom/internal/pgpmime"
	"mailroom/internal/pgpverify"
	"mailroom/internal/store"
)

// The states of the pgpSignature property (a closed set a client can switch on).
const (
	sigVerified   = "verified"
	sigBad        = "bad"
	sigUnknownKey = "unknownKey"
	sigKeyChanged = "keyChanged"
)

type pgpSignature struct {
	State string `json:"state"`
	// Address is the From address the signature was judged against; the
	// ContactKey/decide call names it.
	Address string `json:"address,omitempty"`
	// Fingerprint: the signing key (verified, keyChanged), or what the signature
	// says made it (unknownKey; may be a 16-digit key id when the signature has no
	// full fingerprint).
	Fingerprint string `json:"fingerprint,omitempty"`
	// PreviousFingerprint: keyChanged only; the key it differs from.
	PreviousFingerprint string `json:"previousFingerprint,omitempty"`
	// Refused: keyChanged only; the user already chose to keep the old key.
	Refused bool `json:"refused,omitempty"`
	// Reason refines a `bad` state (#795/#796). The one value today is
	// "keyExpired": the signature checked out over the content and was refused
	// only because the key that made it (Fingerprint) has expired. The client
	// says "signed with a key that has expired", not "this may be forged".
	// Absent for every other bad signature.
	Reason string `json:"reason,omitempty"`
	// SignedAt and KeyExpiredAt (reason keyExpired only): the signature's own
	// creation time and when the key expired. ValidWhenSigned is
	// SignedAt < KeyExpiredAt — context for the wording ("signed on 3 May with a
	// key that expired on 1 June"), never an upgrade of the state: the
	// signature's date is the signer's own claim (ADR-039 §6 amendment).
	SignedAt        *string `json:"signedAt,omitempty"`
	KeyExpiredAt    *string `json:"keyExpiredAt,omitempty"`
	ValidWhenSigned *bool   `json:"validWhenSigned,omitempty"`
}

// sigReasonKeyExpired is the one `reason` a bad signature can carry today.
const sigReasonKeyExpired = "keyExpired"

// heldSigner is the stored key a good signature verified against.
type heldSigner struct {
	key store.ContactKey
	// own: one of the user's own keys (mail the user sent themselves).
	own bool
	// names: the key has a valid user ID with the From address.
	names bool
}

// sigFacts is everything classifySignature decides from. Gathering them touches
// the database and the keys; deciding is pure, so the table is tested directly.
type sigFacts struct {
	res     pgpverify.Result
	address string
	signer  *heldSigner
	// pin / hasPin: the fingerprint pinned for address.
	pin    string
	hasPin bool
	// other: the fingerprint of the best eligible key held for address other than
	// the signer ("" if none). The baseline when nothing is pinned.
	other string
}

// classifySignature turns the facts into the property. pinFirst reports that
// this is the first eligible verification for an address with no pin, which the
// caller records.
func classifySignature(f sigFacts) (sig *pgpSignature, pinFirst bool) {
	mk := func(state, fp string) *pgpSignature {
		return &pgpSignature{State: state, Address: f.address, Fingerprint: fp}
	}
	switch f.res.Verdict {
	case pgpverify.VerdictNotSigned:
		return nil, false
	case pgpverify.VerdictBadSignature, pgpverify.VerdictMalformed:
		if f.res.Verdict == pgpverify.VerdictBadSignature && f.res.KeyExpired {
			// ADR-039 §6 keeps an expired key's signature `bad`; the reason is
			// what stops the reader taking a stale key for a forgery.
			b := mk(sigBad, f.res.ExpiredSignerFingerprint)
			b.Reason = sigReasonKeyExpired
			b.SignedAt = jmapDatePtr(f.res.SignedAt)
			b.KeyExpiredAt = jmapDatePtr(f.res.KeyExpiredAt)
			if f.res.SignedAt != nil && f.res.KeyExpiredAt != nil {
				v := f.res.ValidWhenSigned()
				b.ValidWhenSigned = &v
			}
			return b, false
		}
		return mk(sigBad, ""), false
	case pgpverify.VerdictNoKey:
		return mk(sigUnknownKey, f.res.IssuerFingerprint), false
	case pgpverify.VerdictVerified:
	default:
		return mk(sigBad, ""), false // an unknown verdict is never good news
	}

	fp := f.res.SignerFingerprint
	s := f.signer
	// ADDRESS BINDING: a good signature by a key that does not name the From
	// address says nothing about the sender.
	if s == nil || !s.names {
		return mk(sigUnknownKey, fp), false
	}
	if s.own {
		return mk(sigVerified, fp), false
	}
	k := s.key
	eligible := s.attributable()

	changed := func(prev string) *pgpSignature {
		c := mk(sigKeyChanged, fp)
		c.PreviousFingerprint = prev
		c.Refused = k.Refused
		return c
	}
	switch {
	case k.Refused:
		// Kept refused: reported as a change from what the user kept, if known.
		if f.hasPin && f.pin != fp {
			return changed(f.pin), false
		}
		if f.other != "" {
			return changed(f.other), false
		}
		return mk(sigUnknownKey, fp), false
	case f.hasPin && f.pin != fp:
		if k.Source == store.ContactKeyManual {
			return mk(sigVerified, fp), false // imported or trusted by the user: an explicit act
		}
		return changed(f.pin), false
	case eligible:
		return mk(sigVerified, fp), !f.hasPin
	case f.other != "":
		return changed(f.other), false
	default:
		return mk(sigUnknownKey, fp), false
	}
}

// sigKeys is every key a signature on mail from one address is judged against,
// gathered once per message: the keys held for the From address's contact (of
// every source, eligible or not: eligibility is classifySignature's business),
// the user's own keys, and the pin. The SAME set serves a multipart/signed
// message (RFC 3156 section 6.1, checked here with pgpverify) and a signature
// inside the encryption (section 6.2, checked by the crypto process against
// embeddedCandidates), so the two shapes cannot be judged against different keys.
type sigKeys struct {
	from        string
	contactKeys []store.ContactKey
	ownKeys     []store.UserKey
	pin         string
	hasPin      bool
	now         time.Time
}

// fromAddress is the single From address of a message, normalised, or "".
func fromAddress(outer []byte) string {
	if m, _ := mime.ParseBytes(outer); m != nil {
		if f := m.Header.From(); len(f) == 1 {
			return store.NormalizeAddress(f[0].Address)
		}
	}
	return ""
}

// loadSigKeys reads the keys for the From address of outer (for decrypted mail,
// the OUTER message: headers hidden inside the ciphertext are never used, see
// pgpmime.Rewrap). nil when they cannot be read, which callers treat as "no
// verdict", never as a verdict.
func (a *API) loadSigKeys(ctx context.Context, userID ulid.ULID, outer []byte) *sigKeys {
	k := &sigKeys{from: fromAddress(outer), now: time.Now().UTC()}
	if err := a.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		if k.from != "" {
			if cid, ok, err := tx.ContactIDForAddress(ctx, k.from); err == nil && ok {
				if cks, err := tx.ContactKeysFor(ctx, cid); err == nil {
					k.contactKeys = cks
				}
			}
			var err error
			if k.pin, k.hasPin, err = tx.ContactKeyPin(ctx, k.from); err != nil {
				return err
			}
		}
		if own, err := tx.ListUserKeys(ctx); err == nil {
			k.ownKeys = own
		}
		return nil
	}); err != nil {
		slog.WarnContext(ctx, "could not read keys to check a signature", "user_id", userID.String(), "err", err)
		return nil
	}
	return k
}

// verifyKeys is the key list for pgpverify (section 6.1): the user's own keys and
// every PGP key held for the From address.
func (k *sigKeys) verifyKeys() []pgpverify.PublicKey {
	var keys []pgpverify.PublicKey
	for _, o := range k.ownKeys {
		if len(o.PublicKey) > 0 {
			keys = append(keys, pgpverify.PublicKey{Fingerprint: o.Fingerprint, Binary: o.PublicKey})
		}
	}
	for _, c := range k.contactKeys {
		if c.Kind == store.KeyPGP {
			keys = append(keys, pgpverify.PublicKey{Fingerprint: c.Fingerprint, Binary: c.PublicKey})
		}
	}
	return keys
}

// Bounds on the candidate signer keys sent with a decrypt request. They equal
// pgpwire.MaxDecryptSigners and pgpwire.MaxDecryptSignerBytes (a test in
// cmd/mailroom pins it); this package does not import the wire.
const (
	MaxEmbeddedSigners     = 16
	MaxEmbeddedSignerBytes = 1 << 20
)

// embeddedCandidates is the key list sent to the crypto process for a signature
// inside the encryption (section 6.2): the same keys as verifyKeys, but in an
// ORDER that decides what survives the bound, and only as public key bytes.
//
//  1. the pinned key (what a change is measured against);
//  2. eligible keys held for the address, best source first (what can verify);
//  3. the user's own keys that name the address (mail the user sent themselves);
//  4. ineligible keys held for the address (unauthenticated Autocrypt, refused):
//     they can never be `verified`, but without them a signature by one would
//     read as "no key" instead of the keyChanged or refused state it is;
//  5. the user's other own keys.
//
// Truncated at MaxEmbeddedSigners keys and MaxEmbeddedSignerBytes. A key cut off
// by the bound turns its signature into `unknownKey`, never into `verified`.
func (k *sigKeys) embeddedCandidates() [][]byte {
	type cand struct {
		pub  []byte
		tier int
		rank int
	}
	var cs []cand
	seen := map[string]bool{}
	add := func(fp string, pub []byte, tier, rank int) {
		if len(pub) == 0 || seen[fp] {
			return
		}
		seen[fp] = true
		cs = append(cs, cand{pub: pub, tier: tier, rank: rank})
	}
	for _, c := range k.contactKeys {
		if c.Kind != store.KeyPGP {
			continue
		}
		switch {
		case k.hasPin && c.Fingerprint == k.pin:
			add(c.Fingerprint, c.PublicKey, 1, 0)
		case compose.EligibleSource(c):
			add(c.Fingerprint, c.PublicKey, 2, compose.SourceRank(c))
		}
	}
	for _, o := range k.ownKeys {
		if k.from != "" && pgpkeys.NamesAddress(o.PublicKey, k.from, k.now) {
			add(o.Fingerprint, o.PublicKey, 3, 0)
		}
	}
	for _, c := range k.contactKeys {
		if c.Kind == store.KeyPGP {
			add(c.Fingerprint, c.PublicKey, 4, 0)
		}
	}
	for _, o := range k.ownKeys {
		add(o.Fingerprint, o.PublicKey, 5, 0)
	}
	// Stable: within a tier, contactKeys' newest-first order breaks rank ties.
	sort.SliceStable(cs, func(i, j int) bool {
		if cs[i].tier != cs[j].tier {
			return cs[i].tier < cs[j].tier
		}
		return cs[i].rank > cs[j].rank
	})
	var out [][]byte
	total := 0
	for _, c := range cs {
		if len(out) == MaxEmbeddedSigners {
			break
		}
		if total+len(c.pub) > MaxEmbeddedSignerBytes {
			continue // a smaller later key may still fit
		}
		total += len(c.pub)
		out = append(out, c.pub)
	}
	return out
}

// attributable is THE gate for "this held key speaks for the From address": the
// user's own key naming the address, or a held contact key that names it and
// that the user may rely on (compose.EligibleSource: imported, WKD,
// authenticated Autocrypt; never refused). The verified state and the
// keyExpired reason both pass through it, so a reason that softens `bad` can
// never be earned by a key the verified path would not have trusted.
func (s *heldSigner) attributable() bool {
	if s == nil || !s.names {
		return false
	}
	return s.own || compose.EligibleSource(s.key)
}

// signerFor finds the held key with this primary fingerprint (own keys first,
// then the contact keys held for the From address) and whether it names the
// From address at `at`. nil when no such key is held. Shared by the verified
// path and the keyExpired gate.
func (k *sigKeys) signerFor(fpr string, at time.Time) *heldSigner {
	for _, o := range k.ownKeys {
		if o.Fingerprint == fpr && len(o.PublicKey) > 0 {
			return &heldSigner{own: true, names: pgpkeys.NamesAddress(o.PublicKey, k.from, at)}
		}
	}
	for _, c := range k.contactKeys {
		if c.Kind == store.KeyPGP && c.Fingerprint == fpr {
			return &heldSigner{key: c, names: pgpkeys.NamesAddress(c.PublicKey, k.from, at)}
		}
	}
	return nil
}

// gateKeyExpired keeps the keyExpired reason only when the expired key that
// made the signature passes attributable() — judged as of just before the key
// expired, because a user ID's self-signature may be what expired. Anything
// else (an attacker's own throwaway expired key, a refused key, an
// unauthenticated Autocrypt key, a key that does not name From) is a plain
// `bad`: "signed with a key that has expired" is a benign story, and only a key
// we would otherwise have believed may tell it.
func (k *sigKeys) gateKeyExpired(res pgpverify.Result) pgpverify.Result {
	if res.Verdict != pgpverify.VerdictBadSignature || !res.KeyExpired {
		return res
	}
	at := k.now
	switch {
	case res.KeyExpiredAt != nil:
		at = res.KeyExpiredAt.Add(-time.Second)
	case res.SignedAt != nil:
		at = *res.SignedAt
	}
	if k.signerFor(res.ExpiredSignerFingerprint, at).attributable() {
		return res
	}
	res.KeyExpired, res.ExpiredSignerFingerprint, res.SignedAt, res.KeyExpiredAt = false, "", nil, nil
	return res
}

// judgeSignature turns a cryptographic result into the property: it finds the
// held key that verified, the baseline to compare against, classifies, and pins
// on a first eligible verification. Shared by both RFC 3156 shapes.
func (a *API) judgeSignature(ctx context.Context, userID ulid.ULID, k *sigKeys, res pgpverify.Result) *pgpSignature {
	from, now := k.from, k.now
	res = k.gateKeyExpired(res)
	facts := sigFacts{res: res, address: from, pin: k.pin, hasPin: k.hasPin}
	if res.Verdict == pgpverify.VerdictVerified {
		facts.signer = k.signerFor(res.SignerFingerprint, now)
		// The baseline when nothing is pinned: the best eligible key held other
		// than the one that signed (contactKeys is newest-first; ties keep it).
		best := 0
		for _, c := range k.contactKeys {
			if c.Kind != store.KeyPGP || c.Fingerprint == res.SignerFingerprint ||
				!compose.EligibleSource(c) || !pgpkeys.NamesAddress(c.PublicKey, from, now) {
				continue
			}
			if r := compose.SourceRank(c); r > best {
				best, facts.other = r, c.Fingerprint
			}
		}
	}

	sig, pinFirst := classifySignature(facts)
	if pinFirst && facts.signer != nil {
		// Fail-soft: the display is correct without the pin; the pin only makes
		// the NEXT different key visible.
		if err := a.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
			return tx.PinContactKeyIfAbsent(ctx, from, res.SignerFingerprint, facts.signer.key.Source)
		}); err != nil {
			slog.WarnContext(ctx, "could not pin a verified signing key", "user_id", userID.String(), "err", err)
		}
	}
	return sig
}

// pgpSignatureFor judges the multipart/signed signature on a message (RFC 3156
// section 6.1). signedMsg is the message whose structure is verified (the stored
// bytes, or the rewrapped decrypted entity); outer supplies the From address.
// nil when there is no PGP/MIME signature.
func (a *API) pgpSignatureFor(ctx context.Context, userID ulid.ULID, outer, signedMsg []byte) *pgpSignature {
	// Cheap structural test first: most mail is not signed and costs nothing more.
	if !isMultipartSigned(signedMsg) {
		return nil
	}
	k := a.loadSigKeys(ctx, userID, outer)
	if k == nil {
		return nil
	}
	return a.signedPartSignature(ctx, userID, k, signedMsg)
}

// signedPartSignature verifies a multipart/signed message against k.
func (a *API) signedPartSignature(ctx context.Context, userID ulid.ULID, k *sigKeys, signedMsg []byte) *pgpSignature {
	return a.judgeSignature(ctx, userID, k, pgpverify.Verify(signedMsg, k.verifyKeys()))
}

// isMultipartSigned reports whether msg is RFC 3156 multipart/signed (or claims
// to be: a malformed one is judged, and comes out `bad`).
func isMultipartSigned(msg []byte) bool {
	_, err := pgpmime.SplitSigned(msg)
	return !errors.Is(err, pgpmime.ErrNotSigned)
}

// embeddedVerdict maps the crypto process's section 6.2 result onto the verdicts
// classifySignature already decides from, so the states, the pin and the
// key-changed logic are literally the same code for both shapes.
func embeddedVerdict(e EmbeddedSignature) pgpverify.Result {
	if !e.Signed {
		return pgpverify.Result{Verdict: pgpverify.VerdictNotSigned}
	}
	switch e.Status {
	case EmbeddedVerified:
		if e.SignerFingerprint == "" {
			return pgpverify.Result{Verdict: pgpverify.VerdictBadSignature}
		}
		return pgpverify.Result{Verdict: pgpverify.VerdictVerified, SignerFingerprint: e.SignerFingerprint,
			IssuerFingerprint: e.Issuer}
	case EmbeddedNoKey:
		return pgpverify.Result{Verdict: pgpverify.VerdictNoKey, IssuerFingerprint: e.Issuer}
	case EmbeddedBad:
		r := pgpverify.Result{Verdict: pgpverify.VerdictBadSignature, IssuerFingerprint: e.Issuer}
		if e.KeyExpired && e.ExpiredSignerFingerprint != "" {
			r.KeyExpired, r.ExpiredSignerFingerprint = true, e.ExpiredSignerFingerprint
			r.SignedAt, r.KeyExpiredAt = e.SignedAt, e.KeyExpiredAt
		}
		return r
	default:
		// Any status this build does not know: never good news.
		return pgpverify.Result{Verdict: pgpverify.VerdictBadSignature, IssuerFingerprint: e.Issuer}
	}
}
