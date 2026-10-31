// SPDX-License-Identifier: AGPL-3.0-only

package pgpverify

// ⚠ EVERY FIXTURE HERE IS PRODUCED BY THE REAL THING: a real OpenPGP key, a
// real detached signature, wrapped by the real pgpmime.Sign. None of it is
// hand-written bytes that look plausible.
//
// That is not fussiness. Key publication shipped INERT (#48) because its
// fixtures were hand-made "binary" no producer emits — they encoded a belief
// about the format, and the belief was the thing under test. A verifier tested
// against hand-made signatures would be testing the same belief twice.

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"

	"mailroom/internal/pgpmime"
)

// newSignedMessage builds a real signed message and returns it with the
// signer's binary public key.
func newSignedMessage(t *testing.T, body string) (raw []byte, pub PublicKey) {
	t.Helper()
	entity, err := openpgp.NewEntity("Test Signer", "", "signer@example.com", nil)
	if err != nil {
		t.Fatalf("NewEntity: %v", err)
	}

	msg := []byte("Subject: a real one\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"Content-Transfer-Encoding: quoted-printable\r\n\r\n" + body)

	signed, err := pgpmime.Sign(msg, func(canonical []byte) (string, string, error) {
		var sig bytes.Buffer
		if err := openpgp.ArmoredDetachSign(&sig, entity, bytes.NewReader(canonical), nil); err != nil {
			return "", "", err
		}
		return sig.String(), "pgp-sha256", nil
	})
	if err != nil {
		t.Fatalf("pgpmime.Sign: %v", err)
	}

	var pubBuf bytes.Buffer
	if err := entity.Serialize(&pubBuf); err != nil {
		t.Fatalf("serialize public key: %v", err)
	}
	return signed, PublicKey{
		Fingerprint: strings.ToUpper(string(entity.PrimaryKey.Fingerprint)),
		Binary:      pubBuf.Bytes(),
	}
}

func TestVerifyAcceptsAGenuineSignature(t *testing.T) {
	raw, pub := newSignedMessage(t, "an em=E2=80=94dash and plain text\r\n")

	res := Verify(raw, []PublicKey{pub})
	if res.Verdict != VerdictVerified {
		t.Fatalf("verdict = %q (err=%v), want verified", res.Verdict, res.Err)
	}
	if res.SignerFingerprint == "" {
		t.Error("a verified signature reported no signer fingerprint")
	}
	if res.MicAlg != "pgp-sha256" {
		t.Errorf("micalg = %q, want pgp-sha256", res.MicAlg)
	}
}

// TestVerifyRejectsTamperedContent is the negative control WITHOUT WHICH the
// test above proves nothing: a Verify that returned "verified" unconditionally
// would pass it.
func TestVerifyRejectsTamperedContent(t *testing.T) {
	raw, pub := newSignedMessage(t, "the original body\r\n")

	tampered := bytes.Replace(raw, []byte("the original body"), []byte("the tampered body"), 1)
	if bytes.Equal(tampered, raw) {
		t.Fatal("the fixture was not actually altered; this test would prove nothing")
	}

	res := Verify(tampered, []PublicKey{pub})
	if res.Verdict != VerdictBadSignature {
		t.Errorf("verdict = %q (err=%v), want bad_signature — altered mail must not verify",
			res.Verdict, res.Err)
	}
}

// TestVerifySurvivesTransportRewriting is the point of RFC 3156
// canonicalisation, and the automated form of the question the round-trip on
// a live instance could not answer: a signature must still verify after an
// MTA has rewritten line endings.
//
// ⚠ If this fails, the quoted-printable work in increment 5 does not do what it
// claims and recipients see "bad signature" on perfectly good mail.
func TestVerifySurvivesTransportRewriting(t *testing.T) {
	raw, pub := newSignedMessage(t, "body with an em=E2=80=94dash\r\n")

	// A gateway that normalised CRLF to LF — the classic transport rewrite.
	mangled := bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n"))
	if bytes.Equal(mangled, raw) {
		t.Fatal("the fixture was not actually rewritten")
	}

	res := Verify(mangled, []PublicKey{pub})
	if res.Verdict != VerdictVerified {
		t.Errorf("verdict = %q (err=%v), want verified — canonicalisation exists precisely "+
			"so a line-ending rewrite does not break the signature", res.Verdict, res.Err)
	}
}

// TestVerifyDistinguishesAnUnknownSignerFromABadSignature is the distinction
// the whole report hangs on. Every correspondent's signature will be
// unverifiable until key discovery exists, and reporting those as BAD would
// bury the one verdict that means something has gone wrong.
func TestVerifyDistinguishesAnUnknownSignerFromABadSignature(t *testing.T) {
	raw, _ := newSignedMessage(t, "signed by someone else\r\n")
	_, otherKey := newSignedMessage(t, "unrelated\r\n") // a different key entirely

	res := Verify(raw, []PublicKey{otherKey})
	if res.Verdict != VerdictNoKey {
		t.Errorf("verdict = %q (err=%v), want no_key — we hold no key for this signature, "+
			"which is not the same as the signature being invalid", res.Verdict, res.Err)
	}

	// Positive control on the same message: with the RIGHT key it verifies, so
	// the no_key above is about the key and not about the message.
	raw2, pub2 := newSignedMessage(t, "signed by us\r\n")
	if got := Verify(raw2, []PublicKey{pub2}); got.Verdict != VerdictVerified {
		t.Errorf("the control message did not verify with its own key: %q (%v)",
			got.Verdict, got.Err)
	}
}

func TestVerifyReportsNoKeysAvailable(t *testing.T) {
	raw, _ := newSignedMessage(t, "body\r\n")
	if got := Verify(raw, nil); got.Verdict != VerdictNoKey {
		t.Errorf("verdict = %q, want no_key when no keys are supplied", got.Verdict)
	}
}

func TestVerifyReportsOrdinaryMailAsNotSigned(t *testing.T) {
	plain := []byte("Subject: hello\r\nContent-Type: text/plain\r\n\r\njust mail\r\n")
	res := Verify(plain, nil)
	if res.Verdict != VerdictNotSigned {
		t.Errorf("verdict = %q, want not_signed for ordinary mail", res.Verdict)
	}
	if res.Err != nil {
		t.Errorf("unsigned mail reported an error (%v); it is the ordinary case, not a fault", res.Err)
	}
}

// TestVerifyReportsMalformedSeparately: a container that claims to be signed
// and cannot be taken apart is a producer bug, and calling it a bad signature
// would send someone hunting for a tampering that never happened.
func TestVerifyReportsMalformedSeparately(t *testing.T) {
	broken := []byte("Content-Type: multipart/signed; boundary=\"b\"; " +
		"protocol=\"application/pgp-signature\"\r\n\r\n" +
		"--b\r\nContent-Type: text/plain\r\n\r\nonly one part\r\n--b--\r\n")
	res := Verify(broken, nil)
	if res.Verdict != VerdictMalformed {
		t.Errorf("verdict = %q, want malformed", res.Verdict)
	}
	if res.Err == nil {
		t.Error("a malformed verdict must carry the reason")
	}
}
