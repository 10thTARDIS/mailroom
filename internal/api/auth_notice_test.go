// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"testing"
	"time"

	"mailroom/internal/senderwarn"
	"mailroom/internal/store"
)

// #766: the notice's decision matrix. The matrix is driven through buildEmailBody and
// emailObject, the path Email/get uses, so it covers the wiring and not just
// authNoticeObject's arithmetic.

const (
	hdrTrusted   = "Authentication-Results: mx.provider.example; spf=pass; dkim=pass; dmarc=pass header.from=example.com\r\n"
	hdrUntrusted = "Authentication-Results: other.provider.example; spf=pass; dkim=pass; dmarc=pass header.from=example.com\r\n"
)

func noticeFor(t *testing.T, headers string, entries []string, received time.Time, local, self bool) any {
	t.Helper()
	raw := "From: a@example.net\r\n" + headers + "Subject: hi\r\n\r\nhello\r\n"
	body := buildEmailBody([]byte(raw), hashOf(raw), bodyArgs{trustedAuthservIDs: entries, received: received, authSelf: self})
	return emailObject(store.EmailView{LocallyAuthored: local}, map[string]bool{"authResults": true}, body)["authResults"]
}

func TestAuthNoticeMatrix(t *testing.T) {
	ids := []string{"mx.provider.example"}
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

	// 1. No ids configured: null whatever the message.
	if got := noticeFor(t, "", nil, at, false, false); got != nil {
		t.Errorf("unconfigured, no header: %#v, want null", got)
	}
	if got := noticeFor(t, hdrTrusted, nil, at, false, false); got != nil {
		t.Errorf("unconfigured, header: %#v, want null", got)
	}
	// 2. A trusted header: today's object.
	m := obj(t, noticeFor(t, hdrTrusted, ids, at, false, false))
	if m["present"] != true {
		t.Errorf("trusted header: present = %v, want true", m["present"])
	}
	// 3. Untrusted only, inbound: STILL "unauthenticated". An attacker-writable
	// header must not turn the notice off. Negative control: restoring the
	// silence (null here) reddens this.
	m = obj(t, noticeFor(t, hdrUntrusted, ids, at, false, false))
	if m["present"] != false {
		t.Errorf("untrusted-only inbound: present = %v, want false", m["present"])
	}
	// ... and the same message in Sent/Drafts or locally authored is null.
	if got := noticeFor(t, hdrUntrusted, ids, at, false, true); got != nil {
		t.Errorf("untrusted-only, self: %#v, want null", got)
	}
	// 4. No header at all, inbound and not self: present:false.
	m = obj(t, noticeFor(t, "", ids, at, false, false))
	if m["present"] != false {
		t.Errorf("no header, inbound: present = %v, want false", m["present"])
	}
	// ... and each "self" reason, alone, turns it into null.
	if got := noticeFor(t, "", ids, at, true, false); got != nil {
		t.Errorf("no header, locally authored: %#v, want null", got)
	}
	if got := noticeFor(t, "", ids, at, false, true); got != nil {
		t.Errorf("no header, self (Sent/Drafts/identity): %#v, want null", got)
	}
}

// A date-bounded entry reaches the notice through the message's received time.
func TestAuthNoticeHonoursTheReceivedTimeBound(t *testing.T) {
	entries := []string{"mx.provider.example", "old.provider.example<2026-07-01"}
	old := "Authentication-Results: old.provider.example; dmarc=pass header.from=example.com\r\n"
	before := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)
	after := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)

	if m := obj(t, noticeFor(t, old, entries, before, false, false)); m["present"] != true {
		t.Errorf("received before the bound: present = %v, want true", m["present"])
	}
	// A forged old-id header on NEW mail stays untrusted: never a pass.
	if m := obj(t, noticeFor(t, old, entries, after, false, false)); m["present"] != false {
		t.Errorf("received at the bound: present = %v, want false (untrusted)", m["present"])
	}
	// Zero received time (an undated or archive row) trusts no bounded entry.
	if m := obj(t, noticeFor(t, old, entries, time.Time{}, false, false)); m["present"] != false {
		t.Errorf("zero received time: present = %v, want false (untrusted)", m["present"])
	}
	// The raw (unresolved) bounded entry alone, through the body: also untrusted.
	raw := "From: a@example.net\r\n" + old + "\r\nx\r\n"
	b := buildEmailBody([]byte(raw), hashOf(raw), bodyArgs{trustedAuthservIDs: entries, received: before})
	if !b.authTrusted.Present {
		t.Fatal("CONTROL FAILED: inside the bound the old id is not trusted")
	}
	b = buildEmailBody([]byte(raw), hashOf(raw), bodyArgs{trustedAuthservIDs: entries, received: after})
	if b.authTrusted.Present {
		t.Error("past the bound: the old id was trusted")
	}
}

// selfSpoof: untrusted-only own-domain mail has not affirmatively passed, so
// the warning fires.
func TestSelfSpoofStillFiresOnUntrustedOnlyMail(t *testing.T) {
	ix := senderwarn.NewIndex([]string{"example.com"}, nil)
	ev := store.EmailView{FromAddress: "boss@example.com"}
	build := func(headers string) *emailBody {
		raw := "From: boss@example.com\r\n" + headers + "\r\nhi\r\n"
		return buildEmailBody([]byte(raw), hashOf(raw), bodyArgs{trustedAuthservIDs: []string{"mx.provider.example"}})
	}
	kinds := func(v any) []string {
		var out []string
		if l, ok := v.([]map[string]any); ok {
			for _, w := range l {
				out = append(out, w["kind"].(string))
			}
		}
		return out
	}

	untrusted := build(hdrUntrusted + "Subject: s\r\n")
	if got := kinds(senderWarningsObject(ev, untrusted, ix)); len(got) != 1 || got[0] != "selfSpoof" {
		t.Fatalf("untrusted-only own-domain mail: warnings %v, want [selfSpoof]", got)
	}
	if untrusted.authTrusted.Present {
		t.Fatal("setup: the untrusted header was believed")
	}
	// CONTROL: a trusted pass on the same message suppresses it, so the line
	// above excludes something this setup can produce.
	trusted := build(hdrTrusted + "Subject: s\r\n")
	if got := kinds(senderWarningsObject(ev, trusted, ix)); len(got) != 0 {
		t.Fatalf("CONTROL FAILED: a trusted pass left warnings %v", got)
	}
}
