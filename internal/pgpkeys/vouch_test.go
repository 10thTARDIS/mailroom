// SPDX-License-Identifier: AGPL-3.0-only

package pgpkeys

// ADR-054: the merge exception for a certification by another key held for the
// same contact, and the vouched-for rotation check. Every fixture is made by
// go-crypto's own key generation and certification (SignIdentity), never by
// hand-written packets.
//
// PROBES (run on the way in; each was red, then reverted):
//   - delete the VerifyUserIdSignature call in keepCertification: the
//     "does not verify" merge test goes red;
//   - delete the CheckKeyIdOrFingerprint match there: the "claims another
//     issuer" test goes red;
//   - delete the Assess(P).Revoked refusal in CheckVouch: the three
//     revocation refusals go red;
//   - delete the first-sighting bound (rule 5): the backdated test goes red.

import (
	"bytes"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

const testAddr = "a@example.com"

// uidFor is the user ID genKey gives a key made for email.
func uidFor(email string) string { return "Test <" + email + ">" }

// certify has signer certify subject's user ID uid, dated at.
func certify(t *testing.T, subject, signer *openpgp.Entity, uid string, at time.Time) {
	t.Helper()
	cfg := &packet.Config{Time: func() time.Time { return at }}
	if err := subject.SignIdentity(uid, signer, cfg); err != nil {
		t.Fatalf("SignIdentity: %v", err)
	}
}

// certsOver counts the third-party certifications (types 0x10 to 0x13) in
// binary, read from the raw packets so it does not depend on the code it checks.
func certsOver(t *testing.T, binary []byte) int {
	t.Helper()
	kb, err := readKeyBlock(binary)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	r := packet.NewReader(bytes.NewReader(binary))
	for {
		p, err := r.Next()
		if err != nil {
			return n
		}
		if s, ok := p.(*packet.Signature); ok &&
			s.SigType >= packet.SigTypeGenericCert && s.SigType <= packet.SigTypePositiveCert &&
			!s.CheckKeyIdOrFingerprint(kb.primary) {
			n++
		}
	}
}

// flipLast corrupts the last byte of sig's serialization inside binary (the
// tail of the signature value), leaving the packet parseable.
func flipLast(t *testing.T, binary []byte, e *openpgp.Entity, uid string) []byte {
	t.Helper()
	ident := e.Identities[uid]
	last := ident.Signatures[len(ident.Signatures)-1]
	var sb bytes.Buffer
	if err := last.Serialize(&sb); err != nil {
		t.Fatal(err)
	}
	i := bytes.Index(binary, sb.Bytes())
	if i < 0 {
		t.Fatal("certification not found in the serialized key")
	}
	out := append([]byte(nil), binary...)
	out[i+sb.Len()-1] ^= 0xff
	return out
}

// ---------------------------------------------------------------------------
// Merge: the one third-party certification that survives.
// ---------------------------------------------------------------------------

func mergeFixture(t *testing.T) (now time.Time, held, certified *openpgp.Entity, stored []byte) {
	now = time.Now().UTC().Truncate(time.Second)
	held = genKey(t, testAddr, now.Add(-100*day), 0)
	certified = genKey(t, testAddr, now.Add(-2*day), 0)
	stored = publicBytes(t, certified) // before any certification
	return now, held, certified, stored
}

// TestMergeDropsAStrangersCertification is the FIRST test of the drop that has
// been in Merge since ADR-040, and its positive control.
func TestMergeDropsAStrangersCertification(t *testing.T) {
	now, held, n, stored := mergeFixture(t)
	stranger := genKey(t, "s@example.com", now.Add(-50*day), 0)
	certify(t, n, stranger, uidFor(testAddr), now.Add(-day))
	withCert := publicBytes(t, n)
	if got := certsOver(t, withCert); got != 1 {
		t.Fatalf("fixture: %d third-party certifications, want 1", got)
	}

	// No certifiers: dropped, and the merge reports no change.
	merged, changed, err := Merge(stored, withCert)
	if err != nil || changed || certsOver(t, merged) != 0 {
		t.Errorf("no certifiers: changed=%v certs=%d err=%v; want dropped", changed, certsOver(t, merged), err)
	}
	// A certifier the stranger is not: still dropped.
	merged, changed, err = Merge(stored, withCert, publicBytes(t, held))
	if err != nil || changed || certsOver(t, merged) != 0 {
		t.Errorf("unrelated certifier: changed=%v certs=%d err=%v; want dropped", changed, certsOver(t, merged), err)
	}
	// POSITIVE CONTROL: the stranger IS held for the contact, and it survives.
	merged, changed, err = Merge(stored, withCert, publicBytes(t, stranger))
	if err != nil || !changed || certsOver(t, merged) != 1 {
		t.Errorf("held certifier: changed=%v certs=%d err=%v; want kept", changed, certsOver(t, merged), err)
	}
}

// A certification by a key held for a DIFFERENT contact is a stranger's from
// this contact's side: the store passes only this contact's keys, so the key is
// simply not among the certifiers.
func TestMergeDropsACertificationByAnotherContactsKey(t *testing.T) {
	now, held, n, stored := mergeFixture(t)
	other := genKey(t, "other@example.com", now.Add(-50*day), 0) // another contact's key
	certify(t, n, other, uidFor(testAddr), now.Add(-day))
	merged, _, err := Merge(stored, publicBytes(t, n), publicBytes(t, held)) // this contact holds `held` only
	if err != nil || certsOver(t, merged) != 0 {
		t.Errorf("other contact's certification: certs=%d err=%v; want dropped", certsOver(t, merged), err)
	}
}

// A certification that names a held key but does not verify is dropped; the
// positive control is the same certification unmodified.
func TestMergeDropsACertificationThatDoesNotVerify(t *testing.T) {
	now, held, n, stored := mergeFixture(t)
	certify(t, n, held, uidFor(testAddr), now.Add(-day))
	good := publicBytes(t, n)
	bad := flipLast(t, good, n, uidFor(testAddr))
	if certsOver(t, bad) != 1 {
		t.Fatal("fixture: the corrupted certification should still parse")
	}
	merged, _, err := Merge(stored, bad, publicBytes(t, held))
	if err != nil || certsOver(t, merged) != 0 {
		t.Errorf("unverifiable certification: certs=%d err=%v; want dropped", certsOver(t, merged), err)
	}
	merged, _, err = Merge(stored, good, publicBytes(t, held))
	if err != nil || certsOver(t, merged) != 1 {
		t.Errorf("control (verifying): certs=%d err=%v; want kept", certsOver(t, merged), err)
	}
}

// A genuine signature by a held certifier whose CLAIMED issuer is someone else is
// dropped: the claim is checked as well as the proof. The claim is altered in
// memory (a copy of a real packet), because go-crypto always writes the true
// issuer when it signs.
func TestKeepCertificationChecksTheClaimedIssuer(t *testing.T) {
	now, held, n, _ := mergeFixture(t)
	certify(t, n, held, uidFor(testAddr), now.Add(-day))
	kb, err := readKeyBlock(publicBytes(t, n))
	if err != nil {
		t.Fatal(err)
	}
	ident := n.Identities[uidFor(testAddr)]
	cert := ident.Signatures[len(ident.Signatures)-1]
	heldKB, _ := readKeyBlock(publicBytes(t, held))
	kb.certifiers = []*packet.PublicKey{heldKB.primary}
	u := &uidBlock{uid: ident.UserId}

	if !kb.keepCertification(u, cert) {
		t.Fatal("control: a genuine certification by a held certifier must be kept")
	}
	lying := *cert
	lying.IssuerFingerprint = bytes.Repeat([]byte{7}, 20)
	lying.IssuerKeyId = new(uint64)
	if kb.keepCertification(u, &lying) {
		t.Error("a certification claiming another issuer was kept although it verifies under a held key")
	}
}

// Only the certification types (and the certification revocation, which voids
// one) survive: any other third-party signature type is dropped.
func TestKeepCertificationDropsOtherTypes(t *testing.T) {
	now, held, n, _ := mergeFixture(t)
	certify(t, n, held, uidFor(testAddr), now.Add(-day))
	kb, _ := readKeyBlock(publicBytes(t, n))
	heldKB, _ := readKeyBlock(publicBytes(t, held))
	kb.certifiers = []*packet.PublicKey{heldKB.primary}
	ident := n.Identities[uidFor(testAddr)]
	cert := *ident.Signatures[len(ident.Signatures)-1]
	u := &uidBlock{uid: ident.UserId}
	if !kb.keepCertification(u, &cert) {
		t.Fatal("control failed")
	}
	for _, typ := range []packet.SignatureType{packet.SigTypeKeyRevocation, packet.SigTypeDirectSignature, packet.SigTypeSubkeyBinding} {
		cert.SigType = typ
		if kb.keepCertification(u, &cert) {
			t.Errorf("a third-party signature of type %#x was kept", typ)
		}
	}
}

// The INSERT path: a key stored with its certification and then re-sighted
// unchanged is not a change, and the certification is still there. Without the
// certifier the same re-sighting would strip it (and report a change).
func TestMergeOfAnUnchangedCertifiedCopyIsNoChange(t *testing.T) {
	now, held, n, _ := mergeFixture(t)
	certify(t, n, held, uidFor(testAddr), now.Add(-day))
	stored := publicBytes(t, n)
	merged, changed, err := Merge(stored, stored, publicBytes(t, held))
	if err != nil || changed || certsOver(t, merged) != 1 {
		t.Errorf("with the certifier: changed=%v certs=%d err=%v; want unchanged and kept", changed, certsOver(t, merged), err)
	}
	// Without it the certification is stripped from the merged result. (changed
	// stays false: both sides are normalised the same way, which is why every
	// route must pass the certifiers, or the stored copy is rewritten later.)
	merged, _, err = Merge(stored, stored)
	if err != nil || certsOver(t, merged) != 0 {
		t.Errorf("without it: certs=%d err=%v; want the certification stripped", certsOver(t, merged), err)
	}
	if len(merged) > MaxKeyBytes {
		t.Error("over MaxKeyBytes")
	}
}

// A user ID that carries only a kept certification and no self-signature is not
// emitted: go-crypto would reject the key, and it is not the holder's claim.
func TestMergeDoesNotEmitACertifiedUserIDWithoutASelfSignature(t *testing.T) {
	_, held, n, _ := mergeFixture(t)
	certify(t, n, held, uidFor(testAddr), n.PrimaryKey.CreationTime.Add(time.Hour))
	// Cut N's self-signature of the user ID: keep primary, uid, certification only.
	var buf bytes.Buffer
	if err := n.PrimaryKey.Serialize(&buf); err != nil {
		t.Fatal(err)
	}
	ident := n.Identities[uidFor(testAddr)]
	if err := ident.UserId.Serialize(&buf); err != nil {
		t.Fatal(err)
	}
	if err := ident.Signatures[len(ident.Signatures)-1].Serialize(&buf); err != nil {
		t.Fatal(err)
	}
	heldKB, _ := readKeyBlock(publicBytes(t, held))
	kb, err := readKeyBlock(buf.Bytes(), heldKB.primary)
	if err != nil {
		t.Fatal(err)
	}
	out, err := kb.serialize()
	if err != nil {
		t.Fatal(err)
	}
	r := packet.NewReader(bytes.NewReader(out))
	for {
		p, err := r.Next()
		if err != nil {
			break
		}
		if _, ok := p.(*packet.UserId); ok {
			t.Error("a user ID with a certification and no self-signature was emitted")
		}
	}
}

// ---------------------------------------------------------------------------
// CheckVouch.
// ---------------------------------------------------------------------------

// rot is one rotation under test; mutate a field, then build.
type rot struct {
	now         time.Time
	pCreated    time.Time
	pLife       time.Duration // 0: never expires
	nCreated    time.Time
	nLife       time.Duration
	nEmail      string // N's user ID address (default testAddr)
	certAt      time.Time
	certUID     string // "" = N's own user ID
	firstSeen   time.Time
	skipCert    bool
	address     string
	extraUID    func(*openpgp.Entity) // adds user IDs to N before certifying
	afterCertP  func(*openpgp.Entity) // mutates P after the certification
	afterCertN  func(*openpgp.Entity)
	corruptCert bool
}

func defaultRot(t *testing.T) rot {
	now := time.Now().UTC().Truncate(time.Second)
	return rot{
		now:       now,
		pCreated:  now.Add(-100 * day),
		pLife:     365 * day,
		nCreated:  now.Add(-3 * day),
		nLife:     730 * day,
		nEmail:    testAddr,
		certAt:    now.Add(-47 * time.Hour),
		firstSeen: now.Add(-time.Hour),
		address:   testAddr,
	}
}

func (r rot) build(t *testing.T) (p, n *openpgp.Entity, in VouchInput) {
	t.Helper()
	p = genKey(t, testAddr, r.pCreated, r.pLife)
	n = genKey(t, r.nEmail, r.nCreated, r.nLife)
	if r.extraUID != nil {
		r.extraUID(n)
	}
	uid := r.certUID
	if uid == "" {
		uid = uidFor(r.nEmail)
	}
	if !r.skipCert {
		certify(t, n, p, uid, r.certAt)
	}
	if r.afterCertP != nil {
		r.afterCertP(p)
	}
	if r.afterCertN != nil {
		r.afterCertN(n)
	}
	nb := publicBytes(t, n)
	if r.corruptCert {
		nb = flipLast(t, nb, n, uid)
	}
	return p, n, VouchInput{
		Previous: publicBytes(t, p), New: nb, Address: r.address,
		NewFirstSeen: r.firstSeen, Now: r.now,
	}
}

func TestCheckVouchRefusalsEachWithAPositiveControl(t *testing.T) {
	base := defaultRot(t)
	if v := func() Vouch { _, _, in := base.build(t); return CheckVouch(in) }(); !v.Vouched {
		t.Fatalf("POSITIVE CONTROL: the default rotation is not vouched (%s)", v.Refusal)
	}

	revokeAt := func(reason packet.ReasonForRevocation, at time.Time) func(*openpgp.Entity) {
		return func(p *openpgp.Entity) {
			cfg := &packet.Config{Time: func() time.Time { return at }}
			if err := p.RevokeKey(reason, "test", cfg); err != nil {
				t.Fatal(err)
			}
		}
	}
	cases := []struct {
		name   string
		mut    func(*rot)
		want   string
		compro bool
	}{
		{"not certified at all", func(r *rot) { r.skipCert = true }, VouchRefusedNoCertification, false},
		{"one flipped byte", func(r *rot) { r.corruptCert = true }, VouchRefusedNoCertification, false},
		{"certified a user ID that names another address", func(r *rot) {
			r.extraUID = func(n *openpgp.Entity) {
				if err := n.AddUserId("Other", "", "b@example.com", &packet.Config{Time: func() time.Time { return r.nCreated }}); err != nil {
					t.Fatal(err)
				}
			}
			r.certUID = "Other <b@example.com>"
		}, VouchRefusedNoCertification, false},
		{"certified a user ID with no address", func(r *rot) {
			r.extraUID = func(n *openpgp.Entity) {
				if err := n.AddUserId("just a name", "", "", &packet.Config{Time: func() time.Time { return r.nCreated }}); err != nil {
					t.Fatal(err)
				}
			}
			r.certUID = "just a name"
		}, VouchRefusedNoCertification, false},
		{"P revoked (superseded) AFTER the certification", func(r *rot) {
			r.afterCertP = revokeAt(packet.KeySuperseded, r.now.Add(-time.Hour))
		}, VouchRefusedPreviousRevoked, false},
		{"P revoked (superseded) BEFORE the certification", func(r *rot) {
			r.afterCertP = revokeAt(packet.KeySuperseded, r.certAt.Add(-time.Hour))
		}, VouchRefusedPreviousRevoked, false},
		{"P revoked as compromised", func(r *rot) {
			r.afterCertP = revokeAt(packet.KeyCompromised, r.now.Add(-time.Hour))
		}, VouchRefusedPreviousRevoked, true},
		{"dated before N existed", func(r *rot) { r.certAt = r.nCreated.Add(-time.Hour) }, VouchRefusedBeforeKeyExisted, false},
		{"N does not name the address", func(r *rot) { r.nEmail = "b@example.com" }, VouchRefusedNewDoesNotName, false},
		{"N expired", func(r *rot) {
			r.nCreated, r.nLife = r.now.Add(-10*day), 5*day
			r.certAt = r.nCreated.Add(time.Hour)
			r.firstSeen = r.nCreated.Add(2 * time.Hour)
		}, VouchRefusedNewUnusable, false},
		{"N revoked", func(r *rot) {
			r.afterCertN = func(n *openpgp.Entity) {
				if err := n.RevokeKey(packet.KeyRetired, "test", &packet.Config{Time: func() time.Time { return r.now.Add(-time.Minute) }}); err != nil {
					t.Fatal(err)
				}
			}
		}, VouchRefusedNewUnusable, false},
		{"dated two days in the future", func(r *rot) {
			r.certAt = r.now.Add(48 * time.Hour)
			r.nCreated = r.firstSeen.Add(-day)
		}, VouchRefusedClockSkew, false},
		{"first sighting unknown", func(r *rot) { r.firstSeen = time.Time{} }, VouchRefusedFirstSeenUnknown, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := defaultRot(t)
			c.mut(&r)
			_, _, in := r.build(t)
			v := CheckVouch(in)
			if v.Vouched || v.Refusal != c.want {
				t.Errorf("Vouched=%v Refusal=%q, want refused with %q", v.Vouched, v.Refusal, c.want)
			}
			if v.PreviousCompromised != c.compro {
				t.Errorf("PreviousCompromised=%v, want %v", v.PreviousCompromised, c.compro)
			}
		})
	}
}

// Positive controls for the near-misses the refusals sit next to.
func TestCheckVouchNearMissesAreVouched(t *testing.T) {
	t.Run("same address, another display name", func(t *testing.T) {
		r := defaultRot(t)
		r.extraUID = func(n *openpgp.Entity) {
			if err := n.AddUserId("A Different Name", "", testAddr, &packet.Config{Time: func() time.Time { return r.nCreated }}); err != nil {
				t.Fatal(err)
			}
		}
		r.certUID = "A Different Name <a@example.com>"
		_, _, in := r.build(t)
		if v := CheckVouch(in); !v.Vouched {
			t.Errorf("refused: %s", v.Refusal)
		}
	})
	t.Run("address case differs", func(t *testing.T) {
		r := defaultRot(t)
		r.address = "A@Example.COM"
		_, _, in := r.build(t)
		if v := CheckVouch(in); !v.Vouched {
			t.Errorf("refused: %s", v.Refusal)
		}
	})
	t.Run("one hour after first sighting", func(t *testing.T) {
		r := defaultRot(t)
		r.firstSeen = r.now.Add(-2 * time.Hour)
		r.nCreated = r.firstSeen.Add(-day)
		r.certAt = r.firstSeen.Add(time.Hour)
		_, _, in := r.build(t)
		if v := CheckVouch(in); !v.Vouched {
			t.Errorf("a certification one hour after first sighting was refused: %s", v.Refusal)
		}
	})
	// Rule 6 refuses only a genuinely FUTURE date. A GnuPG user who certifies
	// nine days after the key first arrived made an honest certification.
	t.Run("nine days after first sighting, in the past", func(t *testing.T) {
		r := defaultRot(t)
		r.firstSeen = r.now.Add(-10 * day)
		r.nCreated = r.firstSeen.Add(-day)
		r.certAt = r.firstSeen.Add(9 * day)
		_, _, in := r.build(t)
		if v := CheckVouch(in); !v.Vouched {
			t.Errorf("a late but past certification was refused: %s", v.Refusal)
		}
	})
	t.Run("a few hours in the future is tolerated", func(t *testing.T) {
		r := defaultRot(t)
		r.certAt = r.now.Add(3 * time.Hour)
		_, _, in := r.build(t)
		if v := CheckVouch(in); !v.Vouched {
			t.Errorf("a date within the skew allowance was refused: %s", v.Refusal)
		}
	})
	t.Run("P never expires", func(t *testing.T) {
		r := defaultRot(t)
		r.pLife = 0
		_, _, in := r.build(t)
		if v := CheckVouch(in); !v.Vouched {
			t.Errorf("refused: %s", v.Refusal)
		}
	})
}

// Dated after P's expiry (rule 4). The certification is made while P is valid
// and P's expiry is then moved earlier by a newer self-signature, which is how a
// key comes to expire "before" a certification it already made.
func TestCheckVouchRefusesACertificationDatedAfterTheKeyExpired(t *testing.T) {
	r := defaultRot(t)
	r.afterCertP = func(p *openpgp.Entity) {
		// Newer self-signature: P is now valid for 50 days from creation only.
		resign(t, p, r.pCreated.Add(day), 50*day, 50*day)
	}
	_, _, in := r.build(t)
	v := CheckVouch(in)
	if v.Vouched || v.Refusal != VouchRefusedAfterPreviousEnded {
		t.Errorf("Vouched=%v Refusal=%q, want datedAfterExpiry", v.Vouched, v.Refusal)
	}
}

// The first-sighting bound (rule 5): a thief holding an EXPIRED P dates a
// certification the day before P expired. What it cannot choose is when this
// server first saw N.
func TestCheckVouchBackdatedCertification(t *testing.T) {
	build := func(t *testing.T, firstSeenAfterExpiry time.Duration) Vouch {
		now := time.Now().UTC().Truncate(time.Second)
		pExpiry := now.Add(-365 * day) // expired a year ago
		r := defaultRot(t)
		r.pCreated, r.pLife = pExpiry.Add(-400*day), 400*day
		r.certAt = pExpiry.Add(-day)
		r.nCreated = r.certAt.Add(-time.Hour)
		r.nLife = 0
		r.firstSeen = pExpiry.Add(firstSeenAfterExpiry)
		r.now = r.firstSeen.Add(time.Hour)
		_, _, in := r.build(t)
		return CheckVouch(in)
	}
	// N first seen a year after the expiry: refused by rule 5, not by anything
	// the signature says (it is dated before the expiry and verifies).
	if v := build(t, 365*day); v.Vouched || v.Refusal != VouchRefusedSeenTooLate {
		t.Errorf("seen a year late: Vouched=%v Refusal=%q, want seenTooLate", v.Vouched, v.Refusal)
	}
	// POSITIVE CONTROL: the identical certification, N first seen 20 days after
	// the expiry (inside the 30-day window).
	if v := build(t, 20*day); !v.Vouched {
		t.Errorf("seen 20 days after expiry: refused (%s)", v.Refusal)
	}
	// And just past the window.
	if v := build(t, 31*day); v.Vouched || v.Refusal != VouchRefusedSeenTooLate {
		t.Errorf("seen 31 days after expiry: Vouched=%v Refusal=%q", v.Vouched, v.Refusal)
	}
}

func TestRevokedAsCompromised(t *testing.T) {
	now := time.Now().UTC()
	for _, c := range []struct {
		reason packet.ReasonForRevocation
		want   bool
	}{{packet.KeyCompromised, true}, {packet.KeySuperseded, false}, {packet.NoReason, false}} {
		e := genKey(t, testAddr, now.Add(-day), 0)
		if err := e.RevokeKey(c.reason, "x", &packet.Config{Time: func() time.Time { return now }}); err != nil {
			t.Fatal(err)
		}
		if got := RevokedAsCompromised(publicBytes(t, e)); got != c.want {
			t.Errorf("reason %d: %v, want %v", c.reason, got, c.want)
		}
	}
	if RevokedAsCompromised([]byte("junk")) {
		t.Error("junk reported as compromised")
	}
}
