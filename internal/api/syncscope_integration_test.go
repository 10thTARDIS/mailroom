// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// SELECTIVE SYNC, the JMAP surface (#62): `Mailbox/set` accepts `syncScope` and
// nothing else, and `Mailbox/get` reports it.
//
// Two things here are not ordinary CRUD assertions and are the reason this file
// exists:
//
//  1. `Mailbox/set` REFUSED EVERY MUTATION before this. Opening it for one
//     property has to stay opened for one property — a rename that is silently
//     dropped while a syncScope in the same patch succeeds is worse than a
//     rename that fails, because the client believes the folder was renamed.
//  2. The value `"headers"` must be REFUSED. It was advertised in the schema
//     comment for a long time and never implemented: the sync engine tests only
//     for "off", so a mailbox set to "headers" gets a FULL sync — the exact
//     opposite of what the name promises. Accepting it would be shipping a
//     setting that does the reverse of what it says.

import (
	"testing"

	"mailroom/internal/store"
)

// scopeOf reads a mailbox's syncScope back through Mailbox/get.
func syncScopeOf(t *testing.T, h *composeHarness, f apiFixture, id string) string {
	t.Helper()
	res := call(t, h.api.mailboxGet, f.UserID, map[string]any{"ids": []string{id}})
	list, _ := res["list"].([]any)
	if len(list) != 1 {
		t.Fatalf("Mailbox/get returned %d objects, want 1: %#v", len(list), res)
	}
	obj, _ := list[0].(map[string]any)
	scope, ok := obj["syncScope"].(string)
	if !ok {
		t.Fatalf("Mailbox/get returned no syncScope: %#v", obj)
	}
	return scope
}

// setScope issues Mailbox/set for one mailbox and returns the raw response.
func setSyncScope(t *testing.T, h *composeHarness, f apiFixture, id string, scope any) map[string]any {
	t.Helper()
	return call(t, h.api.mailboxSet, f.UserID, map[string]any{
		"update": map[string]any{id: map[string]any{"syncScope": scope}},
	})
}

func TestMailboxSyncScopeRoundTrips(t *testing.T) {
	h := newComposeHarness(t)
	a, _ := seedTwo(t, h.super)
	box := seedMailbox(t, h.super, a, "Bulk", "").String()

	// A folder is synced unless someone says otherwise.
	if got := syncScopeOf(t, h, a, box); got != "full" {
		t.Errorf("a new mailbox has syncScope %q, want \"full\"", got)
	}

	res := setSyncScope(t, h, a, box, "off")
	updated, _ := res["updated"].(map[string]any)
	if _, ok := updated[box]; !ok {
		t.Fatalf("turning a folder off was refused: %#v", res)
	}
	if got := syncScopeOf(t, h, a, box); got != "off" {
		t.Errorf("syncScope = %q after setting it off", got)
	}

	// AND BACK. Off has to be reversible through the same door it was set — a
	// one-way switch would make this a way to lose a folder rather than a
	// preference.
	if _, ok := setSyncScope(t, h, a, box, "full")["updated"].(map[string]any)[box]; !ok {
		t.Fatal("turning a folder back on was refused")
	}
	if got := syncScopeOf(t, h, a, box); got != "full" {
		t.Errorf("syncScope = %q after setting it back to full", got)
	}
}

// THE STATE MOVES. A Mailbox is a JMAP-visible object, so a scope change appends
// a change row and `newState` must reflect it — otherwise another device either
// re-reads the same change forever or never sees it, and it is precisely the
// devices that did NOT make the change that need to know a folder is now frozen.
func TestMailboxSyncScopeAdvancesTheState(t *testing.T) {
	h := newComposeHarness(t)
	a, _ := seedTwo(t, h.super)
	box := seedMailbox(t, h.super, a, "Bulk", "").String()

	res := setSyncScope(t, h, a, box, "off")
	oldState, _ := res["oldState"].(string)
	newState, _ := res["newState"].(string)
	if oldState == "" || newState == "" {
		t.Fatalf("missing states: %#v", res)
	}
	if oldState == newState {
		t.Error("oldState == newState after a scope change — the change row did not " +
			"reach /changes, so other devices will keep rendering a frozen folder as live")
	}

	// A NO-OP RE-ASSERT DOES NOT. Re-sending the scope it already has must not
	// churn every other device's cursor.
	again := setSyncScope(t, h, a, box, "off")
	if again["oldState"] != again["newState"] {
		t.Errorf("re-asserting the same scope moved the state: %v -> %v",
			again["oldState"], again["newState"])
	}
}

// "headers" IS REFUSED. See the file header: it would silently mean a full sync.
func TestMailboxSyncScopeRefusesHeaders(t *testing.T) {
	h := newComposeHarness(t)
	a, _ := seedTwo(t, h.super)
	box := seedMailbox(t, h.super, a, "Bulk", "").String()

	for _, bad := range []any{"headers", "HEADERS", "", "partial", 42, true, nil} {
		res := setSyncScope(t, h, a, box, bad)
		notUpdated, _ := res["notUpdated"].(map[string]any)
		e, ok := notUpdated[box].(map[string]any)
		if !ok {
			t.Errorf("syncScope %#v was ACCEPTED: %#v", bad, res)
			continue
		}
		props, _ := e["properties"].([]any)
		if len(props) != 1 || props[0] != "syncScope" {
			t.Errorf("syncScope %#v: properties = %v, want [syncScope]", bad, props)
		}
	}
	// Nothing stuck.
	if got := syncScopeOf(t, h, a, box); got != "full" {
		t.Errorf("syncScope = %q after only-refused writes, want \"full\"", got)
	}
}

// Every OTHER property is still refused, and the refusal takes the whole patch
// with it — including a syncScope sitting next to it.
func TestMailboxSetStillRefusesEverythingElse(t *testing.T) {
	h := newComposeHarness(t)
	a, _ := seedTwo(t, h.super)
	box := seedMailbox(t, h.super, a, "Bulk", "").String()

	for _, patch := range []map[string]any{
		{"name": "Renamed"},
		{"parentId": nil},
		{"isSubscribed": false},
		// accountId is read-only (#488): it identifies which account the mailbox
		// belongs to, and a client must never be able to reassign a folder to a
		// different account by patching it.
		{"accountId": store.NewULID().String()},
		// THE ONE THAT MATTERS: a rename smuggled alongside a legal change. If the
		// scope applied and the rename were dropped, the client would believe both
		// landed.
		{"syncScope": "off", "name": "Renamed"},
	} {
		res := call(t, h.api.mailboxSet, a.UserID, map[string]any{
			"update": map[string]any{box: patch},
		})
		if _, ok := res["notUpdated"].(map[string]any)[box]; !ok {
			t.Errorf("patch %#v was accepted: %#v", patch, res)
		}
	}
	// The smuggled scope did NOT apply.
	if got := syncScopeOf(t, h, a, box); got != "full" {
		t.Errorf("syncScope = %q — a refused patch applied half of itself", got)
	}

	// Creating and destroying mailboxes are still not supported.
	res := call(t, h.api.mailboxSet, a.UserID, map[string]any{
		"create":  map[string]any{"n": map[string]any{"name": "New"}},
		"destroy": []string{box},
	})
	if _, ok := res["notCreated"].(map[string]any)["n"]; !ok {
		t.Error("Mailbox/set created a mailbox")
	}
	if _, ok := res["notDestroyed"].(map[string]any)[box]; !ok {
		t.Error("Mailbox/set destroyed a mailbox")
	}
}

// A BAD OBJECT MUST NOT TAKE A GOOD ONE WITH IT. Per-object SetErrors inside one
// transaction are where this codebase has been bitten before (a failed statement
// aborts the whole transaction, SQLSTATE 25P02), so the batch is tested with one
// refused mailbox and one that must still succeed.
func TestMailboxSyncScopeBatchIsolatesFailures(t *testing.T) {
	h := newComposeHarness(t)
	a, _ := seedTwo(t, h.super)
	good := seedMailbox(t, h.super, a, "Bulk", "").String()
	missing := store.NewULID().String()

	res := call(t, h.api.mailboxSet, a.UserID, map[string]any{
		"update": map[string]any{
			missing: map[string]any{"syncScope": "off"}, // notFound
			good:    map[string]any{"syncScope": "off"}, // must still land
		},
	})
	if _, ok := res["notUpdated"].(map[string]any)[missing]; !ok {
		t.Errorf("a missing mailbox was reported as updated: %#v", res)
	}
	if _, ok := res["updated"].(map[string]any)[good]; !ok {
		t.Fatalf("THE GOOD MAILBOX WAS LOST with the bad one: %#v", res)
	}
	if got := syncScopeOf(t, h, a, good); got != "off" {
		t.Errorf("the good mailbox's scope = %q, want \"off\"", got)
	}
}

// Another user's mailbox is `notFound`, not `forbidden` — RLS makes the two
// indistinguishable and the API must not leak the difference.
func TestMailboxSyncScopeIsScopedToItsOwner(t *testing.T) {
	h := newComposeHarness(t)
	a, b := seedTwo(t, h.super)
	box := seedMailbox(t, h.super, a, "Bulk", "").String()

	res := setSyncScope(t, h, b, box, "off")
	e, ok := res["notUpdated"].(map[string]any)[box].(map[string]any)
	if !ok {
		t.Fatalf("user B changed user A's mailbox: %#v", res)
	}
	if e["type"] != "notFound" {
		t.Errorf("type = %v, want notFound (RLS must not distinguish it from missing)", e["type"])
	}
	if got := syncScopeOf(t, h, a, box); got != "full" {
		t.Errorf("user A's mailbox scope = %q after B's attempt", got)
	}

}
