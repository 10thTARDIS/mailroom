// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

import "testing"

// Learned display names are stored cleaned (#831), on both write paths:
// creation and fill-when-empty.
func TestLearnedContactNameStoredCleaned(t *testing.T) {
	f := newSavedFixture(t)

	// Creation: bidi override and zero-width joiner inside a name.
	a := f.addr("evil")
	f.received(a, "Pay\u202Epal\u200D Support")
	if got := f.row(f.idOf(a)).displayName; got != "Paypal Support" {
		t.Errorf("created name = %q, want %q", got, "Paypal Support")
	}

	// A name that is nothing but invisible characters stores "".
	b := f.addr("blank")
	f.received(b, "\u202E\u200B\u200D")
	if got := f.row(f.idOf(b)).displayName; got != "" {
		t.Errorf("all-invisible name = %q, want empty", got)
	}

	// Fill-when-empty: the first message had no name, the second has one with
	// hostile characters; the stored name is the cleaned one.
	c := f.addr("fill")
	f.received(c, "")
	f.received(c, "Al\u2067ice\u200B")
	if got := f.row(f.idOf(c)).displayName; got != "Alice" {
		t.Errorf("filled name = %q, want %q", got, "Alice")
	}
}
