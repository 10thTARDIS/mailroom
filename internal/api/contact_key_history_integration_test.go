// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// Key history over JMAP (#815): a removed correspondent key is never used to
// encrypt or to verify, ContactKey/get shows it only with includeRemoved, its
// history is readable, and a WKD re-sighting revives the same row.

import (
	"context"
	"testing"
	"time"

	"mailroom/internal/store"
)

func (h *encHarness) contactKeyID(t *testing.T, fpr string, includeRemoved bool) (string, map[string]any) {
	t.Helper()
	res := call(t, h.api.contactKeyGet, h.a.UserID, map[string]any{"includeRemoved": includeRemoved})
	for _, raw := range res["list"].([]any) {
		k := raw.(map[string]any)
		if k["fingerprint"] == fpr {
			return k["id"].(string), k
		}
	}
	return "", nil
}

func (h *encHarness) lookupOne(t *testing.T, addr string, wkd bool) map[string]any {
	t.Helper()
	res := call(t, h.api.recipientKeyLookup, h.a.UserID, map[string]any{"addresses": []string{addr}, "wkd": wkd})
	return res["list"].([]any)[0].(map[string]any)
}

func TestRemovedContactKeyIsNeverUsed(t *testing.T) {
	h := newEncHarness(t)
	const addr = "alice@example.com"
	alice := newPeerKey(t, addr, nil)
	fpr := h.seedKey(t, addr, pubOf(t, alice), store.ContactKeyManual, false, time.Now())

	// Positive control: while held, the key verifies and is encrypted to.
	wantState(t, h.sigOf(t, signedFromAlice(t, alice)), "verified")
	if r := h.lookupOne(t, addr, false); r["usable"] != true || r["fingerprint"] != fpr {
		t.Fatalf("control: lookup before removal = %v", r)
	}

	id, _ := h.contactKeyID(t, fpr, false)
	res := call(t, h.api.contactKeySet, h.a.UserID, map[string]any{"destroy": []string{id}})
	if d, _ := res["destroyed"].([]any); len(d) != 1 {
		t.Fatalf("destroy: %v", res)
	}

	// Encrypt: no usable key, and an encrypted send is refused.
	if r := h.lookupOne(t, addr, false); r["usable"] != false || r["reason"] != recipientReasonNoKey {
		t.Errorf("lookup after removal = %v, want noKey", r)
	}
	draft := h.draft(t, []string{addr}, nil)
	before := h.hash(t, draft)
	h.refused(t, draft, h.submit(t, draft, true), before)
	// Verify: the same signature now comes from an unknown key.
	wantState(t, h.sigOf(t, signedFromAlice(t, alice)), "unknownKey")
	// Current display: gone; history display: there, with removedAt.
	if got, _ := h.contactKeyID(t, fpr, false); got != "" {
		t.Errorf("ContactKey/get still lists the removed key")
	}
	gotID, k := h.contactKeyID(t, fpr, true)
	if gotID != id || k["removedAt"] == nil {
		t.Errorf("includeRemoved = %v, want the removed key with removedAt", k)
	}
	hist := call(t, h.api.contactKeyHistory, h.a.UserID, map[string]any{"id": id})
	list := hist["list"].([]any)
	if len(list) == 0 || list[len(list)-1].(map[string]any)["kind"] != store.KeyEventRemoved {
		t.Errorf("history = %v, want a removed event last", hist)
	}

	// WKD: a lookup that finds the SAME key at the address revives the row
	// (ADR-040 §11), so the history stays attached to one id.
	h.fetcher.results[addr] = fetchResult{key: pubOf(t, alice)}
	if r := h.lookupOne(t, addr, true); r["usable"] != true || r["wkd"] != wkdFound {
		t.Fatalf("WKD re-sighting = %v", r)
	}
	if got, k := h.contactKeyID(t, fpr, false); got != id || k["removedAt"] != nil || k["source"] != store.ContactKeyWKD {
		t.Errorf("revived key = %v (id %s), want the same id %s, current, source wkd", k, got, id)
	}
	hist = call(t, h.api.contactKeyHistory, h.a.UserID, map[string]any{"id": id})
	list = hist["list"].([]any)
	if last := list[len(list)-1].(map[string]any); last["kind"] != store.KeyEventFirstSeen || last["source"] != store.ContactKeyWKD {
		t.Errorf("history after revival ends %v, want first_seen from wkd", last)
	}
}

func TestContactKeyHistoryRefusals(t *testing.T) {
	h := newEncHarness(t)
	for _, args := range []map[string]any{{"id": "not-an-id"}, {"id": store.NewULID().String()}} {
		raw, mErr := h.api.contactKeyHistory(context.Background(), h.a.UserID, mustJSON(t, args))
		if mErr == nil {
			t.Errorf("history %v = %s, want an error", args, raw)
		}
	}
}
