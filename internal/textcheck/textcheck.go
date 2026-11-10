// SPDX-License-Identifier: AGPL-3.0-only

// Package textcheck is the one validator for names a user types and Mailroom
// later prints elsewhere: rule names, API key names, OAuth client display
// names. Output is already neutralised (the admin CLI's safeText, the web's
// bdi-wrapped Untrusted); refusing the same characters when the name is SAVED
// removes the problem at the source and protects an output path that forgets.
//
// Existing names are not rewritten: this runs on the way in only.
package textcheck

import (
	"fmt"
	"unicode"
	"unicode/utf8"
)

// IsUnsafeRune reports whether r may not appear in a saved name: a C0 or C1
// control (newline and tab included, so a name is one line), any Unicode
// format (Cf) character (which covers the bidi overrides U+202A to U+202E, the
// isolates U+2066 to U+2069, the marks U+200E, U+200F and U+061C, and the
// zero-width space), and the line and paragraph separators U+2028 and U+2029.
//
// Two Cf characters are allowed: the zero-width non-joiner U+200C and joiner
// U+200D. They are part of ordinary writing (Persian, and other scripts that
// use the non-joiner) and of emoji sequences (a family, a flag, skin tones in
// a profession), they never reorder text, and refusing them stops real names
// being saved.
//
// The admin CLI's safeText uses this same class and adds the rest of
// Default_Ignorable_Code_Point on top, because it only replaces and so can be
// stricter than a refusal that stops a real name being saved.
func IsUnsafeRune(r rune) bool {
	if r == '\u200c' || r == '\u200d' {
		return false
	}
	return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == '\u2028' || r == '\u2029'
}

// CheckName returns nil for a name made only of ordinary characters (any script,
// emoji, combining marks included) and otherwise an error naming the first
// offending character by code point, never by echoing it, so the message is
// itself safe to print. Bytes that are not valid UTF-8 are refused too.
func CheckName(s string) error {
	if !utf8.ValidString(s) {
		return fmt.Errorf("is not valid UTF-8")
	}
	for _, r := range s {
		if IsUnsafeRune(r) {
			return fmt.Errorf("contains a control or invisible formatting character (U+%04X)", r)
		}
	}
	return nil
}
