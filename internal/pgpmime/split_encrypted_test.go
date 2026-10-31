// SPDX-License-Identifier: AGPL-3.0-only

package pgpmime

import (
	"bytes"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"mailroom/internal/mime"
)

// encryptedMessage is an RFC 3156 §4 message built by hand from the RFC's own
// example shape, so the parser is tested against the standard rather than
// against a producer of ours (there is none yet: encrypt is a later PR).
func encryptedMessage(ctlType, ctlBody, dataHeaders, dataBody string, extraPart bool) string {
	b := "From: Alice <alice@example.com>\r\n" +
		"To: Bob <bob@example.com>\r\n" +
		"Subject: outer subject\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/encrypted; boundary=\"foo\";\r\n" +
		"  protocol=\"application/pgp-encrypted\"\r\n" +
		"\r\n" +
		"This is an OpenPGP/MIME encrypted message (RFC 3156).\r\n" +
		"--foo\r\n" +
		"Content-Type: " + ctlType + "\r\n" +
		"\r\n" +
		ctlBody + "\r\n" +
		"--foo\r\n" +
		dataHeaders +
		"\r\n" +
		dataBody + "\r\n"
	if extraPart {
		b += "--foo\r\nContent-Type: text/plain\r\n\r\nstowaway\r\n"
	}
	return b + "--foo--\r\n"
}

const armoredStub = "-----BEGIN PGP MESSAGE-----\r\n\r\nwV4DAAAAAAAAAAASAQdA\r\n=abcd\r\n-----END PGP MESSAGE-----"

func TestSplitEncryptedReturnsTheCiphertextPart(t *testing.T) {
	raw := encryptedMessage("application/pgp-encrypted", "Version: 1",
		"Content-Type: application/octet-stream; name=\"encrypted.asc\"\r\n", armoredStub, false)
	if !IsEncrypted([]byte(raw)) {
		t.Fatal("IsEncrypted = false for an RFC 3156 multipart/encrypted")
	}
	ct, err := SplitEncrypted([]byte(raw))
	if err != nil {
		t.Fatalf("SplitEncrypted: %v", err)
	}
	if string(ct) != armoredStub {
		t.Errorf("ciphertext = %q, want exactly part two's body %q", ct, armoredStub)
	}
}

func TestSplitEncryptedUndoesATransferEncoding(t *testing.T) {
	enc := base64.StdEncoding.EncodeToString([]byte(armoredStub))
	raw := encryptedMessage("application/pgp-encrypted", "Version: 1",
		"Content-Type: application/octet-stream\r\nContent-Transfer-Encoding: base64\r\n",
		enc[:20]+"\r\n"+enc[20:], false)
	ct, err := SplitEncrypted([]byte(raw))
	if err != nil {
		t.Fatalf("SplitEncrypted: %v", err)
	}
	if string(ct) != armoredStub {
		t.Errorf("ciphertext = %q, want the base64-decoded armor", ct)
	}
}

func TestSplitEncryptedRefusesWhatIsNotRFC3156(t *testing.T) {
	ok := "Content-Type: application/octet-stream\r\n"
	for name, tc := range map[string]struct {
		raw     string
		notPGP  bool
		wantErr string
	}{
		"plain text": {raw: "From: a@example.com\r\nContent-Type: text/plain\r\n\r\nhi\r\n", notPGP: true},
		"S/MIME enveloped": {raw: "Content-Type: application/pkcs7-mime; smime-type=enveloped-data\r\n\r\nMIIB\r\n",
			notPGP: true},
		"multipart/encrypted, other protocol": {
			raw: strings.Replace(encryptedMessage("application/pgp-encrypted", "Version: 1", ok, armoredStub, false),
				"application/pgp-encrypted\"\r\n\r\n", "application/x-other\"\r\n\r\n", 1),
			notPGP: true},
		"three parts":       {raw: encryptedMessage("application/pgp-encrypted", "Version: 1", ok, armoredStub, true), wantErr: "3 parts"},
		"wrong control":     {raw: encryptedMessage("text/plain", "Version: 1", ok, armoredStub, false), wantErr: "part one"},
		"no version":        {raw: encryptedMessage("application/pgp-encrypted", "Version: 2", ok, armoredStub, false), wantErr: "Version: 1"},
		"wrong data type":   {raw: encryptedMessage("application/pgp-encrypted", "Version: 1", "Content-Type: text/plain\r\n", armoredStub, false), wantErr: "part two"},
		"empty ciphertext":  {raw: encryptedMessage("application/pgp-encrypted", "Version: 1", ok, "", false), wantErr: "empty"},
		"unknown transfer":  {raw: encryptedMessage("application/pgp-encrypted", "Version: 1", ok+"Content-Transfer-Encoding: x-uue\r\n", armoredStub, false), wantErr: "transfer encoding"},
		"no header/body":    {raw: "Content-Type: multipart/encrypted", wantErr: "separator"},
		"no boundary param": {raw: "Content-Type: multipart/encrypted; protocol=\"application/pgp-encrypted\"\r\n\r\nx\r\n", wantErr: "no boundary"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := SplitEncrypted([]byte(tc.raw))
			if tc.notPGP {
				if !errors.Is(err, ErrNotEncrypted) {
					t.Errorf("err = %v, want ErrNotEncrypted", err)
				}
				if IsEncrypted([]byte(tc.raw)) {
					t.Error("IsEncrypted = true")
				}
				return
			}
			if err == nil || errors.Is(err, ErrNotEncrypted) || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("err = %v, want a malformed-message error mentioning %q", err, tc.wantErr)
			}
		})
	}
}

// Rewrap is what Email/get renders: the outer message headers, then the
// decrypted entity in place of the encrypted body.
func TestRewrapPutsTheDecryptedEntityUnderTheOuterHeaders(t *testing.T) {
	raw := encryptedMessage("application/pgp-encrypted", "Version: 1",
		"Content-Type: application/octet-stream\r\n", armoredStub, false)
	entity := "Content-Type: multipart/mixed; boundary=\"inner\"\r\n\r\n" +
		"--inner\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nthe secret body\r\n" +
		"--inner\r\nContent-Type: application/pdf\r\nContent-Disposition: attachment; filename=\"x.pdf\"\r\n" +
		"Content-Transfer-Encoding: base64\r\n\r\nJVBERi0xLjQK\r\n--inner--\r\n"

	out, err := Rewrap([]byte(raw), []byte(entity))
	if err != nil {
		t.Fatalf("Rewrap: %v", err)
	}
	if strings.Contains(string(out), "multipart/encrypted") || strings.Contains(string(out), "PGP MESSAGE") {
		t.Errorf("the encrypted container survived the rewrap:\n%s", out)
	}
	msg, err := mime.ParseBytes(out)
	if err != nil || msg == nil {
		t.Fatalf("rewrapped message does not parse: %v", err)
	}
	if msg.Header.Subject() != "outer subject" {
		t.Errorf("Subject = %q, want the outer message's", msg.Header.Subject())
	}
	if from := msg.Header.From(); len(from) != 1 || from[0].Address != "alice@example.com" {
		t.Errorf("From = %+v", from)
	}
	if msg.Structure == nil || msg.Structure.ContentType != "multipart/mixed" || len(msg.Structure.SubParts) != 2 {
		t.Fatalf("structure = %+v, want the entity's multipart/mixed with two parts", msg.Structure)
	}
	if got := string(msg.Structure.SubParts[0].Data); !strings.Contains(got, "the secret body") {
		t.Errorf("text part = %q", got)
	}
	if got := string(msg.Structure.SubParts[1].Data); !strings.HasPrefix(got, "%PDF-1.4") {
		t.Errorf("attachment bytes = %q, want the decoded PDF", got)
	}

	if _, err := Rewrap([]byte(raw), []byte("no blank line, so not an entity")); err == nil {
		t.Error("Rewrap accepted a decrypted 'entity' with no header/body separator")
	}
}

// Encryption authenticates nothing: anyone can encrypt to a published key. So
// identity, trace and authentication headers hidden INSIDE the ciphertext must
// never reach the rebuilt header block — only the inner MIME content fields do.
func TestRewrapDropsEveryInnerNonContentHeader(t *testing.T) {
	raw := "From: Alice <alice@example.com>\r\n" +
		"To: Bob <bob@example.com>\r\n" +
		"Subject: outer subject\r\n" +
		"Date: Mon, 19 Oct 2026 10:00:00 +0000\r\n" +
		"Message-ID: <outer@example.com>\r\n" +
		"Authentication-Results: mx.example.com; dmarc=fail\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/encrypted; boundary=\"foo\"; protocol=\"application/pgp-encrypted\"\r\n" +
		"\r\n--foo\r\nContent-Type: application/pgp-encrypted\r\n\r\nVersion: 1\r\n" +
		"--foo\r\nContent-Type: application/octet-stream\r\n\r\n" + armoredStub + "\r\n--foo--\r\n"
	entity := "From: CEO <ceo@yourbank.example>\r\n" +
		"Subject: URGENT wire transfer\r\n" +
		"Reply-To: thief@attacker.example\r\n" +
		"Sender: ceo@yourbank.example\r\n" +
		"To: victim@example.com\r\n" +
		"Date: Tue, 01 Jan 2030 00:00:00 +0000\r\n" +
		"Message-ID: <inner@attacker.example>\r\n" +
		"DKIM-Signature: v=1; a=rsa-sha256; d=yourbank.example; s=x; b=forged\r\n" +
		"Authentication-Results: mx.example.com; dkim=pass; dmarc=pass\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"Content-Transfer-Encoding: 8bit\r\n" +
		"Content-Language: en\r\n" +
		"\r\nthe inner body\r\n"

	out, err := Rewrap([]byte(raw), []byte(entity))
	if err != nil {
		t.Fatalf("Rewrap: %v", err)
	}
	head, body, err := splitMessage(out)
	if err != nil {
		t.Fatal(err)
	}
	fields, err := parseFields(head)
	if err != nil {
		t.Fatal(err)
	}
	count := map[string]int{}
	values := map[string]string{}
	for _, f := range fields {
		count[f.name]++
		values[f.name] = valueOf(unfold(f.raw))
	}
	for name, want := range map[string]string{
		"from":                      "Alice <alice@example.com>",
		"subject":                   "outer subject",
		"to":                        "Bob <bob@example.com>",
		"date":                      "Mon, 19 Oct 2026 10:00:00 +0000",
		"message-id":                "<outer@example.com>",
		"authentication-results":    "mx.example.com; dmarc=fail",
		"content-type":              "text/plain; charset=utf-8",
		"content-transfer-encoding": "8bit",
		"content-language":          "en",
		"mime-version":              "1.0",
	} {
		if count[name] != 1 {
			t.Errorf("%s appears %d times in the rebuilt header, want exactly 1", name, count[name])
		}
		if values[name] != want {
			t.Errorf("%s = %q, want %q (the OUTER value, or the inner content field)", name, values[name], want)
		}
	}
	for _, name := range []string{"reply-to", "sender", "dkim-signature"} {
		if count[name] != 0 {
			t.Errorf("inner %s reached the rebuilt header block", name)
		}
	}
	if bytes.Contains(out, []byte("yourbank")) || bytes.Contains(out, []byte("attacker.example")) {
		t.Errorf("an inner identity header survived:\n%s", out)
	}
	if string(body) != "the inner body\r\n" {
		t.Errorf("body = %q", body)
	}

	// And through the real parser: one From, the outer one.
	msg, err := mime.ParseBytes(out)
	if err != nil || msg == nil {
		t.Fatalf("parse: %v", err)
	}
	if n := msg.Header.FieldCount("From"); n != 1 {
		t.Errorf("From field count = %d, want 1", n)
	}
	if from := msg.Header.From(); len(from) != 1 || from[0].Address != "alice@example.com" {
		t.Errorf("From = %+v, want the outer sender", from)
	}
}
