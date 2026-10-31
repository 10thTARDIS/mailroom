// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"mailroom/internal/clamd"
	"strings"
	"testing"
)

// The report's blind-spot footer is a CLAIM about its own coverage, and a report
// that overstates what it measured is the failure mode this project keeps paying
// for. THREE counters bound what it could see, and all three must be zero before
// "the counts above are over the WHOLE corpus" is a true sentence.
//
// `nested` belongs in that condition for a reason that is easy to miss, which is
// why it was left out of it once: mime.Message.Attachments deliberately EXCLUDES
// parts belonging to an embedded message/rfc822 (internal/mime/message.go), so an
// attachment inside a forwarded mail is counted nowhere above — the parser records
// it on the nested message. A corpus of forwards would report zero attachments and
// call that a complete count.
func TestAttachmentReportFooterOnlyClaimsATotalWhenItSawEverything(t *testing.T) {
	const total, bound = "all three zero", "LOWER BOUND"

	for _, tc := range []struct {
		name                          string
		unreadable, truncated, nested int
		want                          string
	}{
		{"saw everything", 0, 0, 0, total},
		{"some unreadable", 1, 0, 0, bound},
		{"some truncated", 0, 1, 0, bound},
		{"some nested", 0, 0, 1, bound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newAttachAgg()
			a.scanned, a.attachments = 10, 3
			a.unreadable, a.truncated, a.nested = tc.unreadable, tc.truncated, tc.nested

			out := captureStdout(t, func() { a.print(1, clamd.Version{}) })

			if !strings.Contains(out, tc.want) {
				t.Errorf("footer does not say %q:\n%s", tc.want, out)
			}
			// And must not say BOTH: the two sentences contradict each other, so
			// an assertion on only the wanted one could pass on output that also
			// carried its opposite.
			other := total
			if tc.want == total {
				other = bound
			}
			if strings.Contains(out, other) {
				t.Errorf("footer also says %q, contradicting %q:\n%s", other, tc.want, out)
			}
		})
	}
}

// Zero attachments is a finding worth printing — "a scanner would have nothing to
// do here" — but ONLY when the report could see the whole corpus. With anything
// unreadable, truncated or nested, zero means "none found where I could look",
// which is a weaker claim and must not be dressed as the stronger one (#155).
func TestAttachmentReportZeroIsOnlyAFindingWhenNothingWasMissed(t *testing.T) {
	clean := newAttachAgg()
	clean.scanned = 10
	out := captureStdout(t, func() { clean.print(1, clamd.Version{}) })
	if !strings.Contains(out, "That is a finding") {
		t.Errorf("a fully-seen empty corpus should report a finding:\n%s", out)
	}

	// Nested only — neither of the two counters the condition originally checked.
	blind := newAttachAgg()
	blind.scanned, blind.nested = 10, 2
	out = captureStdout(t, func() { blind.print(1, clamd.Version{}) })
	if !strings.Contains(out, "NOT a finding") {
		t.Errorf("an unseen remainder must downgrade the claim:\n%s", out)
	}
	if strings.Contains(out, "nothing to do") {
		t.Errorf("still claims a scanner has nothing to do despite %d nested:\n%s", blind.nested, out)
	}
}

// The projection census exists to tell two identical-looking numbers apart: an
// empty table because there is nothing to scan, and an empty table because
// nothing has ever written it. Both print "rows 0". Only the comparison against
// has_attachments separates them, so that is what these assert (#155, #176).
func TestProjectionReportDistinguishesEmptyFromNeverWritten(t *testing.T) {
	for _, tc := range []struct {
		name             string
		rows, withRows   int
		flagged          int
		want, mustNotSay string
	}{
		{
			// Every deployment today, and the reason this report exists.
			name: "never written", rows: 0, withRows: 0, flagged: 110,
			want: "THE PROJECTION DOES NOT EXIST", mustNotSay: "no attachments at all",
		},
		{
			// A genuinely empty corpus. Zero here IS the correct state.
			name: "nothing to scan", rows: 0, withRows: 0, flagged: 0,
			want: "no attachments at all", mustNotSay: "DOES NOT EXIST",
		},
		{
			// The dangerous middle: a scanner keyed off this table skips these.
			name: "partial", rows: 50, withRows: 40, flagged: 110,
			want: "PARTIAL", mustNotSay: "every message carrying attachments",
		},
		{
			name: "complete", rows: 239, withRows: 110, flagged: 110,
			want: "every message carrying attachments", mustNotSay: "PARTIAL",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newAttachAgg()
			a.attRows, a.attMessagesWithRows, a.flagSaysYes = tc.rows, tc.withRows, tc.flagged

			out := captureStdout(t, func() { a.printProjection(clamd.Version{}) })

			if !strings.Contains(out, tc.want) {
				t.Errorf("projection report does not say %q:\n%s", tc.want, out)
			}
			// The verdicts are mutually exclusive; saying both would be worse
			// than saying the wrong one, because a reader would believe the
			// reassuring half.
			if strings.Contains(out, tc.mustNotSay) {
				t.Errorf("projection report ALSO says %q, contradicting %q:\n%s", tc.mustNotSay, tc.want, out)
			}
			// Whatever the state, the report must never imply it covered ground
			// it cannot back.
			//
			// ⚠ THE PHRASE CHANGED IN #395 AND THE PROPERTY DID NOT. This used
			// to look for "NOT YET REPORTABLE", which was the honest thing to
			// print while the freshness columns did not exist. Migration 00043
			// created them, so the report now says something narrower and more
			// useful: with no scanner configured it has no right-hand side for
			// stamped-vs-current, and it says exactly that rather than rendering
			// "everything is up to date".
			if !strings.Contains(out, "NO SCANNER CONFIGURED") {
				t.Errorf("projection report omits the limits of what it can measure:\n%s", out)
			}
			// ⚠ And it must never resolve that absence into reassurance. This is
			// the assertion the old phrase was really protecting.
			if strings.Contains(out, "up to date\n") {
				t.Errorf("projection report claims freshness with no scanner to ask:\n%s", out)
			}
		})
	}
}
