// SPDX-License-Identifier: AGPL-3.0-only

package compose

// The outbound Autocrypt header (#48) and the one structural property it
// depends on: it must survive signing ON THE OUTER MESSAGE.
//
// ⚠ Signing rewrites the message into multipart/signed, moving Content-* headers
// down into the signed part and leaving everything else on top. A receiving
// Autocrypt client only ever looks at the message's top-level headers, so a
// header that ended up inside the signed part would be invisible — present in
// the bytes, doing nothing, with no error anywhere. That is the failure this
// file exists to exclude.

import (
	"net/mail"
	"strings"
	"testing"

	"mailroom/internal/autocrypt"
)

const outboundFrom = "alice@example.com"

// fakeKey stands in for binary OpenPGP packets. Nothing here parses key
// structure — internal/pgpkeys does that — so arbitrary bytes are the honest
// fixture, and using enough of them forces the header to fold.
func fakeKey() []byte {
	k := make([]byte, 1200)
	for i := range k {
		k[i] = byte(i % 253)
	}
	return k
}

func rawWithAutocrypt(t *testing.T) []byte {
	t.Helper()
	raw := []byte("From: " + outboundFrom + "\r\n" +
		"To: bob@example.net\r\n" +
		"Subject: Hello\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"\r\n" +
		"body text\r\n")
	return prependHeader(raw, autocrypt.HeaderName,
		autocrypt.Header(outboundFrom, fakeKey(), autocryptPreferMutual))
}

// TestAutocryptHeaderSurvivesSigningOnTheOuterMessage is the assertion above.
func TestAutocryptHeaderSurvivesSigningOnTheOuterMessage(t *testing.T) {
	signed, _ := signWithStub(t, rawWithAutocrypt(t))

	msg, err := mail.ReadMessage(strings.NewReader(string(signed)))
	if err != nil {
		t.Fatalf("signed message does not parse: %v", err)
	}
	if ct := msg.Header.Get("Content-Type"); !strings.Contains(ct, "multipart/signed") {
		t.Fatalf("Content-Type = %q, want multipart/signed — the test is not exercising signing", ct)
	}

	peer, ok, err := autocrypt.Parse(msg.Header[autocrypt.HeaderName])
	if err != nil || !ok {
		t.Fatalf("no usable Autocrypt header on the OUTER signed message: ok=%v err=%v", ok, err)
	}
	if string(peer.KeyData) != string(fakeKey()) {
		t.Errorf("key came back as %d bytes, want %d — folding or signing corrupted it",
			len(peer.KeyData), len(fakeKey()))
	}
	if !peer.MatchesFrom(outboundFrom) {
		t.Errorf("addr = %q, want the From address %q", peer.Addr, outboundFrom)
	}
}

// TestAutocryptHeaderIsNotBuriedInTheSignedPart is the negative control for the
// test above, and the one that would actually catch the bug.
//
// ⚠ Without it, an implementation that put the header ONLY inside the signed
// part could still pass the round-trip test if a lenient parser walked the whole
// message. Here the outer headers are cut off and the signed part alone is
// searched: finding Autocrypt only there is the defect.
func TestAutocryptHeaderIsNotBuriedInTheSignedPart(t *testing.T) {
	signed, _ := signWithStub(t, rawWithAutocrypt(t))

	// Everything after the first blank line is the multipart body: the signed
	// part, then the signature part. The outer header block is excluded.
	_, body, found := strings.Cut(string(signed), "\r\n\r\n")
	if !found {
		t.Fatal("cannot split the signed message into headers and body")
	}
	if strings.Contains(body, autocrypt.HeaderName+":") {
		t.Error("the Autocrypt header appears inside the signed body, where no client will look for it")
	}
}

// TestOutboundHeaderDoesNotAdvertiseMutual pins the decision recorded on
// autocryptPreferMutual: advertising `mutual` before decryption exists invites
// mail Mailroom cannot read (#405). Changing that constant is a v1.65.0
// decision, and this test is what makes it a deliberate one.
func TestOutboundHeaderDoesNotAdvertiseMutual(t *testing.T) {
	if autocryptPreferMutual {
		t.Fatal("autocryptPreferMutual is true: Mailroom would be asking correspondents " +
			"to encrypt mail it cannot yet decrypt (#47, #405)")
	}
	if strings.Contains(string(rawWithAutocrypt(t)), "prefer-encrypt") {
		t.Error("outgoing mail advertises prefer-encrypt while decryption does not exist")
	}
}

// TestPrependHeaderKeepsTheMessageParseable guards the shortcut prependHeader
// takes: it does not parse the message, it just puts a field on top.
func TestPrependHeaderKeepsTheMessageParseable(t *testing.T) {
	out := rawWithAutocrypt(t)
	msg, err := mail.ReadMessage(strings.NewReader(string(out)))
	if err != nil {
		t.Fatalf("prependHeader produced an unparseable message: %v", err)
	}
	// The original headers must all still be there and readable.
	if got := msg.Header.Get("Subject"); got != "Hello" {
		t.Errorf("Subject = %q, want %q — prepending disturbed the existing headers", got, "Hello")
	}
	if got := msg.Header.Get("From"); !strings.Contains(got, outboundFrom) {
		t.Errorf("From = %q, want it to contain %q", got, outboundFrom)
	}
	if got := msg.Header.Get("Content-Type"); !strings.Contains(got, "text/plain") {
		t.Errorf("Content-Type = %q — prepending disturbed the entity headers", got)
	}
}
