// SPDX-License-Identifier: AGPL-3.0-only

package compose

// Transfer-encoding of the body (#47 increment 5).
//
// The property under test is RFC 3156 §3's: the bytes leaving here must survive
// a 7-bit hop. Before this, every text part was declared `8bit`, so a single
// em-dash was enough for the first MTA without 8BITMIME to re-encode the body —
// which breaks a signature computed over it, invisibly, with the recipient the
// only party who can see the failure.
//
// ⚠ These tests do NOT assert that the output "looks like" quoted-printable.
// Eyeballing an encoding is how an encoder that mangles content passes review:
// the assertions here are that the output is genuinely 7-bit, and that DECODING
// it returns exactly what went in. A round-trip is the only check that can tell
// a correct encoder from a plausible one.

import (
	"io"
	"mime/quotedprintable"
	"strings"
	"testing"
)

func buildOne(t *testing.T, d Draft) Built {
	t.Helper()
	d.From = Address{Email: "me@example.com"}
	d.To = []Address{{Email: "you@example.net"}}
	built, err := Build(d)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return built
}

// bodyOf returns everything after the first blank line.
func bodyOf(t *testing.T, raw []byte) string {
	t.Helper()
	i := strings.Index(string(raw), "\r\n\r\n")
	if i < 0 {
		t.Fatalf("message has no header/body separator")
	}
	return string(raw)[i+4:]
}

func firstNon7Bit(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] > 127 || s[i] == 0 {
			return i
		}
	}
	return -1
}

// TestNonASCIIBodyIsTransportSafeAndRoundTrips is the point of the change: an
// em-dash must not put an 8-bit byte on the wire, and the recipient must get
// back exactly the characters the user typed.
func TestNonASCIIBodyIsTransportSafeAndRoundTrips(t *testing.T) {
	const text = "Dear Renée — the café's naïve résumé, 日本語, and an emoji 🎉.\nSecond line.\n"

	built := buildOne(t, Draft{TextBody: text})
	raw := string(built.Raw)

	// 1. THE WHOLE MESSAGE is 7-bit. Not just the part we were thinking about:
	//    a stray 8-bit byte anywhere is the same failure.
	if i := firstNon7Bit(raw); i >= 0 {
		t.Errorf("message carries a non-7-bit byte %#x at offset %d — an MTA without "+
			"8BITMIME will re-encode this and break any signature over it", raw[i], i)
	}
	if !strings.Contains(raw, "Content-Transfer-Encoding: quoted-printable\r\n") {
		t.Errorf("non-ASCII body was not declared quoted-printable; headers were:\n%s",
			raw[:strings.Index(raw, "\r\n\r\n")])
	}
	if strings.Contains(raw, "Content-Transfer-Encoding: 8bit") {
		t.Error("body is still declared 8bit — the encoding this change exists to remove")
	}

	// 2. ROUND-TRIP. This is the assertion that separates a correct encoder from
	//    one that merely produces 7-bit output: `=` handling, soft line breaks
	//    and trailing-whitespace escapes must all decode back to the original.
	decoded, err := io.ReadAll(quotedprintable.NewReader(strings.NewReader(bodyOf(t, built.Raw))))
	if err != nil {
		t.Fatalf("decoding the body failed: %v", err)
	}
	if want := normalizeEOL(text); string(decoded) != want {
		t.Errorf("round-trip changed the content.\n got: %q\nwant: %q", decoded, want)
	}
}

// TestASCIIBodyStaysVerbatimAs7Bit guards the other direction. Ordinary mail
// must not be churned into quoted-printable: it would make every raw message
// less readable and rewrite the bytes of messages that were already fine.
func TestASCIIBodyStaysVerbatimAs7Bit(t *testing.T) {
	const text = "Plain ASCII, nothing clever.\nSecond line.\n"

	built := buildOne(t, Draft{TextBody: text})
	raw := string(built.Raw)

	if !strings.Contains(raw, "Content-Transfer-Encoding: 7bit\r\n") {
		t.Error("ASCII body was not declared 7bit")
	}
	if strings.Contains(raw, "quoted-printable") {
		t.Error("ASCII body was needlessly quoted-printable encoded")
	}
	// The bytes themselves must be untouched — this is what makes the change
	// safe for every message that already existed.
	if got, want := bodyOf(t, built.Raw), normalizeEOL(text); got != want {
		t.Errorf("ASCII body was rewritten.\n got: %q\nwant: %q", got, want)
	}
}

// TestOverlongASCIILineForcesQuotedPrintable covers the condition that is easy
// to forget: content can be pure ASCII and still not be legal `7bit`, because
// RFC 5322 caps a line at 998 octets. A pasted URL or a generated paragraph
// reaches that without trying, and an MTA that folds the line to comply breaks
// a signature over it.
func TestOverlongASCIILineForcesQuotedPrintable(t *testing.T) {
	long := strings.Repeat("a", bodyLineLimit+1)

	built := buildOne(t, Draft{TextBody: long + "\n"})
	raw := string(built.Raw)

	if !strings.Contains(raw, "Content-Transfer-Encoding: quoted-printable\r\n") {
		t.Errorf("a %d-character line was declared transmissible as-is; RFC 5322 caps a "+
			"line at %d octets", len(long), bodyLineLimit)
	}
	for _, line := range strings.Split(bodyOf(t, built.Raw), "\r\n") {
		if len(line) > 76 {
			t.Errorf("encoded line is %d chars, over quoted-printable's 76-column limit", len(line))
			break
		}
	}
	decoded, err := io.ReadAll(quotedprintable.NewReader(strings.NewReader(bodyOf(t, built.Raw))))
	if err != nil {
		t.Fatalf("decoding the body failed: %v", err)
	}
	if want := normalizeEOL(long + "\n"); string(decoded) != want {
		t.Errorf("round-trip changed a long line (got %d bytes, want %d)", len(decoded), len(want))
	}
}

// TestBoundaryLineLengthIsExactlyTheLimit pins the boundary of the condition
// above. Off-by-one here would either encode mail that did not need it or
// declare an illegal line 7bit, and both are silent.
func TestBoundaryLineLengthIsExactlyTheLimit(t *testing.T) {
	atLimit := buildOne(t, Draft{TextBody: strings.Repeat("a", bodyLineLimit) + "\n"})
	if !strings.Contains(string(atLimit.Raw), "Content-Transfer-Encoding: 7bit\r\n") {
		t.Errorf("a line of exactly %d octets is legal and should stay 7bit", bodyLineLimit)
	}
	overLimit := buildOne(t, Draft{TextBody: strings.Repeat("a", bodyLineLimit+1) + "\n"})
	if !strings.Contains(string(overLimit.Raw), "quoted-printable") {
		t.Errorf("a line of %d octets is over the limit and must be encoded", bodyLineLimit+1)
	}
}

// TestAlternativePartsAreBothTransportSafe covers the multipart/alternative
// path, which writes parts through a different function and would otherwise be
// an untested way to put 8-bit content on the wire.
func TestAlternativePartsAreBothTransportSafe(t *testing.T) {
	built := buildOne(t, Draft{
		TextBody: "plain — with an em-dash\n",
		HTMLBody: "<p>rich — with one too</p>\n",
	})
	raw := string(built.Raw)

	if i := firstNon7Bit(raw); i >= 0 {
		t.Errorf("multipart/alternative message carries a non-7-bit byte %#x at offset %d",
			raw[i], i)
	}
	if n := strings.Count(raw, "Content-Transfer-Encoding: quoted-printable"); n != 2 {
		t.Errorf("expected both alternative parts to be quoted-printable, found %d", n)
	}
	if strings.Contains(raw, "Content-Transfer-Encoding: 8bit") {
		t.Error("an alternative part is still declared 8bit")
	}
}

// TestTrailingWhitespaceSurvivesEncoding is a signature-specific concern.
// Trailing whitespace is what gateways have always stripped; quoted-printable
// escapes it so it arrives intact, and RFC 3156 canonicalisation then removes it
// from the hash on both sides. Encoding it is what makes those two agree.
func TestTrailingWhitespaceSurvivesEncoding(t *testing.T) {
	const text = "trailing spaces here   \nand a tab\t\nnon-ascii é forces encoding\n"

	built := buildOne(t, Draft{TextBody: text})
	body := bodyOf(t, built.Raw)

	if !strings.Contains(body, "=20") && !strings.Contains(body, "=09") {
		t.Error("trailing whitespace was not escaped; a gateway that strips it would " +
			"change the bytes the recipient hashes")
	}
	decoded, err := io.ReadAll(quotedprintable.NewReader(strings.NewReader(body)))
	if err != nil {
		t.Fatalf("decoding the body failed: %v", err)
	}
	if want := normalizeEOL(text); string(decoded) != want {
		t.Errorf("trailing whitespace did not round-trip.\n got: %q\nwant: %q", decoded, want)
	}
}
