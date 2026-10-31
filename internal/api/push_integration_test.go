// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// Integration tests for the WebPushSubscription surface (#42, ADR-016): the
// set-create -> get round trip (with keys write-only), upsert-on-endpoint,
// destroy, and the session advertising the VAPID applicationServerKey. Runs
// against the throwaway Postgres from scripts/integration-test.sh.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"mailroom/internal/auth"
	"mailroom/internal/store"
)

func TestWebPushSubscriptionRoundTrip(t *testing.T) {
	h := newAPIHarness(t)
	a := seedAPIUser(t, h.super, "push-"+store.NewULID().String())

	const (
		endpoint = "https://push.example.com/send/device-1"
		p256dh   = "BExampleUAPublicKeyBytesBase64Url"
		auth1    = "AuthSecretBase64Url1"
	)

	// Create.
	res := call(t, h.api.pushSet, a.UserID, map[string]any{
		"create": map[string]any{
			"d1": map[string]any{
				"endpoint":   endpoint,
				"keys":       map[string]any{"p256dh": p256dh, "auth": auth1},
				"deviceName": "Test Phone",
				// Ignored since #59: it was never read, and honouring it now
				// would switch on a filter nobody chose. The mode is "all".
				"importantOnly": true,
			},
		},
	})
	created, _ := res["created"].(map[string]any)
	obj, ok := created["d1"].(map[string]any)
	if !ok {
		t.Fatalf("create did not return d1: %v", res)
	}
	id, _ := obj["id"].(string)
	if id == "" {
		t.Fatalf("created object has no id: %v", obj)
	}
	if obj["deviceName"] != "Test Phone" || obj["notifyMode"] != "all" {
		t.Errorf("created object metadata wrong (want notifyMode all, the default): %v", obj)
	}
	if _, present := obj["importantOnly"]; present {
		t.Errorf("importantOnly is still returned; it was replaced by notifyMode (#59): %v", obj)
	}
	// Keys and endpoint are write-only: never echoed.
	assertNoKeyLeak(t, obj, endpoint, p256dh, auth1)

	// Get (ids:null) returns the one subscription, again with no keys/endpoint.
	got := call(t, h.api.pushGet, a.UserID, map[string]any{})
	list, _ := got["list"].([]any)
	if len(list) != 1 {
		t.Fatalf("get returned %d subscriptions, want 1", len(list))
	}
	gotObj, _ := list[0].(map[string]any)
	if gotObj["id"] != id {
		t.Errorf("get id = %v, want %v", gotObj["id"], id)
	}
	if gotObj["createdAt"] == nil {
		t.Errorf("get object missing createdAt: %v", gotObj)
	}
	assertNoKeyLeak(t, got, endpoint, p256dh, auth1)

	// Re-subscribing the SAME endpoint upserts to the same id (not a duplicate).
	res2 := call(t, h.api.pushSet, a.UserID, map[string]any{
		"create": map[string]any{
			"d2": map[string]any{
				"endpoint": endpoint,
				"keys":     map[string]any{"p256dh": "BRotatedKey", "auth": "RotatedAuth"},
			},
		},
	})
	created2, _ := res2["created"].(map[string]any)
	obj2, _ := created2["d2"].(map[string]any)
	if obj2["id"] != id {
		t.Errorf("upsert produced a new id %v, want stable %v", obj2["id"], id)
	}
	if again := call(t, h.api.pushGet, a.UserID, map[string]any{}); len(asArr(again["list"])) != 1 {
		t.Errorf("after upsert, get returned %d rows, want 1", len(asArr(again["list"])))
	}

	// Destroy.
	del := call(t, h.api.pushSet, a.UserID, map[string]any{"destroy": []string{id}})
	if !strInArr(del["destroyed"], id) {
		t.Errorf("destroy did not report %q: %v", id, del)
	}
	if final := call(t, h.api.pushGet, a.UserID, map[string]any{}); len(asArr(final["list"])) != 0 {
		t.Errorf("after destroy, get returned %d rows, want 0", len(asArr(final["list"])))
	}
}

// TestSessionAdvertisesVAPIDKey proves the session's urn:mailroom:push capability
// carries the configured applicationServerKey.
func TestSessionAdvertisesVAPIDKey(t *testing.T) {
	h := newAPIHarness(t)
	a := seedAPIUser(t, h.super, "push-sess-"+store.NewULID().String())

	const vapidPub = "BTestVapidApplicationServerKeyBase64Url"
	pushAPI := New(h.store, WithPushKey(vapidPub))
	handler := auth.DevAuth(pushAPI.Handler(), a.UserID)

	rec := serve(handler, "GET", "/jmap/session")
	if rec.Code != http.StatusOK {
		t.Fatalf("session: status %d, want 200", rec.Code)
	}
	var session struct {
		Capabilities map[string]json.RawMessage `json:"capabilities"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &session); err != nil {
		t.Fatalf("decode session: %v", err)
	}
	raw, ok := session.Capabilities[capPush]
	if !ok {
		t.Fatalf("session does not advertise %s: %s", capPush, rec.Body.String())
	}
	var pushCap struct {
		VAPIDPublicKey string `json:"vapidPublicKey"`
	}
	if err := json.Unmarshal(raw, &pushCap); err != nil {
		t.Fatalf("decode push capability: %v", err)
	}
	if pushCap.VAPIDPublicKey != vapidPub {
		t.Errorf("vapidPublicKey = %q, want %q", pushCap.VAPIDPublicKey, vapidPub)
	}
}

// assertNoKeyLeak fails if a WebPushSubscription response contains the endpoint or
// either encryption key — they are write-only.
func assertNoKeyLeak(t *testing.T, v any, secrets ...string) {
	t.Helper()
	blob, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(blob)
	for _, secret := range secrets {
		if secret != "" && strings.Contains(s, secret) {
			t.Errorf("WebPushSubscription response leaked write-only value %q:\n%s", secret, s)
		}
	}
}

// TestWebPushSubscriptionRefusesSSRFEndpoints covers the hole ADR-026 §2 found:
// PushSubscription/set accepted ANY non-empty string as the endpoint the server
// would later POST to, so an authenticated user could point their "device" at
// the cloud metadata service, a loopback admin port, or an internal host.
//
// The batch deliberately carries one GOOD endpoint alongside the hostile ones.
// A validation that refused everything would look identical to a correct one if
// only refusals were asserted — and "the guard works" and "the feature is dead"
// are the two outcomes a refusal-only test cannot tell apart.
func TestWebPushSubscriptionRefusesSSRFEndpoints(t *testing.T) {
	h := newAPIHarness(t)
	a := seedAPIUser(t, h.super, "push-ssrf-"+store.NewULID().String())

	hostile := map[string]string{
		"metadata":     "https://169.254.169.254/latest/meta-data/",
		"loopback":     "https://127.0.0.1:8080/admin",
		"loopback6":    "https://[::1]:8080/admin",
		"mapped":       "https://[::ffff:127.0.0.1]:8080/admin",
		"rfc1918":      "https://10.0.0.5/internal",
		"cleartext":    "http://push.example.com/send/device-1",
		"notaurl":      "definitely not a url",
		"filescheme":   "file:///etc/passwd",
		"gopherscheme": "gopher://internal.host/",
	}
	create := map[string]any{}
	for id, endpoint := range hostile {
		create[id] = map[string]any{
			"endpoint": endpoint,
			"keys":     map[string]any{"p256dh": "BExampleKey", "auth": "AuthSecret"},
		}
	}
	const goodEndpoint = "https://push.example.com/send/legit-device"
	create["good"] = map[string]any{
		"endpoint": goodEndpoint,
		"keys":     map[string]any{"p256dh": "BExampleKey", "auth": "AuthSecret"},
	}

	res := call(t, h.api.pushSet, a.UserID, map[string]any{"create": create})

	created, _ := res["created"].(map[string]any)
	notCreated, _ := res["notCreated"].(map[string]any)

	for id, endpoint := range hostile {
		if _, ok := created[id]; ok {
			t.Errorf("%s (%s) was ACCEPTED; the server would POST to it", id, endpoint)
		}
		serr, ok := notCreated[id].(map[string]any)
		if !ok {
			t.Errorf("%s (%s) is in neither created nor notCreated: %v", id, endpoint, res)
			continue
		}
		// The refusal has to say what was wrong with the property, or a client
		// cannot tell a rejected endpoint from a server fault.
		if serr["type"] != "invalidProperties" {
			t.Errorf("%s: SetError type = %v, want invalidProperties", id, serr["type"])
		}
		if desc, _ := serr["description"].(string); !strings.Contains(desc, "endpoint") {
			t.Errorf("%s: SetError description does not name the endpoint: %q", id, desc)
		}
	}

	// ...and the legitimate one still went through.
	if _, ok := created["good"]; !ok {
		t.Fatalf("the legitimate https endpoint was refused too — the guard is an "+
			"outage, not a guard: %v", res)
	}
}
