// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// ContactKey/get, ContactKey/set destroy, RecipientKey/lookup's expiry reasons
// and Identity.signingKeyId against a key's packets (#795/#796). Every key is a
// real go-crypto key with a real lifetime.

import (
	"context"
	"testing"
	"time"

	"mailroom/internal/store"
)

func (h *encHarness) contactRows(t *testing.T) int {
	t.Helper()
	var n int
	if err := h.super.QueryRow(context.Background(),
		`SELECT count(*) FROM app.change_log WHERE user_id = $1 AND object_type = 'Contact'`, h.a.UserID.String()).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (h *encHarness) contactID(t *testing.T, address string) string {
	t.Helper()
	var cid string
	if err := h.super.QueryRow(context.Background(),
		`SELECT contact_id FROM app.contact_addresses WHERE user_id=$1 AND address=$2`, h.a.UserID.String(), address).Scan(&cid); err != nil {
		t.Fatal(err)
	}
	return cid
}

func TestContactKeyGetReportsExpiryAndPins(t *testing.T) {
	h := newEncHarness(t)
	now := time.Now().UTC()
	const addr = "peer@example.com"
	created := now.Add(-40 * 24 * time.Hour).Truncate(time.Second)
	expiredFpr := h.seedKey(t, addr, realOwnKey(t, addr, created, 30*24*time.Hour), store.ContactKeyWKD, false, now.Add(-time.Hour))
	soonFpr := h.seedKey(t, addr, realOwnKey(t, addr, now.Add(-time.Hour), 10*24*time.Hour), store.ContactKeyAutocrypt, true, now)
	if _, err := h.super.Exec(context.Background(),
		`INSERT INTO app.contact_key_pins (user_id, address, fingerprint, source) VALUES ($1,$2,$3,'wkd')`,
		h.a.UserID.String(), addr, expiredFpr); err != nil {
		t.Fatal(err)
	}
	cid := h.contactID(t, addr)

	out := call(t, h.api.contactKeyGet, h.a.UserID, map[string]any{"contactIds": []string{cid, store.NewULID().String()}})
	if nf := asArr(out["notFound"]); len(nf) != 1 {
		t.Errorf("notFound = %v, want the one unknown id", nf)
	}
	byFpr := map[string]map[string]any{}
	for _, e := range asArr(out["list"]) {
		m := e.(map[string]any)
		byFpr[m["fingerprint"].(string)] = m
		if _, leaked := m["publicKey"]; leaked {
			t.Fatal("ContactKey/get serialised key material")
		}
	}
	ex, soon := byFpr[expiredFpr], byFpr[soonFpr]
	if ex == nil || soon == nil {
		t.Fatalf("list = %v", out["list"])
	}
	if ex["expired"] != true || ex["usable"] != false || ex["pinned"] != true || ex["source"] != "wkd" ||
		ex["contactId"] != cid || !parseDate(t, ex["createdAt"]).Equal(created) ||
		!parseDate(t, ex["expiresAt"]).Equal(created.Add(30*24*time.Hour)) {
		t.Errorf("expired key = %v", ex)
	}
	if pf := asArr(ex["pinnedFor"]); len(pf) != 1 || pf[0] != addr {
		t.Errorf("pinnedFor = %v", pf)
	}
	if soon["expiringSoon"] != true || soon["usable"] != true || soon["pinned"] != false || soon["authenticated"] != true {
		t.Errorf("soon key = %v", soon)
	}
	all := call(t, h.api.contactKeyGet, h.a.UserID, map[string]any{})
	if len(asArr(all["list"])) != 2 || all["state"] != out["state"] {
		t.Errorf("ids:null list = %d entries, state %v vs %v", len(asArr(all["list"])), all["state"], out["state"])
	}

	// Remove the pinned expired key: the pin goes with it, one Contact row.
	c0 := h.contactRows(t)
	set := call(t, h.api.contactKeySet, h.a.UserID, map[string]any{"destroy": []string{ex["id"].(string)}})
	if d := asArr(set["destroyed"]); len(d) != 1 || set["oldState"] == set["newState"] {
		t.Errorf("destroy = %v", set)
	}
	if got := h.contactRows(t) - c0; got != 1 {
		t.Errorf("destroy wrote %d Contact rows, want 1", got)
	}
	if _, pinned := h.pin(t, addr); pinned {
		t.Error("removing the pinned key left its pin")
	}
	// RecipientKey/lookup: with the pinned expired key gone, the 10-day key is chosen, with its date.
	res := call(t, h.api.recipientKeyLookup, h.a.UserID, map[string]any{"addresses": []string{addr}, "wkd": false})
	r := asArr(res["list"])[0].(map[string]any)
	if r["usable"] != true || r["fingerprint"] != soonFpr || r["expiringSoon"] != true || r["reason"] != nil {
		t.Errorf("lookup with a live key = %v", r)
	}

	// Destroying it again (and an unknown id) is notFound and writes nothing.
	c0 = h.contactRows(t)
	set = call(t, h.api.contactKeySet, h.a.UserID, map[string]any{"destroy": []string{ex["id"].(string), "nope"}})
	if nd, _ := set["notDestroyed"].(map[string]any); len(nd) != 2 {
		t.Errorf("second destroy = %v", set)
	}
	if got := h.contactRows(t) - c0; got != 0 {
		t.Errorf("no-op destroy wrote %d Contact rows", got)
	}
	// A key removed while another is pinned leaves that pin alone.
	h.super.Exec(context.Background(), `INSERT INTO app.contact_key_pins (user_id, address, fingerprint, source) VALUES ($1,$2,$3,'autocrypt')`, //nolint:errcheck
		h.a.UserID.String(), addr, soonFpr)
	other := h.seedKey(t, addr, realOwnKey(t, addr, now.Add(-time.Hour), 0), store.ContactKeyWKD, false, now)
	var otherID string
	for _, e := range asArr(call(t, h.api.contactKeyGet, h.a.UserID, map[string]any{})["list"]) {
		if m := e.(map[string]any); m["fingerprint"] == other {
			otherID = m["id"].(string)
		}
	}
	call(t, h.api.contactKeySet, h.a.UserID, map[string]any{"destroy": []string{otherID}})
	if fpr, ok := h.pin(t, addr); !ok || fpr != soonFpr {
		t.Errorf("removing an unpinned key moved the pin: %q %v", fpr, ok)
	}
	// create and update are refused.
	set = call(t, h.api.contactKeySet, h.a.UserID, map[string]any{"create": map[string]any{"x": map[string]any{}}, "update": map[string]any{otherID: map[string]any{}}})
	if set["notCreated"] == nil || set["notUpdated"] == nil {
		t.Errorf("create/update not refused: %v", set)
	}
}

func TestRecipientKeyLookupSaysExpired(t *testing.T) {
	h := newEncHarness(t)
	now := time.Now().UTC()
	const addr = "lapsed@example.com"
	created := now.Add(-40 * 24 * time.Hour).Truncate(time.Second)
	h.seedKey(t, addr, realOwnKey(t, addr, created, 30*24*time.Hour), store.ContactKeyManual, false, now)
	res := call(t, h.api.recipientKeyLookup, h.a.UserID, map[string]any{"addresses": []string{addr, "nobody@example.com"}, "wkd": false})
	list := asArr(res["list"])
	r, n := list[0].(map[string]any), list[1].(map[string]any)
	if r["usable"] != false || r["reason"] != "expired" || !parseDate(t, r["expiredAt"]).Equal(created.Add(30*24*time.Hour)) {
		t.Errorf("expired-only recipient = %v, want reason expired with the date", r)
	}
	if n["reason"] != "noKey" || n["expiredAt"] != nil {
		t.Errorf("keyless recipient = %v, want reason noKey", n)
	}
	// An INELIGIBLE expired key (unauthenticated Autocrypt) is not a reason: it
	// could never have been used, so the answer stays noKey.
	const addr2 = "spoofed@example.com"
	h.seedKey(t, addr2, realOwnKey(t, addr2, created, 30*24*time.Hour), store.ContactKeyAutocrypt, false, now)
	res = call(t, h.api.recipientKeyLookup, h.a.UserID, map[string]any{"addresses": []string{addr2}, "wkd": false})
	if r := asArr(res["list"])[0].(map[string]any); r["reason"] != "noKey" {
		t.Errorf("ineligible expired key = %v, want noKey", r)
	}
}

// Identity.signingKeyId ("Will sign") reads the bound key's packets (#796,
// ADR-040 amendment): a bound key whose primary has expired no longer reads as
// signing, exactly as compose then sends unsigned. Control: a live bound key does.
func TestIdentitySigningKeyIdFollowsTheKeysPackets(t *testing.T) {
	for _, c := range []struct {
		name     string
		lifetime time.Duration
		created  time.Duration
		wantSign bool
	}{
		{"live key", 0, -time.Hour, true},
		{"expired key", 30 * 24 * time.Hour, -40 * 24 * time.Hour, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, _ := newSigningHarness(t, &stubSigner{})
			a, _ := seedTwo(t, h.super)
			seedSendable(t, h.super, a, "me@example.com", "")
			bindRealKey(t, h, a.UserID, "me@example.com", realOwnKey(t, "me@example.com", time.Now().Add(c.created), c.lifetime))
			list := asArr(call(t, h.api.methods["Identity/get"], a.UserID, map[string]any{})["list"])
			if len(list) != 1 {
				t.Fatalf("identities = %v", list)
			}
			got := list[0].(map[string]any)["signingKeyId"] != nil
			if got != c.wantSign {
				t.Errorf("signingKeyId present = %v, want %v", got, c.wantSign)
			}
		})
	}
}
