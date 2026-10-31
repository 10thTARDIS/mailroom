// SPDX-License-Identifier: AGPL-3.0-only

package pgpmime

import (
	"bytes"
	"testing"
)

// TestCanonicalizeIsExact asserts the output byte for byte rather than by
// property, because every interoperability failure in PGP/MIME is a single byte
// and a property test would have to encode the same rules it is checking.
func TestCanonicalizeIsExact(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{"empty stays empty", "", ""},
		{"bare LF becomes CRLF", "a\nb\n", "a\r\nb\r\n"},
		{"CRLF is left alone", "a\r\nb\r\n", "a\r\nb\r\n"},
		{"mixed endings all become CRLF", "a\r\nb\nc\r\n", "a\r\nb\r\nc\r\n"},
		{"trailing spaces go", "a   \r\n", "a\r\n"},
		{"trailing tabs go", "a\t\t\r\n", "a\r\n"},
		{"mixed trailing whitespace goes", "a \t \t\nb", "a\r\nb"},
		{"interior whitespace stays", "a  b  c \n", "a  b  c\r\n"},
		{"leading whitespace stays", "   a\n", "   a\r\n"},
		{"a line of only whitespace becomes empty", "  \t \n", "\r\n"},

		// The final newline decides whether the last line is terminated, and the
		// recipient's extracted part will or will not have one. These two must
		// not collapse into each other.
		{"no trailing newline is preserved as absent", "a\r\nb", "a\r\nb"},
		{"trailing newline is preserved as present", "a\r\nb\r\n", "a\r\nb\r\n"},
		{"a lone newline is a line", "\n", "\r\n"},
		{"two trailing newlines stay two", "a\n\n", "a\r\n\r\n"},

		// Trailing whitespace on the LAST line, unterminated, is the case that
		// falls out of a loop written per-newline rather than per-line.
		{"trailing whitespace on an unterminated last line", "a\r\nb  ", "a\r\nb"},

		{"a lone CR is content, not a line ending", "a\rb\n", "a\rb\r\n"},
		{"NUL and high bytes are untouched", "a\x00\xffb\n", "a\x00\xffb\r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Canonicalize([]byte(tc.in))
			if string(got) != tc.want {
				t.Errorf("Canonicalize(%q)\n = %q\nwant %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestCanonicalizeIsIdempotent is the property verification actually depends
// on: the recipient canonicalises a part that has already been through this
// once. If the two runs disagree, every signature fails at the far end and
// nowhere here.
func TestCanonicalizeIsIdempotent(t *testing.T) {
	for _, in := range []string{
		"", "\n", "a", "a\n", "a\r\n", "a  \n b \t\n", "a\r\r\n", "a\rb\n",
		"line one\r\nline two   \nline three\t",
		"Content-Type: text/plain\r\n\r\nbody \r\n",
		"\x00\xff\xfe\r\n  \n",
	} {
		once := Canonicalize([]byte(in))
		twice := Canonicalize(once)
		if !bytes.Equal(once, twice) {
			t.Errorf("not idempotent for %q: once=%q twice=%q", in, once, twice)
		}
	}
}

// TestCanonicalizeDoesNotMutateInput guards against the aliasing bytes.Split
// makes easy: the caller still needs the original bytes to transmit.
func TestCanonicalizeDoesNotMutateInput(t *testing.T) {
	in := []byte("a  \nb\t\r\nc")
	keep := append([]byte(nil), in...)
	_ = Canonicalize(in)
	if !bytes.Equal(in, keep) {
		t.Errorf("input was modified: %q, want %q", in, keep)
	}
}
