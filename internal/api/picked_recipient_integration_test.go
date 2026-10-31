// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

import (
	"context"
	"testing"
)

// A RECIPIENT PICKED FROM THE SUGGESTIONS MUST BE SENDABLE (#689).
//
// The composer commits a picked contact as {address, name: displayName}, and
// actions.sendMessage passes that straight through as Email/set `to` — exactly
// the request below. A contact's display name is harvested from real headers,
// so the directory form "Last, First" is ordinary. Before the fix, Build wrote
// it unquoted, the submit path could not parse a mailbox back out of the To
// field, and EmailSubmission/set answered "the message has no recipients"
// while the draft (and its broken bytes) stayed behind. A typed bare address
// carries no name, which is why typed sends were unaffected.
func TestEmailSubmissionSendsToAPickedRecipientWithADirectoryName(t *testing.T) {
	h := newComposeHarness(t)
	ctx := context.Background()
	a, _ := seedTwo(t, h.super)
	seedSendable(t, h.super, a, "me@example.com", "")

	res := call(t, h.api.emailSet, a.UserID, map[string]any{
		"create": map[string]any{
			"d": map[string]any{
				"from":       []map[string]any{{"name": nil, "email": "me@example.com"}},
				"to":         []map[string]any{{"name": "Doe, Jane", "email": "jane@work.example.com"}},
				"subject":    "hello",
				"bodyValues": map[string]any{"body": map[string]any{"value": "hi"}},
				"textBody":   []map[string]any{{"partId": "body", "type": "text/plain"}},
			},
		},
	})
	created, _ := res["created"].(map[string]any)
	obj, ok := created["d"].(map[string]any)
	if !ok {
		t.Fatalf("draft not created: %#v", res)
	}
	emailID, _ := obj["id"].(string)

	// The stored Email reads back as the recipient the user picked — name and
	// mailbox both — not a synthetic address wrapping the raw header text.
	got := call(t, h.api.emailGet, a.UserID, map[string]any{
		"ids":        []string{emailID},
		"properties": []string{"id", "to"},
	})
	list, _ := got["list"].([]any)
	if len(list) != 1 {
		t.Fatalf("Email/get returned %#v", got)
	}
	email, _ := list[0].(map[string]any)
	to, _ := email["to"].([]any)
	if len(to) != 1 {
		t.Fatalf("stored Email to = %#v, want exactly the one picked recipient", email["to"])
	}
	first, _ := to[0].(map[string]any)
	if first["name"] != "Doe, Jane" || first["email"] != "jane@work.example.com" {
		t.Errorf("stored Email to = %#v, want {\"Doe, Jane\" jane@work.example.com}", first)
	}

	sub := call(t, h.api.submissionSet, a.UserID, map[string]any{
		"create": map[string]any{"s": map[string]any{"emailId": emailID}},
	})
	if nc, _ := sub["notCreated"].(map[string]any); len(nc) != 0 {
		t.Fatalf("the send was refused: %#v — a picked recipient must be sendable (#689)", nc)
	}
	if c, _ := sub["created"].(map[string]any); c["s"] == nil {
		t.Fatalf("no submission created: %#v", sub)
	}
	var rcpt string
	if err := h.super.QueryRow(ctx,
		`SELECT array_to_string(envelope_to, ',') FROM app.submissions WHERE user_id = $1 AND message_id = $2`,
		a.UserID.String(), emailID).Scan(&rcpt); err != nil {
		t.Fatalf("read submission envelope: %v", err)
	}
	if rcpt != "jane@work.example.com" {
		t.Errorf("envelope RCPT TO = %q, want jane@work.example.com", rcpt)
	}
}
