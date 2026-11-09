// SPDX-License-Identifier: AGPL-3.0-only

package pgpkeys

// A vouched-for key rotation (#31, ADR-054 section 3): the key a correspondent
// used to have (the PIN, P) certified the user ID of the key they now sign with
// (N). This file decides that, from public keys alone, and decides it afresh on
// every call: nothing here is stored, so a revocation of P that arrives later
// withdraws the vouch by itself.
//
// ⚠ A VOUCH ONLY SOFTENS A PROMPT. Nothing that reads this result may move a pin;
// the user does, by deciding (ContactKey/decide). The operator decision behind
// that is ADR-054 section 4.

import (
	"bytes"
	"strings"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

// MaxVouchClockSkew is how far into the FUTURE (past now) a certification may be
// dated before it is refused as a clock the reader cannot account for (rule 6).
// A date in the past is not skew: a correspondent may certify long after N was
// first seen.
const MaxVouchClockSkew = 24 * time.Hour

// The reasons a vouch is refused (Vouch.Refusal), one per rule of ADR-054
// section 3. They are for tests and logs; the wire only carries the outcome.
const (
	VouchRefusedPreviousUnreadable = "previousUnreadable"
	VouchRefusedPreviousRevoked    = "previousRevoked"      // rule 3
	VouchRefusedNewUnusable        = "newKeyUnusable"       // rule 7: unreadable, revoked, expired
	VouchRefusedNewDoesNotName     = "newKeyDoesNotName"    // rule 7
	VouchRefusedNoCertification    = "noCertification"      // rule 2
	VouchRefusedBeforeKeyExisted   = "datedBeforeKey"       // rule 4
	VouchRefusedAfterPreviousEnded = "datedAfterExpiry"     // rule 4
	VouchRefusedCertExpired        = "certificationExpired" // rule 4
	VouchRefusedSeenTooLate        = "seenTooLate"          // rule 5
	VouchRefusedFirstSeenUnknown   = "firstSeenUnknown"     // rule 5
	VouchRefusedClockSkew          = "clockSkew"            // rule 6
	VouchRefusedCertRevoked        = "certificationRevoked" // P revoked the certification (0x30)
)

// VouchInput is what CheckVouch judges.
type VouchInput struct {
	// Previous is the PINNED key P and New the key N, both binary public keys.
	// The caller has already established rule 1 (P is the pin for Address and is
	// held) and that the message signature verified under N.
	Previous, New []byte
	// Address is the address the prompt is about.
	Address string
	// NewFirstSeen is when this server first stored N (its first_seen event). The
	// zero value means unknown, which refuses (see VouchRefusedFirstSeenUnknown).
	NewFirstSeen time.Time
	Now          time.Time
}

// Vouch is the outcome.
type Vouch struct {
	// Vouched: every rule held. Refusal says which one failed otherwise.
	Vouched bool
	Refusal string
	// PreviousCompromised: P carries a key revocation whose reason is
	// "compromised". Independent of N and of Vouched: it hardens the ordinary
	// prompt (ADR-054 section 3, table row 3).
	PreviousCompromised bool
}

// CheckVouch applies ADR-054 section 3 rules 2 to 7 (rule 1, "P is the pin", and
// the signature check belong to the caller, who holds the pin and the message).
// The first failing rule ends the check.
func CheckVouch(in VouchInput) Vouch {
	var out Vouch
	prev, err := readEntity(in.Previous)
	if err != nil {
		out.Refusal = VouchRefusedPreviousUnreadable
		return out
	}
	out.PreviousCompromised = revokedAsCompromised(prev)

	// Rule 3: P is not revoked, at all.
	pv := assessEntity(prev, in.Now)
	if pv.Revoked {
		out.Refusal = VouchRefusedPreviousRevoked
		return out
	}

	// Rule 7: N passes every existing check for the address unaided.
	next, err := readEntity(in.New)
	if err != nil {
		out.Refusal = VouchRefusedNewUnusable
		return out
	}
	nv := assessEntity(next, in.Now)
	if !nv.CanSign(in.Now) || nv.Expired(in.Now) {
		out.Refusal = VouchRefusedNewUnusable
		return out
	}
	if !entityNamesAddress(next, in.Address, in.Now) {
		out.Refusal = VouchRefusedNewDoesNotName
		return out
	}

	// Rule 2: a certification by P over a self-signed user ID of N that names the
	// address, verifying under P's public key now. Any of the certifications
	// found may be the one; the best refusal is reported when none passes.
	certs, revoked := certificationsBy(in.New, next.PrimaryKey, prev.PrimaryKey, in.Address)
	if len(certs) == 0 {
		out.Refusal = VouchRefusedNoCertification
		if revoked {
			out.Refusal = VouchRefusedCertRevoked
		}
		return out
	}
	refusal := ""
	for _, sig := range certs {
		if r := certificationRefusal(sig, pv, nv, in); r == "" {
			out.Vouched = true
			return out
		} else if refusal == "" {
			refusal = r
		}
	}
	out.Refusal = refusal
	return out
}

// certificationRefusal applies rules 4 to 6 to one certification that already
// verified. "" means it is acceptable.
func certificationRefusal(sig *packet.Signature, pv, nv Validity, in VouchInput) string {
	made := sig.CreationTime
	// Rule 4: made while P was valid and N existed.
	if made.Before(pv.Created) {
		return VouchRefusedBeforeKeyExisted
	}
	if pv.PrimaryExpires != nil && !made.Before(*pv.PrimaryExpires) {
		return VouchRefusedAfterPreviousEnded
	}
	if made.Before(nv.Created) {
		return VouchRefusedBeforeKeyExisted
	}
	// The certification's own expiry (a FUTURE creation time is rule 6's business,
	// not an expiry, so this is not packet.SigExpired).
	if sig.SigLifetimeSecs != nil && *sig.SigLifetimeSecs != 0 &&
		in.Now.After(made.Add(time.Duration(*sig.SigLifetimeSecs)*time.Second)) {
		return VouchRefusedCertExpired
	}
	if in.NewFirstSeen.IsZero() {
		return VouchRefusedFirstSeenUnknown
	}
	// Rule 5: N was seen in time. The one defence against backdating that does
	// not rest on the signer's clock.
	if pv.PrimaryExpires != nil && in.NewFirstSeen.After(pv.PrimaryExpires.Add(ExpiryWarningWindow)) {
		return VouchRefusedSeenTooLate
	}
	// Rule 6: a certification dated in the future is a clock we cannot account
	// for. A date after N was first seen but already past is an honest late
	// certification (a GnuPG user who certifies later), and vouches.
	if made.After(in.Now.Add(MaxVouchClockSkew)) {
		return VouchRefusedClockSkew
	}
	return ""
}

// certificationsBy returns the certifications over a self-signed user ID of N
// that names address, issued by P and VERIFYING under P's public key, with a
// strong hash, and not voided by a later certification revocation (0x30) by P
// over the same user ID (made at or after the certification: a tie fails toward
// asking). revoked reports that at least one certification verified but was
// voided. It reads N's own packets, not the merge's normalised copy, so it never
// depends on what was stored by an earlier version.
func certificationsBy(newBinary []byte, newPrimary, prevPrimary *packet.PublicKey, address string) (out []*packet.Signature, revoked bool) {
	kb, err := readKeyBlock(newBinary)
	if err != nil {
		return nil, false
	}
	var certsAll []*packet.Signature
	certUID := map[*packet.Signature]string{}
	newestRev := map[string]time.Time{}
	selfSigned := map[string]bool{}
	for _, u := range kb.uids {
		if u.self {
			selfSigned[u.uid.Id] = true
		}
	}
	want := strings.ToLower(strings.TrimSpace(address))
	r := packet.NewReader(bytes.NewReader(newBinary))
	var uid string
	inUID := false
	for {
		p, err := r.Next()
		if err != nil {
			break
		}
		switch pkt := p.(type) {
		case *packet.PublicKey:
			if pkt.IsSubkey {
				inUID = false
			}
		case *packet.UserId:
			uid, inUID = pkt.Id, true
		case *packet.Signature:
			if !inUID || !selfSigned[uid] || strings.ToLower(userIDAddress(uid)) != want {
				continue
			}
			isRev := false
			switch pkt.SigType {
			case packet.SigTypeGenericCert, packet.SigTypePersonaCert,
				packet.SigTypeCasualCert, packet.SigTypePositiveCert:
			case packet.SigTypeCertificationRevocation:
				isRev = true
			default:
				continue
			}
			if WeakHash(pkt.Hash) || !pkt.CheckKeyIdOrFingerprint(prevPrimary) {
				continue
			}
			if prevPrimary.VerifyUserIdSignature(uid, newPrimary, pkt) != nil {
				continue
			}
			if isRev {
				if pkt.CreationTime.After(newestRev[uid]) {
					newestRev[uid] = pkt.CreationTime
				}
				continue
			}
			certsAll = append(certsAll, pkt)
			certUID[pkt] = uid
		default:
			inUID = false
		}
	}
	for _, c := range certsAll {
		if rv, ok := newestRev[certUID[c]]; ok && !c.CreationTime.After(rv) {
			revoked = true
			continue
		}
		out = append(out, c)
	}
	return out, revoked
}

// revokedAsCompromised: a verified key revocation on e whose reason is "key
// compromised". go-crypto verifies a key revocation before it keeps it.
func revokedAsCompromised(e *openpgp.Entity) bool {
	for _, rev := range e.Revocations {
		if rev != nil && rev.RevocationReason != nil && *rev.RevocationReason == packet.KeyCompromised {
			return true
		}
	}
	return false
}

// RevokedAsCompromised reports whether binary carries a (verified) key
// revocation for reason "compromised" (ADR-054: the old key a contact reported
// stolen). False for an unreadable key.
func RevokedAsCompromised(binary []byte) bool {
	e, err := readEntity(binary)
	return err == nil && revokedAsCompromised(e)
}

func readEntity(binary []byte) (*openpgp.Entity, error) {
	if _, err := Parse(binary); err != nil {
		return nil, err
	}
	entities, err := openpgp.ReadKeyRing(bytes.NewReader(binary))
	if err != nil || len(entities) == 0 {
		return nil, ErrMalformed
	}
	return entities[0], nil
}

// CertifiedBy reports whether newBinary carries a certification, verifying under
// certifierBinary's public key, over a self-signed user ID naming address. It is
// the artefact check behind key replacement (ADR-054 section 1): the crypto
// process re-reads the bytes it is about to store and asks this.
func CertifiedBy(newBinary, certifierBinary []byte, address string) bool {
	n, err := readEntity(newBinary)
	if err != nil {
		return false
	}
	c, err := readEntity(certifierBinary)
	if err != nil {
		return false
	}
	certs, _ := certificationsBy(newBinary, n.PrimaryKey, c.PrimaryKey, address)
	return len(certs) > 0
}
