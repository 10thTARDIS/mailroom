// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"strings"
	"testing"

	"mailroom/internal/store"
)

// TestWritebackShadowReasonNamesBothSwitches pins the effective-writeback test
// the writeback-ledger report and its --apply refusal now SHARE (ADR-028 §2).
//
// ⚠ THE THIRD CASE IS THE DEFECT. The report used to read the per-account
// column alone, so an account shadowed only by MAILROOM_WRITEBACK_DISABLED
// printed "LIVE (writeback enabled)" while --apply on the very same invocation
// correctly refused it — the report contradicting the command it belongs to. A
// review found it; this pins it.
//
// It is also why the reason is computed by ONE function called from both
// places. Two predicates for one decision is the exact defect three rounds of
// review were spent on elsewhere in this feature, and an operator deciding
// whether to replay a fortnight of real mail is the last person who should be
// handed two answers.
func TestWritebackShadowReasonNamesBothSwitches(t *testing.T) {
	for _, tc := range []struct {
		name       string
		column     bool
		instance   bool
		wantShadow bool
		wantNames  []string // substrings the reason must contain
	}{
		{
			name: "both on: genuinely live", column: true, instance: false,
			wantShadow: false,
		},
		{
			name: "account column off", column: false, instance: false,
			wantShadow: true, wantNames: []string{"writebackEnabled"},
		},
		{
			name: "instance flag alone, column TRUE", column: true, instance: true,
			wantShadow: true, wantNames: []string{"MAILROOM_WRITEBACK_DISABLED"},
		},
		{
			name: "both switches holding", column: false, instance: true,
			wantShadow: true,
			// Both are named: "lift the column" alone would not make this
			// account live, and an operator told only half the reason will try
			// exactly that and be confused when --apply still refuses.
			wantNames: []string{"writebackEnabled", "MAILROOM_WRITEBACK_DISABLED"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := writebackShadowReason(store.Account{WritebackEnabled: tc.column}, tc.instance)
			if shadowed := got != ""; shadowed != tc.wantShadow {
				t.Fatalf("writebackShadowReason(column=%v, instance=%v) = %q; shadowed=%v, want shadowed=%v",
					tc.column, tc.instance, got, shadowed, tc.wantShadow)
			}
			for _, want := range tc.wantNames {
				if !strings.Contains(got, want) {
					t.Errorf("reason %q does not name %q — an operator cannot tell which "+
						"switch to lift", got, want)
				}
			}
		})
	}
}

// TestLedgerReportPrintsStaleAndSupersededPerBucket: the stale and superseded
// counts appear beside every (op, source) bucket, including as zero, and the
// legend that says what they mean is always printed.
//
// ⚠ ZERO IS PRINTED, NOT OMITTED. A report that only mentions "stale" when it is
// non-zero cannot be told apart from a build that never looked; the legend line
// carrying a 0 is the proof that the check ran (CLAUDE.md: "a bounded or empty
// result must report its bound").
//
// NEGATIVE CONTROL: stop printing the two columns or the legend and the
// assertions below fail.
func TestLedgerReportPrintsStaleAndSupersededPerBucket(t *testing.T) {
	out := captureStdout(t, func() {
		printWritebackLedgerBySource([]store.SuppressedIntentRow{
			{Op: "move", Source: "user", Count: 5, Stale: 2},
			{Op: "append", Source: "draft", Count: 3, Superseded: 1},
			{Op: "+kw", Source: "", Count: 7},
		})
	})
	for _, want := range []string{
		"STALE", "SUPERSEDED",
		"move       user              5        2           0",
		"append     draft             3        0           1",
		"+kw        unknown           7        0           0",
		"STALE           2  intent(s)",
		"SUPERSEDED      1  append(s)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report does not contain %q:\n%s", want, out)
		}
	}
	// Zero findings still print the legend, with the zeros.
	none := captureStdout(t, func() {
		printWritebackLedgerBySource([]store.SuppressedIntentRow{{Op: "move", Source: "user", Count: 1}})
	})
	for _, want := range []string{"STALE           0  intent(s)", "SUPERSEDED      0  append(s)"} {
		if !strings.Contains(none, want) {
			t.Errorf("a clean ledger must still say it checked; missing %q:\n%s", want, none)
		}
	}
}
