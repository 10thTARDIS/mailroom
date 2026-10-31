// SPDX-License-Identifier: AGPL-3.0-only

package compose

// Compose-time signing, and the one interaction that would break it silently
// (#47 increment 5).
//
// Signing now happens at submit time and rewrites the STORED message, so the
// bytes that get signed are also the bytes that get filed in Sent. But the bytes
// that go on the WIRE are not quite those: SendPayload strips the Bcc header
// immediately before SMTP DATA, so blind recipients are disclosed to nobody.
//
// ⚠ That is a byte-level edit applied AFTER the signature exists, which is
// exactly the shape of change that invalidates one. It is safe here for a
// specific structural reason — Bcc is a MESSAGE header, and RFC 3156 signs the
// BODY entity, which pgpmime keeps entirely below the header block — and "safe
// for a specific structural reason" is worth a test rather than a paragraph,
// because a later change to either side would break it with no visible symptom
// on this side of the wire.

import (
	"bytes"
	"strings"
	"testing"

	"mailroom/internal/pgpmime"
)

// fakeSignature is a syntactically valid armored block. pgpmime checks for the
// BEGIN/END markers and refuses anything else, so a stub has to look like this
// much; nothing here verifies cryptography, which is internal/pgpkeyd's job.
const fakeSignature = "-----BEGIN PGP SIGNATURE-----\r\n" +
	"\r\niHUEARYIAB0WIQT" + "\r\n" +
	"-----END PGP SIGNATURE-----"

func signWithStub(t *testing.T, raw []byte) (signed []byte, canonical []byte) {
	t.Helper()
	out, err := pgpmime.Sign(raw, func(c []byte) (string, string, error) {
		canonical = append([]byte(nil), c...)
		return fakeSignature, "pgp-sha256", nil
	})
	if err != nil {
		t.Fatalf("pgpmime.Sign: %v", err)
	}
	return out, canonical
}

func bodyAfterHeaders(t *testing.T, raw []byte) []byte {
	t.Helper()
	i := bytes.Index(raw, []byte("\r\n\r\n"))
	if i < 0 {
		t.Fatalf("message has no header/body separator")
	}
	return raw[i+4:]
}

// TestSendPayloadDoesNotDisturbTheSignedBody is the assertion that matters:
// stripping Bcc must not move a single byte of the signed entity.
func TestSendPayloadDoesNotDisturbTheSignedBody(t *testing.T) {
	built, err := Build(Draft{
		From:     Address{Email: "me@example.com"},
		To:       []Address{{Email: "you@example.net"}},
		Bcc:      []Address{{Email: "quiet@example.org"}},
		Subject:  "signed and blind-copied",
		TextBody: "Body with an em-dash — so it is quoted-printable too.\n",
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	signed, _ := signWithStub(t, built.Raw)

	// Precondition, or the rest proves nothing: the stored/signed form must
	// still carry the Bcc the user typed.
	if !bytes.Contains(signed, []byte("Bcc: quiet@example.org")) {
		t.Fatal("the signed message lost the Bcc header — the stored copy must keep it")
	}

	wire := SendPayload(signed)

	if bytes.Contains(wire, []byte("Bcc:")) {
		t.Error("Bcc reached the wire — blind recipients are disclosed")
	}
	if bytes.Contains(wire, []byte("quiet@example.org")) {
		t.Error("the blind recipient's address reached the wire")
	}

	// THE POINT. Everything below the header block — the preamble, part one,
	// the signature part, every boundary — must be byte-identical. Any
	// difference at all means the recipient hashes something other than what
	// was signed, and sees "bad signature" while we log a clean send.
	if got, want := bodyAfterHeaders(t, wire), bodyAfterHeaders(t, signed); !bytes.Equal(got, want) {
		t.Errorf("SendPayload altered the signed body (%d bytes -> %d bytes); the signature "+
			"no longer covers what is transmitted", len(want), len(got))
	}
	if !bytes.Contains(wire, []byte("-----BEGIN PGP SIGNATURE-----")) {
		t.Error("the transmitted message has no signature part")
	}
}

// TestSignedPartIsSevenBitClean ties increment 5's two halves together: the
// quoted-printable change exists so that the entity RFC 3156 signs can survive
// a 7-bit hop. If the composer ever regresses to 8bit, this fails here rather
// than at a recipient's client.
func TestSignedPartIsSevenBitClean(t *testing.T) {
	built, err := Build(Draft{
		From:     Address{Email: "me@example.com"},
		To:       []Address{{Email: "you@example.net"}},
		TextBody: "Renée wrote — “naïve”, 日本語, 🎉\n",
		HTMLBody: "<p>Renée wrote — “naïve”</p>\n",
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	signed, canonical := signWithStub(t, built.Raw)

	for i := 0; i < len(canonical); i++ {
		if canonical[i] > 127 || canonical[i] == 0 {
			t.Fatalf("the SIGNED bytes contain a non-7-bit byte %#x at offset %d — the first "+
				"MTA without 8BITMIME will re-encode this and the signature will fail",
				canonical[i], i)
		}
	}
	for i := 0; i < len(signed); i++ {
		if signed[i] > 127 || signed[i] == 0 {
			t.Fatalf("the transmitted message contains a non-7-bit byte %#x at offset %d",
				signed[i], i)
		}
	}
	// Control on the fixture: a test that signed pure ASCII would pass whatever
	// the encoder did, so prove the input really did need encoding.
	if !strings.Contains(string(signed), "quoted-printable") {
		t.Error("the fixture did not exercise quoted-printable at all — this test would " +
			"pass against an 8bit composer")
	}
}

// TestSignatureCoversContentHeadersNotSubject records an RFC 3156 property that
// is a frequent source of false expectations: PGP/MIME does not authenticate
// the Subject line. Worth pinning so nobody later reports it as a bug, or —
// worse — "fixes" it by signing the message headers and breaking every verifier.
func TestSignatureCoversContentHeadersNotSubject(t *testing.T) {
	built, err := Build(Draft{
		From:     Address{Email: "me@example.com"},
		To:       []Address{{Email: "you@example.net"}},
		Subject:  "this subject is NOT signed",
		TextBody: "body\n",
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	_, canonical := signWithStub(t, built.Raw)

	if bytes.Contains(canonical, []byte("this subject is NOT signed")) {
		t.Error("the Subject was included in the signed entity; RFC 3156 signs the body " +
			"entity only, and a verifier will not expect it there")
	}
	if !bytes.Contains(canonical, []byte("Content-Type: text/plain")) {
		t.Error("the signed entity is missing its own Content-Type — RFC 3156 §5 requires " +
			"the content headers travel with the body into part one")
	}
}
