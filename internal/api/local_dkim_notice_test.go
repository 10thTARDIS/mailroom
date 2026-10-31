// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"testing"
	"time"

	"mailroom/internal/senderwarn"
	"mailroom/internal/store"
)

// #776, ADR-036: Mailroom's own DKIM verdict in the authResults property and
// in selfSpoof. Driven through buildEmailBody and emailObject, the path
// Email/get uses.

var (
	ldAlignedPass = &store.LocalDKIM{Result: "pass", Domain: "example.net", Aligned: true}
	ldUnaligned   = &store.LocalDKIM{Result: "pass", Domain: "esp.example.org", Aligned: false}
	ldFail        = &store.LocalDKIM{Result: "fail_body", Domain: "example.net", Aligned: false}
	// A fail that CLAIMS to be aligned: nothing writes this, which is why it is
	// the right probe for "the API accepts only a pass".
	ldFailAligned = &store.LocalDKIM{Result: "fail_body", Domain: "example.net", Aligned: true}
	ldBlocked     = &store.LocalDKIM{Result: "pass", Domain: "example.net", DisplayBlocked: "duplicate_header:from"}
)

func localNotice(t *testing.T, headers string, local *store.LocalDKIM) any {
	t.Helper()
	raw := "From: a@example.net\r\n" + headers + "Subject: hi\r\n\r\nhello\r\n"
	body := buildEmailBody([]byte(raw), hashOf(raw), bodyArgs{
		trustedAuthservIDs: []string{"mx.provider.example"}, received: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		localDKIM: local,
	})
	return emailObject(store.EmailView{}, map[string]bool{"authResults": true}, body)["authResults"]
}

func TestLocalDKIMAuthResultsMatrix(t *testing.T) {
	// No trusted header + a local aligned pass: Mailroom's verdict, marked.
	m := obj(t, localNotice(t, "", ldAlignedPass))
	if m["present"] != true || m["source"] != "mailroom" || m["dkimDomain"] != "example.net" {
		t.Fatalf("aligned local pass: %#v", m)
	}
	if d, _ := m["dkim"].([]string); len(d) != 1 || d[0] != "pass" {
		t.Errorf("dkim = %#v, want [pass]", m["dkim"])
	}
	for _, k := range []string{"spf", "dmarc"} {
		if v, ok := m[k]; !ok || v != nil {
			t.Errorf("%s = %#v, want present and null (Mailroom checked DKIM only)", k, v)
		}
	}
	// Untrusted headers do not matter either way.
	if m := obj(t, localNotice(t, hdrUntrusted, ldAlignedPass)); m["source"] != "mailroom" {
		t.Errorf("untrusted header + local pass: %#v", m)
	}

	// Everything else leaves "unauthenticated" exactly as before.
	for name, l := range map[string]*store.LocalDKIM{"local fail": ldFail, "unaligned pass": ldUnaligned,
		"blocked pass": ldBlocked, "fail claiming aligned": ldFailAligned, "no row": nil} {
		m := obj(t, localNotice(t, "", l))
		if m["present"] != false {
			t.Errorf("%s: present = %v, want false (unauthenticated)", name, m["present"])
		}
		if _, ok := m["source"]; ok {
			t.Errorf("%s: carries a source marker: %#v", name, m)
		}
	}

	// A trusted header: the provider's verdict wins, local row ignored.
	m = obj(t, localNotice(t, hdrTrusted, ldAlignedPass))
	if _, ok := m["source"]; ok || m["present"] != true {
		t.Errorf("trusted header + local pass: %#v, want the provider's object", m)
	}
	failing := "Authentication-Results: mx.provider.example; dkim=fail\r\n"
	m = obj(t, localNotice(t, failing, ldAlignedPass))
	if d, _ := m["dkim"].([]string); len(d) != 1 || d[0] != "fail" || m["source"] != nil {
		t.Errorf("provider fail + local pass: %#v, want the provider's fail", m)
	}
	// Own mail keeps showing nothing.
	raw := "From: a@example.net\r\nSubject: hi\r\n\r\nhello\r\n"
	body := buildEmailBody([]byte(raw), hashOf(raw), bodyArgs{trustedAuthservIDs: []string{"mx.provider.example"}, authSelf: true, localDKIM: ldAlignedPass})
	if got := emailObject(store.EmailView{}, map[string]bool{"authResults": true}, body)["authResults"]; got != nil {
		t.Errorf("self + local pass: %#v, want null", got)
	}
	// Unconfigured: null.
	body = buildEmailBody([]byte(raw), hashOf(raw), bodyArgs{localDKIM: ldAlignedPass})
	if got := emailObject(store.EmailView{}, map[string]bool{"authResults": true}, body)["authResults"]; got != nil {
		t.Errorf("unconfigured + local pass: %#v, want null", got)
	}
}

func TestLocalDKIMSelfSpoof(t *testing.T) {
	ix := senderwarn.NewIndex([]string{"example.com"}, nil)
	ev := store.EmailView{FromAddress: "boss@example.com"}
	warn := func(headers string, local *store.LocalDKIM) int {
		raw := "From: boss@example.com\r\n" + headers + "\r\nhi\r\n"
		b := buildEmailBody([]byte(raw), hashOf(raw), bodyArgs{trustedAuthservIDs: []string{"mx.provider.example"}, localDKIM: local})
		l, _ := senderWarningsObject(ev, b, ix).([]map[string]any)
		return len(l)
	}
	if n := warn("", nil); n != 1 {
		t.Fatalf("CONTROL FAILED: no verdict at all gives %d warnings, want selfSpoof", n)
	}
	if n := warn("", ldAlignedPass); n != 0 {
		t.Errorf("local aligned pass: %d warnings, want 0 (suppressed)", n)
	}
	for name, l := range map[string]*store.LocalDKIM{"unaligned pass": ldUnaligned, "local fail": ldFail,
		"fail claiming aligned": ldFailAligned, "blocked pass": ldBlocked} {
		if n := warn("", l); n != 1 {
			t.Errorf("%s: %d warnings, want selfSpoof to still fire", name, n)
		}
	}
	// A trusted header's verdict wins even over an aligned local pass: a
	// provider fail keeps the warning.
	if n := warn("Authentication-Results: mx.provider.example; dkim=fail\r\n", ldAlignedPass); n != 1 {
		t.Errorf("provider fail + local pass: %d warnings, want 1", n)
	}
}
