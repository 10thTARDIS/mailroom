// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/rules"
	"mailroom/internal/store"
)

// TestPrintChangesNamesBothIDs pins #558's first defect: a listed row named
// only the RULE's id, on every row that rule touched, where an operator
// reading the row reasonably takes it for the id of the message described on
// the line above. Resolving it against the message store then finds nothing,
// because it never was a message id.
//
// The message id and the rule id here are deliberately distinct ULIDs, so a
// row that printed the rule id under a "msg" label (or vice versa) is caught
// rather than passing by coincidence.
func TestPrintChangesNamesBothIDs(t *testing.T) {
	msgID := ulid.Make()
	ruleID := ulid.Make()
	if msgID == ruleID {
		t.Fatal("test setup: msgID and ruleID must differ")
	}

	cases := []struct {
		name string
		hand []string
	}{
		{name: "plain, no hand-filed label"},
		{name: "conflicts with a hand-filed label", hand: []string{"Shopping"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := backfillChange{
				msg: store.RulePreviewCandidate{
					ID:         msgID,
					From:       "sender@example.com",
					Subject:    "a subject",
					ReceivedAt: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
				},
				rule: rules.Rule{ID: ruleID},
				adds: "Newsletters",
				hand: tc.hand,
			}
			r := &backfillRun{showConf: 0}
			out := captureStdout(t, func() {
				r.printChanges([]backfillChange{c}, "--show-conflicts")
			})

			wantMsg := "msg " + msgID.String()
			wantRule := "rule " + ruleID.String()
			if !strings.Contains(out, wantMsg) {
				t.Errorf("output does not name the MESSAGE id at all (want %q); the row is\n"+
					"unreadable without it (#558):\n%s", wantMsg, out)
			}
			if !strings.Contains(out, wantRule) {
				t.Errorf("output does not name the rule id, labelled as such (want %q):\n%s", wantRule, out)
			}
			// The specific defect: the rule id printed where a reader takes it for
			// the message id, because nothing on the line said whose id it was.
			if strings.Contains(out, msgID.String()) && strings.Contains(out, "rule "+msgID.String()) {
				t.Errorf("the MESSAGE id is labelled as the rule id:\n%s", out)
			}
			if strings.Contains(out, "msg "+ruleID.String()) {
				t.Errorf("the RULE id is labelled as the message id:\n%s", out)
			}
		})
	}
}

// TestReportFooterUndoHintStatesAllTimeScope pins #558's second defect: the
// footer printed after an --apply run reads as "same command with --undo
// --apply" reverses what THIS run just did. It does not — --undo removes
// every label these rules have EVER applied, from any run, ever. On the test
// instance a 4-message apply run was followed by an --undo that changed 1000
// messages (the --max-mutations cap), which is exactly the gap this hint must
// close before recommending the flag.
func TestReportFooterUndoHintStatesAllTimeScope(t *testing.T) {
	r := &backfillRun{apply: true, mutations: 4, maxMut: 1000}
	out := captureStdout(t, r.reportFooter)

	if !strings.Contains(out, "does NOT reverse only") && !strings.Contains(out, "not only") {
		t.Errorf("hint does not disclaim reversing only this run:\n%s", out)
	}
	if !strings.Contains(out, "every label") || !strings.Contains(out, "EVER applied") {
		t.Errorf("hint does not state that --undo removes every label these rules have EVER\n"+
			"applied (the all-time scope), not just this run's 4 messages:\n%s", out)
	}
	if !strings.Contains(out, "whole mailbox") {
		t.Errorf("hint does not make the mailbox-wide scope visible:\n%s", out)
	}
}
