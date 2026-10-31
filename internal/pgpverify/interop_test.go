// SPDX-License-Identifier: AGPL-3.0-only

//go:build interop

package pgpverify_test

// CROSS-IMPLEMENTATION INTEROPERABILITY (Forgejo #263, refs #47 increment 6a).
//
// # The circularity this exists to break
//
// v1.33.0 reported its signed messages verified on a live mailbox, and that
// result was worth having — but look at how the two halves were validated:
//
//   - pgpmime.SplitSigned was tested by ROUND TRIP against our own pgpmime.Sign;
//   - on the instance, Verify checked signatures our own pipeline produced.
//
// The cryptography is not the circular part: ProtonMail/go-crypto signs and
// verifies and it is not ours. What is ours on BOTH sides is the FRAMING — the
// decision about which bytes get hashed. If Sign and SplitSigned shared one
// off-by-one on which CRLF belongs to the boundary delimiter, they would agree
// with each other perfectly, every test would pass, the report would read 4/4,
// and every other OpenPGP implementation in the world would say "bad signature".
//
// So both directions are checked here against GnuPG, which shares no code with
// us at all:
//
//	TestGPGVerifiesAMessageMailroomSigned  — our producer, their verifier
//	TestWeVerifyAMessageGPGSigned          — their producer, our reader
//
// The second is the stronger of the two and is the direction real correspondent
// mail arrives from.
//
// # ⚠ Both are useless without negative controls, so both have them
//
// A test that hands gpg some bytes and a signature can pass because everything
// is correct, or because the assertion never had a way to fail. #131 is this
// project's cautionary tale: three security assertions passed against a page
// that could not have loaded at all. Every check below is paired with a
// tampered variant that MUST be rejected — including one that changes nothing
// but a single trailing CRLF, which is precisely the off-by-one the whole file
// is about.
//
// # ⚠ Why this fails rather than skips when gpg is missing
//
// #263 raised the risk directly: a test that skips when the tool is absent
// "risks silently never running — which would be its own version of this
// problem." It does. So absence of gpg is a FAILURE here, not a skip, and
// opting out requires deliberately setting MAILROOM_SKIP_GPG_INTEROP=1 — an
// explicit act that shows up in the run.
//
// The build tag keeps it out of the ordinary `go test ./...` (the CI runner is
// not guaranteed to have gpg); scripts/pgp-interop-test.sh runs it inside
// golang:1.26, which ships GnuPG 2.4.7, and CI runs that script as its own job.
// Cheap to run, and impossible to skip without saying so.

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"

	"mailroom/internal/pgpmime"
	"mailroom/internal/pgpverify"
)

// gpgRun is one invocation of gpg. stdout is returned SEPARATELY from the log
// because some invocations emit BINARY on stdout (`--export`), and gpg narrates
// on stderr no matter what: concatenating the two appends human-readable text
// to a key and produces a fixture that is subtly, invisibly corrupt.
//
// ⚠ That is not hypothetical — the first version of this file did exactly that,
// and the resulting failure read as "Mailroom rejected a signature GnuPG
// produced", i.e. as the very interop defect the test exists to detect. A
// fixture bug that impersonates the finding is the worst kind (#260), so the
// split is deliberate and the key is parsed below before anything is concluded
// from it.
type gpgRun func(stdin []byte, args ...string) (stdout []byte, log string, err error)

// gpgHome prepares an isolated GnuPG home and returns a runner bound to it.
// Isolated so the test never touches, reads, or writes a developer's real
// keyring.
func gpgHome(t *testing.T) gpgRun {
	t.Helper()

	if os.Getenv("MAILROOM_SKIP_GPG_INTEROP") == "1" {
		t.Skip("MAILROOM_SKIP_GPG_INTEROP=1: cross-implementation interop deliberately not run")
	}
	if _, err := exec.LookPath("gpg"); err != nil {
		// ⚠ NOT a skip. See the file header: an interop test that quietly does
		// not run is the same defect as the circular verification it replaces.
		t.Fatalf("gpg is not on PATH, so nothing here checked interoperability with an "+
			"independent implementation (%v). Run scripts/pgp-interop-test.sh, or set "+
			"MAILROOM_SKIP_GPG_INTEROP=1 to opt out on purpose.", err)
	}

	home := t.TempDir()
	return func(stdin []byte, args ...string) ([]byte, string, error) {
		full := append([]string{
			"--homedir", home,
			"--batch", "--yes", "--no-tty",
			// Keep the agent from prompting or lingering on a build machine.
			"--pinentry-mode", "loopback", "--passphrase", "",
		}, args...)
		cmd := exec.Command("gpg", full...)
		if stdin != nil {
			cmd.Stdin = bytes.NewReader(stdin)
		}
		var out, errb bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &errb
		err := cmd.Run()
		// gpg says everything interesting on stderr, including the verification
		// verdict, so the LOG is both streams — but stdout stays clean.
		return out.Bytes(), out.String() + errb.String(), err
	}
}

// literalPart pulls one part out of a multipart body WITHOUT using
// pgpmime.SplitSigned.
//
// ⚠ THIS INDEPENDENCE IS THE POINT. Extracting with SplitSigned and then asking
// gpg to verify would test that gpg agrees with SplitSigned about a signature
// Sign produced — which is the circle #263 describes, drawn one step wider. This
// is written straight from RFC 2046 §5.1.1 instead: a part begins after
// "--boundary CRLF" and ends immediately BEFORE the "CRLF--boundary" that
// follows it. That CRLF belongs to the delimiter, not to the part.
func literalPart(t *testing.T, msg []byte, boundary string, index int) []byte {
	t.Helper()
	open := []byte("--" + boundary + "\r\n")
	closeAny := []byte("\r\n--" + boundary)

	rest := msg
	for i := 0; ; i++ {
		start := bytes.Index(rest, open)
		if start < 0 {
			t.Fatalf("no part %d: opening delimiter for boundary %q not found", index, boundary)
		}
		rest = rest[start+len(open):]
		end := bytes.Index(rest, closeAny)
		if end < 0 {
			t.Fatalf("part %d is never closed by a delimiter", i)
		}
		if i == index {
			return rest[:end]
		}
		rest = rest[end:]
	}
}

// boundaryOf reads the boundary parameter out of the top-level Content-Type,
// again without our own parser.
func boundaryOf(t *testing.T, msg []byte) string {
	t.Helper()
	const marker = `boundary="`
	i := bytes.Index(msg, []byte(marker))
	if i < 0 {
		t.Fatal("the signed message declares no quoted boundary parameter")
	}
	rest := msg[i+len(marker):]
	j := bytes.IndexByte(rest, '"')
	if j < 0 {
		t.Fatal("unterminated boundary parameter")
	}
	return string(rest[:j])
}

// signedFixture is a message with the properties that make framing hard:
// a body whose final line ends in CRLF, an empty line inside it, and non-ASCII
// content that a transfer encoding has to survive.
const signedFixture = "From: Signer <signer@example.com>\r\n" +
	"To: Correspondent <them@example.com>\r\n" +
	"Subject: Interop\r\n" +
	"Date: Mon, 01 Jan 2024 00:00:00 +0000\r\n" +
	"Message-ID: <interop@example.com>\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"Content-Transfer-Encoding: quoted-printable\r\n" +
	"\r\n" +
	"One line, an em dash =E2=80=94 and an accent =C3=A9.\r\n" +
	"\r\n" +
	"A final line that ends with a newline.\r\n"

// TestGPGVerifiesAMessageMailroomSigned: our producer, their verifier.
//
// If Sign puts the delimiter's CRLF in the wrong place, the bytes an
// independent extractor recovers differ from the bytes Sign hashed, and gpg
// rejects. That is the failure this is here to catch and that a round trip
// against ourselves cannot.
func TestGPGVerifiesAMessageMailroomSigned(t *testing.T) {
	gpg := gpgHome(t)

	entity, err := openpgp.NewEntity("Interop Signer", "", "signer@example.com", nil)
	if err != nil {
		t.Fatalf("generate signing key: %v", err)
	}

	signed, err := pgpmime.Sign([]byte(signedFixture), func(canonical []byte) (string, string, error) {
		var sig bytes.Buffer
		if err := openpgp.ArmoredDetachSign(&sig, entity, bytes.NewReader(canonical), nil); err != nil {
			return "", "", err
		}
		return sig.String(), "pgp-sha256", nil
	})
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	// Give gpg the public half, armored.
	var pub bytes.Buffer
	if err := entity.Serialize(&pub); err != nil {
		t.Fatalf("serialize public key: %v", err)
	}
	if _, log, err := gpg(pub.Bytes(), "--import"); err != nil {
		t.Fatalf("gpg --import: %v\n%s", err, log)
	}

	boundary := boundaryOf(t, signed)
	entityBytes := literalPart(t, signed, boundary, 0)
	sigPart := literalPart(t, signed, boundary, 1)

	// The armored signature begins after part two's own headers.
	armor := sigPart[bytes.Index(sigPart, []byte("\r\n\r\n"))+4:]

	// ⚠ CONTROL ON THE COMPARISON ITSELF. Sign hashes Canonicalize(part one),
	// while gpg hashes whatever file it is handed, byte for byte. Those are the
	// same thing only if canonicalisation is a no-op here — and if it is not, a
	// green result below would be telling us about our own Canonicalize rather
	// than about the wire format. Assert it, so gpg is verifying the bytes as
	// they actually appear in the message.
	if !bytes.Equal(pgpmime.Canonicalize(entityBytes), entityBytes) {
		t.Fatal("part one is not already in canonical form, so gpg would be verifying " +
			"OUR canonicalisation rather than the transmitted bytes — this test would no " +
			"longer be independent and the fixture needs fixing")
	}

	dir := t.TempDir()
	dataPath := filepath.Join(dir, "entity.bin")
	sigPath := filepath.Join(dir, "entity.sig")
	writeFile(t, sigPath, armor)

	verify := func(data []byte) (string, error) {
		writeFile(t, dataPath, data)
		_, log, err := gpg(nil, "--verify", sigPath, dataPath)
		return log, err
	}

	log, err := verify(entityBytes)
	if err != nil {
		t.Fatalf("⚠ GnuPG REJECTED a signature Mailroom produced — our framing disagrees "+
			"with an independent implementation, which is exactly what #263 was opened "+
			"about: %v\n%s", err, log)
	}
	if !strings.Contains(log, "Good signature") {
		t.Errorf("gpg did not report a good signature:\n%s", log)
	}

	// ---- NEGATIVE CONTROLS --------------------------------------------------
	// Without these, `verify` could be returning success for any input and every
	// assertion above would be vacuous.
	t.Run("a tampered body is rejected", func(t *testing.T) {
		bad := bytes.Replace(entityBytes, []byte("em dash"), []byte("em dosh"), 1)
		if bytes.Equal(bad, entityBytes) {
			t.Fatal("the tamper did not change anything; this control proves nothing")
		}
		if out, err := verify(bad); err == nil {
			t.Errorf("gpg ACCEPTED a modified body — the check above cannot fail and "+
				"therefore proved nothing:\n%s", out)
		}
	})

	t.Run("one missing trailing CRLF is rejected", func(t *testing.T) {
		// ⚠ THE OFF-BY-ONE ITSELF. This is the single byte-pair that separates
		// "the CRLF belongs to the delimiter" from "the CRLF belongs to the
		// part". If gpg accepted both, this whole file would be unable to
		// distinguish the two framings and would not be testing anything.
		short := bytes.TrimSuffix(entityBytes, []byte("\r\n"))
		if len(short) == len(entityBytes) {
			t.Fatal("part one does not end in CRLF, so this control is inert — pick a " +
				"fixture whose body ends with a newline")
		}
		if out, err := verify(short); err == nil {
			t.Errorf("gpg accepted part one with its final CRLF removed, so this suite "+
				"cannot tell the two framings apart:\n%s", out)
		}
	})
}

// TestWeVerifyAMessageGPGSigned: their producer, our reader. The direction real
// correspondent mail arrives from, and the stronger half — nothing about the
// signed bytes originates in our code.
func TestWeVerifyAMessageGPGSigned(t *testing.T) {
	gpg := gpgHome(t)

	if _, log, err := gpg(nil, "--quick-generate-key", "GPG Interop <gpgsigner@example.com>",
		"ed25519", "sign", "never"); err != nil {
		t.Fatalf("gpg --quick-generate-key: %v\n%s", err, log)
	}
	// BINARY packets, matching how app.user_keys.public_key actually stores a
	// key — #48's exact failure mode was a fixture that assumed armor.
	pub, log, err := gpg(nil, "--export", "gpgsigner@example.com")
	if err != nil {
		t.Fatalf("gpg --export: %v\n%s", err, log)
	}
	// ⚠ CHECK THE FIXTURE BEFORE CONCLUDING ANYTHING FROM IT. A malformed key
	// makes Verify return no_key, which reads exactly like "our reader rejected
	// GnuPG's signature" — the finding this test is looking for. Parsing it here
	// means a broken fixture is reported as a broken fixture. (The first version
	// of this file appended gpg's stderr to the key and produced precisely that
	// false alarm.)
	if ring, err := openpgp.ReadKeyRing(bytes.NewReader(pub)); err != nil || len(ring) == 0 {
		t.Fatalf("the exported key is not parseable as binary OpenPGP packets (%v, %d "+
			"entities, %d bytes) — this is a FIXTURE problem, not an interop finding",
			err, len(ring), len(pub))
	}

	// Part one, built here rather than by Sign: gpg signs exactly these bytes.
	partOne := []byte("Content-Type: text/plain; charset=utf-8\r\n" +
		"Content-Transfer-Encoding: quoted-printable\r\n" +
		"\r\n" +
		"Signed by GnuPG, verified by Mailroom =E2=80=94 with an accent =C3=A9.\r\n" +
		"\r\n" +
		"A final line that ends with a newline.\r\n")

	dir := t.TempDir()
	dataPath := filepath.Join(dir, "part1.bin")
	writeFile(t, dataPath, partOne)
	armorOut, log, err := gpg(nil, "--armor", "--detach-sign", "--output", "-", dataPath)
	if err != nil {
		t.Fatalf("gpg --detach-sign: %v\n%s", err, log)
	}
	armor := string(armorOut)
	if !strings.Contains(armor, "-----BEGIN PGP SIGNATURE-----") {
		t.Fatalf("gpg did not return an armored signature:\n%s", log)
	}

	// The multipart/signed container, assembled literally per RFC 3156 §5 —
	// again NOT with pgpmime.Sign, so nothing our code decided is baked in.
	const boundary = "interop-boundary-9f3a"
	assemble := func(entity []byte) []byte {
		var b bytes.Buffer
		b.WriteString("From: GPG Interop <gpgsigner@example.com>\r\n")
		b.WriteString("To: Us <us@example.com>\r\n")
		b.WriteString("Subject: Signed elsewhere\r\n")
		b.WriteString("Message-ID: <gpg-interop@example.com>\r\n")
		b.WriteString("MIME-Version: 1.0\r\n")
		b.WriteString("Content-Type: multipart/signed; boundary=\"" + boundary + "\"; " +
			"protocol=\"application/pgp-signature\"; micalg=\"pgp-sha512\"\r\n")
		b.WriteString("\r\n")
		b.WriteString("--" + boundary + "\r\n")
		b.Write(entity)
		b.WriteString("\r\n--" + boundary + "\r\n")
		b.WriteString("Content-Type: application/pgp-signature; name=\"signature.asc\"\r\n")
		b.WriteString("\r\n")
		b.WriteString(strings.TrimRight(armor, "\r\n"))
		b.WriteString("\r\n--" + boundary + "--\r\n")
		return b.Bytes()
	}
	msg := assemble(partOne)

	// ⚠ THE FRAMING ASSERTION, STATED DIRECTLY. Everything else is downstream of
	// this: our splitter must hand back exactly the bytes gpg was given, no
	// delimiter CRLF included and no genuine trailing newline eaten.
	parts, err := pgpmime.SplitSigned(msg)
	if err != nil {
		t.Fatalf("SplitSigned on a GnuPG-produced message: %v", err)
	}
	if !bytes.Equal(parts.Entity, partOne) {
		t.Fatalf("SplitSigned recovered %d bytes, gpg signed %d — our framing differs from "+
			"an independent producer's by %d byte(s)\n got: %q\nwant: %q",
			len(parts.Entity), len(partOne), len(parts.Entity)-len(partOne),
			tail(parts.Entity), tail(partOne))
	}

	res := pgpverify.Verify(msg, []pgpverify.PublicKey{{Fingerprint: "", Binary: pub}})
	if res.Verdict != pgpverify.VerdictVerified {
		t.Fatalf("⚠ Mailroom REJECTED a signature GnuPG produced: verdict=%q err=%v — our "+
			"reader disagrees with an independent producer, which is what would happen to "+
			"real correspondent mail", res.Verdict, res.Err)
	}

	// ---- NEGATIVE CONTROL ---------------------------------------------------
	t.Run("a tampered body is not verified", func(t *testing.T) {
		bad := bytes.Replace(partOne, []byte("an accent"), []byte("no accent"), 1)
		if bytes.Equal(bad, partOne) {
			t.Fatal("the tamper did not change anything; this control proves nothing")
		}
		res := pgpverify.Verify(assemble(bad), []pgpverify.PublicKey{{Binary: pub}})
		if res.Verdict != pgpverify.VerdictBadSignature {
			t.Errorf("verdict = %q, want %q — Verify accepted altered content, so the "+
				"success above proved nothing", res.Verdict, pgpverify.VerdictBadSignature)
		}
	})
}

func writeFile(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// tail is the last few bytes, which is where a framing difference lives.
func tail(b []byte) []byte {
	if len(b) > 24 {
		return b[len(b)-24:]
	}
	return b
}
