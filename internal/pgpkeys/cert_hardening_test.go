// SPDX-License-Identifier: AGPL-3.0-only

package pgpkeys

// Hardening of the third-party certification exception (ADR-054 section 2, from
// the security review of #31): at most the newest certification per certifier and
// user ID is kept, a weak-hash certification is never accepted, and a
// certification revocation by the voucher voids the vouch. Every signature is made
// by go-crypto, including a genuine SHA-1 one.
//
// PROBES (each was red, then reverted):
//   - make uidBlock.offer append instead of keeping the newest: the "300
//     certifications" tests go red;
//   - delete the WeakHash check in keepCertification: the merge weak-hash test
//     goes red; in certificationsBy: the CheckVouch weak-hash test goes red;
//   - delete the newestRev filter in certificationsBy: the revocation tests go red.

import (
	"crypto"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

// certifyHash is certify with a chosen digest. go-crypto refuses to randomise a
// SHA-1 signature through a notation, so that is turned off.
func certifyHash(t *testing.T, subject, signer *openpgp.Entity, uid string, at time.Time, h crypto.Hash) {
	t.Helper()
	off := false
	cfg := &packet.Config{DefaultHash: h, NonDeterministicSignaturesViaNotation: &off, Time: func() time.Time { return at }}
	if err := subject.SignIdentity(uid, signer, cfg); err != nil {
		t.Fatalf("SignIdentity(%v): %v", h, err)
	}
}

// revokeCertification has signer revoke its certification of subject's uid, dated
// at: a genuine 0x30 over the same user ID.
func revokeCertification(t *testing.T, subject, signer *openpgp.Entity, uid string, at time.Time) {
	t.Helper()
	cfg := &packet.Config{Time: func() time.Time { return at }}
	sig := &packet.Signature{
		SigType: packet.SigTypeCertificationRevocation, PubKeyAlgo: signer.PrimaryKey.PubKeyAlgo,
		Hash: crypto.SHA256, CreationTime: at, IssuerKeyId: &signer.PrimaryKey.KeyId,
	}
	if err := sig.SignUserId(uid, subject.PrimaryKey, signer.PrivateKey, cfg); err != nil {
		t.Fatalf("revoke certification: %v", err)
	}
	ident := subject.Identities[uid]
	ident.Signatures = append(ident.Signatures, sig)
}

// ---- at most the newest per (certifier, user ID) ----------------------------------

func TestMergeKeepsOnlyTheNewestCertificationPerCertifierAndUserID(t *testing.T) {
	now, held, n, stored := mergeFixture(t)
	const many = 300
	base := now.Add(-day)
	for i := 0; i < many; i++ {
		certify(t, n, held, uidFor(testAddr), base.Add(time.Duration(i)*time.Second))
	}
	bloated := publicBytes(t, n)
	if got := certsOver(t, bloated); got != many {
		t.Fatalf("fixture: %d certifications, want %d", got, many)
	}

	merged, changed, err := Merge(stored, bloated, publicBytes(t, held))
	if err != nil {
		t.Fatal(err)
	}
	if !changed || certsOver(t, merged) != 1 {
		t.Fatalf("changed=%v certs=%d; want exactly one kept", changed, certsOver(t, merged))
	}
	// Bounded: the key plus one certification, whatever arrived.
	if len(merged) > len(stored)+400 {
		t.Errorf("merged is %d bytes over a %d-byte key: the kept bytes are not bounded", len(merged), len(stored))
	}
	// The one kept is the newest.
	kb, _ := readKeyBlock(merged, mustPrimary(t, held))
	var newest time.Time
	for _, u := range kb.uids {
		for _, th := range u.third {
			newest = th.created
		}
	}
	if want := base.Add((many - 1) * time.Second); !newest.Equal(want) {
		t.Errorf("kept the certification dated %v, want the newest, %v", newest, want)
	}

	// An already-bloated STORED copy is pruned by the next merge, and that is not
	// reported as a change (both sides are normalised alike).
	again, changed, err := Merge(merged, bloated, publicBytes(t, held))
	if err != nil || changed || certsOver(t, again) != 1 {
		t.Errorf("re-merge: changed=%v certs=%d err=%v", changed, certsOver(t, again), err)
	}
	pruned, _, err := Merge(bloated, bloated, publicBytes(t, held))
	if err != nil || certsOver(t, pruned) != 1 {
		t.Errorf("a bloated stored copy kept %d certifications (err %v)", certsOver(t, pruned), err)
	}
}

// Two certifiers each keep their own newest: the bound is per certifier, not a
// global cap that one could use up.
func TestMergeKeepsOneCertificationPerCertifier(t *testing.T) {
	now, held, n, stored := mergeFixture(t)
	other := genKey(t, testAddr, now.Add(-50*day), 0)
	for i := 0; i < 5; i++ {
		certify(t, n, held, uidFor(testAddr), now.Add(-day+time.Duration(i)*time.Second))
		certify(t, n, other, uidFor(testAddr), now.Add(-day+time.Duration(i)*time.Second))
	}
	merged, _, err := Merge(stored, publicBytes(t, n), publicBytes(t, held), publicBytes(t, other))
	if err != nil || certsOver(t, merged) != 2 {
		t.Errorf("certs=%d err=%v; want one per certifier (2)", certsOver(t, merged), err)
	}
}

// An honest renewal still merges after a bloat attempt by the certifier: the
// stored copy stays small, so the renewal fits.
func TestMergeStillTakesARenewalAfterABloatAttempt(t *testing.T) {
	now, held, n, stored := mergeFixture(t)
	for i := 0; i < 300; i++ {
		certify(t, n, held, uidFor(testAddr), now.Add(-day+time.Duration(i)*time.Second))
	}
	merged, _, err := Merge(stored, publicBytes(t, n), publicBytes(t, held))
	if err != nil {
		t.Fatal(err)
	}
	renewed := genRenewed(t, n, now)
	got, changed, err := Merge(merged, publicBytes(t, renewed), publicBytes(t, held))
	if err != nil || !changed {
		t.Fatalf("renewal after a bloat attempt: changed=%v err=%v", changed, err)
	}
	if certsOver(t, got) != 1 {
		t.Errorf("certs=%d after the renewal, want 1", certsOver(t, got))
	}
}

func genRenewed(t *testing.T, e *openpgp.Entity, at time.Time) *openpgp.Entity {
	t.Helper()
	resign(t, e, at, 900*day, 900*day)
	return e
}

func mustPrimary(t *testing.T, e *openpgp.Entity) *packet.PublicKey {
	t.Helper()
	kb, err := readKeyBlock(publicBytes(t, e))
	if err != nil {
		t.Fatal(err)
	}
	return kb.primary
}

// ---- weak hashes ----------------------------------------------------------------------

func TestWeakHash(t *testing.T) {
	for _, h := range []crypto.Hash{crypto.MD5, crypto.SHA1, crypto.RIPEMD160} {
		if !WeakHash(h) {
			t.Errorf("%v is not weak", h)
		}
	}
	for _, h := range []crypto.Hash{crypto.SHA256, crypto.SHA384, crypto.SHA512, crypto.SHA3_256} {
		if WeakHash(h) {
			t.Errorf("%v is weak", h)
		}
	}
}

// A genuine SHA-1 certification, which verifies under the certifier, is refused by
// the merge; the SHA-256 control is kept. Without the hash check it would be kept,
// so this goes red when the check is removed.
func TestMergeRefusesAWeakHashCertification(t *testing.T) {
	now, held, n, stored := mergeFixture(t)
	certifyHash(t, n, held, uidFor(testAddr), now.Add(-day), crypto.SHA1)
	ident := n.Identities[uidFor(testAddr)]
	last := ident.Signatures[len(ident.Signatures)-1]
	if last.Hash != crypto.SHA1 {
		t.Fatalf("fixture hash = %v, want SHA-1", last.Hash)
	}
	if err := held.PrimaryKey.VerifyUserIdSignature(uidFor(testAddr), n.PrimaryKey, last); err != nil {
		t.Fatalf("fixture: the SHA-1 certification must verify, or the test proves nothing: %v", err)
	}
	merged, _, err := Merge(stored, publicBytes(t, n), publicBytes(t, held))
	if err != nil || certsOver(t, merged) != 0 {
		t.Errorf("SHA-1 certification: certs=%d err=%v; want dropped", certsOver(t, merged), err)
	}
	// CONTROL: the same certification with SHA-256 is kept.
	_, held2, n2, stored2 := mergeFixture(t)
	certifyHash(t, n2, held2, uidFor(testAddr), now.Add(-day), crypto.SHA256)
	merged, _, err = Merge(stored2, publicBytes(t, n2), publicBytes(t, held2))
	if err != nil || certsOver(t, merged) != 1 {
		t.Errorf("control (SHA-256): certs=%d err=%v; want kept", certsOver(t, merged), err)
	}
}

func TestCheckVouchRefusesAWeakHashCertification(t *testing.T) {
	r := defaultRot(t)
	build := func(h crypto.Hash) Vouch {
		p := genKey(t, testAddr, r.pCreated, r.pLife)
		n := genKey(t, testAddr, r.nCreated, r.nLife)
		certifyHash(t, n, p, uidFor(testAddr), r.certAt, h)
		return CheckVouch(VouchInput{Previous: publicBytes(t, p), New: publicBytes(t, n),
			Address: testAddr, NewFirstSeen: r.firstSeen, Now: r.now})
	}
	if v := build(crypto.SHA1); v.Vouched || v.Refusal != VouchRefusedNoCertification {
		t.Errorf("SHA-1: Vouched=%v Refusal=%q, want noCertification", v.Vouched, v.Refusal)
	}
	if v := build(crypto.SHA256); !v.Vouched { // control
		t.Errorf("control (SHA-256) refused: %s", v.Refusal)
	}
}

// ---- a certification revocation by the voucher ---------------------------------------

func TestMergeKeepsTheVouchersCertificationRevocation(t *testing.T) {
	now, held, n, stored := mergeFixture(t)
	certify(t, n, held, uidFor(testAddr), now.Add(-2*day))
	revokeCertification(t, n, held, uidFor(testAddr), now.Add(-day))
	merged, _, err := Merge(stored, publicBytes(t, n), publicBytes(t, held))
	if err != nil {
		t.Fatal(err)
	}
	kb, _ := readKeyBlock(merged, mustPrimary(t, held))
	if len(kb.uids) == 0 || len(kb.uids[0].third) != 2 {
		t.Fatalf("kept %d third-party signatures, want the certification and its revocation", len(kb.uids[0].third))
	}
	// Not kept when it is not the certifier's.
	stranger := genKey(t, "s@example.com", now.Add(-50*day), 0)
	revokeCertification(t, n, stranger, uidFor(testAddr), now.Add(-day))
	merged, _, err = Merge(stored, publicBytes(t, n), publicBytes(t, held))
	kb, _ = readKeyBlock(merged, mustPrimary(t, held))
	if err != nil || len(kb.uids[0].third) != 2 {
		t.Errorf("a stranger's revocation changed what was kept: %d (err %v)", len(kb.uids[0].third), err)
	}
}

func TestCheckVouchVoidedByACertificationRevocation(t *testing.T) {
	r := defaultRot(t)
	build := func(revokeAt *time.Time, recertifyAt *time.Time) Vouch {
		p := genKey(t, testAddr, r.pCreated, r.pLife)
		n := genKey(t, testAddr, r.nCreated, r.nLife)
		certify(t, n, p, uidFor(testAddr), r.certAt)
		if revokeAt != nil {
			revokeCertification(t, n, p, uidFor(testAddr), *revokeAt)
		}
		if recertifyAt != nil {
			certify(t, n, p, uidFor(testAddr), *recertifyAt)
		}
		return CheckVouch(VouchInput{Previous: publicBytes(t, p), New: publicBytes(t, n),
			Address: testAddr, NewFirstSeen: r.firstSeen, Now: r.now})
	}
	if v := build(nil, nil); !v.Vouched { // CONTROL
		t.Fatalf("control: no revocation, refused: %s", v.Refusal)
	}
	after := r.certAt.Add(time.Hour)
	if v := build(&after, nil); v.Vouched || v.Refusal != VouchRefusedCertRevoked {
		t.Errorf("revoked after: Vouched=%v Refusal=%q, want certificationRevoked", v.Vouched, v.Refusal)
	}
	// A revocation dated BEFORE the certification does not void it.
	before := r.certAt.Add(-time.Hour)
	if v := build(&before, nil); !v.Vouched {
		t.Errorf("a revocation older than the certification voided it: %s", v.Refusal)
	}
	// Certified again after revoking: the newest certification stands.
	again := after.Add(time.Hour)
	if v := build(&after, &again); !v.Vouched {
		t.Errorf("re-certified after the revocation, refused: %s", v.Refusal)
	}
}
