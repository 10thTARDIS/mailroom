// SPDX-License-Identifier: AGPL-3.0-only

package pgpkeys

// Merge-on-renewal (#795, ADR-040). Every fixture here is produced by
// go-crypto's own key generation and signing — never hand-written packets — so
// what is tested is the format real producers emit, not a belief about it.
//
// Negative controls (each was run by hand on the way in; the PR records the
// result): removing the union in keyBlock.absorb turns the renewal test red;
// replacing Merge's result with the incoming copy turns the replay and
// revocation tests red; deleting the VerifyUserIdSignature/VerifyKeySignature
// checks turns the forged-signature test red.

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

const day = 24 * time.Hour

// genKey makes an Ed25519/X25519 key (production's algorithm, pgpkeyd.Generate)
// created at `created`, whose primary key and encryption subkey expire after
// lifetime (0 = never).
func genKey(t *testing.T, email string, created time.Time, lifetime time.Duration) *openpgp.Entity {
	t.Helper()
	cfg := &packet.Config{
		Algorithm:       packet.PubKeyAlgoEdDSA,
		Curve:           packet.Curve25519,
		Time:            func() time.Time { return created },
		KeyLifetimeSecs: uint32(lifetime / time.Second),
	}
	e, err := openpgp.NewEntity("Test", "", email, cfg)
	if err != nil {
		t.Fatalf("NewEntity: %v", err)
	}
	return e
}

// resign gives every identity and every subkey of e a NEW self-signature made
// at `at`, with key lifetime `lifetime` measured from each key's creation (0 =
// never) — what `gpg --quick-set-expire` and pgpkeyd.Extend produce. It mutates
// e; serialize before calling it to keep the old copy.
func resign(t *testing.T, e *openpgp.Entity, at time.Time, primaryLife, subLife time.Duration) {
	t.Helper()
	cfg := &packet.Config{Time: func() time.Time { return at }}
	for name, ident := range e.Identities {
		cp := *ident.SelfSignature
		cp.Notations = append([]*packet.Notation(nil), ident.SelfSignature.Notations...)
		cp.CreationTime = at
		l := uint32(primaryLife / time.Second)
		cp.KeyLifetimeSecs = &l
		cp.SigLifetimeSecs = nil
		if err := cp.SignUserId(name, e.PrimaryKey, e.PrivateKey, cfg); err != nil {
			t.Fatalf("SignUserId: %v", err)
		}
		ident.SelfSignature = &cp
		ident.Signatures = append(ident.Signatures, &cp)
	}
	for i := range e.Subkeys {
		sub := &e.Subkeys[i]
		cp := *sub.Sig
		cp.Notations = append([]*packet.Notation(nil), sub.Sig.Notations...)
		cp.CreationTime = at
		l := uint32(subLife / time.Second)
		cp.KeyLifetimeSecs = &l
		cp.SigLifetimeSecs = nil
		if err := cp.SignKey(sub.PublicKey, e.PrivateKey, cfg); err != nil {
			t.Fatalf("SignKey: %v", err)
		}
		sub.Sig = &cp
	}
}

func until(t *testing.T, binary []byte, now time.Time) (time.Time, bool) {
	t.Helper()
	v, err := Assess(binary, now)
	if err != nil {
		t.Fatalf("Assess: %v", err)
	}
	u, never := v.UsableUntil()
	return u, never
}

func sameSecond(a, b time.Time) bool { return a.Truncate(time.Second).Equal(b.Truncate(time.Second)) }

// renewalFixture: a key created 300 days ago with a one-year lifetime (so it
// stops working in 65 days), and the same key renewed an hour ago to three
// years from creation.
func renewalFixture(t *testing.T) (now, created time.Time, old, renewed []byte) {
	t.Helper()
	now = time.Now().UTC().Truncate(time.Second)
	created = now.Add(-300 * day)
	e := genKey(t, "alice@example.com", created, 365*day)
	old = publicBytes(t, e)
	resign(t, e, now.Add(-time.Hour), 3*365*day, 3*365*day)
	renewed = publicBytes(t, e)
	return now, created, old, renewed
}

func TestMergeRenewalExtendsUsability(t *testing.T) {
	now, created, old, renewed := renewalFixture(t)

	if u, _ := until(t, old, now); !sameSecond(u, created.Add(365*day)) {
		t.Fatalf("fixture: old copy usable until %v, want %v", u, created.Add(365*day))
	}
	merged, changed, err := Merge(old, renewed)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if !changed {
		t.Error("a renewal was reported as no change, so it would never be written")
	}
	u, never := until(t, merged, now)
	if never || !sameSecond(u, created.Add(3*365*day)) {
		t.Errorf("merged copy usable until %v (never=%v), want the renewal's %v", u, never, created.Add(3*365*day))
	}
	if fpr, err := EncryptionFingerprint(merged, created.Add(400*day)); err != nil || fpr == "" {
		t.Errorf("merged copy cannot be encrypted to after the OLD expiry: %v", err)
	}
	// Same primary key: the fingerprint, and so the pin, is unchanged.
	a, _ := Parse(old)
	b, _ := Parse(merged)
	if a.Fingerprint != b.Fingerprint {
		t.Errorf("fingerprint changed across a merge: %s -> %s", a.Fingerprint, b.Fingerprint)
	}
}

func TestMergeReplayOfOlderCopyDoesNotShorten(t *testing.T) {
	now, created, old, renewed := renewalFixture(t)
	merged, _, err := Merge(old, renewed)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}

	// The stored copy is the merged one; a stale Autocrypt header replays the old.
	again, changed, err := Merge(merged, old)
	if err != nil {
		t.Fatalf("Merge replay: %v", err)
	}
	if changed {
		t.Error("replaying an older copy was reported as a change")
	}
	if u, never := until(t, again, now); never || !sameSecond(u, created.Add(3*365*day)) {
		t.Errorf("after replay usable until %v, want %v", u, created.Add(3*365*day))
	}

	// And with the renewal stored directly (WKD fetched it first), the old copy
	// arriving afterwards does not shorten it either.
	fromRenewed, _, err := Merge(renewed, old)
	if err != nil {
		t.Fatalf("Merge renewed<-old: %v", err)
	}
	if u, never := until(t, fromRenewed, now); never || !sameSecond(u, created.Add(3*365*day)) {
		t.Errorf("renewed copy shortened by an older one: usable until %v", u)
	}
}

func TestMergeRevocationSurvivesReplay(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	e := genKey(t, "bob@example.com", now.Add(-10*day), 0)
	before := publicBytes(t, e)
	if err := e.RevokeKey(packet.KeyCompromised, "lost laptop", &packet.Config{Time: func() time.Time { return now.Add(-time.Hour) }}); err != nil {
		t.Fatalf("RevokeKey: %v", err)
	}
	revoked := publicBytes(t, e)

	// The revocation arrives on top of the stored pre-revocation copy.
	merged, changed, err := Merge(before, revoked)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if !changed {
		t.Error("a revocation was reported as no change")
	}
	if v, _ := Assess(merged, now); !v.Revoked {
		t.Fatal("merged copy is not revoked")
	}

	// Then the pre-revocation copy is replayed.
	replayed, changed, err := Merge(merged, before)
	if err != nil {
		t.Fatalf("Merge replay: %v", err)
	}
	if changed {
		t.Error("replaying the pre-revocation copy was reported as a change")
	}
	v, _ := Assess(replayed, now)
	if !v.Revoked || v.Usable(now) {
		t.Error("replaying the pre-revocation copy un-revoked the key")
	}
	if _, err := EncryptionFingerprint(replayed, now); !errors.Is(err, ErrNoEncryptionKey) {
		t.Errorf("a revoked key can still be encrypted to after a replay: %v", err)
	}
}

// forgeExtension makes a copy of e whose identity and subkey self-signatures
// CLAIM a ten-year lifetime but do not verify: each claim is signed for real,
// then given the signature value of a different (one-day) signature, so the
// claimed hashed data and the signature disagree — what an attacker without the
// private key can produce.
func forgeExtension(t *testing.T, e *openpgp.Entity, at time.Time) []byte {
	t.Helper()
	cfg := &packet.Config{Time: func() time.Time { return at }}
	sign := func(base *packet.Signature, life time.Duration, signFn func(*packet.Signature) error) *packet.Signature {
		cp := *base
		cp.Notations = append([]*packet.Notation(nil), base.Notations...)
		cp.CreationTime = at
		l := uint32(life / time.Second)
		cp.KeyLifetimeSecs = &l
		cp.SigLifetimeSecs = nil
		if err := signFn(&cp); err != nil {
			t.Fatalf("sign: %v", err)
		}
		return &cp
	}
	for name, ident := range e.Identities {
		claim := sign(ident.SelfSignature, 10*365*day, func(s *packet.Signature) error {
			return s.SignUserId(name, e.PrimaryKey, e.PrivateKey, cfg)
		})
		other := sign(ident.SelfSignature, day, func(s *packet.Signature) error {
			return s.SignUserId(name, e.PrimaryKey, e.PrivateKey, cfg)
		})
		claim.EdDSASigR, claim.EdDSASigS = other.EdDSASigR, other.EdDSASigS
		if e.PrimaryKey.VerifyUserIdSignature(name, e.PrimaryKey, claim) == nil {
			t.Fatal("fixture: the forged identity signature verifies")
		}
		ident.Signatures = append(ident.Signatures, claim)
	}
	// Each subkey is emitted a second time carrying the forged binding, after
	// its genuine one: the copy otherwise holds everything the real key does.
	n := len(e.Subkeys)
	for i := 0; i < n; i++ {
		sub := e.Subkeys[i]
		claim := sign(sub.Sig, 10*365*day, func(s *packet.Signature) error {
			return s.SignKey(sub.PublicKey, e.PrivateKey, cfg)
		})
		other := sign(sub.Sig, day, func(s *packet.Signature) error {
			return s.SignKey(sub.PublicKey, e.PrivateKey, cfg)
		})
		claim.EdDSASigR, claim.EdDSASigS = other.EdDSASigR, other.EdDSASigS
		if e.PrimaryKey.VerifyKeySignature(sub.PublicKey, claim) == nil {
			t.Fatal("fixture: the forged subkey signature verifies")
		}
		e.Subkeys = append(e.Subkeys, openpgp.Subkey{PublicKey: sub.PublicKey, Sig: claim})
	}
	return publicBytes(t, e)
}

func TestMergeForgedSelfSignatureDoesNotExtend(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	created := now.Add(-300 * day)
	e := genKey(t, "carol@example.com", created, 365*day)
	old := publicBytes(t, e)
	forged := forgeExtension(t, e, now.Add(-time.Hour))

	merged, changed, err := Merge(old, forged)
	if err != nil {
		t.Fatalf("Merge: %v (a forged signature must be dropped, not poison the merge)", err)
	}
	if changed {
		t.Error("a copy whose only addition is a forged signature was reported as a change")
	}
	if u, never := until(t, merged, now); never || !sameSecond(u, created.Add(365*day)) {
		t.Errorf("a forged self-signature extended usability to %v (never=%v); want %v",
			u, never, created.Add(365*day))
	}
}

func TestMergeRefusesADifferentKeyAndSecretMaterial(t *testing.T) {
	now := time.Now().UTC()
	a := genKey(t, "a@example.com", now.Add(-day), 0)
	b := genKey(t, "a@example.com", now.Add(-day), 0)
	if _, _, err := Merge(publicBytes(t, a), publicBytes(t, b)); !errors.Is(err, ErrDifferentKey) {
		t.Errorf("merging two different keys: err = %v, want ErrDifferentKey", err)
	}
	var priv bytes.Buffer
	if err := a.SerializePrivate(&priv, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Merge(publicBytes(t, a), priv.Bytes()); !errors.Is(err, ErrHasPrivateKey) {
		t.Errorf("merging in a secret key: err = %v, want ErrHasPrivateKey", err)
	}
}

func TestMergeOfIdenticalCopyIsNoChange(t *testing.T) {
	e := genKey(t, "d@example.com", time.Now().Add(-day), 0)
	pub := publicBytes(t, e)
	merged, changed, err := Merge(pub, pub)
	if err != nil || changed {
		t.Fatalf("Merge(x, x): changed=%v err=%v", changed, err)
	}
	if !bytes.Equal(merged, pub) {
		t.Error("normalising a go-crypto-produced key changed its bytes")
	}
}

func TestAssessSubkeyExpiringFirstBoundsUsability(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	created := now.Add(-10 * day)
	e := genKey(t, "e@example.com", created, 0)  // never expires...
	resign(t, e, now.Add(-time.Hour), 0, 20*day) // ...except its encryption subkey
	v, err := Assess(publicBytes(t, e), now)
	if err != nil {
		t.Fatal(err)
	}
	if v.PrimaryExpires != nil {
		t.Errorf("primary expires %v, want never", v.PrimaryExpires)
	}
	u, never := v.UsableUntil()
	if never || !sameSecond(u, created.Add(20*day)) {
		t.Errorf("usable until %v (never=%v), want the subkey's %v", u, never, created.Add(20*day))
	}
	if got := ExpiryBucket(v, now); got != BucketUnder30 {
		t.Errorf("bucket %q, want %q", got, BucketUnder30)
	}
}

func TestExpiryBuckets(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	cases := []struct {
		name     string
		created  time.Time
		lifetime time.Duration
		want     string
	}{
		{"never", now.Add(-day), 0, BucketNever},
		{"expired", now.Add(-40 * day), 30 * day, BucketExpired},
		{"under 30", now.Add(-day), 10 * day, BucketUnder30},
		{"under 90", now.Add(-day), 60 * day, BucketUnder90},
		{"later", now.Add(-day), 400 * day, BucketLater},
	}
	for _, c := range cases {
		e := genKey(t, "b@example.com", c.created, c.lifetime)
		v, err := Assess(publicBytes(t, e), now)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := ExpiryBucket(v, now); got != c.want {
			t.Errorf("%s: bucket %q, want %q", c.name, got, c.want)
		}
	}
	e := genKey(t, "r@example.com", now.Add(-day), 0)
	if err := e.RevokeKey(packet.KeyRetired, "", &packet.Config{Time: func() time.Time { return now }}); err != nil {
		t.Fatal(err)
	}
	v, _ := Assess(publicBytes(t, e), now)
	if got := ExpiryBucket(v, now); got != BucketRevoked {
		t.Errorf("revoked: bucket %q", got)
	}
}
