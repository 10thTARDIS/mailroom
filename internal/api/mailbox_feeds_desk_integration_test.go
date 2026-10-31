// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// Mailbox.feedsDesk over the wire (#442/#446): Mailbox/set accepts it
// alongside syncScope, Mailbox/get reports it back, and — the property this
// increment widened Mailbox/set for — a patch naming an unsupported property
// still refuses the WHOLE patch, including a feedsDesk sitting next to it.

import "testing"

func TestMailboxFeedsDesk(t *testing.T) {
	h := newComposeHarness(t)
	a, _ := seedTwo(t, h.super)
	box := seedMailbox(t, h.super, a, "Bulk", "").String()

	// A folder does not feed the desk unless someone says so.
	get := func() map[string]any {
		res := call(t, h.api.mailboxGet, a.UserID, map[string]any{"ids": []string{box}})
		list, _ := res["list"].([]any)
		if len(list) != 1 {
			t.Fatalf("Mailbox/get returned %d objects, want 1: %#v", len(list), res)
		}
		obj, _ := list[0].(map[string]any)
		return obj
	}
	if fd, _ := get()["feedsDesk"].(bool); fd {
		t.Error("a new mailbox has feedsDesk = true, want false")
	}

	res := call(t, h.api.mailboxSet, a.UserID, map[string]any{
		"update": map[string]any{box: map[string]any{"feedsDesk": true}},
	})
	oldState, _ := res["oldState"].(string)
	updated, _ := res["updated"].(map[string]any)
	if _, ok := updated[box]; !ok {
		t.Fatalf("setting feedsDesk was refused: %#v", res)
	}
	if fd, _ := get()["feedsDesk"].(bool); !fd {
		t.Error("feedsDesk did not land")
	}

	changes := call(t, h.api.mailboxChanges, a.UserID, map[string]any{"sinceState": oldState})
	upd, _ := changes["updated"].([]any)
	found := false
	for _, id := range upd {
		if id == box {
			found = true
		}
	}
	if !found {
		t.Errorf("Mailbox/changes did not report the feedsDesk change: %v", changes)
	}

	// A patch naming BOTH syncScope and feedsDesk applies both.
	res = call(t, h.api.mailboxSet, a.UserID, map[string]any{
		"update": map[string]any{box: map[string]any{"syncScope": "off", "feedsDesk": false}},
	})
	updated, _ = res["updated"].(map[string]any)
	if _, ok := updated[box]; !ok {
		t.Fatalf("a combined syncScope+feedsDesk patch was refused: %#v", res)
	}
	obj := get()
	if scope, _ := obj["syncScope"].(string); scope != "off" {
		t.Errorf("syncScope after the combined patch = %q, want \"off\"", scope)
	}
	if fd, _ := obj["feedsDesk"].(bool); fd {
		t.Error("feedsDesk after the combined patch = true, want false")
	}

	// A patch naming an unsupported property refuses the WHOLE patch — the
	// feedsDesk in it must NOT apply.
	res = call(t, h.api.mailboxSet, a.UserID, map[string]any{
		"update": map[string]any{box: map[string]any{"feedsDesk": true, "name": "Renamed"}},
	})
	notUpdated, _ := res["notUpdated"].(map[string]any)
	if _, ok := notUpdated[box].(map[string]any); !ok {
		t.Fatalf("a patch smuggling `name` alongside feedsDesk was accepted: %#v", res)
	}
	if fd, _ := get()["feedsDesk"].(bool); fd {
		t.Error("feedsDesk applied despite the whole patch being refused")
	}
}
