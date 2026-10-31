// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"encoding/json"
	"fmt"
	"testing"

	"mailroom/internal/store"
)

// parseSubmitRequest, the one parser for sessions, keys and held sends,
// holds every envelope address to store.CanonicalEnvelopeAddress (#852
// review): the envelope goes to SMTP, and a key's envelope recipients lead
// the approval summary. The address rules themselves are
// TestCanonicalEnvelopeAddress's; this pins the wiring, the property named,
// and that the canonical form is what the request carries.
//
// RED PROBE: copy r.Email into the request unchecked, as before, and every
// spoof is accepted.
func TestSubmitEnvelopeAddressesMustBeBare(t *testing.T) {
	id := store.NewULID().String()
	req := func(env map[string]any) (string, []string, *SetError) {
		raw, _ := json.Marshal(map[string]any{"emailId": id, "envelope": env})
		r, serr := parseSubmitRequest(raw)
		return r.EnvelopeFrom, r.EnvelopeTo, serr
	}
	rcpt := func(addrs ...string) map[string]any {
		var out []map[string]any
		for _, a := range addrs {
			out = append(out, map[string]any{"email": a})
		}
		return map[string]any{"rcptTo": out}
	}
	for _, spoof := range []string{
		"alice@example.com (+0 more) with 0 attachments: draft 'notes'",
		"\"alice@example.com (+0 more) with 0 attachments\"@evil.example",
		"Alice <alice@example.com>",
		"alice @example.com",
		"",
	} {
		_, _, serr := req(rcpt("ok@example.net", spoof))
		if serr == nil || serr.Type != setInvalidProps || len(serr.Properties) != 1 || serr.Properties[0] != "envelope/rcptTo/1/email" {
			t.Errorf("rcptTo %q: %+v, want invalidProperties on envelope/rcptTo/1/email", spoof, serr)
		}
	}
	_, _, serr := req(map[string]any{"mailFrom": map[string]any{"email": "Me <me@example.net>"}})
	if serr == nil || serr.Type != setInvalidProps || serr.Properties[0] != "envelope/mailFrom/email" {
		t.Errorf("mailFrom with a display name: %+v, want invalidProperties", serr)
	}
	// Positive controls, carried in canonical (lowercased) form.
	_, got, serr := req(rcpt("bob@example.net", "bob+tag@example.net", "Bob.Smith@Mail.Example.NET", "carol@xn--bcher-kva.example"))
	want := []string{"bob@example.net", "bob+tag@example.net", "bob.smith@mail.example.net", "carol@xn--bcher-kva.example"}
	if serr != nil || fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("bare addresses: %v %+v, want %v", got, serr, want)
	}
	if from, _, serr := req(map[string]any{"mailFrom": map[string]any{"email": "Me+x@Example.net"}}); serr != nil || from != "me+x@example.net" {
		t.Errorf("a bare mailFrom: %q %+v, want me+x@example.net", from, serr)
	}
}
