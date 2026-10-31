// SPDX-License-Identifier: AGPL-3.0-only

package wkd

import "testing"

// TestHashLocalPartAgainstTheDraftVector is the only test here that can tell us
// we implemented the SPECIFICATION rather than something self-consistent.
//
// The vector is published in draft-koch-openpgp-webkey-service itself:
// "Joe.Doe@example.org" maps to iy9q119eutrkn8s1mk4r39qejnbu3n5q. Every other
// assertion in this package compares our code against our code; this one
// compares it against the document a correspondent's GnuPG implements. Without
// it, a wrong alphabet or a wrong hash would produce stable, plausible,
// entirely undiscoverable paths — and the failure would show up as "nobody can
// find my key", months later, with nothing red anywhere.
func TestHashLocalPartAgainstTheDraftVector(t *testing.T) {
	const (
		localPart = "Joe.Doe"
		want      = "iy9q119eutrkn8s1mk4r39qejnbu3n5q"
	)
	if got := HashLocalPart(localPart); got != want {
		t.Fatalf("HashLocalPart(%q) = %q, want %q (draft-koch-openpgp-webkey-service test vector)",
			localPart, got, want)
	}
}

// TestHashIsCaseInsensitiveOnTheLocalPart: the draft down-cases before hashing,
// so Joe.Doe and joe.doe must land in the same place. If they did not, a
// correspondent typing the address with different capitalisation would find
// nothing.
func TestHashIsCaseInsensitiveOnTheLocalPart(t *testing.T) {
	if a, b := HashLocalPart("Joe.Doe"), HashLocalPart("joe.doe"); a != b {
		t.Errorf("case changed the path: %q vs %q", a, b)
	}
	if a, b := HashLocalPart("JOE.DOE"), HashLocalPart("joe.doe"); a != b {
		t.Errorf("case changed the path: %q vs %q", a, b)
	}
}

// TestLowercasingIsASCIIOnly pins the deliberate choice not to use Unicode
// casing. Go's strings.ToLower maps the Turkish dotless capital İ (U+0130) to
// "i̇" (i + combining dot), which would hash to a different path than a client
// doing plain ASCII down-casing asks for. The draft says plain; we do plain.
func TestLowercasingIsASCIIOnly(t *testing.T) {
	const turkish = "İX" // İX
	// ASCII lowering touches only the X.
	if got, want := asciiLower(turkish), "İx"; got != want {
		t.Errorf("asciiLower(%q) = %q, want %q — Unicode casing would move the key's path", turkish, got, want)
	}
}

// TestZBase32IsNotStandardBase32 is a negative control on the alphabet.
//
// RFC 4648 base32 and z-base-32 encode the same bits and differ only in which
// character stands for which value, so a wrong-alphabet bug produces output of
// the right length and shape that is wrong in a way no eyeball catches. This
// asserts the two genuinely disagree, so the draft vector above is testing the
// alphabet and not merely the bit-packing.
func TestZBase32IsNotStandardBase32(t *testing.T) {
	// 0x00 encodes to 'a' in RFC 4648 and to 'y' in z-base-32.
	if got := zBase32([]byte{0x00}); got[0] != 'y' {
		t.Errorf("zBase32(0x00) starts %q, want 'y' — this looks like RFC 4648 base32", got[0])
	}
	if zBase32Alphabet == "abcdefghijklmnopqrstuvwxyz234567" {
		t.Error("the alphabet is RFC 4648's, not the draft's")
	}
}

func TestSplitAddress(t *testing.T) {
	for _, tc := range []struct {
		in            string
		local, domain string
		wantErr       bool
	}{
		{in: "joe@example.com", local: "joe", domain: "example.com"},
		{in: "Joe.Doe@Example.COM", local: "Joe.Doe", domain: "example.com"},
		// The local part keeps its case here: hashing lowercases, but the ?l=
		// parameter and the UID matching want what the user actually typed.
		{in: "a@b@example.com", local: "a@b", domain: "example.com"},
		{in: "nodomain", wantErr: true},
		{in: "@example.com", wantErr: true},
		{in: "joe@", wantErr: true},
		{in: "", wantErr: true},
	} {
		local, domain, err := SplitAddress(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("SplitAddress(%q) accepted an address it should refuse", tc.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("SplitAddress(%q): %v", tc.in, err)
			continue
		}
		if local != tc.local || domain != tc.domain {
			t.Errorf("SplitAddress(%q) = (%q, %q), want (%q, %q)", tc.in, local, domain, tc.local, tc.domain)
		}
	}
}

// TestDirectPathShape pins the path clients actually request. A change here
// silently unpublishes every key.
func TestDirectPathShape(t *testing.T) {
	const want = "/.well-known/openpgpkey/hu/iy9q119eutrkn8s1mk4r39qejnbu3n5q"
	if got := DirectPath("Joe.Doe"); got != want {
		t.Errorf("DirectPath = %q, want %q", got, want)
	}
	const wantAdv = "/.well-known/openpgpkey/example.org/hu/iy9q119eutrkn8s1mk4r39qejnbu3n5q"
	if got := AdvancedPath("Joe.Doe", "Example.ORG"); got != wantAdv {
		t.Errorf("AdvancedPath = %q, want %q", got, wantAdv)
	}
}

// TestHashLength: the draft's encoding of 20 bytes is always 32 characters. A
// short or long name is a malformed request path.
func TestHashLength(t *testing.T) {
	for _, in := range []string{"a", "joe.doe", "a-very-long-local-part-indeed-here"} {
		if got := HashLocalPart(in); len(got) != 32 {
			t.Errorf("HashLocalPart(%q) is %d chars, want 32", in, len(got))
		}
	}
}
