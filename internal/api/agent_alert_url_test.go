// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// Push links open the app's HASH routes (#49): the server answers
// "/settings/api-keys" with 404. RED PROBE: put "/settings/api-keys" back in
// apiKeysAlertURL and every assertion below fails.
func TestAgentAlertURLsAreHashRoutes(t *testing.T) {
	id := ulid.Make()
	k := store.APIKey{ID: id, Name: "bot"}
	if apiKeysAlertURL != "/#/settings/api-keys" {
		t.Errorf("apiKeysAlertURL = %q, want /#/settings/api-keys", apiKeysAlertURL)
	}
	want := "/#/settings/api-keys/" + id.String()
	for _, suspended := range []bool{false, true} {
		if got := anomalyAlert(k, "deletes", "body.", suspended).url; got != want {
			t.Errorf("anomaly alert (suspended %v) url = %q, want %q (that key's detail)", suspended, got, want)
		}
	}
	if !strings.HasPrefix(keyAlertURL(id), "/#/") {
		t.Errorf("keyAlertURL = %q, not a hash route", keyAlertURL(id))
	}
}

// The session advertises a new key's suspendOnAnomaly default from the same
// constant the create uses (#49). RED PROBE: drop the capability entry, or
// change the constant without the create, and this goes red.
func TestAPIKeysCapabilityAdvertisesSuspendDefault(t *testing.T) {
	c, ok := capabilityObjects()[capAPIKeys].(map[string]any)
	if !ok {
		t.Fatalf("no %s capability object", capAPIKeys)
	}
	got, ok := c["defaultSuspendOnAnomaly"].(bool)
	if !ok || got != DefaultSuspendOnAnomaly || !got {
		t.Errorf("defaultSuspendOnAnomaly = %v (present %v), want true (DefaultSuspendOnAnomaly)", c["defaultSuspendOnAnomaly"], ok)
	}
}
