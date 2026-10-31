// SPDX-License-Identifier: AGPL-3.0-only

package api

// Unit test (no database) for parseEventTypes: it is pure, so this covers the
// `types` parameter's parsing without a Postgres-backed EventSource stream.

import (
	"testing"

	"mailroom/internal/store"
)

// TestParseEventTypesAcceptsAreaAlone: a client that only cares about the desk
// (#375) must be able to subscribe to Area on its own, not just as part of
// `types=*` or a longer list — the same single-type path Email/Mailbox/etc.
// already have.
func TestParseEventTypesAcceptsAreaAlone(t *testing.T) {
	got, err := parseEventTypes("Area")
	if err != nil {
		t.Fatalf("parseEventTypes(%q): %v", "Area", err)
	}
	if len(got) != 1 || got[0] != store.ObjectArea {
		t.Errorf("parseEventTypes(%q) = %v, want [%q]", "Area", got, store.ObjectArea)
	}
}

// TestParseEventTypesAcceptsClientPreferencesAlone: the same single-type path
// as Area above, for ClientPreferences (#375, #469) — it has its own /changes
// method and change_log rows, but was missing from eventSourceTypes, so
// parseEventTypes rejected it with a whole-request 400 and the web client's
// only option was polling.
func TestParseEventTypesAcceptsClientPreferencesAlone(t *testing.T) {
	got, err := parseEventTypes("ClientPreferences")
	if err != nil {
		t.Fatalf("parseEventTypes(%q): %v", "ClientPreferences", err)
	}
	if len(got) != 1 || got[0] != store.ObjectClientPreferences {
		t.Errorf("parseEventTypes(%q) = %v, want [%q]", "ClientPreferences", got, store.ObjectClientPreferences)
	}
}
