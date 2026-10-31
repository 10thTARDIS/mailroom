// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// The unsubscribe property, END TO END through Email/get (#58).
//
// The parser's own tests are pure functions over strings. This one exists because of the
// #159 failure mode: a property can be parsed correctly, documented thoroughly, and still
// never reach a client — because the store's SELECT does not fetch the column, or the
// property is not in the body-property set the client asks for. Both of those are silent,
// and both look exactly like "the feature works" from the server's own unit tests.
//
// So this asserts the whole chain: a real row in app.messages, through the store's
// EmailView, through emailObject, out of the actual Email/get handler.

import (
	"context"
	"testing"
)

// setListUnsubscribe writes a raw header onto the fixture's seeded message.
func setListUnsubscribe(t *testing.T, h *apiHarness, f apiFixture, raw string) {
	t.Helper()
	if _, err := h.super.Exec(context.Background(),
		`UPDATE app.messages SET list_unsubscribe = $2 WHERE id = $1`,
		f.MessageID.String(), raw); err != nil {
		t.Fatalf("set list_unsubscribe: %v", err)
	}
}

// emailProp pulls one property off the single object Email/get returned.
func emailProp(t *testing.T, h *apiHarness, f apiFixture, prop string) (any, bool) {
	t.Helper()
	res := call(t, h.api.emailGet, f.UserID, map[string]any{
		"ids":        []string{f.MessageID.String()},
		"properties": []string{"id", prop},
	})
	list, _ := res["list"].([]any)
	if len(list) != 1 {
		t.Fatalf("Email/get returned %d objects, want 1: %#v", len(list), res)
	}
	obj, _ := list[0].(map[string]any)
	v, present := obj[prop]
	return v, present
}

func TestEmailGetCarriesTheUnsubscribeProperty(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	// A message with no header: the property is PRESENT and null. Present matters —
	// absent would mean the server never fetched it, which is a different claim from
	// "this message offers no unsubscribe", and a client cannot tell them apart.
	got, present := emailProp(t, h, a, "unsubscribe")
	if !present {
		t.Fatal("unsubscribe was absent for a message with no header; the client cannot distinguish that from 'not fetched'")
	}
	if got != nil {
		t.Errorf("unsubscribe = %#v, want null for a message with no header", got)
	}

	// Now the real shape: many messages in a real mailbox carry both a
	// mailto and an https entry.
	setListUnsubscribe(t, h, a,
		"<mailto:unsub@example.com?subject=token-abc>, <https://lists.example.com/u?t=abc>")

	got, _ = emailProp(t, h, a, "unsubscribe")
	obj, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("unsubscribe = %#v, want an object", got)
	}
	if url, _ := obj["url"].(string); url != "https://lists.example.com/u?t=abc" {
		t.Errorf("url = %q", url)
	}
	mail, ok := obj["email"].(map[string]any)
	if !ok {
		t.Fatalf("email = %#v, want an object", obj["email"])
	}
	if to, _ := mail["to"].(string); to != "unsub@example.com" {
		t.Errorf("to = %q", to)
	}
	// The subject survives the whole chain. It is the sender's token; a message sent
	// without it arrives and does nothing, so losing it anywhere is a silent failure.
	if subj, _ := mail["subject"].(string); subj != "token-abc" {
		t.Errorf("subject = %q, want the sender's token to survive the round trip", subj)
	}
}

func TestEmailGetRefusesAnUnsafeUnsubscribeTargetEndToEnd(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	// The scheme gate, asserted where it actually matters: through the handler, on a
	// row in the database, not on a string in a table-driven test. A hostile sender
	// controls this header entirely.
	setListUnsubscribe(t, h, a, "<javascript:alert(1)>, <data:text/html,x>, <http://example.com/u>")
	got, present := emailProp(t, h, a, "unsubscribe")
	if !present {
		t.Fatal("unsubscribe was absent rather than null")
	}
	if got != nil {
		t.Fatalf("unsubscribe = %#v, want null — an unsafe target reached the client", got)
	}

	// NEGATIVE CONTROL: the same row with a safe target must produce one, or the
	// assertion above would pass for a property that never works at all.
	setListUnsubscribe(t, h, a, "<https://lists.example.com/u>")
	got, _ = emailProp(t, h, a, "unsubscribe")
	obj, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("unsubscribe = %#v, want an object for a safe target", got)
	}
	if url, _ := obj["url"].(string); url != "https://lists.example.com/u" {
		t.Errorf("url = %q", url)
	}
}

// The property must NOT be in the default set: most real mail carries the header, so
// including it by default would put a sender-controlled string into every list row for a
// question nobody asked on that screen.
func TestUnsubscribeIsNotADefaultEmailProperty(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)
	setListUnsubscribe(t, h, a, "<https://lists.example.com/u>")

	res := call(t, h.api.emailGet, a.UserID, map[string]any{
		"ids": []string{a.MessageID.String()},
	})
	list, _ := res["list"].([]any)
	if len(list) != 1 {
		t.Fatalf("Email/get returned %d objects: %#v", len(list), res)
	}
	obj, _ := list[0].(map[string]any)
	if _, present := obj["unsubscribe"]; present {
		t.Error("unsubscribe is in the default property set; it should cost a client nothing until asked for")
	}
	// NEGATIVE CONTROL: a property that IS in the default set is there, proving the
	// assertion above is about this property and not about an empty response.
	if _, present := obj["subject"]; !present {
		t.Error("subject was missing from the default set — the check above proves nothing")
	}
}
