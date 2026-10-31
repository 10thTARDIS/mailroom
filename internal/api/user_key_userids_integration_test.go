// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// User IDs on the wire (#812): UserKey/get and ContactKey/get list a key's
// self-signed user IDs, UserKey says whether one names the bound address, and
// binding a key that does not name it succeeds with a warning.

import (
	"bytes"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"

	"mailroom/internal/store"
)

func armoredPrivateWithUIDs(t *testing.T, uids ...[2]string) string {
	t.Helper()
	cfg := &packet.Config{Algorithm: packet.PubKeyAlgoEdDSA, Curve: packet.Curve25519}
	e, err := openpgp.NewEntity(uids[0][0], "", uids[0][1], cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range uids[1:] {
		if err := e.AddUserId(u[0], "", u[1], cfg); err != nil {
			t.Fatal(err)
		}
	}
	var buf bytes.Buffer
	w, err := armor.Encode(&buf, openpgp.PrivateKeyType, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.SerializePrivate(w, nil); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func uidEmails(t *testing.T, key map[string]any) map[string]map[string]any {
	t.Helper()
	raw, ok := key["userIds"].([]any)
	if !ok {
		t.Fatalf("userIds missing: %v", key)
	}
	out := map[string]map[string]any{}
	for _, u := range raw {
		m := u.(map[string]any)
		out[m["email"].(string)] = m
	}
	return out
}

func TestUserKeyUserIDsAndTheBindWarning(t *testing.T) {
	h := newKeyHarness(t)
	a := h.a.UserID

	// A generated key: one user ID, the identity's address, primary.
	_, key := h.generate(t, a, h.identA, map[string]any{"bind": true})
	if u := uidEmails(t, key); len(u) != 1 || u["me@example.com"]["primary"] != true {
		t.Errorf("generated key userIds = %v", key["userIds"])
	}
	if key["boundAddressListed"] != true {
		t.Errorf("generated+bound key boundAddressListed = %v, want true", key["boundAddressListed"])
	}

	// An imported key naming two OTHER addresses: listed, and binding it to
	// me@example.com succeeds with the warning.
	imp := h.must(t, "UserKey/import", a, map[string]any{"privateKey": armoredPrivateWithUIDs(t,
		[2]string{"Kim Kleo", "kleo@other.example"}, [2]string{"Second", "second@other.example"})})
	ik := imp["key"].(map[string]any)
	u := uidEmails(t, ik)
	if len(u) != 2 || u["kleo@other.example"]["primary"] != true || u["kleo@other.example"]["name"] != "Kim Kleo" ||
		u["second@other.example"]["primary"] != false || u["second@other.example"]["revoked"] != false {
		t.Errorf("imported key userIds = %v", ik["userIds"])
	}
	if ik["boundAddressListed"] != nil {
		t.Errorf("an unbound key has boundAddressListed %v, want null", ik["boundAddressListed"])
	}
	impID := imp["id"].(string)
	out := h.must(t, "UserKey/set", a, map[string]any{"update": map[string]any{impID: map[string]any{"identityId": h.identA.String()}}})
	up, _ := out["updated"].(map[string]any)
	got, _ := up[impID].(map[string]any)
	if got == nil || got["warning"] != addressNotInKey || got["boundAddressListed"] != false {
		t.Errorf("bind of a key not naming the address: updated = %v, want the addressNotInKey warning", out["updated"])
	}
	k, _ := h.getKey(t, a, impID)
	if k["identityId"] != h.identA.String() || k["boundAddressListed"] != false {
		t.Errorf("after the warned bind: %v (the bind must still happen)", k)
	}

	// Control: binding a key that DOES name the address carries no warning.
	gid, _ := h.generate(t, a, h.identA, nil)
	out = h.must(t, "UserKey/set", a, map[string]any{"update": map[string]any{gid: map[string]any{"identityId": h.identA.String()}}})
	up, _ = out["updated"].(map[string]any)
	if got, _ := up[gid].(map[string]any); got == nil || got["boundAddressListed"] != true || got["warning"] != nil {
		t.Errorf("bind of a key naming the address: updated = %v", out["updated"])
	}
}

func TestContactKeyGetListsUserIDs(t *testing.T) {
	h := newEncHarness(t)
	cfg := &packet.Config{Algorithm: packet.PubKeyAlgoEdDSA, Curve: packet.Curve25519}
	e, err := openpgp.NewEntity("Bob", "", "bob@example.com", cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.AddUserId("Bob at work", "", "bob@work.example", cfg); err != nil {
		t.Fatal(err)
	}
	fpr := h.seedKey(t, "bob@example.com", pubOf(t, e), store.ContactKeyWKD, false, time.Now())
	_, k := h.contactKeyID(t, fpr, false)
	u := uidEmails(t, k)
	if len(u) != 2 || u["bob@example.com"]["primary"] != true || u["bob@work.example"]["name"] != "Bob at work" {
		t.Errorf("ContactKey userIds = %v", k["userIds"])
	}
}
