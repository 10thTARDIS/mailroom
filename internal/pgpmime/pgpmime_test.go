// SPDX-License-Identifier: AGPL-3.0-only

package pgpmime

// The point of this suite is NOT that Sign produces something shaped like
// multipart/signed. Anything can produce that. It is that a verifier holding
// only the public key accepts the result, and that the same verifier rejects it
// when a single byte moves — because a verification that cannot fail says
// nothing about the one that passed.

import (
	"bytes"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

// ---------------------------------------------------------------------------
// A real key and a real signer.
// ---------------------------------------------------------------------------

var (
	entityOnce sync.Once
	entity     *openpgp.Entity
	entityErr  error
)

// testEntity is generated once: Ed25519 is fast, but not free, and every test
// here wants the same key so a failure is never about which key was used.
func testEntity(t *testing.T) *openpgp.Entity {
	t.Helper()
	entityOnce.Do(func() {
		entity, entityErr = openpgp.NewEntity("Signer", "", "signer@example.com", &packet.Config{
			Algorithm: packet.PubKeyAlgoEdDSA,
			Curve:     packet.Curve25519,
			V6Keys:    false,
			Time:      func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) },
		})
	})
	if entityErr != nil {
		t.Fatalf("generate test key: %v", entityErr)
	}
	return entity
}

// recordingSigner signs for real and keeps the bytes it was asked to sign, so a
// test can assert on what was actually hashed rather than on what it assumes
// was hashed.
type recordingSigner struct {
	t      *testing.T
	seen   []byte
	called int
}

func (s *recordingSigner) sign(canonical []byte) (string, string, error) {
	s.called++
	s.seen = append([]byte(nil), canonical...)
	var buf bytes.Buffer
	if err := openpgp.ArmoredDetachSign(&buf, testEntity(s.t), bytes.NewReader(canonical), nil); err != nil {
		return "", "", err
	}
	return buf.String(), "pgp-sha256", nil
}

func newSigner(t *testing.T) *recordingSigner { return &recordingSigner{t: t} }

// ---------------------------------------------------------------------------
// Reading the result back the way a recipient does.
// ---------------------------------------------------------------------------

// assembled is a parsed multipart/signed message: the top-level headers, the
// Content-Type parameters, and the RAW bytes of each part.
//
// The parts are sliced out of the message rather than read through
// mime/multipart, because a verifier needs each part's own header bytes exactly
// as transmitted and multipart.Part hands back a parsed header map instead.
// Re-serialising that map would be this test agreeing with itself.
type assembled struct {
	header    mail.Header
	mediaType string
	params    map[string]string
	parts     [][]byte
}

func parseAssembled(t *testing.T, msg []byte) assembled {
	t.Helper()

	m, err := mail.ReadMessage(bytes.NewReader(msg))
	if err != nil {
		t.Fatalf("the assembled message does not parse as RFC 5322: %v", err)
	}
	mediaType, params, err := mime.ParseMediaType(m.Header.Get("Content-Type"))
	if err != nil {
		t.Fatalf("top-level Content-Type does not parse: %v", err)
	}
	body, err := io.ReadAll(m.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	boundary := params["boundary"]
	if boundary == "" {
		t.Fatal("top-level Content-Type carries no boundary")
	}

	// RFC 2046 §5.1.1: the delimiter is CRLF + "--" + boundary. The leading CRLF
	// is the delimiter's, which is why it is included here and therefore NOT
	// left on the end of the preceding part.
	delim := []byte("\r\n--" + boundary)
	pos := bytes.Index(body, delim)
	if pos < 0 {
		t.Fatalf("no boundary delimiter in the body:\n%s", body)
	}
	var parts [][]byte
	for {
		rest := body[pos+len(delim):]
		if bytes.HasPrefix(rest, []byte("--")) {
			break
		}
		if !bytes.HasPrefix(rest, []byte("\r\n")) {
			t.Fatalf("delimiter is not followed by CRLF: %.20q", rest)
		}
		start := pos + len(delim) + 2
		next := bytes.Index(body[start:], delim)
		if next < 0 {
			t.Fatal("a part is not terminated by a delimiter")
		}
		parts = append(parts, body[start:start+next])
		pos = start + next
	}
	return assembled{header: m.Header, mediaType: mediaType, params: params, parts: parts}
}

// entityBody returns a part's content, after its own headers.
func entityBody(t *testing.T, part []byte) []byte {
	t.Helper()
	i := bytes.Index(part, []byte("\r\n\r\n"))
	if i < 0 {
		t.Fatalf("part has no header/body separator: %.60q", part)
	}
	return part[i+4:]
}

// verifyPart is the recipient's job in one line: canonicalise what arrived,
// then check the detached signature against it with the PUBLIC key only.
func verifyPart(t *testing.T, partOne, signature []byte) error {
	t.Helper()
	_, err := openpgp.CheckArmoredDetachedSignature(
		openpgp.EntityList{testEntity(t)},
		bytes.NewReader(Canonicalize(partOne)),
		bytes.NewReader(signature),
		nil)
	return err
}

const testHeaders = "From: Signer <signer@example.com>\r\n" +
	"To: Recipient <recipient@example.com>\r\n" +
	"Subject: A signed message\r\n" +
	"Date: Thu, 01 Jan 2026 00:00:00 +0000\r\n" +
	"Message-ID: <abc@example.com>\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"Content-Transfer-Encoding: 7bit\r\n"

// ---------------------------------------------------------------------------
// The round trip, and its negative controls.
// ---------------------------------------------------------------------------

func TestSignedMessageVerifiesAgainstTheRealLibrary(t *testing.T) {
	raw := []byte(testHeaders + "\r\nHello, world.\r\n")

	out, err := Sign(raw, newSigner(t).sign)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	got := parseAssembled(t, out)
	if got.mediaType != "multipart/signed" {
		t.Fatalf("media type = %q, want multipart/signed", got.mediaType)
	}
	if got.params["protocol"] != SignatureProtocol {
		t.Errorf("protocol = %q, want %q", got.params["protocol"], SignatureProtocol)
	}
	if got.params["micalg"] != "pgp-sha256" {
		t.Errorf("micalg = %q, want %q", got.params["micalg"], "pgp-sha256")
	}
	if len(got.parts) != 2 {
		t.Fatalf("got %d parts, want 2", len(got.parts))
	}

	sig := entityBody(t, got.parts[1])
	if err := verifyPart(t, got.parts[0], sig); err != nil {
		t.Fatalf("a verifier holding only the public key rejected the message: %v", err)
	}

	// ── NEGATIVE CONTROL 1: one byte of the body.
	t.Run("a tampered body fails verification", func(t *testing.T) {
		bad := append([]byte(nil), got.parts[0]...)
		bad[len(bad)-3] ^= 0x01
		if err := verifyPart(t, bad, sig); err == nil {
			t.Error("verification accepted a tampered body — it cannot observe the failure it claims to exclude")
		}
	})

	// ── NEGATIVE CONTROL 2: the CONTENT HEADERS, which is the whole reason part
	// one is the entity and not just the payload. If this passes, the signature
	// does not actually cover the Content-Type and an attacker may retype the
	// message as text/html.
	t.Run("a tampered content header fails verification", func(t *testing.T) {
		bad := bytes.Replace(got.parts[0], []byte("text/plain"), []byte("text/htmL"), 1)
		if bytes.Equal(bad, got.parts[0]) {
			t.Fatal("the substitution did not apply — this control proves nothing")
		}
		if err := verifyPart(t, bad, sig); err == nil {
			t.Error("verification accepted a retyped content header — the content headers are not signed")
		}
	})

	// ── NEGATIVE CONTROL 3: the off-by-one CRLF. Dropping the part's final line
	// ending is the single most common PGP/MIME defect, and it must be visible.
	t.Run("dropping the final CRLF fails verification", func(t *testing.T) {
		bad := bytes.TrimSuffix(got.parts[0], []byte("\r\n"))
		if bytes.Equal(bad, got.parts[0]) {
			t.Fatal("part one did not end in CRLF — this control proves nothing")
		}
		if err := verifyPart(t, bad, sig); err == nil {
			t.Error("verification accepted a part missing its final CRLF")
		}
	})
}

// TestPartOneIsTheEntityNotThePayload pins the header partition: content
// headers travel into the signed part, message headers stay outside it.
func TestPartOneIsTheEntityNotThePayload(t *testing.T) {
	out, err := Sign([]byte(testHeaders+"\r\nbody\r\n"), newSigner(t).sign)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	got := parseAssembled(t, out)

	for _, h := range []string{"From", "To", "Subject", "Date", "Message-ID"} {
		if got.header.Get(h) == "" {
			t.Errorf("top-level lost the %s header", h)
		}
	}
	if got.header.Get("Content-Transfer-Encoding") != "" {
		t.Error("Content-Transfer-Encoding stayed at the top level; it describes the content and belongs in part one")
	}
	if got.header.Get("MIME-Version") != "1.0" {
		t.Errorf("MIME-Version = %q, want 1.0", got.header.Get("MIME-Version"))
	}

	partOne := string(got.parts[0])
	for _, want := range []string{
		"Content-Type: text/plain; charset=utf-8\r\n",
		"Content-Transfer-Encoding: 7bit\r\n",
	} {
		if !strings.Contains(partOne, want) {
			t.Errorf("part one is missing %q:\n%s", want, partOne)
		}
	}
	if strings.Contains(partOne, "Subject:") {
		t.Error("part one carries the Subject header; RFC 3156 does not sign it and it must not be duplicated")
	}
	// PGP/MIME does not authenticate the message headers. Asserted here so that
	// nobody later reads the passing round-trip test as proof that it does.
	if strings.Contains(partOne, "From:") {
		t.Error("part one carries the From header")
	}
}

// TestOriginalBodyIsBytePreserved is the assertion that the signature covers
// what the recipient will actually see. A body silently re-encoded on the way
// into part one would still verify — against bytes nobody was ever sent.
func TestOriginalBodyIsBytePreserved(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"trailing whitespace", "line with spaces   \r\nline with tab\t\r\n"},
		{"bare LF", "one\ntwo\nthree\n"},
		{"no trailing newline", "no newline at the end"},
		{"blank lines", "one\r\n\r\n\r\ntwo\r\n"},
		{"quoted-printable soft breaks", "a very long line that was wrapped=\r\nhere\r\n"},
		{"high bytes", "caf\xc3\xa9 \xe2\x80\x94 done\r\n"},
		{"empty body", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newSigner(t)
			out, err := Sign([]byte(testHeaders+"\r\n"+tc.body), s.sign)
			if err != nil {
				t.Fatalf("Sign: %v", err)
			}
			got := parseAssembled(t, out)
			body := entityBody(t, got.parts[0])
			if !bytes.Equal(body, []byte(tc.body)) {
				t.Errorf("part one body\n = %q\nwant %q", body, tc.body)
			}
			if err := verifyPart(t, got.parts[0], entityBody(t, got.parts[1])); err != nil {
				t.Errorf("verification failed for a byte-preserved body: %v", err)
			}
		})
	}
}

// TestSignerReceivesTheCanonicalEntity looks at the bytes that were hashed,
// which is the only place the RFC 3156 §5 rules can be checked directly. Every
// other test here would still pass if the canonicalisation were skipped, so
// long as it were skipped consistently on both sides.
func TestSignerReceivesTheCanonicalEntity(t *testing.T) {
	s := newSigner(t)
	body := "trailing spaces here   \nand a tab\t\nplain\n"
	if _, err := Sign([]byte(testHeaders+"\r\n"+body), s.sign); err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if s.called != 1 {
		t.Fatalf("signer called %d times, want 1", s.called)
	}

	seen := s.seen
	if !bytes.HasPrefix(seen, []byte("Content-Type: text/plain; charset=utf-8\r\n")) {
		t.Errorf("the signed bytes do not begin with the entity's content headers:\n%q", seen)
	}
	if !bytes.Contains(seen, []byte("\r\n\r\n")) {
		t.Error("the signed bytes contain no header/body separator, so the headers were not signed with the body")
	}
	for _, line := range bytes.Split(seen, []byte("\r\n")) {
		if bytes.Contains(line, []byte("\n")) {
			t.Errorf("a bare LF survived canonicalisation: %q", line)
		}
		if len(line) > 0 && (line[len(line)-1] == ' ' || line[len(line)-1] == '\t') {
			t.Errorf("trailing whitespace survived canonicalisation: %q", line)
		}
	}
	if !bytes.Equal(seen, Canonicalize(seen)) {
		t.Error("the signed bytes are not a fixed point of Canonicalize")
	}
}

// ---------------------------------------------------------------------------
// The boundary.
// ---------------------------------------------------------------------------

// TestBoundaryCollisionIsAvoided exercises the regeneration path, which 144
// bits of randomness make unreachable in production. A colliding boundary does
// not fail loudly — the recipient's parser truncates part one at the wrong
// place, and the signature then covers content that was never delivered.
func TestBoundaryCollisionIsAvoided(t *testing.T) {
	const colliding = "mailroom-deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"

	restore := boundarySource
	t.Cleanup(func() { boundarySource = restore })
	calls := 0
	boundarySource = func() (string, error) {
		calls++
		if calls == 1 {
			return colliding, nil
		}
		return "mailroom-replacement-" + strconv.Itoa(calls), nil
	}

	body := "a reply quoting something hostile:\r\n--" + colliding + "\r\nand more text\r\n"
	out, err := Sign([]byte(testHeaders+"\r\n"+body), newSigner(t).sign)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if calls < 2 {
		t.Fatalf("the regeneration path never ran (boundarySource called %d times) — this test proves nothing", calls)
	}

	got := parseAssembled(t, out)
	if got.params["boundary"] == colliding {
		t.Fatal("the colliding boundary was used")
	}
	if len(got.parts) != 2 {
		t.Fatalf("got %d parts, want 2 — the message did not survive", len(got.parts))
	}
	if !bytes.Contains(got.parts[0], []byte(colliding)) {
		t.Error("the colliding text was dropped from part one instead of being kept")
	}
	if err := verifyPart(t, got.parts[0], entityBody(t, got.parts[1])); err != nil {
		t.Errorf("verification failed after boundary regeneration: %v", err)
	}

	// And it still parses through a parser that is not this test's.
	assertParsesAsMultipartSigned(t, out)
}

// TestBoundarySearchGivesUp proves the loop is bounded. Without it, a
// boundarySource that always collided would hang rather than fail.
func TestBoundarySearchGivesUp(t *testing.T) {
	const always = "mailroom-always-collides"
	restore := boundarySource
	t.Cleanup(func() { boundarySource = restore })
	boundarySource = func() (string, error) { return always, nil }

	_, err := Sign([]byte(testHeaders+"\r\nsee --"+always+" here\r\n"), newSigner(t).sign)
	if err == nil {
		t.Fatal("Sign accepted a boundary that appears in the content")
	}
}

// TestRandomBoundariesDiffer is the control for the test above: the real source
// must not be returning a constant, or the collision check would be checking a
// fixed string against itself forever.
func TestRandomBoundariesDiffer(t *testing.T) {
	seen := make(map[string]bool)
	for range 16 {
		b, err := randomBoundary()
		if err != nil {
			t.Fatalf("randomBoundary: %v", err)
		}
		if seen[b] {
			t.Fatalf("randomBoundary repeated %q", b)
		}
		if len(b) > 70 {
			t.Errorf("boundary is %d chars, RFC 2046 allows 70", len(b))
		}
		seen[b] = true
	}
}

// assertParsesAsMultipartSigned runs the output through Go's own MIME parser,
// which knows nothing about this package's assumptions.
func assertParsesAsMultipartSigned(t *testing.T, msg []byte) {
	t.Helper()
	m, err := mail.ReadMessage(bytes.NewReader(msg))
	if err != nil {
		t.Fatalf("mail.ReadMessage: %v", err)
	}
	_, params, err := mime.ParseMediaType(m.Header.Get("Content-Type"))
	if err != nil {
		t.Fatalf("ParseMediaType: %v", err)
	}
	mr := multipart.NewReader(m.Body, params["boundary"])
	var types []string
	for {
		p, err := mr.NextRawPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("multipart read: %v", err)
		}
		types = append(types, p.Header.Get("Content-Type"))
		if _, err := io.Copy(io.Discard, p); err != nil {
			t.Fatalf("read part: %v", err)
		}
	}
	if len(types) != 2 {
		t.Fatalf("mime/multipart found %d parts, want 2", len(types))
	}
	if !strings.HasPrefix(types[1], SignatureProtocol) {
		t.Errorf("part two type = %q, want %s", types[1], SignatureProtocol)
	}
}

// ---------------------------------------------------------------------------
// Refusals.
// ---------------------------------------------------------------------------

func TestSignRefusesBadInput(t *testing.T) {
	t.Run("no header/body separator", func(t *testing.T) {
		_, err := Sign([]byte("From: a@example.com\r\nSubject: no body\r\n"), newSigner(t).sign)
		if !errors.Is(err, ErrNoBody) {
			t.Errorf("err = %v, want ErrNoBody", err)
		}
	})

	t.Run("nil signer", func(t *testing.T) {
		if _, err := Sign([]byte(testHeaders+"\r\nx\r\n"), nil); err == nil {
			t.Error("accepted a nil signer")
		}
	})

	t.Run("signer failure is reported", func(t *testing.T) {
		boom := errors.New("the crypto process said no")
		_, err := Sign([]byte(testHeaders+"\r\nx\r\n"),
			func([]byte) (string, string, error) { return "", "", boom })
		if !errors.Is(err, boom) {
			t.Errorf("err = %v, want it to wrap the signer's error", err)
		}
	})

	// A signer that returns anything other than a signature must not produce a
	// message that merely LOOKS signed: the failure would then surface at the
	// recipient, as "bad signature" rather than as "we did not sign this".
	t.Run("signer returned something that is not a signature", func(t *testing.T) {
		for _, bad := range []string{
			"", "not a signature at all",
			"-----BEGIN PGP PUBLIC KEY BLOCK-----\nx\n-----END PGP PUBLIC KEY BLOCK-----\n",
			sigArmorBegin + "\nno end marker\n",
		} {
			_, err := Sign([]byte(testHeaders+"\r\nx\r\n"),
				func([]byte) (string, string, error) { return bad, "pgp-sha256", nil })
			if err == nil {
				t.Errorf("accepted %.40q as a signature", bad)
			}
		}
	})

	// micalg goes straight into a header. CR/LF in it would inject one.
	t.Run("micalg is refused unless it is a token", func(t *testing.T) {
		for _, bad := range []string{
			"", "   ",
			"pgp-sha256\r\nBcc: attacker@example.com",
			"pgp-sha256\"; boundary=\"x",
			"pgp sha256",
		} {
			out, err := Sign([]byte(testHeaders+"\r\nx\r\n"),
				func(c []byte) (string, string, error) {
					var buf bytes.Buffer
					if err := openpgp.ArmoredDetachSign(&buf, testEntity(t), bytes.NewReader(c), nil); err != nil {
						return "", "", err
					}
					return buf.String(), bad, nil
				})
			if err == nil {
				t.Errorf("accepted micalg %q, producing:\n%s", bad, out)
			}
		}
	})
}

// TestSignHandlesAMultipartBody covers the realistic case — a message with an
// attachment — where part one is itself a multipart entity with its own
// boundary that the outer one must not collide with.
func TestSignHandlesAMultipartBody(t *testing.T) {
	inner := "inner-boundary-1234"
	raw := "From: Signer <signer@example.com>\r\n" +
		"Subject: With an attachment\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=\"" + inner + "\"\r\n" +
		"\r\n" +
		"--" + inner + "\r\n" +
		"Content-Type: text/plain\r\n" +
		"\r\n" +
		"see attached\r\n" +
		"--" + inner + "\r\n" +
		"Content-Type: application/octet-stream\r\n" +
		"Content-Transfer-Encoding: base64\r\n" +
		"\r\n" +
		"AAECAwQF\r\n" +
		"--" + inner + "--\r\n"

	out, err := Sign([]byte(raw), newSigner(t).sign)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	got := parseAssembled(t, out)
	if got.params["boundary"] == inner {
		t.Fatal("the outer boundary equals the inner one")
	}
	if len(got.parts) != 2 {
		t.Fatalf("got %d parts, want 2", len(got.parts))
	}
	if !bytes.HasPrefix(got.parts[0], []byte("Content-Type: multipart/mixed; boundary=\""+inner+"\"\r\n")) {
		t.Errorf("part one does not begin with the original multipart headers:\n%.120q", got.parts[0])
	}
	if err := verifyPart(t, got.parts[0], entityBody(t, got.parts[1])); err != nil {
		t.Errorf("verification failed for a multipart body: %v", err)
	}
	assertParsesAsMultipartSigned(t, out)
}

// TestFoldedHeadersSurvive keeps continuation lines attached to their field.
// A Content-Type split across lines that lost its continuation would change the
// entity's type, and the signature would still be perfectly valid over it.
func TestFoldedHeadersSurvive(t *testing.T) {
	raw := "From: Signer <signer@example.com>\r\n" +
		"Subject: folded\r\n" +
		"Content-Type: text/plain;\r\n" +
		"\tcharset=utf-8;\r\n" +
		" format=flowed\r\n" +
		"\r\n" +
		"body\r\n"

	out, err := Sign([]byte(raw), newSigner(t).sign)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	got := parseAssembled(t, out)
	want := "Content-Type: text/plain;\r\n\tcharset=utf-8;\r\n format=flowed\r\n"
	if !bytes.HasPrefix(got.parts[0], []byte(want)) {
		t.Errorf("folded Content-Type was not preserved:\n%.120q\nwant prefix %q", got.parts[0], want)
	}
	// And the folded header must still parse to the same media type once
	// unfolded, which is what a receiving parser will do with it.
	folded := strings.SplitN(string(got.parts[0]), "\r\n\r\n", 2)[0]
	mt, params, err := mime.ParseMediaType(
		strings.ReplaceAll(strings.TrimPrefix(folded, "Content-Type: "), "\r\n", ""))
	if err != nil {
		t.Fatalf("part one Content-Type does not parse: %v", err)
	}
	if mt != "text/plain" || params["charset"] != "utf-8" || params["format"] != "flowed" {
		t.Errorf("part one Content-Type = %q %v", mt, params)
	}
	if err := verifyPart(t, got.parts[0], entityBody(t, got.parts[1])); err != nil {
		t.Errorf("verification failed with folded headers: %v", err)
	}
}
