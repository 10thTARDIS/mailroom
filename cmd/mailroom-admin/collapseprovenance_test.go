// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"strings"
	"testing"

	"mailroom/internal/store"
)

// #859: an EMPTY report must say what it could not see, not just print zeros —
// "no change_log history" and "no collapse ever ran" are different findings.
func TestCollapseProvenanceReportStatesItsBound(t *testing.T) {
	var b bytes.Buffer
	writeCollapseProvenanceReport(&b, store.CollapseProvenanceReport{UserRowsNoRule: 12, UserRowsNoRuleBeforeHorizon: 12}, 5)
	out := b.String()
	for _, want := range []string{
		"change_log holds NO Email history for this user, so this evidence sees nothing at all.",
		"'user' rows without a rule id             : 12",
		"this command cannot say which, and does not guess.",
		"Repairable ('user' -> 'rule'): 0 row(s)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q:\n%s", want, out)
		}
	}
}

func TestCollapseProvenanceReportListsBoundedRows(t *testing.T) {
	var rows []store.CollapseProvenanceRow
	for i := 0; i < 3; i++ {
		rows = append(rows, store.CollapseProvenanceRow{MessageID: "m", LabelID: "l", RuleID: "r"})
	}
	var b bytes.Buffer
	writeCollapseProvenanceReport(&b, store.CollapseProvenanceReport{
		Repairable:        rows,
		RuleIDByAppliedBy: map[string]int{"user": 3, "learned": 1},
	}, 2)
	out := b.String()
	if strings.Count(out, "message m  label l  rule r") != 2 || !strings.Contains(out, "… 1 more not listed") {
		t.Errorf("the listing must stop at -show and say how many it left out:\n%s", out)
	}
	if !strings.Contains(out, "applied_by learned  : 1  <- not produced by a collapse; reported, never changed") {
		t.Errorf("a non-user rule-id bucket must be reported as untouched:\n%s", out)
	}
}
