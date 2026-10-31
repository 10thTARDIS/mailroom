// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"strings"
	"testing"

	"mailroom/internal/ml"
	"mailroom/internal/store"
)

// capture runs print and returns what it wrote, so the assertions below are
// about the REPORT rather than about the counters behind it.
//
// ⚠ THAT DISTINCTION IS THE REASON THIS FILE EXISTS. Twice today a report in
// this package computed a figure correctly, summed it correctly, and printed it
// nowhere (#316's per-block attribution, and this command's own ABSENT split).
// Neither errored; both runs looked complete. A test on the counters would have
// passed in both cases.
func captureAuthReport(a *authAggregate) string {
	var sb strings.Builder
	old := reportOut
	reportOut = &sb
	defer func() { reportOut = old }()
	a.print(3)
	return sb.String()
}

func authRef(local, storedFail bool, from string) store.MessageAuthRef {
	return store.MessageAuthRef{FromAddress: from, LocallyAuthored: local, StoredAuthFail: storedFail}
}

// The ABSENT split is the design constraint increment 0 exists to establish:
// locally authored mail has no Authentication-Results by construction, so it is
// not unverified mail and a badge must not flag it.
func TestAbsentIsSplitByLocallyAuthoredAndPrinted(t *testing.T) {
	a := newAuthAggregate()
	a.scanned = 3
	none := ml.AuthResultsOf(nil) // no method token at all

	a.add(none, authRef(true, false, "me@example.com"), 3)    // our own Sent copy
	a.add(none, authRef(false, false, "them@example.com"), 3) // genuinely unauthenticated
	a.add(ml.AuthResultsOf([]string{"mx; spf=pass; dkim=pass; dmarc=pass"}),
		authRef(false, false, "ok@example.com"), 3)

	if a.absent != 2 || a.absentLocal != 1 || a.absentRemote != 1 {
		t.Fatalf("absent=%d local=%d remote=%d, want 2/1/1", a.absent, a.absentLocal, a.absentRemote)
	}
	if a.present != 1 {
		t.Fatalf("present=%d, want 1", a.present)
	}

	out := captureAuthReport(a)
	for _, want := range []string{
		"Mailroom itself wrote", "arrived from outside",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the report never printed %q — the split was computed and not shown, which "+
				"is the exact failure this test exists for:\n%s", want, out)
		}
	}
	// The example senders must come from the REMOTE side only: listing our own
	// address as "a sender with no auth header" is the misreading the split fixes.
	if strings.Contains(out, "me@example.com") {
		t.Errorf("a locally authored sender appeared in the example list:\n%s", out)
	}
	if !strings.Contains(out, "them@example.com") {
		t.Errorf("the genuinely unauthenticated sender is missing from the examples:\n%s", out)
	}
}

// A DKIM-only failure is invisible in auth_fail by design (the rule needs
// DMARC=fail, or SPF and DKIM failing together). The corpus has three of them,
// so the report has to show that population or increment 1 would be built on a
// number that says zero failures while three messages failed DKIM.
func TestDkimOnlyFailureIsVisibleEvenThoughTheVerdictIsClean(t *testing.T) {
	a := newAuthAggregate()
	a.scanned = 1
	a.add(ml.AuthResultsOf([]string{"mx; spf=pass; dkim=fail; dmarc=pass"}),
		authRef(false, false, "x@example.com"), 3)

	if a.failed["dkim"] != 1 {
		t.Fatalf("failed[dkim] = %d, want 1", a.failed["dkim"])
	}
	if a.clearlyFailed != 0 {
		t.Fatalf("clearlyFailed = %d, want 0 — a DKIM-only failure is not a clear failure "+
			"under the documented rule", a.clearlyFailed)
	}
	out := captureAuthReport(a)
	if !strings.Contains(out, "dkim") || !strings.Contains(out, "fail=1") {
		t.Errorf("the per-method table did not show the DKIM failure:\n%s", out)
	}
}

// A divergence between app.messages.auth_fail and a live parse of the same
// header means one of them is wrong. It must be counted and announced, not
// inferred by a reader comparing two totals.
func TestStoredVersusLiveDisagreementIsCountedAndAnnounced(t *testing.T) {
	a := newAuthAggregate()
	a.scanned = 1
	a.storedAuthFail = 1
	// Stored says "failed"; the header plainly passes.
	a.add(ml.AuthResultsOf([]string{"mx; spf=pass; dkim=pass; dmarc=pass"}),
		authRef(false, true, "x@example.com"), 3)

	if a.disagreeStored != 1 {
		t.Fatalf("disagreeStored = %d, want 1", a.disagreeStored)
	}
	out := captureAuthReport(a)
	if !strings.Contains(out, "DISAGREEMENTS") {
		t.Errorf("a stored-vs-live divergence was not announced:\n%s", out)
	}
}

// A per-method table of zeros over a corpus with no auth headers at all says
// nothing about the parser, so the report must say so rather than let the zeros
// read as "everything passed".
func TestAllAbsentCorpusReportsItsOwnBound(t *testing.T) {
	a := newAuthAggregate()
	a.scanned = 2
	none := ml.AuthResultsOf(nil)
	a.add(none, authRef(false, false, "a@example.com"), 3)
	a.add(none, authRef(false, false, "b@example.com"), 3)

	out := captureAuthReport(a)
	if !strings.Contains(out, "NOT ONE") {
		t.Errorf("a corpus with no auth tokens at all did not report that bound — its zeros "+
			"would read as passes:\n%s", out)
	}
}
