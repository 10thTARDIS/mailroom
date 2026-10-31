// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// #842's API half: Triage/open records the explicit open signal once per
// message, and a mark-read — what the web client sends when a message is
// opened — records NOTHING any more. Cross-user isolation of Triage/open is in
// rls_integration_test.go.

import (
	"testing"
)

func feedbackCount(t *testing.T, h *rlsHarness, msgID, action string) int {
	t.Helper()
	var n int
	superScan(t, h, &n, `SELECT count(*) FROM app.feedback_events WHERE message_id=$1 AND action=$2`, msgID, action)
	return n
}

func TestTriageOpenRecordsOncePerMessage(t *testing.T) {
	h := newRLSHarness(t)
	u := seedTriageUser(t, h)
	id := u.MessageID.String()

	if n := feedbackCount(t, h, id, "open"); n != 0 {
		t.Fatalf("precondition: %d open rows before any call", n)
	}
	for i := 0; i < 2; i++ {
		res, mErr := invokeAs(t, h, "Triage/open", u.UserID, map[string]any{"emailIds": []string{id}})
		if mErr != nil {
			t.Fatalf("Triage/open #%d: %+v", i+1, mErr)
		}
		if !strInArr(res["opened"], id) {
			t.Errorf("Triage/open #%d did not report the message opened: %v", i+1, res)
		}
	}
	if n := feedbackCount(t, h, id, "open"); n != 1 {
		t.Errorf("open rows after two calls = %d, want exactly 1 (once per message)", n)
	}

	// An unknown id is refused, not recorded.
	res, mErr := invokeAs(t, h, "Triage/open", u.UserID, map[string]any{"emailIds": []string{"not-a-ulid"}})
	if mErr != nil {
		t.Fatalf("Triage/open (bad id): %+v", mErr)
	}
	if !mapHasKey(res, "notOpened", "not-a-ulid") {
		t.Errorf("a malformed id was not refused: %v", res)
	}
}

// The web client marks a message read when it is opened. That used to record
// mark_read_no_open, a NEGATIVE, on every message the user read. It must record
// nothing now.
//
// NEGATIVE CONTROL in the same test: a flag through the same Email/set seam
// DOES record a row, so a zero above is the seam staying quiet on a mark-read,
// not the seam being dead or the probe unable to see a write.
func TestMarkReadRecordsNoFeedback(t *testing.T) {
	h := newRLSHarness(t)
	u := seedTriageUser(t, h)
	id := u.MessageID.String()

	if _, mErr := invokeAs(t, h, "Email/set", u.UserID, map[string]any{
		"update": map[string]any{id: map[string]any{"keywords/$seen": true}},
	}); mErr != nil {
		t.Fatalf("Email/set seen: %+v", mErr)
	}
	var all int
	superScan(t, h, &all, `SELECT count(*) FROM app.feedback_events WHERE message_id=$1`, id)
	if all != 0 {
		t.Errorf("a mark-read recorded %d feedback rows, want 0", all)
	}
	if n := feedbackCount(t, h, id, "mark_read_no_open"); n != 0 {
		t.Errorf("mark_read_no_open rows = %d, want 0", n)
	}

	if _, mErr := invokeAs(t, h, "Email/set", u.UserID, map[string]any{
		"update": map[string]any{id: map[string]any{"keywords/$flagged": true}},
	}); mErr != nil {
		t.Fatalf("Email/set flag: %+v", mErr)
	}
	if n := feedbackCount(t, h, id, "flag"); n != 1 {
		t.Fatalf("control: a flag through the same seam recorded %d rows, want 1 — the probe cannot see writes", n)
	}
}
