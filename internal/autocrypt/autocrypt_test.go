// SPDX-License-Identifier: AGPL-3.0-only

package autocrypt

import (
	"encoding/base64"
	"errors"
	"net/mail"
	"strings"
	"testing"
)

// key is a stand-in for binary OpenPGP packets. This package never parses key
// structure — that is internal/pgpkeys' job — so any bytes exercise it.
var key = []byte{0x99, 0x01, 0x0d, 0x04, 0xde, 0xad, 0xbe, 0xef}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func TestParseReadsAddrKeyAndPreference(t *testing.T) {
	p, ok, err := Parse([]string{"addr=alice@example.com; prefer-encrypt=mutual; keydata=" + b64(key)})
	if err != nil || !ok {
		t.Fatalf("Parse: ok=%v err=%v, want a usable peer", ok, err)
	}
	if p.Addr != "alice@example.com" {
		t.Errorf("Addr = %q", p.Addr)
	}
	if !p.PreferEncrypt {
		t.Error("PreferEncrypt = false, want true for prefer-encrypt=mutual")
	}
	if string(p.KeyData) != string(key) {
		t.Errorf("KeyData = %x, want %x", p.KeyData, key)
	}
}

// TestParseAbsentHeaderIsNotAnError separates "no Autocrypt" from "bad
// Autocrypt". Almost all mail is the former, and reporting it as a rejection
// would make the two indistinguishable in any census.
func TestParseAbsentHeaderIsNotAnError(t *testing.T) {
	p, ok, err := Parse(nil)
	if ok || err != nil {
		t.Fatalf("Parse(nil) = %+v, ok=%v, err=%v; want zero, false, nil", p, ok, err)
	}
}

// TestParseIgnoreRules pins every case Autocrypt Level 1 says must be ignored.
//
// ⚠ These are the security-relevant half of the parser. A parser that is merely
// lenient here does not fail loudly — it accepts a key claim the spec says is
// not usable, and stores it under someone's address.
func TestParseIgnoreRules(t *testing.T) {
	good := b64(key)
	cases := []struct {
		name   string
		values []string
		why    string
	}{
		{
			"two headers",
			[]string{"addr=a@example.com; keydata=" + good, "addr=a@example.com; keydata=" + good},
			"Level 1: more than one Autocrypt header means ignore ALL of them — picking one lets a sender make two clients disagree about the same person's key",
		},
		{
			"unknown critical attribute",
			[]string{"addr=a@example.com; keydata=" + good + "; futurething=1"},
			"an unknown attribute NOT starting with _ is critical: an old parser must refuse rather than read a header on terms it does not understand",
		},
		{"missing addr", []string{"keydata=" + good}, "addr is required"},
		{"missing keydata", []string{"addr=a@example.com"}, "keydata is required"},
		{"empty addr", []string{"addr=; keydata=" + good}, "an empty addr matches no From"},
		{"empty keydata", []string{"addr=a@example.com; keydata="}, "an empty key is not a key"},
		{"undecodable keydata", []string{"addr=a@example.com; keydata=!!!not-base64!!!"}, "keydata must be base64"},
		{"attribute with no =", []string{"addr=a@example.com; keydata=" + good + "; bare"}, "a malformed attribute makes the header unparseable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, ok, err := Parse(tc.values)
			if ok || !errors.Is(err, ErrIgnore) {
				t.Fatalf("ok=%v err=%v, want ignored — %s", ok, err, tc.why)
			}
		})
	}
}

// TestParseNonCriticalAttributeIsSkipped is the control for the critical-unknown
// case above: without it, a parser that ignored EVERY header with an extra
// attribute would pass that test while being wrong.
func TestParseNonCriticalAttributeIsSkipped(t *testing.T) {
	p, ok, err := Parse([]string{"addr=a@example.com; _vendor=whatever; keydata=" + b64(key)})
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v, want the header accepted with the _-prefixed attribute skipped", ok, err)
	}
	if string(p.KeyData) != string(key) {
		t.Error("keydata was lost while skipping a non-critical attribute")
	}
}

// TestParseUnknownPreferenceIsNoPreference — the spec's fail-safe direction.
func TestParseUnknownPreferenceIsNoPreference(t *testing.T) {
	for _, v := range []string{"nopreference", "MUTUALLY", "", "yes"} {
		p, ok, err := Parse([]string{"addr=a@example.com; prefer-encrypt=" + v + "; keydata=" + b64(key)})
		if err != nil || !ok {
			t.Fatalf("prefer-encrypt=%q: ok=%v err=%v, want accepted", v, ok, err)
		}
		if p.PreferEncrypt {
			t.Errorf("prefer-encrypt=%q read as mutual; only exactly \"mutual\" may", v)
		}
	}
}

// TestParseOversizeKeyIsRejectedBeforeAllocating guards the one place an
// inbound header decides how much memory a message costs.
func TestParseOversizeKeyIsRejectedBeforeAllocating(t *testing.T) {
	huge := strings.Repeat("A", (MaxKeyData/3*4)+64)
	if _, ok, err := Parse([]string{"addr=a@example.com; keydata=" + huge}); ok || !errors.Is(err, ErrIgnore) {
		t.Fatalf("ok=%v err=%v, want an oversize key ignored", ok, err)
	}
}

// TestMatchesFrom is the check that stops a sender claiming someone else's
// address. Without it, storing the key would poison a third party's contact.
func TestMatchesFrom(t *testing.T) {
	p := Peer{Addr: "Alice@Example.COM"}
	if !p.MatchesFrom("alice@example.com") {
		t.Error("address comparison must be case-insensitive")
	}
	if p.MatchesFrom("mallory@example.com") {
		t.Error("a key claiming a DIFFERENT address than From must not match")
	}
	if (Peer{}).MatchesFrom("") {
		t.Error("an empty claimed address must never match")
	}
}

// TestHeaderRoundTrips is the property that matters: what we emit, we can read.
// It also proves the folding is legal, by parsing the result as a real mail
// header rather than eyeballing it.
func TestHeaderRoundTrips(t *testing.T) {
	big := make([]byte, 3000) // forces many folds
	for i := range big {
		big[i] = byte(i % 251)
	}
	value := Header("alice@example.com", big, false)

	msg, err := mail.ReadMessage(strings.NewReader(
		"From: alice@example.com\r\n" + HeaderName + ": " + value + "\r\n\r\nbody\r\n"))
	if err != nil {
		t.Fatalf("the emitted header is not a legal mail header: %v", err)
	}
	got, ok, err := Parse(msg.Header[HeaderName])
	if err != nil || !ok {
		t.Fatalf("our own header did not parse: ok=%v err=%v", ok, err)
	}
	if string(got.KeyData) != string(big) {
		t.Errorf("key survived folding as %d bytes, want %d", len(got.KeyData), len(big))
	}
	if !got.MatchesFrom("alice@example.com") {
		t.Error("emitted addr does not match the From it was built for")
	}
}

// TestHeaderLineLength pins the folding itself. A header line long enough to be
// refolded or truncated by an MTA would corrupt the base64 in transit, and the
// round-trip test above cannot see that because it never crosses a wire.
func TestHeaderLineLength(t *testing.T) {
	big := make([]byte, 4000)
	full := HeaderName + ": " + Header("alice@example.com", big, true)
	for i, line := range strings.Split(full, "\r\n") {
		if len(line) > foldAt {
			t.Fatalf("line %d is %d octets, want <= %d: %q", i, len(line), foldAt, line)
		}
	}
}

// TestHeaderOmitsPreferEncryptUnlessAsked pins the decision in Header's doc:
// advertising `mutual` before decryption exists invites mail Mailroom cannot
// read. This is what would have to change, deliberately, in v1.65.0.
func TestHeaderOmitsPreferEncryptUnlessAsked(t *testing.T) {
	if strings.Contains(Header("a@example.com", key, false), "prefer-encrypt") {
		t.Error("prefer-encrypt must be absent unless explicitly requested")
	}
	if !strings.Contains(Header("a@example.com", key, true), "prefer-encrypt=mutual") {
		t.Error("prefer-encrypt=mutual must appear when requested")
	}
}
