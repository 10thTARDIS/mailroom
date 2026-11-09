// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// Mailbox/changes after a count change (ADR-052, #115), through the method.

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestMailboxChangesNamesOnlyTheCountsAfterAMove(t *testing.T) {
	h := newComposeHarness(t)
	a, _ := seedTwo(t, h.super)
	id := seedRawMessage(t, h, a, []byte(headerFixture))
	trash := a.TrashID

	got := call(t, h.api.mailboxGet, a.UserID, map[string]any{"ids": nil, "properties": []string{"id"}})
	since := got["state"].(string)

	call(t, h.api.emailSet, a.UserID, map[string]any{"update": map[string]any{
		id.String(): map[string]any{"mailboxIds": map[string]any{trash.String(): true}}}})

	res := call(t, h.api.mailboxChanges, a.UserID, map[string]any{"sinceState": since})
	updated, _ := res["updated"].([]any)
	if len(updated) != 2 {
		t.Fatalf("updated = %v, want the Inbox and Trash", updated)
	}
	if res["newState"] == since {
		t.Errorf("newState did not move: %v", res["newState"])
	}
	want := []any{"totalEmails", "unreadEmails", "totalThreads", "unreadThreads"}
	if !reflect.DeepEqual(res["updatedProperties"], want) {
		t.Errorf("updatedProperties = %v, want %v", res["updatedProperties"], want)
	}

	// A change that is NOT only counts (a scope change) makes it null again.
	call(t, h.api.mailboxSet, a.UserID, map[string]any{"update": map[string]any{
		a.InboxID.String(): map[string]any{"syncScope": "off"}}})
	res = call(t, h.api.mailboxChanges, a.UserID, map[string]any{"sinceState": since})
	b, _ := json.Marshal(res["updatedProperties"])
	if string(b) != "null" {
		t.Errorf("updatedProperties after a scope change = %s, want null", b)
	}
}
