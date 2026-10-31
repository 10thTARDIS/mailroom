// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"mailroom/internal/ml"
	"mailroom/internal/store"
)

func captureAuthserv(a *authservAggregate) string {
	var sb strings.Builder
	old := reportOut
	reportOut = &sb
	defer func() { reportOut = old }()
	a.print()
	return sb.String()
}

func refAt(month string, local bool) store.MessageAuthRef {
	t, _ := time.Parse("2006-01", month)
	return store.MessageAuthRef{ReceivedAt: t.Add(48 * time.Hour), LocallyAuthored: local, ProviderDated: true}
}

// The two hypotheses this report exists to tell apart, each built as it would
// look: a migrated block of months topped by a FOREIGN provider's id, and a
// recent month topped by a SIBLING MX the trust list does not name.
func TestAuthservReportSeparatesMigrationFromASiblingMX(t *testing.T) {
	a := newAuthservAggregate([]string{"MX13.Provider.example"})
	trusted := "mx13.provider.example; dmarc=pass header.from=example.com"
	sibling := "mx12.provider.example; dmarc=pass header.from=example.com"
	foreign := "mx.old-provider.example; spf=pass"
	a.scanned = 7
	a.add(refAt("2025-03", false), []string{foreign}, false, false, nil)
	a.add(refAt("2025-03", false), []string{foreign, "relay.old-provider.example 1; dkim=pass"}, false, false, nil)
	a.add(refAt("2026-10", false), []string{trusted}, false, false, nil)
	a.add(refAt("2026-10", false), []string{sibling}, false, false, nil)
	// Trusted, but NOT topmost: a pass the topmost-only parse ignores.
	a.add(refAt("2026-10", false), []string{sibling, trusted}, false, false, nil)
	a.add(refAt("2026-10", true), nil, false, false, nil)
	a.add(refAt("2026-10", false), []string{"; garbage"}, false, false, nil)

	if r := a.ids["mx13.provider.example"]; r == nil || r.anywhere != 2 || r.topmost != 1 {
		t.Fatalf("trusted id row = %+v", r)
	}
	if r := a.ids["mx12.provider.example"]; r == nil || r.anywhere != 2 || r.topmost != 2 {
		t.Fatalf("sibling row = %+v", r)
	}
	if r := a.ids[unreadableID]; r == nil || r.topmost != 1 || r.anywhere != 1 {
		t.Fatalf("an unreadable authserv-id must be counted, not dropped: %+v", r)
	}
	old, now := a.months["2025-03"], a.months["2026-10"]
	if old.untrustedOnly != 2 || old.trustedAny != 0 || old.topIDs["mx.old-provider.example"] != 2 {
		t.Fatalf("migrated month = %+v", old)
	}
	if now.messages != 5 || now.noHeader != 1 || now.noHeaderLocal != 1 || now.trustedTop != 1 ||
		now.trustedAny != 2 || now.untrustedOnly != 2 {
		t.Fatalf("current month = %+v", now)
	}

	out := captureAuthserv(a)
	for _, want := range []string{
		"mx13.provider.example", "YES", "mx12.provider.example", "mx.old-provider.example",
		"2025-03", "2026-10", "no Authentication-Results at all : 1 of 7 readable (14.3%), of which Mailroom wrote 1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q:\n%s", want, out)
		}
	}
	// Hostnames, never addresses: header.from domains and senders stay out.
	if strings.Contains(out, "@") || strings.Contains(out, "example.com") {
		t.Fatalf("the report printed something from a header body:\n%s", out)
	}
}

func TestAuthservReportBoundsTheIDTableAndSaysSo(t *testing.T) {
	a := newAuthservAggregate(nil)
	for i := 0; i < maxAuthservRows+5; i++ {
		a.scanned++
		a.add(refAt("2026-10", false), []string{fmt.Sprintf("mx%d.example.net; spf=pass", i)}, false, false, nil)
	}
	out := captureAuthserv(a)
	if !strings.Contains(out, "… 5 more authserv-ids not shown (topmost on 5 messages)") {
		t.Fatalf("the bound is not reported:\n%s", out)
	}
	if !strings.Contains(out, "NONE configured") {
		t.Fatalf("no-trust case not stated:\n%s", out)
	}
}

func refOn(day string, local bool) store.MessageAuthRef {
	t, _ := time.Parse("2006-01-02", day)
	return store.MessageAuthRef{ReceivedAt: t.Add(13 * time.Hour), LocallyAuthored: local, ProviderDated: true}
}

// #766: first and last TOPMOST date per id, exact UTC dates, and notice counts
// per category under the configured (date-bounded) rule.
func TestAuthservReportDatesAndNoticeCategories(t *testing.T) {
	a := newAuthservAggregate([]string{"mx.provider.example", "old.provider.example<2026-07-01"})
	old := "old.provider.example; dmarc=pass header.from=example.com"
	cur := "mx.provider.example; dmarc=pass header.from=example.com"
	relay := "mta10.provider.example; spf=pass"
	a.scanned = 9
	a.add(refOn("2026-01-05", false), []string{old}, false, false, nil)   // trusted (before bound)
	a.add(refOn("2026-06-30", false), []string{old}, false, false, nil)   // trusted, last day inside
	a.add(refOn("2026-07-01", false), []string{old}, false, false, nil)   // forged-looking: bound is exclusive -> still unauthenticated
	a.add(refOn("2026-08-01", false), []string{cur}, false, false, nil)   // trusted
	a.add(refOn("2026-08-02", false), []string{relay}, false, false, nil) // untrusted-only: still unauthenticated
	a.add(refOn("2026-08-03", false), nil, false, false, nil)             // unauthenticated
	a.add(refOn("2026-08-04", false), nil, true, false, nil)              // self (Sent/Drafts)
	a.add(refOn("2026-08-05", true), nil, false, false, nil)              // self (locally authored)
	a.add(refOn("2026-08-06", true), []string{relay}, true, false, nil)   // self wins over an untrusted header

	if got := a.notices[ml.NoticeTrusted]; got != 3 {
		t.Errorf("trusted = %d, want 3", got)
	}
	// The untrusted-only messages (old id at the bound, relay) still show the
	// notice: 1 header-less + 2 untrusted-only inbound. The self one with an
	// untrusted header is self.
	if got := a.notices[ml.NoticeUnauthenticated]; got != 3 {
		t.Errorf("unauthenticated = %d, want 3", got)
	}
	if a.untrustedOnly != 3 {
		t.Errorf("report-only untrustedOnly = %d, want 3", a.untrustedOnly)
	}
	if got := a.notices[ml.NoticeSelf]; got != 3 {
		t.Errorf("self = %d, want 3", got)
	}
	if got := a.notices[ml.NoticeUnconfigured]; got != 0 {
		t.Errorf("unconfigured = %d, want 0", got)
	}
	r := a.ids["old.provider.example"]
	if topDate(r.firstTop) != "2026-01-05" || topDate(r.lastTop) != "2026-07-01" {
		t.Errorf("old id topmost dates = %s..%s", topDate(r.firstTop), topDate(r.lastTop))
	}

	out := captureAuthserv(a)
	for _, want := range []string{
		"FIRST-TOP", "LAST-TOP", "2026-01-05", "2026-07-01", "before 2026-07-01",
		"trusted ", "unauthenticated", "self", "unconfigured", "total",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q:\n%s", want, out)
		}
	}
}

// The no-id deployment lands every message in its own category, never in the
// silenced or unauthenticated ones.
func TestAuthservReportCountsUnconfiguredSeparately(t *testing.T) {
	a := newAuthservAggregate(nil)
	a.scanned = 2
	a.add(refOn("2026-08-03", false), nil, false, false, nil)
	a.add(refOn("2026-08-03", false), []string{"x.example; spf=pass"}, false, false, nil)
	if a.notices[ml.NoticeUnconfigured] != 2 || len(a.notices) != 1 {
		t.Fatalf("notices = %v, want only unconfigured=2", a.notices)
	}
}

// #766: no-AR messages split into exactly four rows, and From matching an
// identity is NOT self (a spoofer writes From).
func TestAuthservReportSplitsNoARAndFromIsNotSelf(t *testing.T) {
	a := newAuthservAggregate([]string{"mx.provider.example"})
	a.scanned = 5
	a.add(refOn("2026-08-01", true), nil, false, false, nil)  // locally authored
	a.add(refOn("2026-08-02", false), nil, true, false, nil)  // Sent/Drafts
	a.add(refOn("2026-08-03", false), nil, false, true, nil)  // From an identity, inbound
	a.add(refOn("2026-08-04", false), nil, false, false, nil) // other inbound
	a.add(refOn("2026-08-05", false), nil, false, true, nil)  // From an identity, inbound
	want := map[string]int{noARLocal: 1, noARSentDrafts: 1, noARFromOwn: 2, noAROther: 1}
	for k, n := range want {
		if a.noAR[k] != n {
			t.Errorf("noAR[%s] = %d, want %d", k, a.noAR[k], n)
		}
	}
	// The notice treats the spoof-able From as inbound: 3 unauthenticated.
	if got := a.notices[ml.NoticeUnauthenticated]; got != 3 {
		t.Errorf("unauthenticated = %d, want 3 (From matching an identity is not self)", got)
	}
	if got := a.notices[ml.NoticeSelf]; got != 2 {
		t.Errorf("self = %d, want 2", got)
	}
}

// #766: a row the provider did not date (archive import) is judged at the zero
// time, so a bounded entry never trusts it, whatever received_at says.
func TestAuthservReportUndatedRowsTrustNoBoundedEntry(t *testing.T) {
	a := newAuthservAggregate([]string{"old.provider.example<2099-01-01"})
	ref := refOn("2026-01-12", false)
	hdr := []string{"old.provider.example; dmarc=pass header.from=example.com"}
	a.add(ref, hdr, false, false, nil)
	ref.ProviderDated = false
	a.add(ref, hdr, false, false, nil)
	if a.notices[ml.NoticeTrusted] != 1 || a.notices[ml.NoticeUnauthenticated] != 1 {
		t.Fatalf("notices = %v, want trusted=1 (provider-dated) and unauthenticated=1 (undated)", a.notices)
	}
}
