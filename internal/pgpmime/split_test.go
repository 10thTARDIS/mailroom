// SPDX-License-Identifier: AGPL-3.0-only

package pgpmime

// SplitSigned has exactly one job: recover the bytes that were signed.
//
// ⚠ THE ONLY TEST THAT SETTLES THAT IS A ROUND TRIP AGAINST Sign. Asserting
// that the recovered entity "looks like" a MIME part would pass against an
// implementation off by the one CRLF that RFC 2046 §5.1.1 assigns to the
// boundary delimiter — the classic PGP/MIME off-by-one, which produces a
// message that verifies nowhere and looks perfect in a diff.
//
// So the assertions below compare what Split recovers against what Sign
// actually handed the signer, captured at the moment it did.

import (
	"bytes"
	"strings"
	"testing"
)

const testSig = "-----BEGIN PGP SIGNATURE-----\r\n\r\niHUEARYIAB0WIQT\r\n-----END PGP SIGNATURE-----"

// signCapturing signs raw with a stub, returning the wrapped message and the
// exact canonical bytes the signer was given.
func signCapturing(t *testing.T, raw []byte) (wrapped, canonical []byte) {
	t.Helper()
	out, err := Sign(raw, func(c []byte) (string, string, error) {
		canonical = append([]byte(nil), c...)
		return testSig, "pgp-sha256", nil
	})
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	return out, canonical
}

func TestSplitRecoversExactlyWhatWasSigned(t *testing.T) {
	cases := map[string][]byte{
		"plain": []byte("Subject: hi\r\nContent-Type: text/plain\r\n\r\nbody\r\n"),
		// ⚠ A body with NO trailing newline. "a\n" and "a" are different content
		// and must hash differently; conflating them is the off-by-one this test
		// exists for.
		"no trailing newline": []byte("Subject: hi\r\nContent-Type: text/plain\r\n\r\nbody"),
		// A body ending in a BLANK line — the trailing CRLF is content, and the
		// delimiter's CRLF is not.
		"trailing blank line": []byte("Subject: hi\r\nContent-Type: text/plain\r\n\r\nbody\r\n\r\n"),
		"quoted-printable": []byte("Subject: hi\r\nContent-Type: text/plain; charset=utf-8\r\n" +
			"Content-Transfer-Encoding: quoted-printable\r\n\r\nan em=E2=80=94dash\r\n"),
		"multipart body": []byte("Subject: hi\r\nContent-Type: multipart/alternative; boundary=\"inner\"\r\n" +
			"\r\n--inner\r\nContent-Type: text/plain\r\n\r\ntext\r\n--inner\r\n" +
			"Content-Type: text/html\r\n\r\n<p>html</p>\r\n--inner--\r\n"),
	}

	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			wrapped, canonical := signCapturing(t, raw)

			parts, err := SplitSigned(wrapped)
			if err != nil {
				t.Fatalf("SplitSigned: %v", err)
			}

			// THE ASSERTION. Canonicalising what Split recovered must reproduce
			// the bytes Sign handed the signer, exactly.
			if got := Canonicalize(parts.Entity); !bytes.Equal(got, canonical) {
				t.Errorf("recovered entity does not canonicalise to what was signed\n"+
					" got  (%d bytes): %q\n want (%d bytes): %q",
					len(got), got, len(canonical), canonical)
			}
			if !bytes.Contains(parts.Signature, []byte(sigArmorBegin)) {
				t.Error("no armored signature recovered")
			}
			if parts.MicAlg != "pgp-sha256" {
				t.Errorf("micalg = %q, want pgp-sha256", parts.MicAlg)
			}
		})
	}
}

// TestSplitSurvivesLineEndingRewriting is the case RFC 3156 canonicalisation
// exists for. An MTA that rewrote CRLF to LF must not defeat verification one
// step before it would have worked.
func TestSplitSurvivesLineEndingRewriting(t *testing.T) {
	raw := []byte("Subject: hi\r\nContent-Type: text/plain\r\n\r\nbody\r\n")
	wrapped, canonical := signCapturing(t, raw)

	mangled := bytes.ReplaceAll(wrapped, []byte("\r\n"), []byte("\n"))
	// Control: the transport really did change the bytes.
	if bytes.Equal(mangled, wrapped) {
		t.Fatal("the fixture was not actually rewritten")
	}

	parts, err := SplitSigned(mangled)
	if err != nil {
		t.Fatalf("SplitSigned on LF-normalised mail: %v", err)
	}
	if got := Canonicalize(parts.Entity); !bytes.Equal(got, canonical) {
		t.Errorf("canonicalising an LF-rewritten message did not reproduce the signed bytes\n"+
			" got: %q\nwant: %q", got, canonical)
	}
}

// TestSplitRefusesWhatIsNotAPGPSignedMessage: each of these must come back as
// ErrNotSigned rather than as a broken signature, because the two lead to
// completely different reports — "this mail is not signed" versus "this
// signature failed".
func TestSplitRefusesWhatIsNotAPGPSignedMessage(t *testing.T) {
	cases := map[string]string{
		"plain text": "Subject: hi\r\nContent-Type: text/plain\r\n\r\nbody\r\n",
		"multipart but not signed": "Content-Type: multipart/mixed; boundary=\"b\"\r\n\r\n" +
			"--b\r\nContent-Type: text/plain\r\n\r\nx\r\n--b--\r\n",
		// ⚠ S/MIME. Identical container, different protocol — and reporting it
		// as a failed PGP signature would be a lie about someone else's mail.
		"smime": "Content-Type: multipart/signed; boundary=\"b\"; " +
			"protocol=\"application/pkcs7-signature\"\r\n\r\n" +
			"--b\r\nContent-Type: text/plain\r\n\r\nx\r\n--b\r\n" +
			"Content-Type: application/pkcs7-signature\r\n\r\nzzz\r\n--b--\r\n",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := SplitSigned([]byte(raw)); err == nil {
				t.Error("accepted a message that is not PGP-signed")
			} else if !strings.Contains(err.Error(), "not a multipart/signed") {
				t.Errorf("err = %v, want ErrNotSigned — the report must distinguish "+
					"'unsigned' from 'signature failed'", err)
			}
		})
	}
}

// TestSplitRejectsAMissingSignaturePart guards the case where the container
// claims to be signed and is not: two parts are required, and picking whatever
// is there would mean verifying against something arbitrary.
func TestSplitRejectsAMissingSignaturePart(t *testing.T) {
	raw := "Content-Type: multipart/signed; boundary=\"b\"; " +
		"protocol=\"application/pgp-signature\"; micalg=\"pgp-sha256\"\r\n\r\n" +
		"--b\r\nContent-Type: text/plain\r\n\r\nx\r\n--b--\r\n"
	if _, err := SplitSigned([]byte(raw)); err == nil {
		t.Error("accepted a multipart/signed with only one part")
	}
}

// TestSplitRejectsASignaturePartThatIsNotOne: a container whose second part is
// not an armored signature must be refused rather than handed to a verifier,
// which would report a crypto failure for what is a structural problem.
func TestSplitRejectsASignaturePartThatIsNotOne(t *testing.T) {
	raw := "Content-Type: multipart/signed; boundary=\"b\"; " +
		"protocol=\"application/pgp-signature\"; micalg=\"pgp-sha256\"\r\n\r\n" +
		"--b\r\nContent-Type: text/plain\r\n\r\nx\r\n--b\r\n" +
		"Content-Type: application/pgp-signature\r\n\r\nnot armor at all\r\n--b--\r\n"
	if _, err := SplitSigned([]byte(raw)); err == nil {
		t.Error("accepted a signature part containing no armor")
	}
}
