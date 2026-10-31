// SPDX-License-Identifier: AGPL-3.0-only

package pgpmail

import (
	"strings"
	"testing"

	"mailroom/internal/mime"
)

// THESE ARE POSITIVE CONTROLS, and they are the point of this file.
//
// The prevalence report this package exists for is EXPECTED to return zero on
// many mailboxes. A zero from a
// detector that never fires is indistinguishable from a zero from a mailbox
// with no encrypted mail, and this project has already shipped that mistake
// once (`suggest-report`, a report whose expected output was zero). So every signal is proved to
// fire against a real message structure here; only then does a zero out there
// mean anything.
//
// The fixtures are RFC-shaped rather than minimal: RFC 3156 puts the protocol
// on the multipart container, and a detector that read only leaf parts would
// pass a sloppier test while missing every correctly-formed PGP/MIME message.

func parse(t *testing.T, raw string) *mime.Message {
	t.Helper()
	msg, err := mime.ParseBytes([]byte(strings.ReplaceAll(raw, "\n", "\r\n")))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return msg
}

// RFC 3156 §5: the signature is a sibling part and the protocol lives on the
// multipart/signed container.
func TestDetectsPGPMIMESignature(t *testing.T) {
	msg := parse(t, `From: alice@example.com
To: bob@example.com
Subject: signed
Content-Type: multipart/signed; boundary="b1"; micalg=pgp-sha256;
 protocol="application/pgp-signature"
MIME-Version: 1.0

--b1
Content-Type: text/plain

hello
--b1
Content-Type: application/pgp-signature; name="signature.asc"

-----BEGIN PGP SIGNATURE-----

iQEzBAEBCAAdFiEE
-----END PGP SIGNATURE-----
--b1--
`)
	got := Detect(msg)
	if !got.PGPMIMESigned {
		t.Errorf("PGP/MIME signature not detected: %+v", got)
	}
	if !got.Any() || !got.AnyPGP() {
		t.Errorf("Any/AnyPGP false on a signed message: %+v", got)
	}
	if got.AnySMIME() {
		t.Errorf("a PGP signature was counted as S/MIME: %+v", got)
	}
}

// RFC 3156 §4: the control part plus the ciphertext, under multipart/encrypted.
func TestDetectsPGPMIMEEncryption(t *testing.T) {
	msg := parse(t, `From: alice@example.com
Subject: encrypted
Content-Type: multipart/encrypted; boundary="b2";
 protocol="application/pgp-encrypted"
MIME-Version: 1.0

--b2
Content-Type: application/pgp-encrypted

Version: 1
--b2
Content-Type: application/octet-stream

-----BEGIN PGP MESSAGE-----

hQEMAw
-----END PGP MESSAGE-----
--b2--
`)
	got := Detect(msg)
	if !got.PGPMIMEEncrypted {
		t.Errorf("PGP/MIME encryption not detected: %+v", got)
	}
}

// RFC 4880 §7, the pre-MIME cleartext framework — still common from mailing
// lists and older clients.
func TestDetectsInlineArmor(t *testing.T) {
	signed := parse(t, `From: alice@example.com
Content-Type: text/plain

-----BEGIN PGP SIGNED MESSAGE-----
Hash: SHA256

hello
-----BEGIN PGP SIGNATURE-----
iQEz
-----END PGP SIGNATURE-----
`)
	if got := Detect(signed); !got.PGPInlineSigned {
		t.Errorf("inline cleartext signature not detected: %+v", got)
	}

	enc := parse(t, `From: alice@example.com
Content-Type: text/plain

-----BEGIN PGP MESSAGE-----

hQEMAw
-----END PGP MESSAGE-----
`)
	if got := Detect(enc); !got.PGPInlineMessage {
		t.Errorf("inline armored message not detected: %+v", got)
	}

	key := parse(t, `From: alice@example.com
Content-Type: text/plain

-----BEGIN PGP PUBLIC KEY BLOCK-----

mQINBF
-----END PGP PUBLIC KEY BLOCK-----
`)
	if got := Detect(key); !got.PGPPublicKey {
		t.Errorf("inline public key block not detected: %+v", got)
	}
}

// Autocrypt is a HEADER-only signal on an otherwise ordinary message, and it is
// the one that matters most for #48: it says the sender's client would accept
// an encrypted reply, whether or not this message is encrypted.
func TestDetectsAutocryptOnAnOrdinaryMessage(t *testing.T) {
	msg := parse(t, `From: alice@example.com
To: bob@example.com
Subject: perfectly ordinary
Autocrypt: addr=alice@example.com; keydata=mQINBF
Content-Type: text/plain

nothing encrypted here at all
`)
	got := Detect(msg)
	if !got.Autocrypt {
		t.Errorf("Autocrypt header not detected: %+v", got)
	}
	if got.PGPMIMESigned || got.PGPMIMEEncrypted || got.PGPInlineSigned {
		t.Errorf("an unencrypted message was reported as signed/encrypted: %+v", got)
	}
	if !got.Any() {
		t.Error("Any() false despite an Autocrypt header — the peer-key signal would be invisible")
	}
}

// S/MIME must be counted SEPARATELY, because the go/no-go decision (#47, spec
// §18 item 6) turns on this number alone. A combined total would bury it.
func TestDetectsSMIMEAndKeepsItSeparate(t *testing.T) {
	sig := parse(t, `From: alice@example.com
Content-Type: multipart/signed; boundary="b3"; micalg=sha-256;
 protocol="application/pkcs7-signature"
MIME-Version: 1.0

--b3
Content-Type: text/plain

hello
--b3
Content-Type: application/pkcs7-signature; name="smime.p7s"

MIAGCS
--b3--
`)
	got := Detect(sig)
	if !got.SMIMESigned {
		t.Errorf("S/MIME signature not detected: %+v", got)
	}
	if got.AnyPGP() {
		t.Errorf("S/MIME counted as PGP — the go/no-go number would be wrong: %+v", got)
	}
	if !got.AnySMIME() {
		t.Errorf("AnySMIME false on an S/MIME message: %+v", got)
	}

	enc := parse(t, `From: alice@example.com
Content-Type: application/pkcs7-mime; smime-type=enveloped-data; name="smime.p7m"
Content-Transfer-Encoding: base64

MIAGCSqGSIb3DQEHA6CA
`)
	if got := Detect(enc); !got.SMIMEEncrypted {
		t.Errorf("S/MIME enveloped data not detected: %+v", got)
	}
}

// A forwarded encrypted message is still encrypted mail in this mailbox, so
// DetectWithNested (the census) walks nested message/rfc822 trees. Detect does
// NOT: the nested message is not this message's own content (#405).
func TestDetectsThroughNestedMessage(t *testing.T) {
	msg := parse(t, `From: carol@example.com
Subject: fwd
Content-Type: multipart/mixed; boundary="b4"
MIME-Version: 1.0

--b4
Content-Type: text/plain

see attached
--b4
Content-Type: message/rfc822

From: alice@example.com
Content-Type: multipart/encrypted; boundary="inner";
 protocol="application/pgp-encrypted"
MIME-Version: 1.0

--inner
Content-Type: application/pgp-encrypted

Version: 1
--inner--
--b4--
`)
	if got := DetectWithNested(msg); !got.PGPMIMEEncrypted {
		t.Errorf("encryption inside a forwarded message not detected: %+v", got)
	}
	if got := Detect(msg); got.PGPMIMEEncrypted {
		t.Errorf("a forwarded encrypted message marked the OUTER message encrypted (#405): %+v", got)
	}
}

// RFC 3156 section 5: signed-only PGP/MIME is not encrypted (#405 control).
func TestSignedOnlyPGPMIMEIsNotEncrypted(t *testing.T) {
	msg := parse(t, `From: alice@example.com
Subject: signed
Content-Type: multipart/signed; boundary="s1"; micalg=pgp-sha256;
 protocol="application/pgp-signature"
MIME-Version: 1.0

--s1
Content-Type: text/plain

hello
--s1
Content-Type: application/pgp-signature; name="signature.asc"

-----BEGIN PGP SIGNATURE-----

iQEzBAEBCAAdFiEE
-----END PGP SIGNATURE-----
--s1--
`)
	got := Detect(msg)
	if !got.PGPMIMESigned {
		t.Fatalf("control: signed PGP/MIME not detected: %+v", got)
	}
	if got.PGPMIMEEncrypted || got.SMIMEEncrypted {
		t.Errorf("signed-only PGP/MIME marked encrypted: %+v", got)
	}
}

// smime-type is a sender-written label. Opaque-signed S/MIME
// (smime-type=signed-data) still counts as encrypted: an attacker can put that
// label on enveloped-data, and ciphertext must never be scanned as clean (#405).
func TestSignedDataLabelDoesNotDefeatEncryptedDetection(t *testing.T) {
	msg := parse(t, `From: alice@example.com
Subject: labelled signed
MIME-Version: 1.0
Content-Type: application/pkcs7-mime; smime-type=signed-data;
 name=smime.p7m
Content-Transfer-Encoding: base64
Content-Disposition: attachment; filename=smime.p7m

MIAGCSqGSIb3DQEHA6CA
`)
	if got := Detect(msg); !got.SMIMEEncrypted {
		t.Errorf("pkcs7-mime labelled signed-data not treated as encrypted: %+v", got)
	}
}

// RFC 8551 section 3.2.2 / 3.3: enveloped-data and authEnveloped-data are
// encrypted, and a missing smime-type stays encrypted (conservative).
func TestEnvelopedSMIMEIsEncrypted(t *testing.T) {
	for _, ct := range []string{
		`application/pkcs7-mime; smime-type=enveloped-data; name=smime.p7m`,
		`application/pkcs7-mime; smime-type=authEnveloped-data; name=smime.p7m`,
		`application/x-pkcs7-mime; smime-type=enveloped-data; name=smime.p7m`,
		`application/pkcs7-mime; name=smime.p7m`,
	} {
		msg := parse(t, "From: alice@example.com\nMIME-Version: 1.0\nContent-Type: "+ct+
			"\nContent-Transfer-Encoding: base64\n\nMIAGCSqGSIb3DQEHA6CA\n")
		if got := Detect(msg); !got.SMIMEEncrypted || got.SMIMESigned {
			t.Errorf("%s: %+v, want encrypted and not signed", ct, got)
		}
	}
}

// THE NEGATIVE CONTROL. Without it, a Detect that returned every field true
// would pass every test above — and the report's headline number is a count of
// messages where Any() is FALSE, so a detector that over-fires is exactly as
// wrong as one that never fires.
func TestOrdinaryMailDetectsNothing(t *testing.T) {
	for _, raw := range []string{
		`From: a@example.com
Content-Type: text/plain

just a normal message
`,
		`From: a@example.com
Content-Type: multipart/alternative; boundary="b5"
MIME-Version: 1.0

--b5
Content-Type: text/plain

hello
--b5
Content-Type: text/html

<p>hello</p>
--b5--
`,
		// Talking ABOUT PGP is not using it. The armor detector matches the
		// literal header line, so prose mentioning it must stay clean.
		`From: a@example.com
Content-Type: text/plain

Have you considered PGP? I hear BEGIN PGP MESSAGE is the marker.
`,
	} {
		if got := Detect(parse(t, raw)); got.Any() {
			t.Errorf("ordinary mail reported apparatus: %+v\n---\n%s", got, raw)
		}
	}
}

// A nil message is "nothing observed", not a panic — the report feeds it
// messages that failed to parse.
func TestNilMessageIsEmpty(t *testing.T) {
	if got := Detect(nil); got.Any() {
		t.Errorf("nil message reported apparatus: %+v", got)
	}
}

// ⚠ THESE TWO EXIST BECAUSE THE PROBE FOUND THE OTHERS COULD NOT FAIL.
//
// TestDetectsPGPMIMESignature uses a well-formed RFC 3156 message, in which the
// container carries protocol="application/pgp-signature" AND the leaf part is
// application/pgp-signature. Detect checks both, so deleting EITHER check left
// the whole suite green — the container path and the leaf path were each
// masking the other, and neither was verified.
//
// Each fixture below is missing the signal the other one relies on, so exactly
// one code path can produce the answer.

// CONTAINER ONLY: the protocol parameter is the sole PGP marker, because the
// signature part is mislabelled application/octet-stream — which real clients
// do, and which a truncated download produces too.
func TestDetectsPGPMIMESignatureFromTheContainerAlone(t *testing.T) {
	msg := parse(t, `From: alice@example.com
Content-Type: multipart/signed; boundary="c1"; micalg=pgp-sha256;
 protocol="application/pgp-signature"
MIME-Version: 1.0

--c1
Content-Type: text/plain

hello
--c1
Content-Type: application/octet-stream

MIAGCS
--c1--
`)
	if got := Detect(msg); !got.PGPMIMESigned {
		t.Errorf("a signed message whose only PGP marker is the container protocol was missed: %+v", got)
	}
}

// LEAF ONLY: a bare application/pgp-signature part under a plain
// multipart/mixed, with no protocol parameter anywhere.
func TestDetectsPGPMIMESignatureFromTheLeafAlone(t *testing.T) {
	msg := parse(t, `From: alice@example.com
Content-Type: multipart/mixed; boundary="c2"
MIME-Version: 1.0

--c2
Content-Type: text/plain

hello
--c2
Content-Type: application/pgp-signature; name="signature.asc"

-----BEGIN PGP SIGNATURE-----
iQEz
-----END PGP SIGNATURE-----
--c2--
`)
	if got := Detect(msg); !got.PGPMIMESigned {
		t.Errorf("a bare pgp-signature part with no container protocol was missed: %+v", got)
	}
}

// The same split for encryption, for the same reason.
func TestDetectsPGPMIMEEncryptionFromTheContainerAlone(t *testing.T) {
	msg := parse(t, `From: alice@example.com
Content-Type: multipart/encrypted; boundary="c3";
 protocol="application/pgp-encrypted"
MIME-Version: 1.0

--c3
Content-Type: application/octet-stream

Version: 1
--c3
Content-Type: application/octet-stream

hQEMAw
--c3--
`)
	if got := Detect(msg); !got.PGPMIMEEncrypted {
		t.Errorf("an encrypted message whose only marker is the container protocol was missed: %+v", got)
	}
}
