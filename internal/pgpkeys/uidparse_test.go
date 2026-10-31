// SPDX-License-Identifier: AGPL-3.0-only

package pgpkeys

import (
	"errors"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

// The user ID strings are what a hostile or merely unusual key can carry. The
// address is the LAST <...> group, accepted only when it is a plain address and
// ends the user ID; anything else has no address (fail closed). #822.
func TestParseUserID(t *testing.T) {
	cases := []struct {
		id, name, email string
	}{
		{"Alice Example <alice@example.org>", "Alice Example", "alice@example.org"},
		{"<alice@example.org>", "", "alice@example.org"},
		{"alice@example.org", "", "alice@example.org"},
		{"  Alice <alice@example.org>  ", "Alice", "alice@example.org"},
		{"Alice (work) <alice@example.org>", "Alice (work)", "alice@example.org"},
		// The bug: the first group is "b".
		{"Alice <b>Bold</b> <alice@example.org>", "Alice <b>Bold</b>", "alice@example.org"},
		{"Alice <a@x.example> <b@y.example>", "Alice <a@x.example>", "b@y.example"},
		{"<b> <alice@example.org>", "<b>", "alice@example.org"},
		// No address at all.
		{"Alice Example", "Alice Example", ""},
		{"", "", ""},
		{"Alice <b>Bold</b>", "Alice <b>Bold</b>", ""},
		{"Alice <>", "Alice <>", ""},
		{"Alice <notanaddress>", "Alice <notanaddress>", ""},
		// Ambiguous or hostile: no address.
		{"Alice <alice@example.org> trailing", "Alice <alice@example.org> trailing", ""},
		{"Alice <alice@example.org> (comment)", "Alice <alice@example.org> (comment)", ""},
		{"Alice <<alice@example.org>>", "Alice <<alice@example.org>>", ""},
		{"Alice <x<alice@example.org>", "Alice <x", "alice@example.org"}, // the last group is clean
		{"Alice <a@b@example.org>", "Alice <a@b@example.org>", ""},
		{"Alice <a@example.org, b@example.org>", "Alice <a@example.org, b@example.org>", ""},
		{"Alice <\"x y\"@example.org>", "Alice <\"x y\"@example.org>", ""},
		{"Alice <Bob <bob@example.org>>", "Alice <Bob <bob@example.org>>", ""},
		{"Alice <@route:alice@example.org>", "Alice <@route:alice@example.org>", ""},
		{"Alice <alice@example.org\x00>", "Alice <alice@example.org\x00>", ""},
		// Unicode and bidi stay inside the string, never invented.
		{"Aliçe <alice@example.org>", "Aliçe", "alice@example.org"},
		{"\u202eAlice <alice@example.org>", "\u202eAlice", "alice@example.org"},
		{"Alice <alice@exam\u202eple.org>", "Alice <alice@exam\u202eple.org>", ""},
		{"Alice <élise@example.org>", "Alice", "élise@example.org"},
	}
	for _, c := range cases {
		name, email := parseUserID(c.id)
		if name != c.name || email != c.email {
			t.Errorf("parseUserID(%q) = (%q, %q), want (%q, %q)", c.id, name, email, c.name, c.email)
		}
	}
}

// rawUIDEntity is a real key whose only user ID is exactly id. go-crypto's
// NewEntity refuses angle brackets in a name, so the user ID is replaced and
// re-signed by the primary key, as gpg would have made it.
func rawUIDEntity(t *testing.T, id string) *openpgp.Entity {
	t.Helper()
	e := newEntity(t, "Placeholder", "placeholder@example.org")
	var old *openpgp.Identity
	for _, ident := range e.Identities {
		old = ident
	}
	sig := *old.SelfSignature
	if err := sig.SignUserId(id, e.PrimaryKey, e.PrivateKey, nil); err != nil {
		t.Fatalf("SignUserId: %v", err)
	}
	e.Identities = map[string]*openpgp.Identity{id: {
		Name: id, UserId: &packet.UserId{Id: id}, SelfSignature: &sig, Signatures: []*packet.Signature{&sig},
	}}
	return e
}

// Real keys (go-crypto makes the user ID packet from name and address): the
// address list, NamesAddress and the display list agree, and the failure
// closes. The negative control is the pre-fix behaviour, where the first
// group "b" was the address.
func TestUserIDWithBracketsInTheNameAgreesEverywhere(t *testing.T) {
	cases := []struct {
		id       string
		wantAddr string // "" = the key names nothing
	}{
		{"Alice <b>Bold</b> <alice@example.org>", "alice@example.org"},
		{"Alice <a@x.example> <alice@example.org>", "alice@example.org"},
		{"Alice <<b>> <alice@example.org>", "alice@example.org"},
		{"Alice <b>Bold</b>", ""},
		{"Alice Example", ""},
		{"Alice <alice@example.org> trailing", ""},
	}
	for _, c := range cases {
		e := rawUIDEntity(t, c.id)
		pub := publicBytes(t, e)
		now := time.Now() // after the key exists: a self-signature dated later than now is not yet valid

		k, err := Parse(pub)
		if err != nil {
			t.Fatalf("%q: Parse: %v", c.id, err)
		}
		var gotAddrs []string
		gotAddrs = append(gotAddrs, k.Addresses...)
		if c.wantAddr == "" && len(gotAddrs) != 0 {
			t.Errorf("%q: Addresses = %v, want none", c.id, gotAddrs)
		}
		if c.wantAddr != "" && (len(gotAddrs) != 1 || gotAddrs[0] != c.wantAddr) {
			t.Errorf("%q: Addresses = %v, want [%s]", c.id, gotAddrs, c.wantAddr)
		}

		for _, probe := range []string{"alice@example.org", "b", "b@example.org", "a@x.example"} {
			want := probe == c.wantAddr
			if got := NamesAddress(pub, probe, now); got != want {
				t.Errorf("%q: NamesAddress(%q) = %v, want %v", c.id, probe, got, want)
			}
		}

		ids, err := UserIDs(pub, now)
		if err != nil || len(ids) != 1 {
			t.Fatalf("%q: UserIDs = %v, %v", c.id, ids, err)
		}
		if ids[0].Email != c.wantAddr {
			t.Errorf("%q: display email = %q, want %q", c.id, ids[0].Email, c.wantAddr)
		}
		// The display and the matching agree: whatever is shown as the address
		// is exactly what NamesAddress accepts, and nothing else.
		if ids[0].Email != "" && !NamesAddress(pub, ids[0].Email, now) {
			t.Errorf("%q: shown address %q is not one the key names", c.id, ids[0].Email)
		}
	}
}

// The mismatch warning path: a key whose user ID has a bracketed name and a
// different address does not cover/gate for the address in the bracket.
func TestBracketedNameKeyStillGatesEncryption(t *testing.T) {
	e := rawUIDEntity(t, "Alice <bob@example.org> <alice@example.org>")
	now := time.Now()
	pub := publicBytes(t, e)
	if _, err := EncryptionFingerprintFor(pub, "alice@example.org", now); err != nil {
		t.Errorf("the real address was refused: %v", err)
	}
	if _, err := EncryptionFingerprintFor(pub, "bob@example.org", now); !errors.Is(err, ErrNamesOtherAddress) {
		t.Errorf("the address planted in the name was accepted: %v", err)
	}
	k, err := Parse(pub)
	if err != nil {
		t.Fatal(err)
	}
	if k.CoversAddress("bob@example.org") || !k.CoversAddress("alice@example.org") {
		t.Errorf("CoversAddress disagrees with the user ID: %v", k.Addresses)
	}
}

// Primary user ID selection and the lists are stable: map order is random, so
// without a tie-break two same-second user IDs flip between runs.
func TestPrimaryUserIDIsDeterministic(t *testing.T) {
	e := newEntity(t, "Alice", "a@example.org")
	for _, a := range []string{"b@example.org", "c@example.org", "d@example.org", "e@example.org"} {
		if err := e.AddUserId("Alice", "", a, nil); err != nil {
			t.Fatal(err)
		}
	}
	// Make every self-signature identical in time and flags, so only the
	// tie-break decides.
	var ref = e.Identities["Alice <a@example.org>"].SelfSignature.CreationTime
	for name, ident := range e.Identities {
		ident.SelfSignature.CreationTime = ref
		ident.SelfSignature.IsPrimaryId = nil
		// Re-sign: Serialize reuses the signed subpackets, so editing the
		// struct alone would change nothing on the wire.
		if err := ident.SelfSignature.SignUserId(name, e.PrimaryKey, e.PrivateKey, nil); err != nil {
			t.Fatal(err)
		}
	}
	pub := publicBytes(t, e)
	first, err := Parse(pub)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 40; i++ {
		k, err := Parse(pub)
		if err != nil {
			t.Fatal(err)
		}
		for j := range k.UserIDs {
			if k.UserIDs[j] != first.UserIDs[j] {
				t.Fatalf("UserIDs order changed between parses: %v vs %v", first.UserIDs, k.UserIDs)
			}
		}
		ids, err := UserIDs(pub, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if len(ids) == 0 || ids[0].Email != "a@example.org" || !ids[0].Primary {
			t.Fatalf("primary flipped: %+v", ids)
		}
	}
}
