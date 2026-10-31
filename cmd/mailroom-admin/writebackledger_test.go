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
