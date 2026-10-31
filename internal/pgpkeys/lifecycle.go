// SPDX-License-Identifier: AGPL-3.0-only

package pgpkeys

// Key lifecycle on the PUBLIC side (#795, ADR-040): how long a key stays usable,
// and how two copies of the same key are merged when a renewal arrives.
//
// Both live here, in the application, for the reason the package comment gives:
// they read public keys only. Re-signing a key to extend it needs the private
// half and lives in the crypto process (internal/pgpkeyd); merging the result
// into a correspondent's stored copy does not.

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

// ExpiryWarningDays is how many days ahead of a key's end of usability Mailroom
// starts warning about it (#795). ONE constant, shared by the report that sizes
// it, the JMAP `expiringSoon` flags, and the session capability that tells the
// client the same number (`keyExpiryWarningDays`), so the threshold measured is
// the threshold shipped and the threshold shown.
const ExpiryWarningDays = 30

// ExpiryWarningWindow is ExpiryWarningDays as a duration.
const ExpiryWarningWindow = ExpiryWarningDays * 24 * time.Hour

// ExpiryReportHorizon is the report's second bucket edge: keys usable for
// longer than the warning window but not by much.
const ExpiryReportHorizon = 90 * 24 * time.Hour

// Validity is what a public key's packets say about how long it can be
// encrypted to. It is computed from the newest self-signatures, the way
// go-crypto (and every OpenPGP implementation) chooses which one is in force.
type Validity struct {
	// Revoked: the primary key carries a valid key revocation, or its primary
	// user ID is revoked. Either way nothing can be encrypted to it again.
	Revoked bool
	// HasEncryptionKey: at least one non-revoked encryption-capable key exists
	// (a subkey flagged for communications encryption, or a primary key flagged
	// for it when there is no such subkey). A signing-only key has none.
	HasEncryptionKey bool
	// PrimaryExpires is when the primary key stops being valid: the earlier of
	// its key-expiry (creation + key lifetime) and its self-signature's own
	// expiry. nil means it never expires.
	PrimaryExpires *time.Time
	// EncryptionExpires is when the LAST encryption-capable key stops being
	// valid. nil means one of them never expires (or there is none — see
	// HasEncryptionKey).
	EncryptionExpires *time.Time
	// Created is the primary key's own creation time, from its packet: when the
	// key was MADE, which for a correspondent's key is not when we first saw it.
	Created time.Time
}

// UsableUntil is the moment the key can no longer be encrypted to: the earliest
// of the primary key's expiry and the encryption key's expiry. A subkey can
// expire before its primary, and it is the subkey a correspondent encrypts to,
// so the primary alone is not the answer (#795). never is true when neither
// expires. Revocation and a missing encryption key are NOT expressed here —
// callers check Revoked and HasEncryptionKey first.
func (v Validity) UsableUntil() (until time.Time, never bool) {
	switch {
	case v.PrimaryExpires == nil && v.EncryptionExpires == nil:
		return time.Time{}, true
	case v.PrimaryExpires == nil:
		return *v.EncryptionExpires, false
	case v.EncryptionExpires == nil:
		return *v.PrimaryExpires, false
	case v.PrimaryExpires.Before(*v.EncryptionExpires):
		return *v.PrimaryExpires, false
	default:
		return *v.EncryptionExpires, false
	}
}

// Usable reports whether the key can be encrypted to at now: not revoked, has
// an encryption key, and not past UsableUntil.
func (v Validity) Usable(now time.Time) bool {
	if v.Revoked || !v.HasEncryptionKey {
		return false
	}
	until, never := v.UsableUntil()
	return never || now.Before(until)
}

// ExpiringSoon reports whether a key that is usable at now stops being usable
// within ExpiryWarningWindow. A key that is already unusable (or never expires)
// is not "expiring soon": it is expired, revoked, or fine.
func (v Validity) ExpiringSoon(now time.Time) bool {
	if !v.Usable(now) {
		return false
	}
	until, never := v.UsableUntil()
	return !never && until.Sub(now) < ExpiryWarningWindow
}

// Expired reports whether the key has passed its usable-until (not revoked, has
// an encryption key, and the moment is behind us). It is the one reading that
// lets a caller say "expired on <date>" instead of a generic "not usable".
func (v Validity) Expired(now time.Time) bool {
	if v.Revoked || !v.HasEncryptionKey {
		return false
	}
	until, never := v.UsableUntil()
	return !never && !now.Before(until)
}

// CanSign reports whether the key's OWN PACKETS still let it make a signature
// that verifies at now: not revoked, and the primary key not expired (#795,
// ADR-040). It is the packet half of "Will sign"; the column half
// (decrypt_only, revoked_at, private half present) is the store's
// signingKeyUsable. Compose's choice to sign and the Identity projection's
// signingKeyId both read this one method, so the two cannot disagree.
func (v Validity) CanSign(now time.Time) bool {
	return !v.Revoked && (v.PrimaryExpires == nil || now.Before(*v.PrimaryExpires))
}

// OwnKeyFitness reads one of the user's OWN public keys and says what it is
// still good for at now: advertise (it can be encrypted to, so an outgoing
// Autocrypt header may carry it) and sign (CanSign).
//
// ⚠ A KEY THIS CANNOT READ KEEPS ITS OLD TREATMENT (advertise and sign). Only
// the crypto process writes own-key rows, from go-crypto output, so an
// unreadable one is an anomaly the crypto process reports on its own; turning
// it into a silent "stop signing" here would hide it. Only a key whose OWN
// PACKETS say it has lapsed is held back.
func OwnKeyFitness(pub []byte, now time.Time) (advertise, sign bool) {
	v, err := Assess(pub, now)
	if err != nil {
		return true, true
	}
	return v.Usable(now), v.CanSign(now)
}

// Assess reads a BINARY public key and reports its Validity at now. It refuses
// what Parse refuses (oversized, unreadable, secret material).
func Assess(binary []byte, now time.Time) (Validity, error) {
	if _, err := Parse(binary); err != nil {
		return Validity{}, err
	}
	entities, err := openpgp.ReadKeyRing(bytes.NewReader(binary))
	if err != nil || len(entities) == 0 {
		return Validity{}, ErrMalformed
	}
	return assessEntity(entities[0], now), nil
}

func assessEntity(e *openpgp.Entity, now time.Time) Validity {
	var v Validity
	if e.PrimaryKey != nil {
		v.Created = e.PrimaryKey.CreationTime.UTC()
	}
	self, ident := e.PrimarySelfSignature()
	if e.Revoked(now) || (ident != nil && ident.Revoked(now)) {
		v.Revoked = true
	}
	if self != nil {
		v.PrimaryExpires = keyExpiry(e.PrimaryKey, self)
	}

	found := false
	var latest *time.Time
	never := false
	for i := range e.Subkeys {
		sub := &e.Subkeys[i]
		if sub.Sig == nil || !sub.Sig.FlagsValid || !sub.Sig.FlagEncryptCommunications ||
			!sub.PublicKey.PubKeyAlgo.CanEncrypt() || sub.Revoked(now) {
			continue
		}
		found = true
		exp := keyExpiry(sub.PublicKey, sub.Sig)
		if exp == nil {
			never = true
			continue
		}
		if latest == nil || exp.After(*latest) {
			latest = exp
		}
	}
	if !found && self != nil && self.FlagsValid && self.FlagEncryptCommunications &&
		e.PrimaryKey.PubKeyAlgo.CanEncrypt() {
		found = true
		if exp := keyExpiry(e.PrimaryKey, self); exp != nil {
			latest = exp
		} else {
			never = true
		}
	}
	v.HasEncryptionKey = found
	if found && !never {
		v.EncryptionExpires = latest
	}
	return v
}

// keyExpiry is the earlier of a key's expiry under sig (creation + key
// lifetime) and sig's own expiry. nil when neither is set.
func keyExpiry(pk *packet.PublicKey, sig *packet.Signature) *time.Time {
	var out *time.Time
	if sig.KeyLifetimeSecs != nil && *sig.KeyLifetimeSecs != 0 {
		t := pk.CreationTime.Add(time.Duration(*sig.KeyLifetimeSecs) * time.Second)
		out = &t
	}
	if sig.SigLifetimeSecs != nil && *sig.SigLifetimeSecs != 0 {
		t := sig.CreationTime.Add(time.Duration(*sig.SigLifetimeSecs) * time.Second)
		if out == nil || t.Before(*out) {
			out = &t
		}
	}
	return out
}

// The expiry buckets `pgp-report` counts keys into (#795: report first, so the
// warning threshold is chosen on real data).
const (
	BucketRevoked      = "revoked"
	BucketNoEncryption = "no-encryption-key"
	BucketExpired      = "expired"
	BucketUnder30      = "<30d"
	BucketUnder90      = "<90d"
	BucketLater        = "later"
	BucketNever        = "never"
)

// ExpiryBuckets lists the buckets in report order.
var ExpiryBuckets = []string{
	BucketExpired, BucketUnder30, BucketUnder90, BucketLater, BucketNever,
	BucketRevoked, BucketNoEncryption,
}

// ExpiryBucket files a Validity into one bucket at now. Revocation wins over
// expiry, so a revoked key is never also counted as expired.
func ExpiryBucket(v Validity, now time.Time) string {
	if v.Revoked {
		return BucketRevoked
	}
	if !v.HasEncryptionKey {
		return BucketNoEncryption
	}
	until, never := v.UsableUntil()
	switch {
	case never:
		return BucketNever
	case !now.Before(until):
		return BucketExpired
	case until.Sub(now) < ExpiryWarningWindow:
		return BucketUnder30
	case until.Sub(now) < ExpiryReportHorizon:
		return BucketUnder90
	default:
		return BucketLater
	}
}

// ---------------------------------------------------------------------------
// Merge on renewal.
// ---------------------------------------------------------------------------

// ErrDifferentKey means the two copies handed to Merge are not the same primary
// key. Same fingerprint is the precondition of merging at all.
var ErrDifferentKey = errors.New("pgpkeys: the two copies are not the same primary key")

// Merge combines two copies of the SAME OpenPGP key, the way GnuPG's import
// does, and reports whether the result carries anything the stored copy did not
// (#795, ADR-040).
//
// The result is the UNION of both copies' user IDs, subkeys and signatures for
// one primary key, keeping only SELF-signatures that verify against that primary
// key. Expiry and revocation are then whatever the merged packets say, which is
// what go-crypto computes from them on every read:
//
//   - A RENEWAL extends usability: its newer self-signature (later creation
//     time, later expiry) is added, and the newest self-signature is the one in
//     force.
//   - A REPLAYED OLDER COPY cannot shorten it: its signatures are already held
//     (or are older than the ones held), so nothing it carries becomes the one
//     in force.
//   - A REVOCATION SURVIVES any later merge: a union never removes a signature,
//     and a pre-revocation copy has nothing that undoes one.
//   - A FORGED self-signature (one that does not verify) is DROPPED, so a copy
//     carrying a fake later expiry extends nothing.
//
// ⚠ NOT A REPLACE, AND THAT IS THE WHOLE POINT. Replacing the stored copy with
// the incoming one would let anyone who can put a key in an Autocrypt header —
// which is anyone who can send mail — strip a revocation by replaying the copy
// from before it.
//
// Third-party certifications are DROPPED rather than kept unverified: nothing in
// Mailroom reads them, and keeping whatever a stranger attaches would let one
// inflate a stored key past MaxKeyBytes and so block every later renewal of it.
//
// changed compares the merged bytes against the stored copy NORMALISED the same
// way (filtered and re-serialised), so a re-sighting of an identical key — or of
// one that differs only in dropped material — is not a change and costs no
// write. stored must be a key this package can read; incoming may be anything,
// and is refused with an error rather than half-merged.
func Merge(stored, incoming []byte) (merged []byte, changed bool, err error) {
	base, err := readKeyBlock(stored)
	if err != nil {
		return nil, false, fmt.Errorf("stored copy: %w", err)
	}
	normalised, err := base.serialize()
	if err != nil {
		return nil, false, err
	}
	in, err := readKeyBlock(incoming)
	if err != nil {
		return nil, false, fmt.Errorf("incoming copy: %w", err)
	}
	if !bytes.Equal(base.primary.Fingerprint, in.primary.Fingerprint) {
		return nil, false, ErrDifferentKey
	}
	base.absorb(in)
	merged, err = base.serialize()
	if err != nil {
		return nil, false, err
	}
	if len(merged) > MaxKeyBytes {
		return nil, false, ErrTooLarge
	}
	// The merged key must still be one every reader in this codebase accepts.
	// Anything else would replace a usable stored key with an unreadable one.
	if _, err := Parse(merged); err != nil {
		return nil, false, fmt.Errorf("merged copy: %w", err)
	}
	return merged, !bytes.Equal(merged, normalised), nil
}

// keyBlock is one transferable public key reduced to what Merge keeps: the
// primary key, its verified key-level signatures, and its user IDs and subkeys,
// each with their verified self-signatures. Order is first-seen, so merging a
// copy that adds nothing reproduces the normalised stored copy byte for byte.
type keyBlock struct {
	primary *packet.PublicKey
	keySigs sigSet
	uids    []*uidBlock
	subkeys []*subkeyBlock
}

type uidBlock struct {
	uid  *packet.UserId
	sigs sigSet
}

type subkeyBlock struct {
	pub  *packet.PublicKey
	sigs sigSet
	// binding: at least one verified binding signature. A subkey without one
	// is unreadable to go-crypto (the whole key fails to parse), so it is not
	// emitted.
	binding bool
}

// sigSet is an insertion-ordered, de-duplicated set of serialized signatures.
type sigSet struct {
	order [][]byte
	seen  map[string]bool
}

func (s *sigSet) add(b []byte) {
	if s.seen == nil {
		s.seen = map[string]bool{}
	}
	if s.seen[string(b)] {
		return
	}
	s.seen[string(b)] = true
	s.order = append(s.order, b)
}

func (s *sigSet) absorb(o sigSet) {
	for _, b := range o.order {
		s.add(b)
	}
}

// readKeyBlock parses the FIRST transferable public key in binary, verifying
// every signature against its primary key and dropping any that does not
// verify. A blob carrying secret key material is refused outright.
func readKeyBlock(binary []byte) (*keyBlock, error) {
	if len(binary) == 0 {
		return nil, ErrMalformed
	}
	if len(binary) > MaxKeyBytes {
		return nil, ErrTooLarge
	}
	r := packet.NewReader(bytes.NewReader(binary))
	kb := &keyBlock{}

	// Where the next signature attaches: the primary (before any user ID or
	// subkey), a user ID, a subkey, or nowhere (a user attribute or packet we
	// do not keep — its signatures go with it).
	const (
		atPrimary = iota
		atUID
		atSubkey
		atNothing
	)
	at := atPrimary
	var curUID *uidBlock
	var curSub *subkeyBlock

	for {
		p, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrMalformed, err)
		}
		switch pkt := p.(type) {
		case *packet.PrivateKey:
			return nil, ErrHasPrivateKey
		case *packet.PublicKey:
			if !pkt.IsSubkey {
				if kb.primary != nil {
					// A second primary key: the end of this key. Parse files a
					// blob under its first entity, and so does this.
					return kb.finish()
				}
				if !pkt.PubKeyAlgo.CanSign() {
					return nil, fmt.Errorf("%w: primary key cannot certify", ErrMalformed)
				}
				kb.primary = pkt
				at = atPrimary
				continue
			}
			if kb.primary == nil {
				return nil, ErrMalformed
			}
			curSub = kb.subkey(pkt)
			at = atSubkey
		case *packet.UserId:
			if kb.primary == nil {
				return nil, ErrMalformed
			}
			curUID = kb.userID(pkt)
			at = atUID
		case *packet.Signature:
			if kb.primary == nil {
				return nil, ErrMalformed
			}
			switch at {
			case atPrimary:
				kb.addKeySig(pkt)
			case atUID:
				kb.addUIDSig(curUID, pkt)
			case atSubkey:
				kb.addSubkeySig(curSub, pkt)
			}
		default:
			// User attributes (photo IDs), trust packets and anything unknown:
			// not kept, and neither are the signatures that follow them.
			if kb.primary == nil {
				return nil, ErrMalformed
			}
			at = atNothing
		}
	}
	return kb.finish()
}

func (kb *keyBlock) finish() (*keyBlock, error) {
	if kb.primary == nil {
		return nil, ErrMalformed
	}
	return kb, nil
}

func (kb *keyBlock) userID(u *packet.UserId) *uidBlock {
	for _, b := range kb.uids {
		if b.uid.Id == u.Id {
			return b
		}
	}
	b := &uidBlock{uid: u}
	kb.uids = append(kb.uids, b)
	return b
}

func (kb *keyBlock) subkey(pk *packet.PublicKey) *subkeyBlock {
	for _, b := range kb.subkeys {
		if bytes.Equal(b.pub.Fingerprint, pk.Fingerprint) {
			return b
		}
	}
	b := &subkeyBlock{pub: pk}
	kb.subkeys = append(kb.subkeys, b)
	return b
}

// bySelf reports whether sig claims to be made by the primary key. A claim only:
// the Verify* call that follows is what decides.
func (kb *keyBlock) bySelf(sig *packet.Signature) bool {
	return sig.CheckKeyIdOrFingerprint(kb.primary)
}

func (kb *keyBlock) addKeySig(sig *packet.Signature) {
	if !kb.bySelf(sig) {
		return
	}
	var err error
	switch sig.SigType {
	case packet.SigTypeKeyRevocation:
		err = kb.primary.VerifyRevocationSignature(sig)
	case packet.SigTypeDirectSignature:
		err = kb.primary.VerifyDirectKeySignature(sig)
	default:
		return
	}
	if err != nil {
		return
	}
	if b, ok := serializeSig(sig); ok {
		kb.keySigs.add(b)
	}
}

func (kb *keyBlock) addUIDSig(u *uidBlock, sig *packet.Signature) {
	switch sig.SigType {
	case packet.SigTypeGenericCert, packet.SigTypePersonaCert, packet.SigTypeCasualCert,
		packet.SigTypePositiveCert, packet.SigTypeCertificationRevocation:
	default:
		return
	}
	if !kb.bySelf(sig) {
		return // third-party certification: dropped (see Merge)
	}
	if err := kb.primary.VerifyUserIdSignature(u.uid.Id, kb.primary, sig); err != nil {
		return
	}
	if b, ok := serializeSig(sig); ok {
		u.sigs.add(b)
	}
}

func (kb *keyBlock) addSubkeySig(s *subkeyBlock, sig *packet.Signature) {
	if !kb.bySelf(sig) {
		return
	}
	switch sig.SigType {
	case packet.SigTypeSubkeyBinding:
		if err := kb.primary.VerifyKeySignature(s.pub, sig); err != nil {
			return
		}
		s.binding = true
	case packet.SigTypeSubkeyRevocation:
		if err := kb.primary.VerifySubkeyRevocationSignature(sig, s.pub); err != nil {
			return
		}
	default:
		return
	}
	if b, ok := serializeSig(sig); ok {
		s.sigs.add(b)
	}
}

func serializeSig(sig *packet.Signature) ([]byte, bool) {
	var buf bytes.Buffer
	if err := sig.Serialize(&buf); err != nil {
		return nil, false
	}
	return buf.Bytes(), true
}

// absorb adds o's user IDs, subkeys and signatures to kb. o has the same primary
// key (Merge checks the fingerprint first).
func (kb *keyBlock) absorb(o *keyBlock) {
	kb.keySigs.absorb(o.keySigs)
	for _, u := range o.uids {
		kb.userID(u.uid).sigs.absorb(u.sigs)
	}
	for _, s := range o.subkeys {
		dst := kb.subkey(s.pub)
		dst.sigs.absorb(s.sigs)
		dst.binding = dst.binding || s.binding
	}
}

// serialize writes the block as a transferable public key: primary, key-level
// signatures, user IDs with their signatures, subkeys with theirs. A user ID left
// with no verified signature, and a subkey with no verified binding, are omitted
// — go-crypto would refuse the whole key over either.
func (kb *keyBlock) serialize() ([]byte, error) {
	var buf bytes.Buffer
	if err := kb.primary.Serialize(&buf); err != nil {
		return nil, fmt.Errorf("pgpkeys: serialize primary: %w", err)
	}
	for _, b := range kb.keySigs.order {
		buf.Write(b)
	}
	for _, u := range kb.uids {
		if len(u.sigs.order) == 0 {
			continue
		}
		if err := u.uid.Serialize(&buf); err != nil {
			return nil, fmt.Errorf("pgpkeys: serialize user id: %w", err)
		}
		for _, b := range u.sigs.order {
			buf.Write(b)
		}
	}
	for _, s := range kb.subkeys {
		if !s.binding {
			continue
		}
		if err := s.pub.Serialize(&buf); err != nil {
			return nil, fmt.Errorf("pgpkeys: serialize subkey: %w", err)
		}
		for _, b := range s.sigs.order {
			buf.Write(b)
		}
	}
	return buf.Bytes(), nil
}
