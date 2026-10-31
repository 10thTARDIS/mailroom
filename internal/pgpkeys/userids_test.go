// SPDX-License-Identifier: AGPL-3.0-only

package pgpkeys

import (
	"bytes"
	stdcrypto "crypto"
	"strings"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

// uidTestKey is a real go-crypto key with several user IDs: the primary, a
// second one, a REVOKED one, one whose name carries control and bidi characters,
// and one with an over-long name. Then, appended to its packets, a user ID with
// NO self-signature and one whose "self-signature" is another user ID's (so it
// does not verify).
func uidTestKey(t *testing.T) []byte {
	t.Helper()
	now := time.Now().Add(-time.Hour)
	cfg := &packet.Config{Algorithm: packet.PubKeyAlgoEdDSA, Curve: packet.Curve25519, Time: func() time.Time { return now }}
	e, err := openpgp.NewEntity("Alice Example", "", "alice@example.com", cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range [][2]string{
		{"Alice Work", "alice@work.example"},
		{"Alice Old", "old@example.com"},
		{"\x1b[31mEvil\u202emoc.elpmaxe\x07", "evil@example.com"},
		{strings.Repeat("L", 300), "long@example.com"},
	} {
		if err := e.AddUserId(u[0], "", u[1], cfg); err != nil {
			t.Fatalf("AddUserId: %v", err)
		}
	}
	// Revoke "Alice Old" with a real certification revocation by the primary.
	later := now.Add(time.Minute)
	for name, ident := range e.Identities {
		if !strings.Contains(name, "old@example.com") {
			continue
		}
		rev := *ident.SelfSignature
		rev.Notations = nil
		rev.SigType = packet.SigTypeCertificationRevocation
		rev.CreationTime = later
		rev.Hash = stdcrypto.SHA256
		if err := rev.SignUserId(name, e.PrimaryKey, e.PrivateKey, &packet.Config{Time: func() time.Time { return later }}); err != nil {
			t.Fatalf("revoke uid: %v", err)
		}
		ident.Revocations = append(ident.Revocations, &rev)
		ident.Signatures = append(ident.Signatures, &rev)
	}
	var buf bytes.Buffer
	if err := e.Serialize(&buf); err != nil {
		t.Fatal(err)
	}
	// A bare user ID: anyone can append one; it carries no signature.
	if err := packet.NewUserId("Bare", "", "bare@example.com").Serialize(&buf); err != nil {
		t.Fatal(err)
	}
	// A user ID carrying another user ID's self-signature: it does not verify.
	if err := packet.NewUserId("Forged", "", "forged@example.com").Serialize(&buf); err != nil {
		t.Fatal(err)
	}
	if err := e.PrimaryIdentity().SelfSignature.Serialize(&buf); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestUserIDsListsOnlySelfSignedAndCleansThem(t *testing.T) {
	pub := uidTestKey(t)
	uids, err := UserIDs(pub, time.Now())
	if err != nil {
		t.Fatalf("UserIDs: %v", err)
	}
	byEmail := map[string]UserID{}
	for _, u := range uids {
		byEmail[u.Email] = u
	}
	if len(uids) != 5 {
		t.Errorf("listed %d user IDs, want 5 (the self-signed ones): %+v", len(uids), uids)
	}
	if uids[0].Email != "alice@example.com" || !uids[0].Primary || uids[0].Name != "Alice Example" {
		t.Errorf("first = %+v, want the primary Alice Example <alice@example.com>", uids[0])
	}
	for _, u := range uids[1:] {
		if u.Primary {
			t.Errorf("%+v is marked primary too", u)
		}
	}
	if w, ok := byEmail["alice@work.example"]; !ok || w.Revoked || w.Name != "Alice Work" {
		t.Errorf("work = %+v", w)
	}
	if o, ok := byEmail["old@example.com"]; !ok || !o.Revoked {
		t.Errorf("the revoked user ID = %+v, want revoked", o)
	}
	for _, gone := range []string{"bare@example.com", "forged@example.com"} {
		if _, ok := byEmail[gone]; ok {
			t.Errorf("%s has no verifying self-signature and is listed anyway", gone)
		}
	}
	if ev := byEmail["evil@example.com"].Name; ev != "[31mEvilmoc.elpmaxe" {
		t.Errorf("control/format characters survived: %q", ev)
	}
	if l := byEmail["long@example.com"].Name; len([]rune(l)) != MaxUserIDFieldRunes {
		t.Errorf("over-long name is %d runes, want capped at %d", len([]rune(l)), MaxUserIDFieldRunes)
	}
}

// TestUserIDsPositiveControlForTheExcludedOnes: the same two user IDs ARE
// listed when they carry a real self-signature, so their absence above is the
// signature rule at work, not a parsing accident.
func TestUserIDsPositiveControlForTheExcludedOnes(t *testing.T) {
	cfg := &packet.Config{Algorithm: packet.PubKeyAlgoEdDSA, Curve: packet.Curve25519}
	e, err := openpgp.NewEntity("Alice", "", "alice@example.com", cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range [][2]string{{"Bare", "bare@example.com"}, {"Forged", "forged@example.com"}} {
		if err := e.AddUserId(u[0], "", u[1], cfg); err != nil {
			t.Fatal(err)
		}
	}
	var buf bytes.Buffer
	if err := e.Serialize(&buf); err != nil {
		t.Fatal(err)
	}
	uids, err := UserIDs(buf.Bytes(), time.Now())
	if err != nil || len(uids) != 3 {
		t.Fatalf("control: %+v %v, want 3 user IDs", uids, err)
	}
}

func TestCleanUserIDField(t *testing.T) {
	for in, want := range map[string]string{
		"Alice":                   "Alice",
		"  Alice\t":               "Alice",
		"A\u200bl\u2066ice\u202e": "Alice",
		"\x00\x07\x1b":            "",
		"Ünïcødé 名前":              "Ünïcødé 名前",
	} {
		if got := cleanUserIDField(in); got != want {
			t.Errorf("cleanUserIDField(%q) = %q, want %q", in, got, want)
		}
	}
}
