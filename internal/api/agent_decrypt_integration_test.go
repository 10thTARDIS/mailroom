// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

import (
	"context"
	"encoding/json"
	"testing"

	"mailroom/internal/store"
)

// An API key never makes the server use the user's private key (#49, ADR-042
// §3): its Email/get of an encrypted message is the message as stored, marked
// encrypted, decryption not attempted. Positive control: the session's read of
// the same message decrypts.
func TestDecryptOnOpenIsNeverDoneForAnAPIKey(t *testing.T) {
	h := newDecryptHarness(t, true)
	a, _ := seedTwo(t, h.super)
	msgID := seedRawMessage(t, h.composeHarness, a, encryptedRFC3156(t, h.dec.key, decryptedEntity))

	key := store.APIKey{ID: store.NewULID(), Scopes: []string{scopeMailRead}}
	raw, mErr := h.api.emailGet(withPrincipal(context.Background(), newPrincipal(key, "")), a.UserID,
		mustJSON(t, map[string]any{"ids": []string{msgID.String()}, "properties": bodyProps,
			"fetchTextBodyValues": true}))
	if mErr != nil {
		t.Fatal(mErr)
	}
	var res map[string]any
	_ = json.Unmarshal(raw, &res)
	obj := res["list"].([]any)[0].(map[string]any)
	if h.dec.count() != 0 || obj["encrypted"] != true || obj["decrypted"] != nil {
		t.Fatalf("key read: %d decrypt calls, encrypted=%v decrypted=%v — want 0, true, null",
			h.dec.count(), obj["encrypted"], obj["decrypted"])
	}
	if obj := getEmail(t, h, a, msgID, bodyProps); obj["decrypted"] != true || h.dec.count() != 1 {
		t.Fatalf("session read did not decrypt (calls %d): the key result above proves nothing", h.dec.count())
	}
}

// Mass send: the send that would cross MassSendThreshold within the window is
// refused, recorded as an anomaly and pushed; the ones before it go.
func TestAgentKeyMassSendIsRefused(t *testing.T) {
	h := newAgentHarness(t)
	a, _ := seedTwo(t, h.super)
	addr := "bulk-" + a.UserID.String()[20:] + "@example.com"
	seedSendable(t, h.super, a, addr, "")
	tok, keyID := h.newKey(t, a.UserID, map[string]any{"name": "bulk",
		"scopes": []string{"mail:read", "mail:draft", "mail:send"}, "confirmScopes": []string{}, "ratePerMinute": 600})
	for i := range MassSendThreshold + 1 {
		_, made := h.keyCall(t, tok, "Email/set", draftCreateArgs(addr, "bulk"))
		draft := asObj(asObj(made["created"])["d"])["id"].(string)
		name, res := h.keyCall(t, tok, "EmailSubmission/set", map[string]any{
			"create": map[string]any{"s": map[string]any{"emailId": draft}}})
		if i < MassSendThreshold {
			if _, ok := asObj(res["created"])["s"]; !ok {
				t.Fatalf("send %d under the threshold: %s %v", i+1, name, res)
			}
			continue
		}
		wantForbidden(t, "the send past the threshold", name, res)
	}
	var subs int
	_ = h.super.QueryRow(context.Background(), `SELECT count(*) FROM app.submissions WHERE user_id=$1`,
		a.UserID.String()).Scan(&subs)
	if subs != MassSendThreshold {
		t.Errorf("%d submissions, want exactly %d", subs, MassSendThreshold)
	}
	found := false
	for _, e := range h.events(t, keyID) {
		found = found || e == "anomaly:mass_send"
	}
	if !found {
		t.Error("no mass_send anomaly in the audit")
	}
	h.alerts.waitFor(t, func(al fakeAlert) bool { return al.urgent && al.user == a.UserID })
}
