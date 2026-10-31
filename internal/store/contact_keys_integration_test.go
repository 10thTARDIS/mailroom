// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// app.contact_keys, which had no writer at all until #48. The ingest suite
// covers the end-to-end Autocrypt path; these are the store-level behaviours it
// does not reach — re-discovery, key rotation, and the source vocabulary.

import (
	"context"
	"crypto/sha256"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"
)

type contactKeyFixture struct {
	h      *harness
	st     *Store
	userID ulid.ULID
}

func newContactKeyFixture(t *testing.T) *contactKeyFixture {
	t.Helper()
	h := newHarness(t)
	f := &contactKeyFixture{h: h, st: h.appStore(), userID: NewULID()}
	if _, err := h.super.Exec(context.Background(),
		`INSERT INTO app.users (id, oidc_subject, email) VALUES ($1, $2, $3)`,
		f.userID.String(), "sub-"+f.userID.String(), f.userID.String()+"@example.com"); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return f
}

// seedContact creates a contact with one address and returns its id.
func (f *contactKeyFixture) seedContact(t *testing.T, addr string) ulid.ULID {
	t.Helper()
	ctx := context.Background()
	id := NewULID()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := f.h.super.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed contact: %v", err)
		}
	}
	exec(`INSERT INTO app.contacts (id, user_id, display_name) VALUES ($1, $2, $3)`,
		id.String(), f.userID.String(), addr)
	exec(`INSERT INTO app.contact_addresses (id, user_id, contact_id, address)
	      VALUES ($1, $2, $3, $4)`,
		NewULID().String(), f.userID.String(), id.String(), addr)
	return id
}

func fakeKeyBytes(seed string) []byte {
	h := sha256.Sum256([]byte(seed))
	return append([]byte{0x99, 0x01, 0x0d}, h[:]...)
}

// TestContactKeyRediscoveryIsNotANewKey is what makes running discovery
// repeatedly cheap, and what makes "how many NEW keys did that find" a real
// number rather than a count of rows touched.
func TestContactKeyRediscoveryIsNotANewKey(t *testing.T) {
	f := newContactKeyFixture(t)
	ctx := context.Background()
	contact := f.seedContact(t, "carol@example.com")

	row := ContactKeyRow{
		ContactID: contact, Kind: KeyPGP,
		Fingerprint: "AAAA1111BBBB2222CCCC3333DDDD4444EEEE5555",
		PublicKey:   fakeKeyBytes("carol"), Source: ContactKeyAutocrypt,
	}

	var first, second bool
	if err := f.st.UserTx(ctx, f.userID, func(tx *UserTx) error {
		var e error
		if first, e = tx.UpsertContactKey(ctx, row); e != nil {
			return e
		}
		second, e = tx.UpsertContactKey(ctx, row)
		return e
	}); err != nil {
		t.Fatalf("UpsertContactKey: %v", err)
	}
	if !first {
		t.Error("the first sighting of a key must report created=true")
	}
	if second {
		t.Error("re-seeing the same key reported created=true — a repeated discovery would look like a find")
	}
}

// TestContactKeyRotationKeepsBothKeys. People rotate keys and publish more than
// one; dropping the old one would break verification of everything already
// received. Choosing which key to ENCRYPT with is a separate decision that
// belongs with #47, and it needs this history to exist.
func TestContactKeyRotationKeepsBothKeys(t *testing.T) {
	f := newContactKeyFixture(t)
	ctx := context.Background()
	contact := f.seedContact(t, "dave@example.com")

	if err := f.st.UserTx(ctx, f.userID, func(tx *UserTx) error {
		for _, fpr := range []string{
			"1111111111111111111111111111111111111111",
			"2222222222222222222222222222222222222222",
		} {
			if _, e := tx.UpsertContactKey(ctx, ContactKeyRow{
				ContactID: contact, Kind: KeyPGP, Fingerprint: fpr,
				PublicKey: fakeKeyBytes(fpr), Source: ContactKeyWKD,
			}); e != nil {
				return e
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("UpsertContactKey: %v", err)
	}

	var keys []ContactKey
	if err := f.st.UserTx(ctx, f.userID, func(tx *UserTx) error {
		var e error
		keys, e = tx.ContactKeysFor(ctx, contact)
		return e
	}); err != nil {
		t.Fatalf("ContactKeysFor: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("contact holds %d key(s), want 2 — a rotation must not silently replace the old key", len(keys))
	}
}

// TestCountContactKeysSplitsBySource. "12 keys" cannot distinguish a working
// WKD fetcher from a working Autocrypt reader, and those two failing are
// different problems with different fixes.
func TestCountContactKeysSplitsBySource(t *testing.T) {
	f := newContactKeyFixture(t)
	ctx := context.Background()
	a := f.seedContact(t, "erin@example.com")
	b := f.seedContact(t, "frank@example.com")

	if err := f.st.UserTx(ctx, f.userID, func(tx *UserTx) error {
		if _, e := tx.UpsertContactKey(ctx, ContactKeyRow{
			ContactID: a, Fingerprint: "AAAA", PublicKey: fakeKeyBytes("a"), Source: ContactKeyAutocrypt,
		}); e != nil {
			return e
		}
		_, e := tx.UpsertContactKey(ctx, ContactKeyRow{
			ContactID: b, Fingerprint: "BBBB", PublicKey: fakeKeyBytes("b"), Source: ContactKeyWKD,
		})
		return e
	}); err != nil {
		t.Fatalf("UpsertContactKey: %v", err)
	}

	var counts map[string]int
	if err := f.st.UserTx(ctx, f.userID, func(tx *UserTx) error {
		var e error
		counts, e = tx.CountContactKeys(ctx)
		return e
	}); err != nil {
		t.Fatalf("CountContactKeys: %v", err)
	}
	if counts[ContactKeyAutocrypt] != 1 || counts[ContactKeyWKD] != 1 {
		t.Fatalf("counts = %v, want one autocrypt and one wkd", counts)
	}
}

// TestUpsertContactKeyRejectsAnUnknownSource keeps the `source` column's
// vocabulary closed. A typo'd source would silently create a category no report
// counts and no future encrypt decision knows how to weigh.
func TestUpsertContactKeyRejectsAnUnknownSource(t *testing.T) {
	f := newContactKeyFixture(t)
	ctx := context.Background()
	contact := f.seedContact(t, "grace@example.com")

	err := f.st.UserTx(ctx, f.userID, func(tx *UserTx) error {
		_, e := tx.UpsertContactKey(ctx, ContactKeyRow{
			ContactID: contact, Fingerprint: "CCCC", PublicKey: fakeKeyBytes("c"), Source: "telepathy",
		})
		return e
	})
	if err == nil || !strings.Contains(err.Error(), "unknown source") {
		t.Fatalf("error = %v, want a refusal naming the unknown source", err)
	}
}

// TestContactIDForAddressIsCaseInsensitive — addresses arrive in whatever case
// a sender used, and the column is citext precisely so this holds.
func TestContactIDForAddressIsCaseInsensitive(t *testing.T) {
	f := newContactKeyFixture(t)
	ctx := context.Background()
	want := f.seedContact(t, "helen@example.com")

	if err := f.st.UserTx(ctx, f.userID, func(tx *UserTx) error {
		got, ok, e := tx.ContactIDForAddress(ctx, "HELEN@Example.COM")
		if e != nil {
			return e
		}
		if !ok || got != want {
			t.Errorf("ContactIDForAddress(mixed case) = %v ok=%v, want %v", got, ok, want)
		}
		// And the control: an address nobody has must not resolve.
		if _, ok, e = tx.ContactIDForAddress(ctx, "nobody@example.com"); e != nil {
			return e
		} else if ok {
			t.Error("an unknown address resolved to a contact")
		}
		return nil
	}); err != nil {
		t.Fatalf("ContactIDForAddress: %v", err)
	}
}
