// SPDX-License-Identifier: AGPL-3.0-only

package pgpmime

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// fakeArmored is the shape Encrypt insists on. It is NOT a real message: this
// package holds no key and does no cryptography, so what is tested is the
// framing, which must be read back by this package's own SplitEncrypted and
// Rewrap (and, in internal/pgpkeyd's tests, by go-crypto and GnuPG).
const fakeArmored = "-----BEGIN PGP MESSAGE-----\n\nwcBMA1234567890ABCDEFGHIJKL\n=abcd\n-----END PGP MESSAGE-----\n"

func fakeEncrypter(seen *[]byte) EncryptFunc {
	return func(entity []byte) (string, error) {
		if seen != nil {
			*seen = append([]byte(nil), entity...)
		}
		return fakeArmored, nil
	}
}

const encMsg = "From: me@example.com\r\nTo: you@example.com\r\nSubject: hello\r\nAutocrypt: addr=me@example.com; keydata=AAAA\r\n" +
	"MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n" +
	"the body =E2=80=94 stays inside\r\n"

func TestEncryptFramingRoundTripsThroughSplitEncrypted(t *testing.T) {
	var entity []byte
	out, err := Encrypt([]byte(encMsg), fakeEncrypter(&entity))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if !IsEncrypted(out) {
		t.Fatal("output is not recognised as multipart/encrypted")
	}

	// The ciphertext part reads back as the armor the encrypter returned.
	ct, err := SplitEncrypted(out)
	if err != nil {
		t.Fatalf("SplitEncrypted rejects our own output: %v", err)
	}
	want := strings.TrimRight(strings.ReplaceAll(fakeArmored, "\n", "\r\n"), "\r\n")
	if string(ct) != want {
		t.Errorf("ciphertext part = %q, want %q", ct, want)
	}

	// The ENTITY handed to the encrypter carries the content headers and the body,
	// and nothing else.
	if !bytes.HasPrefix(entity, []byte("Content-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n")) ||
		!bytes.HasSuffix(entity, []byte("stays inside\r\n")) {
		t.Errorf("entity = %q", entity)
	}
	for _, leaked := range []string{"From:", "Subject:", "Autocrypt:", "MIME-Version"} {
		if bytes.Contains(entity, []byte(leaked)) {
			t.Errorf("the entity inside the ciphertext carries %q", leaked)
		}
	}

	// The OUTER message keeps every non-content header, in order, and none of the
	// body.
	head, _, _ := splitMessage(out)
	h := string(head)
	if i, j := strings.Index(h, "From:"), strings.Index(h, "Subject:"); i < 0 || j < i {
		t.Errorf("outer headers lost or reordered:\n%s", h)
	}
	if !strings.Contains(h, "Autocrypt: addr=me@example.com") {
		t.Errorf("the Autocrypt header did not stay on the outer message:\n%s", h)
	}
	if strings.Count(h, "Content-Type:") != 1 || strings.Count(h, "MIME-Version") != 1 {
		t.Errorf("outer Content-Type/MIME-Version duplicated or missing:\n%s", h)
	}
	if strings.Contains(string(out), "stays inside") {
		t.Error("the body appears in the clear")
	}

	// And decrypt-on-open's rewrap puts the entity back under the outer headers.
	back, err := Rewrap(out, entity)
	if err != nil {
		t.Fatalf("Rewrap: %v", err)
	}
	if !bytes.Contains(back, []byte("stays inside")) || !bytes.Contains(back, []byte("Subject: hello")) {
		t.Errorf("rewrapped message lost the body or the Subject:\n%s", back)
	}
}

func TestEncryptRefusesWhatIsNotACiphertext(t *testing.T) {
	cases := map[string]EncryptFunc{
		"empty": func([]byte) (string, error) { return "", nil },
		"a public key": func([]byte) (string, error) {
			return "-----BEGIN PGP PUBLIC KEY BLOCK-----\n\n-----END PGP PUBLIC KEY BLOCK-----\n", nil
		},
		"a signature": func([]byte) (string, error) {
			return "-----BEGIN PGP SIGNATURE-----\n\n-----END PGP SIGNATURE-----\n", nil
		},
		"begin but no end": func([]byte) (string, error) { return "-----BEGIN PGP MESSAGE-----\n\nxx\n", nil },
	}
	for name, enc := range cases {
		if out, err := Encrypt([]byte(encMsg), enc); err == nil {
			t.Errorf("%s: accepted, producing %d bytes that look encrypted and are not", name, len(out))
		}
	}
	boom := errors.New("boom")
	if _, err := Encrypt([]byte(encMsg), func([]byte) (string, error) { return "", boom }); !errors.Is(err, boom) {
		t.Errorf("an encrypter error was not propagated: %v", err)
	}
	if _, err := Encrypt([]byte(encMsg), nil); err == nil {
		t.Error("nil encrypter accepted")
	}
	if _, err := Encrypt([]byte("no separator at all"), fakeEncrypter(nil)); err == nil {
		t.Error("a message with no header/body separator was accepted")
	}
}

// A message that was signed first: the signed container is the entity.
func TestEncryptOfASignedMessageEncryptsTheMultipartSigned(t *testing.T) {
	signed, err := Sign([]byte(encMsg), newSigner(t).sign)
	if err != nil {
		t.Fatal(err)
	}
	var entity []byte
	if _, err := Encrypt(signed, fakeEncrypter(&entity)); err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(entity, []byte("Content-Type: multipart/signed;")) {
		t.Errorf("the entity is not the multipart/signed container: %.80q", entity)
	}
	if _, err := SplitSigned(append([]byte("MIME-Version: 1.0\r\n"), entity...)); err != nil {
		t.Errorf("the entity inside the ciphertext does not split as multipart/signed: %v", err)
	}
}
