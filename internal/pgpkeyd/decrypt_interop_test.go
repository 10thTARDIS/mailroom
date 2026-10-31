// SPDX-License-Identifier: AGPL-3.0-only

//go:build interop

package pgpkeyd

// CROSS-IMPLEMENTATION DECRYPT (#47 6b; closes the key-shape half of #263, C4
// in #301): GnuPG encrypts, the REAL daemon decrypts — through its own key
// configuration (Ed25519 primary, Curve25519 subkey, v4, made by Generate),
// over its socket, and through pgpmime.SplitEncrypted's RFC 3156 framing.
//
// Run by scripts/pgp-interop-test.sh inside golang:1.27, which ships GnuPG.
// Like internal/pgpverify's interop file, a missing gpg FAILS rather than
// skips; opting out needs MAILROOM_SKIP_GPG_INTEROP=1.

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"

	"mailroom/internal/pgpmime"
	"mailroom/internal/pgpwire"
)

func gpgRunner(t *testing.T) func(stdin []byte, args ...string) ([]byte, string, error) {
	t.Helper()
	if os.Getenv("MAILROOM_SKIP_GPG_INTEROP") == "1" {
		t.Skip("MAILROOM_SKIP_GPG_INTEROP=1: cross-implementation interop deliberately not run")
	}
	if _, err := exec.LookPath("gpg"); err != nil {
		t.Fatalf("gpg is not on PATH, so nothing here checked interoperability (%v); "+
			"run scripts/pgp-interop-test.sh or set MAILROOM_SKIP_GPG_INTEROP=1", err)
	}
	home := t.TempDir()
	return func(stdin []byte, args ...string) ([]byte, string, error) {
		full := append([]string{"--homedir", home, "--batch", "--yes", "--no-tty",
			"--pinentry-mode", "loopback", "--passphrase", ""}, args...)
		cmd := exec.Command("gpg", full...)
		if stdin != nil {
			cmd.Stdin = bytes.NewReader(stdin)
		}
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		err := cmd.Run()
		return out.Bytes(), out.String() + errb.String(), err
	}
}

func TestInteropGPGEncryptsDaemonDecrypts(t *testing.T) {
	gpg := gpgRunner(t)
	h := startDaemon(t)
	user := newUserID()
	gen := generate(t, h, user, "interop@example.com")

	if _, log, err := gpg([]byte(gen.Key.PublicKey), "--import"); err != nil {
		t.Fatalf("gpg --import of the daemon's public key: %v\n%s", err, log)
	}
	ct, log, err := gpg([]byte(secretEntity), "--armor", "--trust-model", "always",
		"--encrypt", "--recipient", "interop@example.com")
	if err != nil {
		t.Fatalf("gpg --encrypt to the daemon's key: %v\n%s", err, log)
	}
	if !bytes.Contains(ct, []byte("BEGIN PGP MESSAGE")) {
		t.Fatalf("gpg did not produce an armored message: %q", ct)
	}
	if pk, _, err := gpg(ct, "--list-packets"); err == nil {
		t.Logf("gpg's packets (for the record of which format agreed with us):\n%s", pk)
	}

	// Through the RFC 3156 framing, exactly as Email/get does it.
	raw := "From: a@example.com\r\nMIME-Version: 1.0\r\n" +
		"Content-Type: multipart/encrypted; boundary=\"b\"; protocol=\"application/pgp-encrypted\"\r\n\r\n" +
		"--b\r\nContent-Type: application/pgp-encrypted\r\n\r\nVersion: 1\r\n" +
		"--b\r\nContent-Type: application/octet-stream\r\n\r\n" + string(ct) + "\r\n--b--\r\n"
	split, err := pgpmime.SplitEncrypted([]byte(raw))
	if err != nil {
		t.Fatalf("SplitEncrypted: %v", err)
	}
	resp, err := decryptAs(t, h, user, split)
	if err != nil {
		t.Fatalf("the daemon could not decrypt GnuPG's message to the daemon's own key: %v", err)
	}
	if string(resp.Entity) != secretEntity {
		t.Errorf("entity = %q, want %q", resp.Entity, secretEntity)
	}

	// NEGATIVE CONTROL: GnuPG encrypting to a key the daemon does not hold must
	// fail, opaquely. Without it, "decrypted" could mean "returned anything".
	if _, log, err := gpg(nil, "--quick-generate-key", "other@example.com", "future-default", "default", "never"); err != nil {
		t.Fatalf("gpg key generation: %v\n%s", err, log)
	}
	ct2, log, err := gpg([]byte(secretEntity), "--armor", "--trust-model", "always",
		"--encrypt", "--recipient", "other@example.com")
	if err != nil {
		t.Fatalf("gpg --encrypt to its own key: %v\n%s", err, log)
	}
	resp, err = decryptAs(t, h, user, ct2)
	assertOpaque(t, "GnuPG message to a key the user does not hold", resp, err)
	if strings.Contains(string(ct2), secretEntity) {
		t.Fatal("fixture: gpg's ciphertext contains the plaintext")
	}
}

// THE OTHER DIRECTION (#47, ADR-038): the REAL daemon encrypts, GnuPG decrypts.
// The recipient's key is one GnuPG made itself, so what is checked is that an
// independent implementation accepts the daemon's session-key packets, its
// SEIPD packet and its armor, and that the integrity protection is real
// (negative control: one flipped bit and GnuPG refuses).
func TestInteropDaemonEncryptsGPGDecrypts(t *testing.T) {
	gpg := gpgRunner(t)
	h := startDaemon(t)
	user := newUserID()
	sender := generate(t, h, user, "sender@example.com")

	if _, log, err := gpg(nil, "--quick-generate-key", "rcpt@example.com", "default", "default", "never"); err != nil {
		t.Fatalf("gpg key generation: %v\n%s", err, log)
	}
	pub, log, err := gpg(nil, "--export", "rcpt@example.com") // binary, as contact_keys stores it
	if err != nil || len(pub) == 0 {
		t.Fatalf("gpg --export: %v\n%s", err, log)
	}

	resp, err := encryptAs(t, h, user, sender.Key.KeyID, secretEntity, rcpt("rcpt@example.com", pub))
	if err != nil {
		t.Fatalf("the daemon could not encrypt to a GnuPG-made key: %v", err)
	}

	// GnuPG's own account of the packets: integrity protected (MDC), and two
	// session keys (the recipient and the sender).
	pk, log, err := gpg([]byte(resp.Ciphertext), "--list-packets")
	if err != nil && !strings.Contains(log, "encrypted") {
		t.Fatalf("gpg --list-packets: %v\n%s", err, log)
	}
	pks := log // gpg() returns stdout+stderr as its second value; adding pk would count everything twice
	_ = pk
	if !strings.Contains(pks, "mdc_method") && !strings.Contains(pks, "AEAD") {
		t.Errorf("GnuPG does not report an integrity-protected data packet:\n%s", pks)
	}
	if n := strings.Count(pks, ":pubkey enc packet:"); n != 2 {
		t.Errorf("GnuPG sees %d public-key encrypted session keys, want 2:\n%s", n, pks)
	}

	// THE ASSERTION: GnuPG, holding only its own secret key, decrypts it.
	out, log, err := gpg([]byte(resp.Ciphertext), "--decrypt")
	if err != nil {
		t.Fatalf("GnuPG could not decrypt what the daemon encrypted to its key: %v\n%s", err, log)
	}
	if string(out) != secretEntity {
		t.Errorf("GnuPG decrypted %q, want %q", out, secretEntity)
	}

	// NEGATIVE CONTROL: the same message with a bit flipped in the integrity
	// trailer must NOT decrypt. Without it, "GnuPG decrypted" could mean
	// "GnuPG ignored the MDC".
	bad := tamperTail(t, []byte(resp.Ciphertext), 3)
	if out, _, err := gpg(bad, "--decrypt"); err == nil && string(out) == secretEntity {
		t.Error("GnuPG accepted a tampered ciphertext; the interop assertion above proves nothing about integrity")
	}
}

// SIGNED AND ENCRYPTED BY GnuPG IN ONE PASS (RFC 3156 section 6.2, #47 rc.2):
// `gpg --sign --encrypt` puts the one-pass signature, literal data and signature
// packets inside the encryption — the form Thunderbird sends. The REAL daemon
// decrypts it and checks the signature against a candidate key GnuPG made: a
// signer the application knows is verified; a signer it does not is noKey (the
// API turns that into unknownKey), naming GnuPG's key.
func TestInteropGPGSignEncryptsDaemonVerifies(t *testing.T) {
	gpg := gpgRunner(t)
	h := startDaemon(t)
	user := newUserID()
	gen := generate(t, h, user, "interop@example.com")
	if _, log, err := gpg([]byte(gen.Key.PublicKey), "--import"); err != nil {
		t.Fatalf("gpg --import of the daemon's public key: %v\n%s", err, log)
	}
	for _, who := range []string{"signer@example.com", "unknown@example.com"} {
		if _, log, err := gpg(nil, "--quick-generate-key", who, "default", "default", "never"); err != nil {
			t.Fatalf("gpg key generation for %s: %v\n%s", who, err, log)
		}
	}
	known, log, err := gpg(nil, "--export", "signer@example.com") // binary, as contact_keys stores it
	if err != nil || len(known) == 0 {
		t.Fatalf("gpg --export: %v\n%s", err, log)
	}
	primaryFpr := func(who string) string {
		out, log, err := gpg(nil, "--with-colons", "--list-keys", who)
		if err != nil {
			t.Fatalf("gpg --list-keys %s: %v\n%s", who, err, log)
		}
		for _, line := range strings.Split(string(out), "\n") {
			if f := strings.Split(line, ":"); len(f) > 9 && f[0] == "fpr" {
				return strings.ToUpper(f[9]) // the first fpr record is the primary's
			}
		}
		t.Fatalf("no fingerprint for %s in:\n%s", who, out)
		return ""
	}
	signEncrypt := func(signer string) []byte {
		ct, log, err := gpg([]byte(secretEntity), "--armor", "--trust-model", "always",
			"--sign", "--local-user", signer, "--encrypt", "--recipient", "interop@example.com",
			"--recipient", signer) // also to the signer, so gpg can list the decrypted packets
		if err != nil {
			t.Fatalf("gpg --sign --encrypt as %s: %v\n%s", signer, err, log)
		}
		return ct
	}

	ct := signEncrypt("signer@example.com")
	// GnuPG's own account: the signature is INSIDE the encryption (decrypting
	// with --list-packets shows the one-pass signature packet), not a nested
	// multipart/signed.
	if _, pks, _ := gpg(ct, "--list-packets"); !strings.Contains(pks, "onepass_sig packet") {
		t.Fatalf("gpg --sign --encrypt did not produce a one-pass signed message:\n%s", pks)
	} else {
		t.Logf("gpg's packets:\n%s", pks)
	}
	raw := "From: signer@example.com\r\nMIME-Version: 1.0\r\n" +
		"Content-Type: multipart/encrypted; boundary=\"b\"; protocol=\"application/pgp-encrypted\"\r\n\r\n" +
		"--b\r\nContent-Type: application/pgp-encrypted\r\n\r\nVersion: 1\r\n" +
		"--b\r\nContent-Type: application/octet-stream\r\n\r\n" + string(ct) + "\r\n--b--\r\n"
	split, err := pgpmime.SplitEncrypted([]byte(raw))
	if err != nil {
		t.Fatalf("SplitEncrypted: %v", err)
	}

	resp := mustDecryptSigned(t, h, user, split, known)
	if string(resp.Entity) != secretEntity {
		t.Errorf("entity = %q, want %q", resp.Entity, secretEntity)
	}
	if s := resp.Signature; !s.Signed || s.Status != pgpwire.EmbeddedVerified || s.SignerFingerprint != primaryFpr("signer@example.com") {
		t.Errorf("GnuPG's signature by a known key = %+v, want verified by %s", s, primaryFpr("signer@example.com"))
	}

	// NEGATIVE CONTROL: a GnuPG signer the application does not know.
	resp = mustDecryptSigned(t, h, user, signEncrypt("unknown@example.com"), known)
	unknownFpr := primaryFpr("unknown@example.com")
	s := resp.Signature
	if !s.Signed || s.Status != pgpwire.EmbeddedNoKey {
		t.Errorf("GnuPG's signature by an unknown key = %+v, want noKey", s)
	}
	named := s.IssuerFingerprint == unknownFpr || (s.IssuerKeyID != "" && strings.HasSuffix(unknownFpr, s.IssuerKeyID))
	if !named {
		t.Errorf("noKey names %+v; GnuPG's key is %s", s, unknownFpr)
	}
}
