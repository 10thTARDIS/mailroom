// SPDX-License-Identifier: AGPL-3.0-only

package pgpverify

// A signature by a key that has since EXPIRED (#795/#796). Real go-crypto keys
// with real lifetimes, signed while valid; nothing hand-made.

import (
	"bytes"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/packet"

	"mailroom/internal/pgpmime"
)

// signedByShortLivedKey makes a key created at created with the given lifetime,
// and a message signed with it at signedAt (which must be inside the lifetime:
// go-crypto refuses to sign with a key that is expired at the signing time).
func signedByShortLivedKey(t *testing.T, body string, created time.Time, lifetime time.Duration, signedAt time.Time) ([]byte, PublicKey) {
	t.Helper()
	e, err := openpgp.NewEntity("Short Lived", "", "signer@example.com", &packet.Config{
		Algorithm: packet.PubKeyAlgoEdDSA, Curve: packet.Curve25519,
		Time:            func() time.Time { return created },
		KeyLifetimeSecs: uint32(lifetime / time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	msg := []byte("Subject: old mail\r\nContent-Type: text/plain\r\n\r\n" + body)
	signed, err := pgpmime.Sign(msg, func(canonical []byte) (string, string, error) {
		var sig bytes.Buffer
		cfg := &packet.Config{Time: func() time.Time { return signedAt }}
		if err := openpgp.ArmoredDetachSign(&sig, e, bytes.NewReader(canonical), cfg); err != nil {
			return "", "", err
		}
		return sig.String(), "pgp-sha256", nil
	})
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	var pub bytes.Buffer
	if err := e.Serialize(&pub); err != nil {
		t.Fatal(err)
	}
	return signed, PublicKey{Fingerprint: fingerprintOf(e, ""), Binary: pub.Bytes()}
}

func TestVerifyReportsAnExpiredKeyAsAReasonNotAForgery(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	created := now.Add(-40 * 24 * time.Hour)
	signedAt := now.Add(-35 * 24 * time.Hour)
	raw, pub := signedByShortLivedKey(t, "signed while the key was valid\r\n", created, 30*24*time.Hour, signedAt)

	res := Verify(raw, []PublicKey{pub})
	if res.Verdict != VerdictBadSignature {
		t.Fatalf("verdict = %q, want bad_signature (ADR-039 keeps an expired key's signature bad)", res.Verdict)
	}
	if !res.KeyExpired {
		t.Fatalf("KeyExpired = false (err=%v); the signature checked out and only the key's date failed it", res.Err)
	}
	if res.ExpiredSignerFingerprint != pub.Fingerprint {
		t.Errorf("ExpiredSignerFingerprint = %q, want %q", res.ExpiredSignerFingerprint, pub.Fingerprint)
	}
	if res.SignedAt == nil || !res.SignedAt.Equal(signedAt) {
		t.Errorf("SignedAt = %v, want %v", res.SignedAt, signedAt)
	}
	wantExp := created.Add(30 * 24 * time.Hour)
	if res.KeyExpiredAt == nil || !res.KeyExpiredAt.Equal(wantExp) {
		t.Errorf("KeyExpiredAt = %v, want %v", res.KeyExpiredAt, wantExp)
	}
	if !res.ValidWhenSigned() {
		t.Error("ValidWhenSigned = false for a signature made five days before the key expired")
	}
}

// The negative control: the SAME expired key over ALTERED content is a plain bad
// signature with no reason. go-crypto checks the signature before the key's
// date, so an expired key must never soften a forgery into "expired".
func TestVerifyTamperedContentByAnExpiredKeyIsPlainBad(t *testing.T) {
	now := time.Now().UTC()
	raw, pub := signedByShortLivedKey(t, "the original body\r\n", now.Add(-40*24*time.Hour), 30*24*time.Hour, now.Add(-35*24*time.Hour))
	tampered := bytes.Replace(raw, []byte("the original body"), []byte("the tampered body"), 1)
	if bytes.Equal(tampered, raw) {
		t.Fatal("fixture not altered")
	}
	res := Verify(tampered, []PublicKey{pub})
	if res.Verdict != VerdictBadSignature || res.KeyExpired {
		t.Fatalf("verdict = %q KeyExpired = %v, want bad_signature with no expiry reason", res.Verdict, res.KeyExpired)
	}
}

// A live key is unaffected: still verified, no reason.
func TestVerifyALiveShortLivedKeyStillVerifies(t *testing.T) {
	now := time.Now().UTC()
	raw, pub := signedByShortLivedKey(t, "fresh\r\n", now.Add(-time.Hour), 30*24*time.Hour, now.Add(-time.Minute))
	if res := Verify(raw, []PublicKey{pub}); res.Verdict != VerdictVerified || res.KeyExpired {
		t.Fatalf("verdict = %q KeyExpired = %v, want verified", res.Verdict, res.KeyExpired)
	}
}

func TestValidWhenSignedNeedsBothDates(t *testing.T) {
	a := time.Date(2026, 5, 3, 0, 0, 0, 0, time.UTC)
	b := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		r    Result
		want bool
	}{
		{Result{KeyExpired: true, SignedAt: &a, KeyExpiredAt: &b}, true},
		{Result{KeyExpired: true, SignedAt: &b, KeyExpiredAt: &a}, false},
		{Result{KeyExpired: true, SignedAt: &a}, false},
		{Result{KeyExpired: false, SignedAt: &a, KeyExpiredAt: &b}, false},
	}
	for i, c := range cases {
		if got := c.r.ValidWhenSigned(); got != c.want {
			t.Errorf("case %d: ValidWhenSigned = %v, want %v", i, got, c.want)
		}
	}
}
