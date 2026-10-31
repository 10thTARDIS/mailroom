// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"
)

// The user-agent descriptions on the devices list (#937). Table-driven, with real
// user agents, because the order of the rules is the logic: Edge and Opera say
// Chrome, Chrome says Safari, an iPhone says Mac OS X, Android says Linux.
func TestDescribeUserAgent(t *testing.T) {
	tests := []struct {
		name, ua, want string
	}{
		{"Firefox on Linux", "Mozilla/5.0 (X11; Linux x86_64; rv:131.0) Gecko/20100101 Firefox/131.0", "Firefox on Linux"},
		{"Firefox on Windows", "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:131.0) Gecko/20100101 Firefox/131.0", "Firefox on Windows"},
		{"Firefox on macOS", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10.15; rv:131.0) Gecko/20100101 Firefox/131.0", "Firefox on macOS"},
		{"Firefox on Android", "Mozilla/5.0 (Android 14; Mobile; rv:131.0) Gecko/131.0 Firefox/131.0", "Firefox on Android"},
		{"Firefox on iPhone", "Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) FxiOS/131.0 Mobile/15E148 Safari/605.1.15", "Firefox on iPhone"},
		{"Chrome on Windows", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36", "Chrome on Windows"},
		{"Chrome on Android", "Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Mobile Safari/537.36", "Chrome on Android"},
		{"Chrome on Linux", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36", "Chrome on Linux"},
		{"Chrome on ChromeOS", "Mozilla/5.0 (X11; CrOS x86_64 14541.0.0) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36", "Chrome on ChromeOS"},
		{"Chrome on iPhone", "Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) CriOS/130.0.0.0 Mobile/15E148 Safari/604.1", "Chrome on iPhone"},
		{"Edge on Windows", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36 Edg/130.0.0.0", "Edge on Windows"},
		{"Edge on Android", "Mozilla/5.0 (Linux; Android 14; SM-S918B) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Mobile Safari/537.36 EdgA/130.0.0.0", "Edge on Android"},
		{"Opera on Windows", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36 OPR/115.0.0.0", "Opera on Windows"},
		{"Samsung Internet", "Mozilla/5.0 (Linux; Android 14; SAMSUNG SM-S918B) AppleWebKit/537.36 (KHTML, like Gecko) SamsungBrowser/26.0 Chrome/122.0.0.0 Mobile Safari/537.36", "Samsung Internet on Android"},
		{"Safari on iPhone", "Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Mobile/15E148 Safari/604.1", "Safari on iPhone"},
		{"Safari on iPad", "Mozilla/5.0 (iPad; CPU OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Mobile/15E148 Safari/604.1", "Safari on iPad"},
		{"Safari on macOS", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Safari/605.1.15", "Safari on macOS"},
		{"an installed iPhone web app, which drops the Safari token", "Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Mobile/15E148", "Safari on iPhone"},
		{"a browser with no recognisable system", "Mozilla/5.0 Firefox/131.0", "Firefox"},
		{"curl", "curl/8.5.0", unknownBrowser},
		{"empty", "", unknownBrowser},
		{"a script", "python-requests/2.31", unknownBrowser},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := describeUserAgent(tc.ua); got != tc.want {
				t.Errorf("describeUserAgent(%q) = %q, want %q", tc.ua, got, tc.want)
			}
		})
	}
}

// The raw user agent is attacker-controlled text of up to 512 bytes. Whatever it
// holds, the description is one of a closed set of words, never the input.
func TestDescribeUserAgentNeverEchoesTheInput(t *testing.T) {
	hostile := "<script>alert(1)</script> Firefox/1 " + strings.Repeat("A", 400)
	got := describeUserAgent(hostile)
	if strings.Contains(got, "script") || strings.Contains(got, "AAAA") || len(got) > 40 {
		t.Fatalf("description %q echoes the user agent", got)
	}
}

func TestDevicesStateIsOrderIndependentAndMovesWithTheList(t *testing.T) {
	a, b := ulid.Make().String(), ulid.Make().String()
	if devicesState([]string{a, b}) != devicesState([]string{b, a}) {
		t.Error("the state depends on the order of the ids")
	}
	if devicesState([]string{a, b}) == devicesState([]string{a}) {
		t.Error("the state did not move when a device was removed")
	}
	if devicesState(nil) == "" {
		t.Error("an empty list has no state")
	}
}

// Dev-auth has no sessions (no SessionFromContext): get is an empty list and set
// destroys nothing. No store is touched, so this runs without a database.
func TestSignedInDevices_DevAuthHasNone(t *testing.T) {
	a := &API{}
	user := ulid.Make()

	raw, mErr := a.signedInDeviceGet(context.Background(), user, json.RawMessage(`{}`))
	if mErr != nil {
		t.Fatalf("get: %+v", mErr)
	}
	var got struct {
		List     []any    `json:"list"`
		NotFound []string `json:"notFound"`
		State    string   `json:"state"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.List) != 0 || got.State == "" {
		t.Fatalf("get = %s, want an empty list with a state", raw)
	}

	id := ulid.Make().String()
	raw, mErr = a.signedInDeviceSet(context.Background(), user,
		json.RawMessage(`{"destroy":["`+id+`","*others*"],"create":{"c1":{}},"update":{"`+id+`":{}}}`))
	if mErr != nil {
		t.Fatalf("set: %+v", mErr)
	}
	var set struct {
		Destroyed    []string            `json:"destroyed"`
		NotDestroyed map[string]SetError `json:"notDestroyed"`
		NotCreated   map[string]SetError `json:"notCreated"`
		NotUpdated   map[string]SetError `json:"notUpdated"`
	}
	if err := json.Unmarshal(raw, &set); err != nil {
		t.Fatal(err)
	}
	if len(set.Destroyed) != 0 {
		t.Errorf("dev-auth destroyed %v", set.Destroyed)
	}
	if set.NotDestroyed[id].Type != setNotFound {
		t.Errorf("notDestroyed = %+v, want %s notFound", set.NotDestroyed, id)
	}
	if _, ok := set.NotDestroyed["*others*"]; ok {
		t.Error("*others* was reported as a failure; under dev-auth it is a no-op")
	}
	if set.NotCreated["c1"].Type != setForbidden || set.NotUpdated[id].Type != setForbidden {
		t.Errorf("create/update must be forbidden: %+v %+v", set.NotCreated, set.NotUpdated)
	}
}

func TestDevicesCapabilityIsAdvertisedAndGatesBothMethods(t *testing.T) {
	if _, ok := capabilityObjects()[capDevices]; !ok {
		t.Fatalf("%s is not in the session capabilities", capDevices)
	}
	if _, ok := accountCapabilityObjects()[capDevices]; !ok {
		t.Fatalf("%s is not in the per-account capabilities", capDevices)
	}
	for _, m := range []string{"SignedInDevice/get", "SignedInDevice/set"} {
		if methodCapabilities[m] != capDevices {
			t.Errorf("%s is gated on %q, want %s", m, methodCapabilities[m], capDevices)
		}
		if !keyPolicies[m].sessionOnly {
			t.Errorf("%s is reachable by an API key", m)
		}
	}
}
