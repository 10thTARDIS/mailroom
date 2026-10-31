// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

import (
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp/packet"

	"mailroom/internal/store"
)

// TestContactKeyRefreshSeesARevocationServedSecond: a WKD answer may be the
// correspondent's new key FOLLOWED by their revoked old one (ADR-040 §4). The
// revocation of the key we hold is found in the second block and merged
// (result revoked, one revoked_seen event), and the new key goes to the
// decision path as well.
func TestContactKeyRefreshSeesARevocationServedSecond(t *testing.T) {
	h := newEncHarness(t)
	const addr = "gina@example.com"
	old, _, e := expiringPeer(t, addr, 300*rfDay, 10*rfDay)
	fpr := h.seedKey(t, addr, old, store.ContactKeyWKD, false, time.Now())
	id, _ := h.contactKeyID(t, fpr, false)
	if err := e.RevokeKey(packet.KeySuperseded, "", &packet.Config{}); err != nil {
		t.Fatal(err)
	}
	revoked := pubOf(t, e)
	next := newPeerKey(t, addr, nil)
	served := append(append([]byte{}, pubOf(t, next)...), revoked...)
	h.fetcher.results[addr] = fetchResult{key: served}

	e0 := h.eventCount(t, id)
	out, mErr := h.refresh(t, id)
	if mErr != nil {
		t.Fatal(mErr)
	}
	if out["result"] != refreshRevoked || out["newFingerprint"] != hexOf(next) {
		t.Errorf("refresh = %v, want revoked plus the new key", out)
	}
	if k := out["key"].(map[string]any); k["revoked"] != true || k["usable"] != false {
		t.Errorf("the held key after the refresh = %v, want revoked", k)
	}
	if n := h.eventCount(t, id) - e0; n != 1 {
		t.Errorf("%d events on the held key, want 1 (revoked_seen)", n)
	}
	hist := call(t, h.api.contactKeyHistory, h.a.UserID, map[string]any{"id": id})
	list := hist["list"].([]any)
	if last := list[len(list)-1].(map[string]any); last["kind"] != store.KeyEventRevokedSeen || last["source"] != store.ContactKeyWKD {
		t.Errorf("last event = %v, want revoked_seen from wkd", last)
	}
}
