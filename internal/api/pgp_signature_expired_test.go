// SPDX-License-Identifier: AGPL-3.0-only

package api

// The keyExpired reason on a bad signature (#795/#796) is a BENIGN story
// ("signed with a key that has since expired"), so only a key the verified path
// would have believed may tell it (heldSigner.attributable, via
// sigKeys.gateKeyExpired). An attacker signing with their own throwaway,
// already-expired key must get a plain `bad`. Both RFC 3156 shapes: detached
// (pgpverify.Verify over a real signed message) and embedded (the crypto
// process's wire answer mapped by embeddedVerdict). Real go-crypto keys with
// real lifetimes throughout.

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/packet"

	"mailroom/internal/pgpmime"
	"mailroom/internal/pgpverify"
	"mailroom/internal/store"
)

type expiredFixture struct {
	entity   *openpgp.Entity
	pub      []byte
	fpr      string
	signedAt time.Time
	expired  time.Time
	signed   []byte // a multipart/signed message signed while the key was valid
}

func newExpiredSigner(t *testing.T, email string) expiredFixture {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	created := now.Add(-40 * 24 * time.Hour)
	e, err := openpgp.NewEntity("Signer", "", email, &packet.Config{
		Algorithm: packet.PubKeyAlgoEdDSA, Curve: packet.Curve25519,
		Time:            func() time.Time { return created },
		KeyLifetimeSecs: uint32((30 * 24 * time.Hour) / time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	signedAt := now.Add(-35 * 24 * time.Hour)
	msg := []byte("From: " + email + "\r\nSubject: old\r\nContent-Type: text/plain\r\n\r\nhello\r\n")
	signed, err := pgpmime.Sign(msg, func(canonical []byte) (string, string, error) {
		var sig bytes.Buffer
		cfg := &packet.Config{Time: func() time.Time { return signedAt }}
		if err := openpgp.ArmoredDetachSign(&sig, e, bytes.NewReader(canonical), cfg); err != nil {
			return "", "", err
		}
		return sig.String(), "pgp-sha256", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var pub bytes.Buffer
	if err := e.Serialize(&pub); err != nil {
		t.Fatal(err)
	}
	return expiredFixture{
		entity: e, pub: pub.Bytes(),
		fpr:      strings.ToUpper(fmt.Sprintf("%X", e.PrimaryKey.Fingerprint)),
		signedAt: signedAt, expired: created.Add(30 * 24 * time.Hour), signed: signed,
	}
}

// embeddedAnswer is what the crypto process reports for the same signature
// inside an encrypted message (see pgpkeyd's TestDecryptEmbeddedSignatureByAnExpiredKeySaysSo).
func (f expiredFixture) embeddedAnswer() EmbeddedSignature {
	sa, ex := f.signedAt, f.expired
	return EmbeddedSignature{Signed: true, Status: EmbeddedBad, Issuer: f.fpr,
		KeyExpired: true, ExpiredSignerFingerprint: f.fpr, SignedAt: &sa, KeyExpiredAt: &ex}
}

func TestKeyExpiredReasonOnlyForAnAttributableSigner(t *testing.T) {
	const from = "alice@example.com"
	genuine := newExpiredSigner(t, from)
	other := newExpiredSigner(t, "bob@example.com")
	contact := func(f expiredFixture, source string, auth, refused bool) store.ContactKey {
		return store.ContactKey{Kind: store.KeyPGP, Fingerprint: f.fpr, PublicKey: f.pub,
			Source: source, Authenticated: auth, Refused: refused}
	}
	cases := []struct {
		name       string
		signer     expiredFixture
		keys       *sigKeys
		wantReason bool
	}{
		{"genuine held WKD key for From", genuine,
			&sigKeys{contactKeys: []store.ContactKey{contact(genuine, store.ContactKeyWKD, false, false)}}, true},
		{"the user's own key naming From", genuine,
			&sigKeys{ownKeys: []store.UserKey{{Fingerprint: genuine.fpr, PublicKey: genuine.pub}}}, true},
		{"attacker key from an unauthenticated Autocrypt header", genuine,
			&sigKeys{contactKeys: []store.ContactKey{contact(genuine, store.ContactKeyAutocrypt, false, false)}}, false},
		{"a refused held key", genuine,
			&sigKeys{contactKeys: []store.ContactKey{contact(genuine, store.ContactKeyManual, false, true)}}, false},
		{"a held key that does not name From", other,
			&sigKeys{contactKeys: []store.ContactKey{contact(other, store.ContactKeyWKD, false, false)}}, false},
	}
	a := &API{}
	for _, c := range cases {
		c.keys.from, c.keys.now = from, time.Now().UTC()
		check := func(shape string, sig *pgpSignature) {
			t.Helper()
			if sig == nil || sig.State != sigBad {
				t.Fatalf("%s/%s: %+v, want state bad", c.name, shape, sig)
			}
			if got := sig.Reason == sigReasonKeyExpired; got != c.wantReason {
				t.Errorf("%s/%s: reason %q, want keyExpired=%v", c.name, shape, sig.Reason, c.wantReason)
			}
			if !c.wantReason && (sig.Fingerprint != "" || sig.SignedAt != nil || sig.KeyExpiredAt != nil || sig.ValidWhenSigned != nil) {
				t.Errorf("%s/%s: a plain bad carries expiry details: %+v", c.name, shape, sig)
			}
			if c.wantReason && (sig.ValidWhenSigned == nil || !*sig.ValidWhenSigned || sig.Fingerprint != c.signer.fpr) {
				t.Errorf("%s/%s: %+v, want validWhenSigned and the signer's fingerprint", c.name, shape, sig)
			}
		}
		// Detached: the real verifier against the keys held.
		res := pgpverify.Verify(c.signer.signed, c.keys.verifyKeys())
		if !res.KeyExpired {
			t.Fatalf("%s: fixture did not produce a KeyExpired result (%q, %v)", c.name, res.Verdict, res.Err)
		}
		check("detached", a.judgeSignature(t.Context(), store.NewULID(), c.keys, res))
		// Embedded: the crypto process's answer for the same signature.
		check("embedded", a.judgeSignature(t.Context(), store.NewULID(), c.keys, embeddedVerdict(c.signer.embeddedAnswer())))
	}

	// Embedded only: the crypto process can be handed a candidate that is not a
	// held key for From at all (an attacker's throwaway key). Not held → plain bad.
	keys := &sigKeys{from: from, now: time.Now().UTC(),
		contactKeys: []store.ContactKey{contact(genuine, store.ContactKeyWKD, false, false)}}
	if sig := a.judgeSignature(t.Context(), store.NewULID(), keys, embeddedVerdict(other.embeddedAnswer())); sig == nil || sig.State != sigBad || sig.Reason != "" {
		t.Errorf("an unheld expired signer: %+v, want plain bad", sig)
	}
}
