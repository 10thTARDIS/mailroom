// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// DISMISSING A FAILED SEND (migration 00059).
//
// A submission the provider refused for good stayed in the Outbox and the
// rail's count forever: destroy is refused (a submission is the record of a
// send), and discarding the draft only soft-trashes it. `dismissed: true` on
// EmailSubmission/set update is the act — legal only while the submission is
// 'failed', appending the EmailSubmission change row so every device's Outbox
// drops it.

import (
	"context"
	"testing"
)

// submitAndSetState creates a real submission through the API, then forces its
// stored state (the send worker is not running here) and returns its id.
func submitAndSetState(t *testing.T, h *composeHarness, a apiFixture, subject, state string) string {
	t.Helper()
	draft := draftFor(t, h, a, "me@example.com", subject)
	res := call(t, h.api.submissionSet, a.UserID, map[string]any{
		"create": map[string]any{"s": map[string]any{"emailId": draft}},
	})
	obj, ok := res["created"].(map[string]any)["s"].(map[string]any)
	if !ok {
		t.Fatalf("submit refused: %#v", res)
	}
	id, _ := obj["id"].(string)
	if _, err := h.super.Exec(context.Background(),
		`UPDATE app.submissions SET state = $2, smtp_response = '550 5.7.1 message rejected' WHERE id = $1`,
		id, state); err != nil {
		t.Fatalf("set state %s: %v", state, err)
	}
	return id
}

func dismiss(t *testing.T, h *composeHarness, a apiFixture, id string, val any) map[string]any {
	t.Helper()
	return call(t, h.api.submissionSet, a.UserID, map[string]any{
		"update": map[string]any{id: map[string]any{"dismissed": val}},
	})
}

func submissionChanges(t *testing.T, h *composeHarness, a apiFixture, id string) int {
	t.Helper()
	return sendCount(t, h, `SELECT count(*) FROM app.change_log
		 WHERE user_id = $1 AND object_type = 'EmailSubmission' AND object_id = $2`, a.UserID.String(), id)
}

func TestDismissAFailedSubmission(t *testing.T) {
	h := newComposeHarness(t)
	a, _ := seedTwo(t, h.super)
	seedSendable(t, h.super, a, "me@example.com", "")
	id := submitAndSetState(t, h, a, "refused as spam", "failed")
	draftBefore := sendCount(t, h, `SELECT count(*) FROM app.messages m JOIN app.submissions s
		 ON s.message_id = m.id WHERE s.id = $1 AND m.trashed_at IS NULL`, id)
	before := submissionChanges(t, h, a, id)

	res := dismiss(t, h, a, id, true)
	if _, ok := res["updated"].(map[string]any)[id]; !ok {
		t.Fatalf("dismissing a failed submission was refused: %#v", res)
	}
	if got := submissionChanges(t, h, a, id) - before; got != 1 {
		t.Errorf("EmailSubmission change rows added = %d, want exactly 1 (the invariant)", got)
	}
	got := call(t, h.api.submissionGet, a.UserID, map[string]any{"ids": []string{id}})
	obj, _ := got["list"].([]any)[0].(map[string]any)
	if v, _ := obj["dismissedAt"].(string); v == "" {
		t.Errorf("EmailSubmission.dismissedAt = %#v after dismissing, want a UTCDate", obj["dismissedAt"])
	}
	if obj["mailroomState"] != "failed" {
		t.Errorf("mailroomState = %v after dismissing, want failed — dismissing hides it, it does not rewrite it", obj["mailroomState"])
	}
	// The draft is untouched: Edit must still be possible.
	if after := sendCount(t, h, `SELECT count(*) FROM app.messages m JOIN app.submissions s
		 ON s.message_id = m.id WHERE s.id = $1 AND m.trashed_at IS NULL`, id); after != draftBefore || after != 1 {
		t.Errorf("the draft behind the dismissed send: %d live before, %d after, want 1 and 1", draftBefore, after)
	}

	// Again: already dismissed is a no-op, not an error and not a second row.
	res = dismiss(t, h, a, id, true)
	if _, ok := res["updated"].(map[string]any)[id]; !ok {
		t.Errorf("re-dismissing was refused: %#v", res)
	}
	if got := submissionChanges(t, h, a, id) - before; got != 1 {
		t.Errorf("after a second dismiss, change rows added = %d, want still 1", got)
	}
}

// THE NEGATIVE CONTROL: only a FAILED send may be dismissed. A pending one is
// still going (cancel it instead), a sent one is not in the Outbox. Each is
// refused, stays undismissed, and appends nothing.
func TestDismissRefusesAnythingButAFailedSubmission(t *testing.T) {
	h := newComposeHarness(t)
	a, _ := seedTwo(t, h.super)
	seedSendable(t, h.super, a, "me@example.com", "")

	for _, state := range []string{"scheduled", "sending", "sent", "canceled"} {
		id := submitAndSetState(t, h, a, "state "+state, state)
		before := submissionChanges(t, h, a, id)
		res := dismiss(t, h, a, id, true)
		se, ok := res["notUpdated"].(map[string]any)[id].(map[string]any)
		if !ok || se["type"] != setInvalidProps {
			t.Errorf("%s: want notUpdated %s, got %#v", state, setInvalidProps, res)
		}
		if n := sendCount(t, h, `SELECT count(*) FROM app.submissions WHERE id = $1 AND dismissed_at IS NOT NULL`, id); n != 0 {
			t.Errorf("%s: dismissed_at was set on a %s submission", state, state)
		}
		if got := submissionChanges(t, h, a, id) - before; got != 0 {
			t.Errorf("%s: change rows added = %d for a refused dismiss, want 0", state, got)
		}
	}

	// `false` is refused, not ignored: there is no un-dismiss.
	id := submitAndSetState(t, h, a, "false is refused", "failed")
	res := dismiss(t, h, a, id, false)
	if _, ok := res["notUpdated"].(map[string]any)[id]; !ok {
		t.Errorf("dismissed:false was accepted: %#v", res)
	}
}
