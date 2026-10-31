// SPDX-License-Identifier: AGPL-3.0-only

package attachscan

import (
	"testing"

	"mailroom/internal/clamd"
	"mailroom/internal/mime"
)

// ⚠ THIS IS WHERE THE JUDGEMENT LIVES NOW, so this is where it is pinned.
//
// Before the extraction these rules existed once, inside ingest. They now have
// two callers with deliberately different error policies (ingest fail-closed
// with a circuit breaker, rescan best-effort), and the reason to share them is
// that a disagreement would make an attachment's verdict depend on which path
// touched it last. Each caller keeps a test of its own loop; these pin the rules
// both loops apply.
func parse(t *testing.T, raw string) *mime.Message {
	t.Helper()
	m, err := mime.ParseBytes([]byte(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return m
}

const withAttachment = "From: a@example.com\r\nSubject: s\r\nMIME-Version: 1.0\r\n" +
	"Content-Type: multipart/mixed; boundary=b\r\n\r\n" +
	"--b\r\nContent-Type: text/plain\r\n\r\nbody\r\n" +
	"--b\r\nContent-Type: application/octet-stream\r\n" +
	"Content-Disposition: attachment; filename=\"f.bin\"\r\n\r\nPAYLOAD\r\n--b--\r\n"

const encrypted = "From: a@example.com\r\nSubject: s\r\nMIME-Version: 1.0\r\n" +
	"Content-Type: multipart/encrypted; protocol=\"application/pgp-encrypted\"; boundary=b\r\n\r\n" +
	"--b\r\nContent-Type: application/pgp-encrypted\r\n\r\nVersion: 1\r\n" +
	"--b\r\nContent-Type: application/octet-stream\r\n\r\nciphertext\r\n--b--\r\n"

// §5a and §6: the two situations where a scan would SUCCEED and be wrong, which
// is worse than one that fails.
func TestForcedUnknownCoversTruncated(t *testing.T) {
	ordinary := parse(t, withAttachment)
	if _, forced := ForcedUnknown(ordinary); forced {
		t.Fatal("an ordinary message was forced to unknown; every case below would be vacuous")
	}

	truncated := parse(t, withAttachment)
	truncated.Truncated = true
	if _, forced := ForcedUnknown(truncated); !forced {
		t.Error("a truncated walk was not forced to unknown — clean-on-a-prefix (§5a)")
	}

	// Encryption is a property of a subtree, decided by EncryptedPaths (below).

	// ⚠ Inline PGP is deliberately NOT forced: its ciphertext sits in a text
	// body, so no attachment bytes are being blessed, and folding it in would
	// mark ordinary attachments unknown because a sender pasted an armored
	// block into the message.
	inline := parse(t, "From: a@example.com\r\nSubject: s\r\n\r\n"+
		"-----BEGIN PGP MESSAGE-----\r\nabc\r\n-----END PGP MESSAGE-----\r\n")
	if _, forced := ForcedUnknown(inline); forced {
		t.Error("inline PGP forced unknown; that marks scannable attachments unscannable")
	}

	if _, forced := ForcedUnknown(nil); forced {
		t.Error("a nil message was forced to unknown")
	}
}

// Zero bytes cannot be clean: clamd answers OK to an empty INSTREAM, so a part
// that decoded to nothing would otherwise be recorded as examined.
func TestHasNoBytes(t *testing.T) {
	m := parse(t, withAttachment)
	parts := m.AttachmentParts()
	if len(parts) != 1 {
		t.Fatalf("fixture has %d attachment parts, want 1", len(parts))
	}
	if HasNoBytes(parts[0]) {
		t.Error("a part with content reported as empty; the control fails")
	}
	empty := parts[0]
	empty.Node.Data = nil
	if !HasNoBytes(empty) {
		t.Error("a part with no bytes was not detected")
	}
	if !HasNoBytes(mime.AttachmentPart{}) {
		t.Error("a part with a nil node was not detected")
	}
}

// Unknown never carries a signature, because nothing matched; Verdict carries
// clamd's own name, because something did.
func TestUnknownAndVerdictMapping(t *testing.T) {
	parts := parse(t, withAttachment).AttachmentParts()
	u := Unknown(parts)
	if len(u) != 1 || u[0].Status != string(clamd.Unknown) || u[0].Signature != "" {
		t.Errorf("Unknown = %+v, want one unknown with no signature", u)
	}
	if u[0].PartPath != parts[0].Path {
		t.Errorf("PartPath = %q, want %q — a verdict must name a part a download URL resolves", u[0].PartPath, parts[0].Path)
	}

	v := Verdict(parts[0], clamd.Verdict{State: clamd.Flagged, Signature: "Eicar-Test-Signature"})
	if v.Status != string(clamd.Flagged) || v.Signature != "Eicar-Test-Signature" {
		t.Errorf("Verdict = %+v, want flagged with the signature preserved", v)
	}
}

func partPaths(m *mime.Message) []string {
	var out []string
	for _, ap := range m.AttachmentParts() {
		out = append(out, ap.Path)
	}
	return out
}

// §6, scoped (#405): the encrypted subtree's parts are in the set, siblings and
// forwarded-message content are not.
func TestEncryptedPathsIsScopedToTheEncryptedSubtree(t *testing.T) {
	// Control: the whole message is encrypted, so its attachment part is in.
	whole := parse(t, encrypted)
	enc := EncryptedPaths(whole)
	if parts := whole.AttachmentParts(); len(parts) == 0 || !AllEncrypted(parts, enc) {
		t.Fatalf("PGP/MIME ciphertext not covered: parts=%v enc=%v", partPaths(whole), enc)
	}

	// An ordinary message with its own PDF plus a FORWARDED encrypted message.
	fwd := parse(t, "From: a@example.com\r\nSubject: fwd\r\nMIME-Version: 1.0\r\n"+
		"Content-Type: multipart/mixed; boundary=o\r\n\r\n"+
		"--o\r\nContent-Type: text/plain\r\n\r\nsee attached\r\n"+
		"--o\r\nContent-Type: application/pdf\r\nContent-Disposition: attachment; filename=\"r.pdf\"\r\n\r\n%PDF-1.4\r\n"+
		"--o\r\nContent-Type: message/rfc822\r\n\r\n"+encrypted+"\r\n--o--\r\n")
	if len(fwd.Nested) != 1 {
		t.Fatalf("fixture: nested = %d, want 1", len(fwd.Nested))
	}
	// Only the forwarded message/rfc822 leaf (path 3) is covered; the outer PDF
	// (path 2) is plaintext.
	if got := EncryptedPaths(fwd); len(got) != 1 || !got["3"] || got["2"] {
		t.Errorf("want exactly the forwarded leaf 3 covered, the outer PDF 2 not (#405): %v", got)
	}

	// A plaintext PDF NEXT TO an encrypted PGP/MIME node: only the node's
	// subtree is covered.
	sib := parse(t, "From: a@example.com\r\nSubject: s\r\nMIME-Version: 1.0\r\n"+
		"Content-Type: multipart/mixed; boundary=o\r\n\r\n"+
		"--o\r\nContent-Type: application/pdf\r\nContent-Disposition: attachment; filename=\"r.pdf\"\r\n\r\n%PDF-1.4\r\n"+
		"--o\r\nContent-Type: multipart/encrypted; protocol=\"application/pgp-encrypted\"; boundary=b\r\n\r\n"+
		"--b\r\nContent-Type: application/pgp-encrypted\r\n\r\nVersion: 1\r\n"+
		"--b\r\nContent-Type: application/octet-stream\r\n\r\nciphertext\r\n--b--\r\n"+
		"--o--\r\n")
	got := EncryptedPaths(sib)
	parts := sib.AttachmentParts()
	if len(parts) < 2 {
		t.Fatalf("fixture: %d attachment parts, want >= 2", len(parts))
	}
	if got[parts[0].Path] {
		t.Errorf("the sibling PDF %s was covered by the encrypted subtree", parts[0].Path)
	}
	if !got[parts[len(parts)-1].Path] {
		t.Errorf("the ciphertext part %s was not covered: %v", parts[len(parts)-1].Path, got)
	}
}

// smime-type is a sender-written label, so signed-data does not exempt a
// pkcs7-mime part: it stays covered like its enveloped-data twin (#405).
func TestEncryptedPathsCoversPKCS7WhateverItsLabel(t *testing.T) {
	for _, typ := range []string{"signed-data", "enveloped-data"} {
		m := parse(t, "From: a@example.com\r\nSubject: s\r\nMIME-Version: 1.0\r\n"+
			"Content-Type: application/pkcs7-mime; smime-type="+typ+"; name=smime.p7m\r\n"+
			"Content-Transfer-Encoding: base64\r\n\r\nMIAGCSqGSIb3DQEHA6CA\r\n")
		if got := EncryptedPaths(m); len(got) != 1 {
			t.Errorf("%s: pkcs7-mime not covered: %v", typ, got)
		}
	}
}

// Forward of a forward of an encrypted message: the outer rfc822 leaf is
// unknown, the outer PDF is not.
func TestEncryptedPathsCoversADoublyForwardedEncryptedMessage(t *testing.T) {
	m := parse(t, "From: a@example.com\r\nSubject: fwd\r\nMIME-Version: 1.0\r\n"+
		"Content-Type: multipart/mixed; boundary=o\r\n\r\n"+
		"--o\r\nContent-Type: application/pdf\r\nContent-Disposition: attachment; filename=\"r.pdf\"\r\n\r\n%PDF-1.4\r\n"+
		"--o\r\nContent-Type: message/rfc822\r\n\r\n"+
		"From: b@example.com\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=m\r\n\r\n"+
		"--m\r\nContent-Type: text/plain\r\n\r\nfwd of fwd\r\n"+
		"--m\r\nContent-Type: message/rfc822\r\n\r\n"+encrypted+"\r\n--m--\r\n"+
		"\r\n--o--\r\n")
	got := EncryptedPaths(m)
	if !got["2"] || got["1"] || len(got) != 1 {
		t.Errorf("want only the outer forwarded leaf 2 covered (%v); parts=%v", got, partPaths(m))
	}
}
